package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMissingContentTypeSniffClassification(t *testing.T) {
	tests := []struct {
		name string
		body string
		json bool
	}{
		{name: "leading json whitespace", body: " \t\r\n{\"Name\":\"value\"}", json: true},
		{name: "empty", body: ""},
		{name: "whitespace only", body: " \t\r\n"},
		{name: "512 whitespace", body: strings.Repeat(" ", missingContentTypeSniffSize)},
		{name: "scalar", body: "true"},
		{name: "raw", body: "media"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, candidate, err := sniffMissingContentType(strings.NewReader(tt.body))
			if err != nil || candidate != tt.json {
				t.Fatal("missing content type sniff classification was incorrect")
			}
		})
	}
}

func TestMissingContentTypeWhitespaceBoundaryReplay(t *testing.T) {
	for _, tt := range []struct {
		name      string
		body      string
		candidate bool
	}{
		{name: "511 whitespace json", body: strings.Repeat(" ", 511) + `{"Name":"value"}`, candidate: true},
		{name: "512 whitespace raw", body: strings.Repeat(" ", 512) + `{"Name":"value"}`, candidate: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reader, candidate, err := sniffMissingContentType(strings.NewReader(tt.body))
			if err != nil || candidate != tt.candidate {
				t.Fatal("whitespace boundary classification was incorrect")
			}
			if !candidate {
				data, readErr := io.ReadAll(reader)
				if readErr != nil || string(data) != tt.body {
					t.Fatal("whitespace boundary raw replay changed bytes")
				}
			}
		})
	}
}

func TestMissingContentTypeJSONAndRawReplay(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantBody string
		json     bool
	}{
		{name: "valid json", body: ` {"Name":"value"}`, json: true},
		{name: "invalid object raw", body: `{"url":"http://backend.test/seg?api_key=backend-token"`, wantBody: `{"url":"http://backend.test/seg?api_key=backend-token"`},
		{name: "raw bytes", body: "raw-media", wantBody: "raw-media"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil)
			writer := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
			resp := missingContentTypeResponse(req, strings.NewReader(tt.body), int64(len(tt.body)))
			server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "")
			if writer.Code != http.StatusOK || len(store.AuditLogs) != 0 {
				t.Fatal("missing content type response failed")
			}
			if tt.json {
				if !strings.Contains(writer.Body.String(), `"Name":"value"`) || writer.Header().Get("Content-Length") != "" {
					t.Fatal("valid missing content type json was not rewritten locally")
				}
				return
			}
			if writer.Body.String() != tt.wantBody {
				t.Fatal("missing content type raw body was rewritten or duplicated")
			}
		})
	}
}

func TestMissingContentTypeJSONUsesGatewayLocalUserData(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	_ = store.SavePlaybackState(context.Background(), PlaybackState{GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-1", PlaybackPositionTicks: 4200})
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item-1", nil)
	writer := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
	body := " \t" + `{"Id":"item-1","UserData":{"Played":true,"PlaybackPositionTicks":9999}}`
	resp := missingContentTypeResponse(req, strings.NewReader(body), int64(len(body)))
	server.writeProxyResponse(writer, req, "/Items/item-1", resp, &Session{GatewayUserID: "u1", SyntheticUserID: "gateway-user"}, "", "")
	var item map[string]any
	if writer.Code != http.StatusOK || json.Unmarshal(writer.Body.Bytes(), &item) != nil {
		t.Fatal("missing content type json was not returned")
	}
	data, _ := item["UserData"].(map[string]any)
	if data == nil || data["Played"] != false || int(data["PlaybackPositionTicks"].(float64)) != 4200 {
		t.Fatal("missing content type json did not use gateway-local user data")
	}
}

func TestMissingContentTypeStreamsLargeRawAndDeadlineOrder(t *testing.T) {
	body := bytes.Repeat([]byte("x"), proxyJSONLimit+1)
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil)
	writer := &discardDeadlineWriter{}
	resp := missingContentTypeResponse(req, bytes.NewReader(body), int64(len(body)))
	server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "")
	if writer.status != http.StatusOK || writer.bytes != int64(len(body)) || !writer.deadlineBeforeHeader || len(store.AuditLogs) != 0 {
		t.Fatal("large raw missing content type response was not streamed")
	}
}

