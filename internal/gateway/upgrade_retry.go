package gateway

import (
	"errors"
	"net/http"
)

// upgradeRetryTransport retries exactly one definitive backend 401 before the
// ReverseProxy takes ownership of a successful upgraded connection.
type upgradeRetryTransport struct {
	base         http.RoundTripper
	server       *Server
	original     *http.Request
	session      *Session
	gatewayToken string
	rel          string
}

func (t *upgradeRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	failedToken := t.session.BackendToken
	// Refresh must not inherit ReverseProxy's internal trace context. The retry
	// itself keeps req.Context so its cancellation/tracing remains intact.
	if refreshErr := t.server.refreshBackendSession(t.original.Context(), t.session, failedToken); refreshErr != nil {
		if !errors.Is(refreshErr, ErrUnauthorized) {
			t.server.auditBackendTokenRefresh(t.original, t.rel, t.session, "backend_token_refresh_failure", "backend token refresh failed after upgrade unauthorized response", http.StatusUnauthorized)
		}
		return resp, nil
	}
	t.server.auditBackendTokenRefresh(t.original, t.rel, t.session, "backend_token_refresh", "backend token refreshed after upgrade unauthorized response", http.StatusOK)
	_ = resp.Body.Close()
	retryURL, err := t.server.proxyURL(t.session, t.rel, t.original.URL.RawQuery, t.gatewayToken)
	if err != nil {
		return nil, err
	}
	// req has already passed through ReverseProxy's Director and hop-header
	// handling. Preserve that prepared shape, replacing only target and auth.
	retry := req.Clone(req.Context())
	retry.URL = retryURL
	retry.Host = retryURL.Host
	retry.RequestURI = ""
	t.server.rewriteRequestHeaders(retry.Header, t.session, t.gatewayToken)
	return base.RoundTrip(retry)
}
