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

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

func TestMediaBufferServerEnabledAndDisabledResponses(t *testing.T) {
	tests := []struct {
		name          string
		enabled       bool
		status        int
		contentLength int64
	}{
		{name: "enabled 200 known", enabled: true, status: http.StatusOK, contentLength: 5},
		{name: "enabled 200 unknown", enabled: true, status: http.StatusOK, contentLength: -1},
		{name: "enabled 206 known", enabled: true, status: http.StatusPartialContent, contentLength: 5},
		{name: "enabled 206 unknown", enabled: true, status: http.StatusPartialContent, contentLength: -1},
		{name: "disabled remains synchronous", status: http.StatusOK, contentLength: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var controller *mediaBuffer
			if tt.enabled {
				controller = mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
			}
			server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}}, NewMemoryStore())
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
			body := newMediaBufferServerBody(bytes.NewBufferString("media"))
			resp := mediaBufferServerResponse(req, tt.status, tt.contentLength, body)
			owner := wrapResponseBodyOnce(resp)
			writer := httptest.NewRecorder()
			server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
			if writer.Code != tt.status || writer.Body.String() != "media" || server.ActiveMediaCopies() != 0 {
				t.Fatalf("status=%d body=%q inflight=%d", writer.Code, writer.Body.String(), server.ActiveMediaCopies())
			}
			if tt.contentLength >= 0 && writer.Header().Get("Content-Length") != "5" {
				t.Fatalf("Content-Length=%q", writer.Header().Get("Content-Length"))
			}
			if tt.contentLength < 0 && writer.Header().Get("Content-Length") != "" {
				t.Fatalf("unexpected Content-Length=%q", writer.Header().Get("Content-Length"))
			}
			if tt.enabled {
				if body.closeCount != 1 {
					t.Fatalf("enabled source closes=%d", body.closeCount)
				}
				assertMediaBufferServerIdle(t, controller)
			} else {
				if body.closeCount != 0 {
					t.Fatalf("disabled copier closed source %d times", body.closeCount)
				}
				_ = owner.Close()
			}
		})
	}
}

func TestMediaBufferServerPlainReaderPathsRemainSynchronous(t *testing.T) {
	controller := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}}, NewMemoryStore())
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	body := newMediaBufferServerBody(bytes.NewBufferString("media"))
	resp := mediaBufferServerResponse(req, http.StatusOK, 5, body)
	writer := httptest.NewRecorder()
	server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	if writer.Body.String() != "media" || body.closeCount != 0 {
		t.Fatalf("plain response body=%q closes=%d", writer.Body.String(), body.closeCount)
	}

	second := httptest.NewRecorder()
	server.copyMediaReaderOrAbort(second, req, "/Videos/item/stream", bytes.NewBufferString("plain"), 5, http.StatusOK, &Session{})
	if second.Body.String() != "plain" {
		t.Fatalf("plain-reader output=%q", second.Body.String())
	}
	if got := controller.Snapshot(); got.ActiveRequests != 0 || got.Allocated != 0 || got.Owned != 0 {
		t.Fatalf("controller used by ineligible path: %+v", got)
	}
}

func TestMediaBufferServerDeadlineAndCountedTelemetry(t *testing.T) {
	controller := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	meter := telemetry.NewByteMeter()
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, Meter: meter}, NewMemoryStore())
	payload := bytes.Repeat([]byte("m"), 3*mediaCopyBufferSize+7)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	body := newMediaBufferServerBody(bytes.NewReader(payload))
	resp := mediaBufferServerResponse(req, http.StatusOK, int64(len(payload)), body)
	wrapResponseBodyOnce(resp)
	writer := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
	server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	if len(writer.calls) < 2 || writer.calls[0] != "deadline" || writer.calls[1] != "header" {
		t.Fatalf("deadline/header order=%v", writer.calls)
	}
	ingress, egress := meter.Totals()
	if ingress != uint64(len(payload)) || egress != uint64(len(payload)) || meter.ActiveTransferCount() != 0 {
		t.Fatalf("traffic ingress=%d egress=%d active=%d", ingress, egress, meter.ActiveTransferCount())
	}
	assertMediaBufferServerIdle(t, controller)
}

