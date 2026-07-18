package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestActiveMediaCopiesTracksInflightCopy(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	if server.ActiveMediaCopies() != 0 {
		t.Fatalf("initial ActiveMediaCopies=%d", server.ActiveMediaCopies())
	}

	// Block the copy until we observe the inflight counter.
	started := make(chan struct{})
	release := make(chan struct{})
	src := &blockingMediaReader{started: started, release: release, payload: []byte("media")}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	req.Header.Set("Range", "bytes=0-")
	writer := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		server.copyMediaReaderOrAbort(writer, req, "/Videos/item/stream", src, int64(len(src.payload)), http.StatusOK, &Session{})
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("copy did not start")
	}
	if got := server.ActiveMediaCopies(); got != 1 {
		t.Fatalf("during copy ActiveMediaCopies=%d want 1", got)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("copy did not finish")
	}
	if got := server.ActiveMediaCopies(); got != 0 {
		t.Fatalf("after copy ActiveMediaCopies=%d want 0", got)
	}
}

func TestMediaGateBlocksReconfigureWhileCopyActive(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)

	// Hold a shared media-copy lock (simulates concurrent RLock copies).
	server.beginMediaCopy()
	if got := server.ActiveMediaCopies(); got != 1 {
		t.Fatalf("ActiveMediaCopies=%d want 1", got)
	}

	// Non-force reconfigure must fail immediately while a copy holds RLock.
	if _, err := server.TryAcquireReconfigure(false); !errors.Is(err, ErrActiveMedia) {
		t.Fatalf("TryAcquireReconfigure(false) err=%v want ErrActiveMedia", err)
	}

	// Force reconfigure waits for copies; run it in a goroutine and release the copy.
	type acquireResult struct {
		release func()
		err     error
	}
	acquired := make(chan acquireResult, 1)
	go func() {
		rel, err := server.TryAcquireReconfigure(true)
		acquired <- acquireResult{release: rel, err: err}
	}()

	// Give the force path a moment to block on Lock.
	select {
	case res := <-acquired:
		if res.release != nil {
			res.release()
		}
		t.Fatalf("force reconfigure acquired while copy still held: err=%v", res.err)
	case <-time.After(50 * time.Millisecond):
	}

	server.endMediaCopy()

	var res acquireResult
	select {
	case res = <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("force reconfigure did not acquire after copy released")
	}
	if res.err != nil {
		t.Fatalf("TryAcquireReconfigure(true) err=%v", res.err)
	}

	// While exclusive reconfigure holds the gate, new non-force attempts fail.
	if _, err := server.TryAcquireReconfigure(false); !errors.Is(err, ErrActiveMedia) {
		t.Fatalf("second TryAcquireReconfigure(false) while held: err=%v", err)
	}

	res.release()

	// After release, non-force reconfigure succeeds.
	rel, err := server.TryAcquireReconfigure(false)
	if err != nil {
		t.Fatalf("TryAcquireReconfigure(false) after drain: %v", err)
	}
	rel()

	if got := server.ActiveMediaCopies(); got != 0 {
		t.Fatalf("final ActiveMediaCopies=%d want 0", got)
	}
}

func TestMediaGateConcurrentCopiesBlockTryLock(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)

	const n = 4
	started := make(chan struct{}, n)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.beginMediaCopy()
			started <- struct{}{}
			<-release
			server.endMediaCopy()
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("copy did not start")
		}
	}
	if got := server.ActiveMediaCopies(); got != n {
		t.Fatalf("ActiveMediaCopies=%d want %d", got, n)
	}
	if _, err := server.TryAcquireReconfigure(false); !errors.Is(err, ErrActiveMedia) {
		t.Fatalf("TryLock while %d copies: err=%v", n, err)
	}
	close(release)
	wg.Wait()
	rel, err := server.TryAcquireReconfigure(false)
	if err != nil {
		t.Fatalf("after copies done: %v", err)
	}
	rel()
}

type blockingMediaReader struct {
	started chan struct{}
	release chan struct{}
	payload []byte
	once    bool
	off     int
}

func (r *blockingMediaReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		close(r.started)
		<-r.release
	}
	if r.off >= len(r.payload) {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.off:])
	r.off += n
	return n, nil
}

func TestCopyMediaBodyUsesExplicitLoopAndTracksExactLength(t *testing.T) {
	payload := bytes.Repeat([]byte("media"), 7000)
	reader := &fastPathReader{Reader: bytes.NewReader(payload)}
	writer := &fastPathWriter{}
	result := copyMediaBody(writer, reader, int64(len(payload)))
	if result.Err != nil || result.BytesRead != int64(len(payload)) || result.BytesWritten != int64(len(payload)) || result.Direction != "" || !bytes.Equal(writer.Bytes(), payload) {
		t.Fatal("media copy did not complete with exact accounting")
	}
}

