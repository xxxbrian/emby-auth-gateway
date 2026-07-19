package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

// legacyRequestBodyLimit bounds replay memory while retaining compatibility
// with large JSON/plugin payloads that legitimately use the fallback surface.
const legacyRequestBodyLimit = 16 << 20

type legacyHTTPUpstream struct {
	client  *http.Client
	audit   func(context.Context, AuditLog)
	refresh func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error)
	emit    func(observe.Event)
}

func newLegacyHTTPUpstream(client *http.Client, audit func(context.Context, AuditLog), refresh func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error), emit func(observe.Event)) *legacyHTTPUpstream {
	streaming := newStreamingHTTPClient(client)
	streaming.CheckRedirect = RejectUpstreamRedirect
	return &legacyHTTPUpstream{client: streaming, audit: audit, refresh: refresh, emit: emit}
}

func (l *legacyHTTPUpstream) RoundTripLegacy(in legacyUpstreamRequest) (resp *http.Response, err error) {
	status := 0
	if in.Request != nil {
		status = 0
	}
	defer func() {
		if l.audit != nil {
			outcome := "error"
			if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 400 {
				outcome = "ok"
			}
			path := ""
			method := ""
			if in.Request != nil && in.Request.URL != nil {
				path, method = in.Request.URL.Path, in.Request.Method
			}
			ctx := context.Background()
			if in.Request != nil {
				ctx = in.Request.Context()
			}
			l.audit(ctx, AuditLog{Event: "legacy_proxy_request", Method: method, Path: path, Status: status, ErrorKind: outcome})
		}
	}()
	if err = validateLegacyRequest(in); err != nil {
		return nil, err
	}
	var body []byte
	if in.Request.Body != nil && in.Request.Body != http.NoBody {
		body, err = io.ReadAll(io.LimitReader(in.Request.Body, legacyRequestBodyLimit+1))
		if err != nil {
			return nil, err
		}
		if len(body) > legacyRequestBodyLimit {
			return nil, fmt.Errorf("%w: legacy body", ErrRequestBodyTooLarge)
		}
	}
	snapshot := in.Snapshot
	for attempt := 0; attempt < 2; attempt++ {
		started := time.Now()
		resp, err = l.do(in, snapshot, body)
		if resp != nil {
			status = resp.StatusCode
		}
		l.emitAttempt(in, status, err, time.Since(started))
		if err != nil || resp.StatusCode != http.StatusUnauthorized || attempt != 0 || l.refresh == nil {
			return resp, err
		}
		refreshed, confirmed, refreshErr := l.refresh(in.Request.Context(), snapshot)
		in.notifyRefreshResult(upstreamRefreshResult{Confirmed: confirmed, Err: refreshErr})
		if refreshErr != nil || !confirmed {
			return resp, nil
		}
		_ = resp.Body.Close()
		snapshot = refreshed
		if in.SnapshotRef != nil {
			*in.SnapshotRef = refreshed
		}
	}
	return resp, nil
}

func validateLegacyRequest(in legacyUpstreamRequest) error {
	if in.Request == nil || in.Request.URL == nil || in.Session == nil || !validSnapshotCredentials(in.Snapshot) {
		return fmt.Errorf("%w: incomplete legacy request", ErrBadRequest)
	}
	d := routeclass.Classify(in.Request.Method, in.Request.URL.Path)
	if d.Ownership != routeclass.LegacyProxy || d.Operation != routeclass.OperationLegacyProxy || !d.MethodAllowed {
		return fmt.Errorf("%w: legacy route ownership", ErrForbidden)
	}
	if in.Request.Method == http.MethodConnect || in.Request.Header.Get("Upgrade") != "" {
		return fmt.Errorf("%w: upgrade not allowed", ErrForbidden)
	}
	return nil
}

