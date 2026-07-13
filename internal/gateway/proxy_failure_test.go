package gateway

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestClassifyProxyFailurePrecedence(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if classifyProxyFailure(canceled, timeoutMediaError{}) != proxyFailureCanceled {
		t.Fatal("context cancellation did not take precedence")
	}
	if classifyProxyFailure(context.Background(), errors.Join(errors.New("wrapped"), context.Canceled)) != proxyFailureCanceled {
		t.Fatal("wrapped cancellation was not classified")
	}
	if classifyProxyFailure(context.Background(), errors.Join(errors.New("wrapped"), context.DeadlineExceeded)) != proxyFailureTimeout {
		t.Fatal("wrapped deadline was not classified")
	}
	if classifyProxyFailure(context.Background(), timeoutMediaError{}) != proxyFailureTimeout {
		t.Fatal("network timeout was not classified")
	}
	if classifyProxyFailure(context.Background(), errors.New("failure")) != proxyFailureOther {
		t.Fatal("generic failure was not classified")
	}
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	if classifyProxyFailure(expired, timeoutMediaError{}) != proxyFailureCanceled {
		t.Fatal("expired request deadline did not take cancellation precedence")
	}
}

func TestPreHeaderProxyFailureAuditsSafely(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		ctx        context.Context
		wantStatus int
		wantKind   string
		wantAudit  bool
	}{
		{name: "timeout", err: timeoutMediaError{}, ctx: context.Background(), wantStatus: http.StatusGatewayTimeout, wantKind: "upstream_timeout", wantAudit: true},
		{name: "generic", err: errors.New("http://backend.test/path?api_key=backend-token"), ctx: context.Background(), wantStatus: http.StatusBadGateway, wantKind: "upstream_request_error", wantAudit: true},
		{name: "canceled", err: timeoutMediaError{}, ctx: canceledContext(), wantAudit: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items", nil).WithContext(tt.ctx)
			writer := httptest.NewRecorder()
			if !tt.wantAudit {
				requireAbortHandler(t, func() {
					server.handlePreHeaderProxyFailure(writer, req, "/Items", &Session{}, tt.err, proxyFailureDetails{Event: "proxy_backend_unavailable", AuditMessage: "backend unavailable", ClientBody: "backend unavailable", FallbackKind: "upstream_request_error", Duration: time.Millisecond})
				})
				if len(store.AuditLogs) != 0 || writer.Body.Len() != 0 {
					t.Fatal("canceled request produced an audit or body")
				}
				return
			}
			server.handlePreHeaderProxyFailure(writer, req, "/Items", &Session{}, tt.err, proxyFailureDetails{Event: "proxy_backend_unavailable", AuditMessage: "backend unavailable", ClientBody: "backend unavailable", FallbackKind: "upstream_request_error", Duration: time.Millisecond, UpstreamStatus: http.StatusPartialContent})
			if writer.Code != tt.wantStatus || len(store.AuditLogs) != 1 {
				t.Fatal("pre-header failure response or audit was incorrect")
			}
			entry := store.AuditLogs[0]
			if entry.ErrorKind != tt.wantKind || entry.Direction != mediaDirectionUpstream || entry.BytesTransferred != 0 || entry.DurationMS != 1 || entry.UpstreamStatus != http.StatusPartialContent || entry.ResponseCommitted || entry.Status != tt.wantStatus {
				t.Fatal("pre-header failure audit fields were incorrect")
			}
			if strings.Contains(entry.Message, "backend-token") || strings.Contains(writer.Body.String(), "backend-token") {
				t.Fatal("pre-header failure exposed backend details")
			}
		})
	}
}