func TestCopyMediaBodyClassifiesShortWriteAndUpstreamFailures(t *testing.T) {
	payload := bytes.Repeat([]byte("media"), 4000)
	shortWriter := &shortMediaWriter{written: len(payload) / 2}
	short := copyMediaBody(shortWriter, bytes.NewReader(payload), int64(len(payload)))
	if !errors.Is(short.Err, io.ErrShortWrite) || short.Direction != mediaDirectionDownstream || short.BytesWritten != int64(len(payload)/2) || shortWriter.calls != 1 {
		t.Fatal("short write was not classified as downstream")
	}
	partialErr := errors.New("write failed")
	partialWriter := &partialErrorMediaWriter{err: partialErr}
	partial := copyMediaBody(partialWriter, bytes.NewBufferString("media"), 5)
	if !errors.Is(partial.Err, partialErr) || partial.Direction != mediaDirectionDownstream || partial.BytesWritten != 2 || partialWriter.calls != 1 {
		t.Fatal("partial write error was not accounted as downstream")
	}
	readFailure := copyMediaBody(io.Discard, errorMediaReader{err: errors.New("upstream failure")}, -1)
	if readFailure.Err == nil || readFailure.Direction != mediaDirectionUpstream {
		t.Fatal("read failure was not classified as upstream")
	}
	truncated := copyMediaBody(io.Discard, bytes.NewBufferString("short"), 10)
	if !errors.Is(truncated.Err, io.ErrUnexpectedEOF) || truncated.Direction != mediaDirectionUpstream {
		t.Fatal("truncated response was not classified as upstream")
	}
}

func TestCopyMediaBodyDoesNotWriteBeyondExpectedLength(t *testing.T) {
	writer := &fastPathWriter{}
	result := copyMediaBody(writer, bytes.NewBufferString("media"), 3)
	if !errors.Is(result.Err, errMediaLengthMismatch) || result.Direction != mediaDirectionUpstream || result.BytesRead != 5 || result.BytesWritten != 3 || writer.String() != "med" {
		t.Fatal("overlength response was not limited to the declared length")
	}
	zero := copyMediaBody(io.Discard, bytes.NewBufferString("media"), 0)
	if !errors.Is(zero.Err, errMediaLengthMismatch) || zero.Direction != mediaDirectionUpstream || zero.BytesWritten != 0 {
		t.Fatal("declared empty response accepted body bytes")
	}
}