func (l *legacyHTTPUpstream) do(in legacyUpstreamRequest, snapshot upstreamRequestSnapshot, rawBody []byte) (*http.Response, error) {
	rel := strings.ReplaceAll(in.Request.URL.Path, in.Session.SyntheticUserID, snapshot.userID)
	u, err := backendURL(snapshot.baseURL, rel)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	parsed.RawQuery = in.Request.URL.RawQuery
	q, err := parseRawQuery(parsed.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed query", ErrBadRequest)
	}
	rewriteLegacyQuery(q, legacyCredentialValues(in.Request, q), ExtractToken(in.Request), in.Session, snapshot)
	parsed.RawQuery = q.Encode()
	var body io.Reader
	if len(rawBody) != 0 {
		data := stringBytesRewrite(rawBody, in.Session, snapshot, ExtractToken(in.Request))
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(in.Request.Context(), in.Request.Method, parsed.String(), body)
	if err != nil {
		return nil, err
	}
	copyLegacyRequestHeaders(req.Header, in.Request.Header)
	sanitizeHopHeaders(req.Header)
	rewriteManagedHeaders(req.Header, snapshot)
	resp, err := l.client.Do(req)
	if err != nil {
		_ = closeResponseOnError(resp)
		return resp, err
	}
	wrapResponseBodyOnce(resp)
	return resp, nil
}

func copyLegacyRequestHeaders(dst, src http.Header) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") || strings.EqualFold(name, "Cookie") || strings.HasPrefix(lower, "x-emby-") || strings.HasPrefix(lower, "x-mediabrowser-") {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func rewriteLegacyQuery(query url.Values, credentials map[string]struct{}, gatewayToken string, session *Session, snapshot upstreamRequestSnapshot) {
	for key, values := range query {
		if isEgressCredentialAliasQueryKey(key) {
			delete(query, key)
			continue
		}
		kept := values[:0]
		for _, value := range values {
			if _, sensitive := credentials[value]; sensitive {
				continue
			}
			if gatewayToken != "" && value == gatewayToken {
				continue
			}
			kept = append(kept, strings.ReplaceAll(value, session.SyntheticUserID, snapshot.userID))
		}
		if len(kept) == 0 {
			delete(query, key)
			continue
		}
		query[key] = kept
	}
	query.Set("api_key", snapshot.token)
}

func legacyCredentialValues(request *http.Request, query url.Values) map[string]struct{} {
	credentials := make(map[string]struct{})
	for key, values := range query {
		if !isEgressCredentialQueryKey(key) {
			continue
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				credentials[value] = struct{}{}
			}
		}
	}
	for name, values := range request.Header {
		lower := strings.ToLower(name)
		for _, value := range values {
			value = strings.TrimSpace(value)
			switch {
			case lower == "x-emby-token", lower == "x-mediabrowser-token", lower == "proxy-authorization":
				if value != "" {
					credentials[value] = struct{}{}
				}
			case lower == "authorization", lower == "x-emby-authorization", lower == "x-mediabrowser-authorization":
				if value != "" {
					credentials[value] = struct{}{}
				}
				if token := ParseEmbyAuthHeader(value).Token; token != "" {
					credentials[token] = struct{}{}
				}
			case lower == "cookie":
				for _, part := range strings.Split(value, ";") {
					_, cookieValue, ok := strings.Cut(strings.TrimSpace(part), "=")
					if ok && cookieValue != "" {
						credentials[cookieValue] = struct{}{}
					}
				}
			}
		}
	}
	return credentials
}

func (l *legacyHTTPUpstream) emitAttempt(in legacyUpstreamRequest, status int, err error, duration time.Duration) {
	if l.emit == nil {
		return
	}
	outcome := observe.OutcomeError
	if err == nil && status >= 200 && status < 400 {
		outcome = observe.OutcomeOK
	}
	l.emit(observe.Event{Kind: observe.KindUpstreamRequest, RouteClass: observe.RouteOther, Outcome: outcome, StatusClass: observe.StatusClassOf(status), ErrorKind: upstreamPurposeLegacy.String(), Direction: observe.DirectionUpstream, Method: requestMethod(in.Request), DurationMS: duration.Milliseconds()})
}

func stringBytesRewrite(data []byte, session *Session, snapshot upstreamRequestSnapshot, gatewayToken string) []byte {
	text := string(data)
	if gatewayToken != "" {
		text = strings.ReplaceAll(text, gatewayToken, snapshot.token)
	}
	text = strings.ReplaceAll(text, session.SyntheticUserID, snapshot.userID)
	return []byte(text)
}

var _ LegacyHTTPUpstream = (*legacyHTTPUpstream)(nil)