func TestMissingContentTypeWritesDecisiveByteBeforeNextRead(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil)
	reader := &writeBeforeNextReadReader{}
	writer := &writeBeforeNextReadWriter{reader: reader}
	resp := missingContentTypeResponse(req, reader, -1)
	server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "")
	if writer.String() != "raw" || len(store.AuditLogs) != 0 {
		t.Fatal("missing content type stream did not preserve raw ordering")
	}
}

func TestMissingContentTypeJSONCandidateLimitAndPreHeaderFailures(t *testing.T) {
	tests := []struct {
		name       string
		body       io.Reader
		ctx        context.Context
		wantStatus int
		wantKind   string
		canceled   bool
	}{
		{name: "json candidate too large", body: strings.NewReader("{" + strings.Repeat("x", proxyJSONLimit)), ctx: context.Background(), wantStatus: http.StatusBadGateway, wantKind: "upstream_read_error"},
		{name: "sniff timeout", body: errorMediaReader{err: timeoutMediaError{}}, ctx: context.Background(), wantStatus: http.StatusGatewayTimeout, wantKind: "upstream_timeout"},
		{name: "sniff canceled", body: errorMediaReader{err: errors.New("https://backend.test/?api_key=backend-token")}, ctx: canceledContext(), canceled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil).WithContext(tt.ctx)
			writer := httptest.NewRecorder()
			resp := missingContentTypeResponse(req, tt.body, -1)
			if tt.canceled {
				requireAbortHandler(t, func() { server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "") })
				if len(store.AuditLogs) != 0 || len(writer.Header()) != 0 || writer.Body.Len() != 0 {
					t.Fatal("canceled sniff wrote an audit, headers, or body")
				}
				return
			}
			server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "")
			if writer.Code != tt.wantStatus || len(store.AuditLogs) != 1 || strings.Contains(writer.Body.String(), "backend-token") {
				t.Fatal("missing content type pre-header failure response was incorrect")
			}
			entry := store.AuditLogs[0]
			if entry.Event != "proxy_read_failed" || entry.Message != "backend response read failed" || entry.ErrorKind != tt.wantKind || entry.Direction != mediaDirectionUpstream || entry.BytesTransferred != 0 || entry.UpstreamStatus != http.StatusOK || entry.ResponseCommitted || entry.Status != tt.wantStatus {
				t.Fatal("missing content type pre-header failure audit was incorrect")
			}
		})
	}
}

func TestMissingContentTypeSameCallReadErrors(t *testing.T) {
	for _, tt := range []struct {
		name      string
		body      string
		err       error
		ctx       context.Context
		canceled  bool
		preHeader bool
		wantKind  string
	}{
		{name: "raw timeout after byte", body: "r", err: timeoutMediaError{}, ctx: context.Background(), wantKind: "upstream_read_error"},
		{name: "raw generic after byte", body: "r", err: errors.New("backend read"), ctx: context.Background(), wantKind: "upstream_read_error"},
		{name: "whitespace timeout before decision", body: " ", err: timeoutMediaError{}, ctx: context.Background(), preHeader: true, wantKind: "upstream_timeout"},
		{name: "whitespace canceled before decision", body: " ", err: errors.New("backend read"), ctx: canceledContext(), canceled: true, preHeader: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil).WithContext(tt.ctx)
			writer := &mediaFailureWriter{}
			resp := missingContentTypeResponse(req, &sameCallReadError{data: []byte(tt.body), err: tt.err}, -1)
			if tt.canceled {
				requireAbortHandler(t, func() { server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "") })
				if len(store.AuditLogs) != 0 || len(writer.Header()) != 0 {
					t.Fatal("canceled whitespace sniff produced output")
				}
				return
			}
			if tt.preHeader {
				server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "")
				if len(store.AuditLogs) != 1 || store.AuditLogs[0].ErrorKind != tt.wantKind || store.AuditLogs[0].ResponseCommitted || store.AuditLogs[0].Status != http.StatusGatewayTimeout {
					t.Fatal("whitespace sniff error was not pre-header timeout")
				}
				return
			}
			requireAbortHandler(t, func() { server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "") })
			if len(store.AuditLogs) != 1 {
				t.Fatal("raw same-call read error was not audited")
			}
			entry := store.AuditLogs[0]
			if entry.ErrorKind != tt.wantKind || entry.Direction != mediaDirectionUpstream || entry.BytesTransferred != 1 || !entry.ResponseCommitted || entry.UpstreamStatus != http.StatusOK {
				t.Fatal("raw same-call read error did not preserve directional accounting")
			}
		})
	}
}