func TestInitialProxyDoFailureUsesStructuredHandler(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		ctx        context.Context
		wantStatus int
		wantKind   string
		wantAudit  bool
	}{
		{name: "timeout", err: timeoutMediaError{}, ctx: context.Background(), wantStatus: http.StatusGatewayTimeout, wantKind: "upstream_timeout", wantAudit: true},
		{name: "generic", err: errors.New("http://backend.test/?api_key=backend-token"), ctx: context.Background(), wantStatus: http.StatusBadGateway, wantKind: "upstream_request_error", wantAudit: true},
		{name: "canceled", err: timeoutMediaError{}, ctx: canceledContext(), wantAudit: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			store.Sessions[HashToken("gateway-token")] = testSession("http://backend.test/emby")
			client := &http.Client{Transport: proxyFailureRoundTripper{err: tt.err}}
			server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: client}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items?api_key=gateway-token", nil).WithContext(tt.ctx)
			writer := httptest.NewRecorder()
			if !tt.wantAudit {
				requireAbortHandler(t, func() { server.ServeHTTP(writer, req) })
				if len(store.AuditLogs) != 0 || writer.Body.Len() != 0 {
					t.Fatal("canceled proxy request produced an audit or body")
				}
				return
			}
			server.ServeHTTP(writer, req)
			if writer.Code != tt.wantStatus || len(store.AuditLogs) != 1 {
				t.Fatal("initial proxy failure response or audit was incorrect")
			}
			entry := store.AuditLogs[0]
			if entry.Event != "proxy_backend_unavailable" || entry.ErrorKind != tt.wantKind || entry.Direction != mediaDirectionUpstream || entry.BytesTransferred != 0 || entry.ResponseCommitted || entry.Status != tt.wantStatus {
				t.Fatal("initial proxy failure audit fields were incorrect")
			}
			if strings.Contains(entry.Message, "backend-token") || strings.Contains(writer.Body.String(), "backend-token") {
				t.Fatal("initial proxy failure exposed backend details")
			}
		})
	}
}

func TestRefreshedProxyRetryFailureUsesStructuredHandler(t *testing.T) {
	for _, tt := range []struct {
		name       string
		err        error
		wantStatus int
		wantKind   string
		canceled   bool
	}{
		{name: "timeout", err: timeoutMediaError{}, wantStatus: http.StatusGatewayTimeout, wantKind: "upstream_timeout"},
		{name: "canceled", err: context.Canceled, canceled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			session := testSession("http://backend.test/emby")
			store.Sessions[HashToken("gateway-token")] = session
			store.Mappings["u1"] = UserMapping{GatewayUserID: "u1", BackendAccountID: "b1", Enabled: true, BackendAccount: BackendAccount{ID: "b1", BaseURL: session.BackendBaseURL, Username: "shared", Password: "backend-pass", BackendToken: "backend-token", BackendUserID: "backend-user", Enabled: true}}
			client := &http.Client{Transport: &retryFailureRoundTripper{retryErr: tt.err}}
			server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: client}, store)
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items?api_key=gateway-token", nil)
			writer := httptest.NewRecorder()
			if tt.canceled {
				requireAbortHandler(t, func() { server.ServeHTTP(writer, req) })
				if hasAuditEvent(store, "proxy_backend_unavailable") {
					t.Fatal("retry cancellation produced a backend unavailable audit")
				}
				return
			}
			server.ServeHTTP(writer, req)
			if writer.Code != tt.wantStatus || len(store.AuditLogs) < 2 {
				t.Fatal("retry failure response or audit was incorrect")
			}
			entry := store.AuditLogs[len(store.AuditLogs)-1]
			if entry.Event != "proxy_backend_unavailable" || entry.ErrorKind != tt.wantKind || entry.Direction != mediaDirectionUpstream || entry.ResponseCommitted || entry.Status != tt.wantStatus {
				t.Fatal("retry failure audit fields were incorrect")
			}
		})
	}
}