func TestMediaBufferServerHTTP1Cancellation(t *testing.T) {
	testMediaBufferServerCancellation(t, false)
}

func TestMediaBufferServerHTTP2Cancellation(t *testing.T) {
	testMediaBufferServerCancellation(t, true)
}

func testMediaBufferServerCancellation(t *testing.T, useHTTP2 bool) {
	t.Helper()
	controller := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	store := NewMemoryStore()
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}}, store)
	body := &mediaBufferServerBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	handlerDone := make(chan struct{})
	protocol := make(chan int, 1)
	httpServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		protocol <- r.ProtoMajor
		resp := mediaBufferServerResponse(r, http.StatusOK, -1, body)
		wrapResponseBodyOnce(resp)
		server.writeProxyResponseWithSnapshot(w, r, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	}))
	httpServer.EnableHTTP2 = useHTTP2
	if useHTTP2 {
		httpServer.StartTLS()
	} else {
		httpServer.Start()
	}
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		resp, requestErr := httpServer.Client().Do(req)
		if requestErr != nil {
			return
		}
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}()
	awaitMediaBufferSignal(t, body.started)
	cancel()
	awaitMediaBufferSignal(t, body.closed)
	awaitMediaBufferSignal(t, handlerDone)
	awaitMediaBufferSignal(t, clientDone)
	protoMajor := <-protocol
	if useHTTP2 && protoMajor != 2 {
		t.Fatalf("protocol major=%d", protoMajor)
	}
	if len(store.AuditLogs) != 0 || server.ActiveMediaCopies() != 0 {
		t.Fatalf("audits=%d inflight=%d", len(store.AuditLogs), server.ActiveMediaCopies())
	}
	assertMediaBufferServerIdle(t, controller)
}

func TestMediaBufferServerGateHeldThroughJoinAuditAndAbort(t *testing.T) {
	controller := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	store := NewMemoryStore()
	meter := telemetry.NewByteMeter()
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, Meter: meter}, store)
	headerCommitted := make(chan struct{})
	allowCopy := make(chan struct{})
	beforeAudit := make(chan struct{})
	allowAudit := make(chan struct{})
	server.mediaBufferHooks = &mediaBufferServerHooks{
		afterHeaderCommit: func() {
			close(headerCommitted)
			<-allowCopy
		},
		beforeFailureAudit: func() {
			close(beforeAudit)
			<-allowAudit
		},
	}
	body := &mediaBufferServerJoinBody{
		optionalRead:  make(chan struct{}),
		closed:        make(chan struct{}),
		closeObserved: make(chan struct{}),
		allowReturn:   make(chan struct{}),
	}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	resp := mediaBufferServerResponse(req, http.StatusOK, -1, body)
	wrapResponseBodyOnce(resp)
	writer := &mediaBufferServerFailureWriter{readBlocked: body.optionalRead, err: timeoutMediaError{}}
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	}()
	awaitMediaBufferSignal(t, headerCommitted)
	assertMediaBufferServerPhase(t, server, controller, meter, 1, 1, 1, 0)
	close(allowCopy)
	awaitMediaBufferSignal(t, body.closeObserved)
	assertMediaBufferServerPhase(t, server, controller, meter, 1, 1, 1, 0)
	close(body.allowReturn)
	awaitMediaBufferSignal(t, beforeAudit)
	assertMediaBufferServerPhase(t, server, controller, meter, 1, 0, 0, 0)
	close(allowAudit)
	result := awaitMediaBufferAny(t, done)
	if result != http.ErrAbortHandler {
		t.Fatalf("panic=%v", result)
	}
	if server.ActiveMediaCopies() != 0 || len(store.AuditLogs) != 1 || store.AuditLogs[0].Event != "proxy_media_downstream_timeout" {
		t.Fatalf("inflight=%d audits=%#v", server.ActiveMediaCopies(), store.AuditLogs)
	}
	assertMediaBufferServerIdle(t, controller)
	release, err := server.TryAcquireReconfigure(false)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func assertMediaBufferServerPhase(t *testing.T, server *Server, controller *mediaBuffer, meter *telemetry.ByteMeter, activeCopies, activeRequests, activeTransfers, audits int) {
	t.Helper()
	if server.ActiveMediaCopies() != activeCopies {
		t.Fatalf("active copies=%d want %d", server.ActiveMediaCopies(), activeCopies)
	}
	snapshot := controller.Snapshot()
	if snapshot.ActiveRequests != activeRequests {
		t.Fatalf("controller snapshot=%+v want active requests %d", snapshot, activeRequests)
	}
	if meter.ActiveTransferCount() != activeTransfers {
		t.Fatalf("active transfers=%d want %d", meter.ActiveTransferCount(), activeTransfers)
	}
	if got := len(server.store.(*MemoryStore).AuditLogs); got != audits {
		t.Fatalf("audits=%d want %d", got, audits)
	}
	if _, err := server.TryAcquireReconfigure(false); !errors.Is(err, ErrActiveMedia) {
		t.Fatalf("reconfigure error=%v want %v", err, ErrActiveMedia)
	}
}