func TestMissingContentTypeSameCallEOFReplaysOnce(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil)
	writer := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
	resp := missingContentTypeResponse(req, &sameCallReadError{data: []byte("raw"), err: io.EOF}, 3)
	server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "")
	if writer.Code != http.StatusOK || writer.Body.String() != "raw" || len(store.AuditLogs) != 0 {
		t.Fatal("same-call EOF did not replay raw data exactly once")
	}
}

func TestMissingContentTypeDeadlineOrderAndDispatchCompatibility(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil)
	sequence := []string{}
	reader := &orderedRawReader{sequence: &sequence}
	writer := &orderedDeadlineWriter{sequence: &sequence}
	server.writeProxyResponse(writer, req, "/Items/item", missingContentTypeResponse(req, reader, 3), &Session{}, "", "")
	if strings.Join(sequence, ",") != "sniff,deadline,header,write" {
		t.Fatal("missing content type deadline ordering was incorrect")
	}

	non200 := missingContentTypeResponse(req, strings.NewReader("raw"), 3)
	non200.StatusCode = http.StatusPartialContent
	bufferedWriter := httptest.NewRecorder()
	server.writeProxyResponse(bufferedWriter, req, "/Items/item", non200, &Session{}, "", "")
	if bufferedWriter.Code != http.StatusPartialContent || bufferedWriter.Body.String() != "raw" {
		t.Fatal("non-200 empty content type no longer uses buffered behavior")
	}

	explicitJSON := missingContentTypeResponse(req, strings.NewReader(`{"Name":"value"}`), 16)
	explicitJSON.Header.Set("Content-Type", "application/json")
	jsonWriter := httptest.NewRecorder()
	server.writeProxyResponse(jsonWriter, req, "/Items/item", explicitJSON, &Session{}, "", "")
	if jsonWriter.Code != http.StatusOK || !strings.Contains(jsonWriter.Body.String(), `"Name":"value"`) {
		t.Fatal("explicit JSON no longer uses existing buffered path")
	}
}

func TestMissingContentTypeDirectionalCopyFailures(t *testing.T) {
	tests := []struct {
		name      string
		body      io.Reader
		length    int64
		writerErr error
		wantKind  string
		wantBytes int64
	}{
		{name: "truncation", body: strings.NewReader("raw"), length: 4, wantKind: "upstream_unexpected_eof", wantBytes: 3},
		{name: "overlength", body: strings.NewReader("raw"), length: 2, wantKind: "upstream_length_mismatch", wantBytes: 2},
		{name: "upstream read", body: &rawThenErrorReader{err: errors.New("upstream")}, length: -1, wantKind: "upstream_read_error", wantBytes: 1},
		{name: "downstream timeout", body: strings.NewReader("raw"), length: 3, writerErr: timeoutMediaError{}, wantKind: "downstream_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil)
			writer := &mediaFailureWriter{err: tt.writerErr}
			resp := missingContentTypeResponse(req, tt.body, tt.length)
			requireAbortHandler(t, func() { server.writeProxyResponse(writer, req, "/Items/item", resp, &Session{}, "", "") })
			if len(store.AuditLogs) != 1 {
				t.Fatal("missing content type copy failure was not audited")
			}
			entry := store.AuditLogs[0]
			if entry.ErrorKind != tt.wantKind || entry.UpstreamStatus != http.StatusOK || !entry.ResponseCommitted || entry.BytesTransferred != tt.wantBytes {
				t.Fatal("missing content type copy failure audit was incorrect")
			}
		})
	}
}