func TestUpgradeProxyFailureUsesStructuredHandler(t *testing.T) {
	target, err := url.Parse("http://backend.test/emby")
	if err != nil {
		t.Fatal("failed to parse upgrade target")
	}
	for _, tt := range []struct {
		name       string
		err        error
		wantStatus int
		wantKind   string
		canceled   bool
	}{
		{name: "timeout", err: timeoutMediaError{}, wantStatus: http.StatusGatewayTimeout, wantKind: "upstream_timeout"},
		{name: "generic", err: errors.New("https://backend.test/socket?api_key=backend-token"), wantStatus: http.StatusBadGateway, wantKind: "upstream_request_error"},
		{name: "canceled", err: timeoutMediaError{}, canceled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			server := NewServer(Config{HTTPClient: &http.Client{Transport: proxyFailureRoundTripper{err: tt.err}}}, store)
			ctx := context.Background()
			if tt.canceled {
				ctx = canceledContext()
			}
			req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/socket", nil).WithContext(ctx)
			writer := httptest.NewRecorder()
			if tt.canceled {
				requireAbortHandler(t, func() { server.handleUpgradeProxy(writer, req, target, &Session{}, "", "/socket") })
				if len(store.AuditLogs) != 0 || len(writer.Header()) != 0 || writer.Body.Len() != 0 {
					t.Fatal("canceled upgrade produced an audit, headers, or body")
				}
				return
			}
			server.handleUpgradeProxy(writer, req, target, &Session{}, "", "/socket")
			if writer.Code != tt.wantStatus || len(store.AuditLogs) != 1 {
				t.Fatal("upgrade failure response or audit was incorrect")
			}
			entry := store.AuditLogs[0]
			if entry.Event != "proxy_backend_unavailable" || entry.ErrorKind != tt.wantKind || entry.Direction != mediaDirectionUpstream || entry.ResponseCommitted || entry.Status != tt.wantStatus {
				t.Fatal("upgrade failure audit fields were incorrect")
			}
			if strings.Contains(entry.Message, "backend-token") || strings.Contains(writer.Body.String(), "backend-token") {
				t.Fatal("upgrade failure exposed backend details")
			}
		})
	}
}

func TestUpgradeProxySuppressesFailureAfterSuccessfulHijack(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{HTTPClient: &http.Client{Transport: upgrade101RoundTripper{}}}, store)
	target, err := url.Parse("http://backend.test/emby")
	if err != nil {
		t.Fatal("failed to parse upgrade target")
	}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/socket", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	writer := &hijackFailureWriter{header: http.Header{}}
	wrappedWriter := &unwrapOnlyResponseWriter{ResponseWriter: writer}

	server.handleUpgradeProxy(wrappedWriter, req, target, &Session{}, "", "/socket")
	if len(store.AuditLogs) != 0 || writer.writeHeaders != 0 || writer.writes != 0 || !writer.hijacked {
		t.Fatal("post-hijack upgrade failure produced an audit or fallback response")
	}
}

func TestUpgradeResponseWriterDoesNotFinalizeOtherInformationalResponses(t *testing.T) {
	writer := &upgradeResponseWriter{ResponseWriter: httptest.NewRecorder()}
	writer.WriteHeader(http.StatusEarlyHints)
	if writer.finalResponse.Load() {
		t.Fatal("non-final informational response marked final")
	}
	writer.WriteHeader(http.StatusSwitchingProtocols)
	if !writer.finalResponse.Load() {
		t.Fatal("switching protocols response did not mark final")
	}
}

