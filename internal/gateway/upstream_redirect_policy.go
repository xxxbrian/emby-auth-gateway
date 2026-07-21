package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxMediaRedirectHops        = 5
	streamDialTimeout           = 15 * time.Second
	streamTLSHandshakeTimeout   = 10 * time.Second
	streamResponseHeaderTimeout = 20 * time.Second
	streamIdleConnectionTimeout = 90 * time.Second
)

var (
	ErrUpstreamRedirectRejected = errors.New("upstream redirect rejected")
	ErrMediaRedirectLimit       = errors.New("media redirect limit exceeded")
	ErrMediaRedirectScheme      = errors.New("media redirect scheme rejected")
	ErrMediaRedirectDowngrade   = errors.New("media redirect HTTPS downgrade rejected")
)

// RejectUpstreamRedirect is the fixed redirect callback for metadata,
// negotiation and managed-auth egress.
func RejectUpstreamRedirect(*http.Request, []*http.Request) error {
	return ErrUpstreamRedirectRejected
}

// CloneMediaRedirectRequest validates a media redirect and returns an isolated
// request clone. Cross-origin clones have gateway/backend credentials removed.
func CloneMediaRedirectRequest(next *http.Request, via []*http.Request, gatewayToken, backendToken string) (*http.Request, error) {
	if next == nil || next.URL == nil || len(via) == 0 || via[len(via)-1] == nil || via[len(via)-1].URL == nil {
		return nil, ErrMediaRedirectScheme
	}
	if len(via) > maxMediaRedirectHops {
		return nil, ErrMediaRedirectLimit
	}
	if !isHTTPURL(next.URL) {
		return nil, ErrMediaRedirectScheme
	}
	for _, previous := range via {
		if previous == nil || previous.URL == nil || !isHTTPURL(previous.URL) {
			return nil, ErrMediaRedirectScheme
		}
		if strings.EqualFold(previous.URL.Scheme, "https") && strings.EqualFold(next.URL.Scheme, "http") {
			return nil, ErrMediaRedirectDowngrade
		}
	}

	clone := next.Clone(next.Context())
	if sameOrigin(clone.URL, via[len(via)-1].URL) {
		return clone, nil
	}

	stripCrossOriginMediaHeaders(clone.Header)
	clone.URL.RawQuery = stripMediaRedirectCredentials(clone.URL.RawQuery, gatewayToken, backendToken)
	return clone, nil
}

func isHTTPURL(target *url.URL) bool {
	return target != nil && target.Host != "" && (strings.EqualFold(target.Scheme, "http") || strings.EqualFold(target.Scheme, "https"))
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		normalizedPort(a) == normalizedPort(b)
}

func normalizedPort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	switch strings.ToLower(target.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func stripCrossOriginMediaHeaders(header http.Header) {
	for name := range header {
		lower := strings.ToLower(name)
		if strings.EqualFold(name, "Cookie") ||
			strings.EqualFold(name, "Authorization") ||
			strings.EqualFold(name, "Proxy-Authorization") ||
			strings.EqualFold(name, "Referer") ||
			strings.HasPrefix(lower, "x-emby-") ||
			strings.HasPrefix(lower, "x-mediabrowser-") {
			delete(header, name)
		}
	}
}

// newStreamingHTTPClient isolates long-lived body transfers from auth/metadata
// total-request deadlines while retaining bounded connection/header phases.
func newStreamingHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	client.Timeout = 0
	client.Jar = nil
	switch transport := base.Transport.(type) {
	case nil:
		client.Transport = boundedStreamingTransport(http.DefaultTransport.(*http.Transport).Clone())
	case *http.Transport:
		client.Transport = boundedStreamingTransport(transport.Clone())
	default:
		// Preserve injected RoundTrippers exactly so deterministic tests and
		// specialized production transports remain usable.
		client.Transport = transport
	}
	return &client
}

func boundedStreamingTransport(transport *http.Transport) *http.Transport {
	dialContext := transport.DialContext
	if dialContext == nil {
		dialer := &net.Dialer{Timeout: streamDialTimeout, KeepAlive: 30 * time.Second}
		dialContext = dialer.DialContext
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, streamDialTimeout)
		defer cancel()
		return dialContext(dialCtx, network, address)
	}
	transport.TLSHandshakeTimeout = streamTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = streamResponseHeaderTimeout
	transport.IdleConnTimeout = streamIdleConnectionTimeout
	return transport
}

func stripMediaRedirectCredentials(rawQuery, gatewayToken, backendToken string) string {
	if rawQuery == "" {
		return ""
	}
	credentials := map[string]struct{}{
		gatewayToken: {},
		backendToken: {},
	}
	delete(credentials, "")
	segments := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(segments))
	for _, segment := range segments {
		rawKey, rawValue, hasValue := strings.Cut(segment, "=")
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			key = rawKey
		}
		if isMediaRedirectCredentialKey(key) {
			continue
		}
		if hasValue {
			value, err := url.QueryUnescape(rawValue)
			if err == nil {
				if _, sensitive := credentials[value]; sensitive {
					continue
				}
			}
		}
		kept = append(kept, segment)
	}
	return strings.Join(kept, "&")
}

func isMediaRedirectCredentialKey(key string) bool {
	return isEgressCredentialQueryKey(key)
}

// upstreamRedirectPolicy binds purpose at transport construction time. The
// request cannot select a weaker redirect policy through headers or query data.
func upstreamRedirectPolicy(purpose upstreamPurpose, gatewayToken, backendToken string) func(*http.Request, []*http.Request) error {
	if purpose != upstreamPurposeMedia {
		return RejectUpstreamRedirect
	}
	return func(next *http.Request, via []*http.Request) error {
		clone, err := CloneMediaRedirectRequest(next, via, gatewayToken, backendToken)
		if err != nil {
			return err
		}
		next.Header = clone.Header
		next.URL = clone.URL
		return nil
	}
}
