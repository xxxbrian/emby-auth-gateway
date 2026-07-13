package gateway

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type proxyFailureClass int

const (
	proxyFailureCanceled proxyFailureClass = iota
	proxyFailureTimeout
	proxyFailureOther
)

type proxyFailureDetails struct {
	Event          string
	AuditMessage   string
	ClientBody     string
	FallbackKind   string
	Duration       time.Duration
	UpstreamStatus int
}

func classifyProxyFailure(ctx context.Context, err error) proxyFailureClass {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return proxyFailureCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) || isTimeoutError(err) {
		return proxyFailureTimeout
	}
	return proxyFailureOther
}

func (s *Server) handlePreHeaderProxyFailure(w http.ResponseWriter, r *http.Request, rel string, session *Session, err error, details proxyFailureDetails) {
	class := classifyProxyFailure(r.Context(), err)
	if class == proxyFailureCanceled {
		for name := range w.Header() {
			delete(w.Header(), name)
		}
		panic(http.ErrAbortHandler)
	}
	status := http.StatusBadGateway
	errorKind := details.FallbackKind
	if class == proxyFailureTimeout {
		status = http.StatusGatewayTimeout
		errorKind = "upstream_timeout"
	}
	s.audit(r.Context(), AuditLog{
		GatewayUserID:     sessionGatewayUserID(session),
		SyntheticUserID:   sessionSyntheticUserID(session),
		Event:             details.Event,
		Message:           details.AuditMessage,
		RemoteIP:          remoteIP(r),
		Method:            r.Method,
		Path:              rel,
		Status:            status,
		ErrorKind:         errorKind,
		Direction:         mediaDirectionUpstream,
		BytesTransferred:  0,
		DurationMS:        int(details.Duration.Milliseconds()),
		UpstreamStatus:    details.UpstreamStatus,
		ResponseCommitted: false,
	})
	http.Error(w, details.ClientBody, status)
}

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func closeResponseOnError(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	return resp.StatusCode
}

type upgradeResponseWriter struct {
	http.ResponseWriter
	finalResponse atomic.Bool
	hijacked      atomic.Bool
}

func (w *upgradeResponseWriter) WriteHeader(status int) {
	if status == http.StatusSwitchingProtocols || status >= http.StatusOK {
		w.finalResponse.Store(true)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *upgradeResponseWriter) Write(p []byte) (int, error) {
	w.finalResponse.Store(true)
	return w.ResponseWriter.Write(p)
}

func (w *upgradeResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, readWriter, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked.Store(true)
	}
	return conn, readWriter, err
}

func (w *upgradeResponseWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *upgradeResponseWriter) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *upgradeResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *upgradeResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}