func TestBufferedPreHeaderReadFailureMatrix(t *testing.T) {
	families := []struct {
		name        string
		contentType string
		path        string
	}{
		{name: "m3u8", contentType: "application/vnd.apple.mpegurl", path: "/Videos/item/master.m3u8"},
		{name: "json", contentType: "application/problem+json", path: "/Items/item"},
		{name: "empty type", contentType: "", path: "/Items/item"},
	}
	failures := []struct {
		name       string
		err        error
		canceled   bool
		wantStatus int
		wantKind   string
	}{
		{name: "canceled", err: errors.New("https://backend.test/path?api_key=backend-token"), canceled: true},
		{name: "timeout", err: timeoutMediaError{}, wantStatus: http.StatusGatewayTimeout, wantKind: "upstream_timeout"},
		{name: "generic", err: errors.New("https://backend.test/path?api_key=backend-token"), wantStatus: http.StatusBadGateway, wantKind: "upstream_read_error"},
	}
	for _, family := range families {
		for _, failure := range failures {
			t.Run(family.name+"/"+failure.name, func(t *testing.T) {
				store := NewMemoryStore()
				server := NewServer(Config{}, store)
				ctx := context.Background()
				if failure.canceled {
					ctx = canceledContext()
				}
				req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby"+family.path, nil).WithContext(ctx)
				writer := httptest.NewRecorder()
				resp := &http.Response{StatusCode: http.StatusPartialContent, Header: http.Header{"Content-Type": []string{family.contentType}}, Body: io.NopCloser(errorMediaReader{err: failure.err}), ContentLength: -1, Request: req}
				if failure.canceled {
					requireAbortHandler(t, func() { server.writeProxyResponse(writer, req, family.path, resp, &Session{}, "", "") })
					if len(store.AuditLogs) != 0 || len(writer.Header()) != 0 || writer.Body.Len() != 0 {
						t.Fatal("canceled read produced an audit, headers, or body")
					}
					return
				}
				server.writeProxyResponse(writer, req, family.path, resp, &Session{}, "", "")
				if writer.Code != failure.wantStatus || len(store.AuditLogs) != 1 {
					t.Fatal("buffered read failure response or audit was incorrect")
				}
				entry := store.AuditLogs[0]
				if entry.Event != "proxy_read_failed" || entry.Message != "backend response read failed" || entry.ErrorKind != failure.wantKind || entry.Direction != mediaDirectionUpstream || entry.BytesTransferred != 0 || entry.UpstreamStatus != http.StatusPartialContent || entry.ResponseCommitted || entry.Status != failure.wantStatus {
					t.Fatal("buffered read failure audit fields were incorrect")
				}
				if strings.Contains(entry.Message, "backend-token") || strings.Contains(writer.Body.String(), "backend-token") {
					t.Fatal("buffered read failure exposed backend details")
				}
			})
		}
	}
}