func TestMediaCopyFailureAuditing(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          io.ReadCloser
		length        int64
		writerErr     error
		contentType   string
		rangeHeader   string
		contentRange  string
		cancel        bool
		wantAudits    int
		wantEvent     string
		wantKind      string
		wantBytes     int64
		wantDirection string
	}{
		{name: "downstream timeout committed 200", status: http.StatusOK, body: io.NopCloser(bytes.NewBufferString("media")), length: 5, writerErr: timeoutMediaError{}, wantAudits: 1, wantEvent: "proxy_media_downstream_timeout", wantKind: "downstream_timeout", wantDirection: mediaDirectionDownstream},
		{name: "downstream timeout committed 206", status: http.StatusPartialContent, body: io.NopCloser(bytes.NewBufferString("media")), length: 5, writerErr: timeoutMediaError{}, wantAudits: 1, wantEvent: "proxy_media_downstream_timeout", wantKind: "downstream_timeout", wantDirection: mediaDirectionDownstream},
		{name: "downstream broken pipe", status: http.StatusOK, body: io.NopCloser(bytes.NewBufferString("media")), length: 5, writerErr: errors.New("broken pipe"), wantAudits: 0},
		{name: "context cancellation wins", status: http.StatusOK, body: io.NopCloser(bytes.NewBufferString("media")), length: 5, writerErr: timeoutMediaError{}, cancel: true, wantAudits: 0},
		{name: "upstream read error", status: http.StatusOK, body: io.NopCloser(errorMediaReader{err: errors.New("backend-token")}), length: -1, wantAudits: 1, wantEvent: "proxy_media_upstream_failed", wantKind: "upstream_read_error", wantDirection: mediaDirectionUpstream},
		{name: "upstream truncation", status: http.StatusPartialContent, body: io.NopCloser(bytes.NewBufferString("short")), length: 10, wantAudits: 1, wantEvent: "proxy_media_upstream_failed", wantKind: "upstream_unexpected_eof", wantBytes: 5, wantDirection: mediaDirectionUpstream},
		{name: "upstream overlength", status: http.StatusOK, body: io.NopCloser(bytes.NewBufferString("media")), length: 3, wantAudits: 1, wantEvent: "proxy_media_upstream_failed", wantKind: "upstream_length_mismatch", wantBytes: 3, wantDirection: mediaDirectionUpstream},
		{name: "videos range empty content type downstream timeout", status: http.StatusOK, body: io.NopCloser(bytes.NewBufferString("media")), length: 5, writerErr: timeoutMediaError{}, contentType: "", rangeHeader: "bytes=0-", wantAudits: 1, wantEvent: "proxy_media_downstream_timeout", wantKind: "downstream_timeout", wantDirection: mediaDirectionDownstream},
		{name: "videos 206 unknown content type upstream truncation", status: http.StatusPartialContent, body: io.NopCloser(bytes.NewBufferString("short")), length: 10, contentType: "application/unknown", wantAudits: 1, wantEvent: "proxy_media_upstream_failed", wantKind: "upstream_unexpected_eof", wantBytes: 5, wantDirection: mediaDirectionUpstream},
		{name: "videos content range unknown content type upstream truncation", status: http.StatusOK, body: io.NopCloser(bytes.NewBufferString("short")), length: 10, contentType: "application/unknown", contentRange: "bytes 0-4/10", wantAudits: 1, wantEvent: "proxy_media_upstream_failed", wantKind: "upstream_unexpected_eof", wantBytes: 5, wantDirection: mediaDirectionUpstream},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{}, store)
			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil).WithContext(ctx)
			req.Header.Set("Range", tt.rangeHeader)
			writer := &mediaFailureWriter{err: tt.writerErr}
			contentType := tt.contentType
			if contentType == "" && tt.rangeHeader == "" {
				contentType = "video/mp4"
			}
			header := http.Header{}
			if contentType != "" {
				header.Set("Content-Type", contentType)
			}
			header.Set("Content-Range", tt.contentRange)
			resp := &http.Response{StatusCode: tt.status, Header: header, Body: tt.body, ContentLength: tt.length, Request: req}

			requireAbortHandler(t, func() {
				server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
			})
			if len(store.AuditLogs) != tt.wantAudits {
				t.Fatal("unexpected media failure audit count")
			}
			if tt.wantAudits == 0 {
				return
			}
			entry := store.AuditLogs[0]
			if entry.Event != tt.wantEvent || entry.Message != mediaAuditMessage(tt.wantDirection) || entry.ErrorKind != tt.wantKind || entry.Status != tt.status || entry.UpstreamStatus != tt.status || !entry.ResponseCommitted || entry.BytesTransferred != tt.wantBytes || entry.Direction != tt.wantDirection {
				t.Fatal("media failure audit fields were incorrect")
			}
		})
	}
}

func mediaAuditMessage(direction string) string {
	if direction == mediaDirectionDownstream {
		return "downstream media stream timed out"
	}
	return "upstream media stream failed"
}

func requireAbortHandler(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("media copy failure did not abort the response")
		}
	}()
	fn()
}

type fastPathReader struct{ *bytes.Reader }

func (fastPathReader) WriteTo(io.Writer) (int64, error) { panic("WriterTo must not be called") }

type fastPathWriter struct{ bytes.Buffer }

func (*fastPathWriter) ReadFrom(io.Reader) (int64, error) { panic("ReaderFrom must not be called") }

type shortMediaWriter struct {
	written int
	calls   int
}

func (w *shortMediaWriter) Write([]byte) (int, error) {
	w.calls++
	return w.written, nil
}

type partialErrorMediaWriter struct {
	err   error
	calls int
}

func (w *partialErrorMediaWriter) Write([]byte) (int, error) {
	w.calls++
	return 2, w.err
}

type errorMediaReader struct{ err error }

func (r errorMediaReader) Read([]byte) (int, error) { return 0, r.err }

type timeoutMediaError struct{}

func (timeoutMediaError) Error() string   { return "write timeout" }
func (timeoutMediaError) Timeout() bool   { return true }
func (timeoutMediaError) Temporary() bool { return true }

type mediaFailureWriter struct {
	header http.Header
	err    error
}

func (w *mediaFailureWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (*mediaFailureWriter) WriteHeader(int) {}
func (w *mediaFailureWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}
func (*mediaFailureWriter) SetWriteDeadline(time.Time) error { return nil }