func TestMissingContentTypeDeadlineWarningOnlyOnce(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item", nil)
	for range 2 {
		writer := httptest.NewRecorder()
		server.writeProxyResponse(writer, req, "/Items/item", missingContentTypeResponse(req, strings.NewReader("raw"), 3), &Session{}, "", "")
		if writer.Code != http.StatusOK || writer.Body.String() != "raw" {
			t.Fatal("unsupported deadline writer did not receive raw stream")
		}
	}
	if len(store.AuditLogs) != 1 || store.AuditLogs[0].Event != "media_write_deadline_clear_failed" {
		t.Fatal("missing content type deadline warning was not limited")
	}
}

func missingContentTypeResponse(req *http.Request, body io.Reader, length int64) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(body), ContentLength: length, Request: req}
}

type discardDeadlineWriter struct {
	header               http.Header
	status               int
	bytes                int64
	deadlineBeforeHeader bool
	deadlineSet          bool
}

func (w *discardDeadlineWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *discardDeadlineWriter) WriteHeader(status int) {
	w.status = status
	w.deadlineBeforeHeader = w.deadlineSet
}
func (w *discardDeadlineWriter) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	return len(p), nil
}
func (w *discardDeadlineWriter) SetWriteDeadline(time.Time) error {
	w.deadlineSet = true
	return nil
}

type rawThenErrorReader struct {
	read bool
	err  error
}

type sameCallReadError struct {
	data []byte
	err  error
	read bool
}

func (r *sameCallReadError) Read(p []byte) (int, error) {
	if r.read {
		return 0, r.err
	}
	r.read = true
	return copy(p, r.data), r.err
}

type orderedRawReader struct {
	sequence *[]string
	read     bool
}

func (r *orderedRawReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	*r.sequence = append(*r.sequence, "sniff")
	copy(p, "raw")
	return 3, io.EOF
}

type orderedDeadlineWriter struct {
	header   http.Header
	sequence *[]string
}

func (w *orderedDeadlineWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *orderedDeadlineWriter) SetWriteDeadline(time.Time) error {
	*w.sequence = append(*w.sequence, "deadline")
	return nil
}
func (w *orderedDeadlineWriter) WriteHeader(int) { *w.sequence = append(*w.sequence, "header") }
func (w *orderedDeadlineWriter) Write(p []byte) (int, error) {
	*w.sequence = append(*w.sequence, "write")
	return len(p), nil
}

type writeBeforeNextReadReader struct {
	reads   int
	written bool
}

func (r *writeBeforeNextReadReader) Read(p []byte) (int, error) {
	r.reads++
	switch r.reads {
	case 1:
		p[0] = 'r'
		return 1, nil
	case 2:
		if !r.written {
			return 0, errors.New("second read happened before write")
		}
		p[0] = 'a'
		return 1, nil
	case 3:
		p[0] = 'w'
		return 1, nil
	default:
		return 0, io.EOF
	}
}

type writeBeforeNextReadWriter struct {
	bytes.Buffer
	reader *writeBeforeNextReadReader
}

func (w *writeBeforeNextReadWriter) Header() http.Header { return http.Header{} }
func (w *writeBeforeNextReadWriter) WriteHeader(int)     {}
func (w *writeBeforeNextReadWriter) Write(p []byte) (int, error) {
	if w.reader.reads == 1 {
		w.reader.written = true
	}
	return w.Buffer.Write(p)
}
func (*writeBeforeNextReadWriter) SetWriteDeadline(time.Time) error { return nil }

func (r *rawThenErrorReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, r.err
	}
	r.read = true
	p[0] = 'r'
	return 1, nil
}