func TestImageInitialReadFailureMatrix(t *testing.T) {
	for _, partial := range []bool{false, true} {
		for _, failure := range []struct {
			name       string
			err        error
			canceled   bool
			wantStatus int
			wantKind   string
		}{
			{name: "canceled", err: errors.New("https://backend.test/path?api_key=backend-token"), canceled: true},
			{name: "timeout", err: timeoutMediaError{}, wantStatus: http.StatusGatewayTimeout, wantKind: "upstream_timeout"},
			{name: "generic", err: errors.New("https://backend.test/path?api_key=backend-token"), wantStatus: http.StatusBadGateway, wantKind: "upstream_read_error"},
		} {
			name := "empty"
			if partial {
				name = "partial"
			}
			t.Run(name+"/"+failure.name, func(t *testing.T) {
				store := NewMemoryStore()
				server := NewServer(Config{}, store)
				ctx := context.Background()
				if failure.canceled {
					ctx = canceledContext()
				}
				req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item/Images/Primary", nil).WithContext(ctx)
				writer := httptest.NewRecorder()
				var reader io.Reader = errorMediaReader{err: failure.err}
				if partial {
					reader = partialReadError{err: failure.err}
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/gif"}, "ETag": []string{"etag"}, "Last-Modified": []string{"date"}}, Body: io.NopCloser(reader), ContentLength: -1, Request: req}
				if failure.canceled {
					requireAbortHandler(t, func() { server.writeProxyResponse(writer, req, "/Items/item/Images/Primary", resp, &Session{}, "", "") })
					if len(store.AuditLogs) != 0 || len(writer.Header()) != 0 || writer.Body.Len() != 0 {
						t.Fatal("canceled image read produced an audit, headers, or body")
					}
					return
				}
				server.writeProxyResponse(writer, req, "/Items/item/Images/Primary", resp, &Session{}, "", "")
				if writer.Code != failure.wantStatus || len(store.AuditLogs) != 1 || writer.Body.Len() != len("invalid image response\n") {
					t.Fatal("image read failure response or audit was incorrect")
				}
				entry := store.AuditLogs[0]
				if entry.Event != "proxy_invalid_image" || entry.Message != "backend image response read failed" || entry.ErrorKind != failure.wantKind || entry.Direction != mediaDirectionUpstream || entry.BytesTransferred != 0 || entry.UpstreamStatus != http.StatusOK || entry.ResponseCommitted || entry.Status != failure.wantStatus {
					t.Fatal("image read failure audit fields were incorrect")
				}
				if writer.Header().Get("Cache-Control") != "no-store" || writer.Header().Get("ETag") != "" || writer.Header().Get("Last-Modified") != "" || strings.Contains(entry.Message, "backend-token") || strings.Contains(writer.Body.String(), "backend-token") {
					t.Fatal("image read failure leaked data or retained cache headers")
				}
			})
		}
	}
}

func TestImagePartialReadEOFRemainsValid(t *testing.T) {
	store := NewMemoryStore()
	server := NewServer(Config{}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Items/item/Images/Primary", nil)
	writer := httptest.NewRecorder()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/gif"}}, Body: io.NopCloser(partialEOFReader{}), ContentLength: 1, Request: req}
	server.writeProxyResponse(writer, req, "/Items/item/Images/Primary", resp, &Session{}, "", "")
	if writer.Code != http.StatusOK || writer.Body.String() != "x" || len(store.AuditLogs) != 0 {
		t.Fatal("partial image read ending in EOF was not preserved")
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

type partialReadError struct{ err error }

func (r partialReadError) Read(p []byte) (int, error) {
	p[0] = 'x'
	return 1, r.err
}

type partialEOFReader struct{}

func (partialEOFReader) Read(p []byte) (int, error) {
	p[0] = 'x'
	return 1, io.EOF
}

type proxyFailureRoundTripper struct{ err error }

func (r proxyFailureRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, r.err
}

type retryFailureRoundTripper struct {
	items    int
	retryErr error
}

func (r *retryFailureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Path {
	case "/emby/Items":
		r.items++
		if r.items == 1 {
			return testTransportResponse(http.StatusUnauthorized, ""), nil
		}
		return nil, r.retryErr
	case "/emby/System/Info":
		return testTransportResponse(http.StatusUnauthorized, ""), nil
	case "/emby/Users/AuthenticateByName":
		return testTransportResponse(http.StatusOK, `{"AccessToken":"new-token","ServerId":"backend-server","User":{"Id":"backend-user","Name":"shared"}}`), nil
	case "/emby/Sessions/Logout":
		return testTransportResponse(http.StatusNoContent, ""), nil
	default:
		return nil, errors.New("unexpected transport request")
	}
}

func testTransportResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

type upgrade101RoundTripper struct{}

func (upgrade101RoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusSwitchingProtocols,
		Header:     http.Header{"Connection": []string{"Upgrade"}, "Upgrade": []string{"websocket"}},
		Body:       upgradeResponseBody{},
	}, nil
}

type upgradeResponseBody struct{}

func (upgradeResponseBody) Read([]byte) (int, error)    { return 0, io.EOF }
func (upgradeResponseBody) Write(p []byte) (int, error) { return len(p), nil }
func (upgradeResponseBody) Close() error                { return nil }

type hijackFailureWriter struct {
	header       http.Header
	writeHeaders int
	writes       int
	hijacked     bool
}

type unwrapOnlyResponseWriter struct{ http.ResponseWriter }

func (w *unwrapOnlyResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *hijackFailureWriter) Header() http.Header { return w.header }
func (w *hijackFailureWriter) WriteHeader(int)     { w.writeHeaders++ }
func (w *hijackFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	return len(p), nil
}
func (w *hijackFailureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	conn := failingWriteConn{}
	return conn, bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)), nil
}

type failingWriteConn struct{}

func (failingWriteConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (failingWriteConn) Write([]byte) (int, error)        { return 0, errors.New("write failed") }
func (failingWriteConn) Close() error                     { return nil }
func (failingWriteConn) LocalAddr() net.Addr              { return testNetAddr("local") }
func (failingWriteConn) RemoteAddr() net.Addr             { return testNetAddr("remote") }
func (failingWriteConn) SetDeadline(time.Time) error      { return nil }
func (failingWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (failingWriteConn) SetWriteDeadline(time.Time) error { return nil }

type testNetAddr string

func (a testNetAddr) Network() string { return "test" }
func (a testNetAddr) String() string  { return string(a) }
