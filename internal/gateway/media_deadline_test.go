package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMediaWriteDeadlineClearedBeforeResponseHeader(t *testing.T) {
	server := NewServer(Config{}, NewMemoryStore())
	writer := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	resp := mediaDeadlineResponse(req, "video/mp4", http.StatusOK)

	server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	if len(writer.calls) < 2 || writer.calls[0] != "deadline" || writer.calls[1] != "header" {
		t.Fatal("media deadline was not cleared before the response header")
	}
}

func TestMediaWriteDeadlineResponseSelection(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		rel          string
		contentType  string
		status       int
		rangeHeader  string
		contentRange string
		want         bool
	}{
		{name: "video", method: http.MethodGet, rel: "/Videos/item/stream", contentType: "video/mp4", status: http.StatusOK, want: true},
		{name: "audio", method: http.MethodGet, rel: "/Audio/item/stream", contentType: "audio/mpeg", status: http.StatusOK, want: true},
		{name: "octet stream", method: http.MethodGet, rel: "/Items/item/Download", contentType: "application/octet-stream", status: http.StatusOK, want: true},
		{name: "videos range with empty content type", method: http.MethodGet, rel: "/Videos/item/stream", status: http.StatusOK, rangeHeader: "bytes=0-", want: true},
		{name: "videos partial with invalid content type", method: http.MethodGet, rel: "/Videos/item/stream", contentType: "application/unknown", status: http.StatusPartialContent, want: true},
		{name: "videos content range", method: http.MethodGet, rel: "/Videos/item/stream", contentType: "application/unknown", status: http.StatusOK, contentRange: "bytes 0-1/2", want: true},
		{name: "image", method: http.MethodGet, rel: "/Items/item/Images/Primary", contentType: "image/gif", status: http.StatusOK, want: false},
		{name: "json", method: http.MethodGet, rel: "/System/Info", contentType: "application/json", status: http.StatusOK, want: false},
		{name: "videos json range", method: http.MethodGet, rel: "/Videos/item/stream", contentType: "application/json", status: http.StatusOK, rangeHeader: "bytes=0-", want: false},
		{name: "videos problem json partial", method: http.MethodGet, rel: "/Videos/item/stream", contentType: "application/problem+json", status: http.StatusPartialContent, want: false},
		{name: "videos json content range", method: http.MethodGet, rel: "/Videos/item/stream", contentType: "application/json", status: http.StatusOK, contentRange: "bytes 0-1/2", want: false},
		{name: "m3u8", method: http.MethodGet, rel: "/Videos/item/master.m3u8", contentType: "application/vnd.apple.mpegurl", status: http.StatusOK, want: false},
		{name: "m3u8 path with octet stream", method: http.MethodGet, rel: "/Videos/item/master.m3u8", contentType: "application/octet-stream", status: http.StatusOK, want: false},
		{name: "head", method: http.MethodHead, rel: "/Videos/item/stream", contentType: "video/mp4", status: http.StatusOK, want: false},
		{name: "no body", method: http.MethodGet, rel: "/Videos/item/stream", contentType: "video/mp4", status: http.StatusNoContent, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewServer(Config{}, NewMemoryStore())
			writer := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
			req := httptest.NewRequest(tt.method, "http://gateway.test/emby"+tt.rel, nil)
			req.Header.Set("Range", tt.rangeHeader)
			resp := mediaDeadlineResponse(req, tt.contentType, tt.status)
			resp.Header.Set("Content-Range", tt.contentRange)

			server.writeProxyResponseWithSnapshot(writer, req, tt.rel, resp, &Session{}, upstreamRequestSnapshot{}, "", "")
			got := len(writer.deadlines) == 1
			if got != tt.want {
				t.Fatal("unexpected write deadline behavior")
			}
		})
	}
}

func TestMediaWriteDeadlineUnsupportedWriterAuditsOnceAndWritesResponse(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	for range 2 {
		writer := httptest.NewRecorder()
		server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", mediaDeadlineResponse(req, "video/mp4", http.StatusOK), &Session{}, upstreamRequestSnapshot{}, "", "")
		if writer.Code != http.StatusOK || writer.Body.Len() == 0 {
			t.Fatal("unsupported writer did not receive the media response")
		}
	}
	if len(store.AuditLogs) != 1 {
		t.Fatal("unsupported deadline warning was not limited to one audit")
	}
	entry := store.AuditLogs[0]
	if entry.Event != "media_write_deadline_clear_failed" || entry.Message != "unable to clear media stream write deadline" || entry.Status != http.StatusOK {
		t.Fatal("unsupported deadline audit was incorrect")
	}
}

func TestMediaWriteDeadlineAllowsSlowHTTP1Stream(t *testing.T) {
	testSlowMediaStream(t, false)
}

func TestMediaWriteDeadlineAllowsSlowHTTP2Stream(t *testing.T) {
	testSlowMediaStream(t, true)
}

func testSlowMediaStream(t *testing.T, tls bool) {
	t.Helper()
	payload := bytes.Repeat([]byte("media"), 4096)
	server := NewServer(Config{}, NewMemoryStore())
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := &delayedMediaReader{chunks: [][]byte{payload[:len(payload)/2], payload[len(payload)/2:]}, delay: 450 * time.Millisecond}
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"video/mp4"}}, Body: io.NopCloser(body), ContentLength: -1, Request: r}
		server.writeProxyResponseWithSnapshot(w, r, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	}))
	httpServer.Config.WriteTimeout = 150 * time.Millisecond
	httpServer.EnableHTTP2 = tls
	if tls {
		httpServer.StartTLS()
	} else {
		httpServer.Start()
	}
	defer httpServer.Close()

	client := httpServer.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Get(httpServer.URL)
	if err != nil {
		t.Fatal("slow media request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(data, payload) {
		t.Fatal("slow media stream was incomplete")
	}
	if tls && resp.ProtoMajor != 2 {
		t.Fatal("TLS media stream did not use HTTP/2")
	}
}

func mediaDeadlineResponse(req *http.Request, contentType string, status int) *http.Response {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewBufferString("media")), ContentLength: 5, Request: req}
}

type deadlineRecordingWriter struct {
	*httptest.ResponseRecorder
	calls     []string
	deadlines []time.Time
}

func (w *deadlineRecordingWriter) SetWriteDeadline(deadline time.Time) error {
	w.calls = append(w.calls, "deadline")
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineRecordingWriter) WriteHeader(statusCode int) {
	w.calls = append(w.calls, "header")
	w.ResponseRecorder.WriteHeader(statusCode)
}

type delayedMediaReader struct {
	chunks [][]byte
	delay  time.Duration
	index  int
	offset int
}

func (r *delayedMediaReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	if r.index > 0 && r.offset == 0 {
		time.Sleep(r.delay)
	}
	chunk := r.chunks[r.index]
	n := copy(p, chunk[r.offset:])
	r.offset += n
	if r.offset == len(chunk) {
		r.index++
		r.offset = 0
	}
	return n, nil
}