func mediaBufferServerResponse(req *http.Request, status int, contentLength int64, body io.ReadCloser) *http.Response {
	header := http.Header{"Content-Type": []string{"video/mp4"}}
	if status == http.StatusPartialContent {
		header.Set("Content-Range", "bytes 0-4/5")
	}
	return &http.Response{StatusCode: status, Header: header, Body: body, ContentLength: contentLength, Request: req}
}

type mediaBufferServerBody struct {
	io.Reader
	closeCount int
}

func newMediaBufferServerBody(reader io.Reader) *mediaBufferServerBody {
	return &mediaBufferServerBody{Reader: reader}
}

func (b *mediaBufferServerBody) Close() error {
	b.closeCount++
	return nil
}

type mediaBufferServerBlockingBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *mediaBufferServerBlockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("closed")
}

func (b *mediaBufferServerBlockingBody) Close() error {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

type mediaBufferServerJoinBody struct {
	step          int
	optionalRead  chan struct{}
	closed        chan struct{}
	closeObserved chan struct{}
	allowReturn   chan struct{}
}

func (b *mediaBufferServerJoinBody) Read(p []byte) (int, error) {
	if b.step == 0 {
		b.step++
		return copy(p, []byte("media")), nil
	}
	close(b.optionalRead)
	<-b.closed
	close(b.closeObserved)
	<-b.allowReturn
	return 0, errors.New("closed optional read")
}

func (b *mediaBufferServerJoinBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

type mediaBufferServerFailureWriter struct {
	header      http.Header
	readBlocked <-chan struct{}
	err         error
}

func (w *mediaBufferServerFailureWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (*mediaBufferServerFailureWriter) WriteHeader(int) {}
func (w *mediaBufferServerFailureWriter) Write([]byte) (int, error) {
	<-w.readBlocked
	return 0, w.err
}
func (*mediaBufferServerFailureWriter) SetWriteDeadline(time.Time) error { return nil }

func assertMediaBufferServerIdle(t *testing.T, controller *mediaBuffer) {
	t.Helper()
	controller.assertInvariants()
	snapshot := controller.Snapshot()
	if snapshot.ActiveRequests != 0 || snapshot.Owned != 0 || snapshot.Free != snapshot.Allocated || snapshot.Allocated > snapshot.HardBudget {
		t.Fatalf("controller not idle: %+v", snapshot)
	}
}

func awaitMediaBufferAny(t *testing.T, values <-chan any) any {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for media buffer server result")
		return nil
	}
}
