package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

var errCrossOriginRedirectBody = errors.New("cross-origin redirect with request body rejected")

type redirectCredentialTokensKey struct{}

func withRedirectCredentialTokens(ctx context.Context, tokens ...string) context.Context {
	existing := credentialTokensFromContext(ctx)
	merged := make(map[string]struct{}, len(existing)+len(tokens))
	for token := range existing {
		merged[token] = struct{}{}
	}
	for _, token := range tokens {
		if token = strings.TrimSpace(token); token != "" {
			merged[token] = struct{}{}
		}
	}
	return context.WithValue(ctx, redirectCredentialTokensKey{}, merged)
}

func credentialTokensFromContext(ctx context.Context) map[string]struct{} {
	if ctx == nil {
		return nil
	}
	tokens, _ := ctx.Value(redirectCredentialTokensKey{}).(map[string]struct{})
	return tokens
}

func newProxyClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{Transport: defaultProxyTransport()}
	} else {
		copy := *client
		client = &copy
	}

	originalCheckRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if originalCheckRedirect != nil {
			if err := originalCheckRedirect(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}

		if len(via) > 0 && !sameOrigin(req.URL, via[0].URL) {
			sanitizeCrossOriginRedirect(req, redirectAuthTokens(via[0]))
			if req.Body != nil && req.Body != http.NoBody {
				return errCrossOriginRedirectBody
			}
		}
		return nil
	}
	return client
}

func redirectAuthTokens(req *http.Request) map[string]struct{} {
	tokens := redirectTokens(req)
	for token := range credentialTokensFromContext(req.Context()) {
		if tokens == nil {
			tokens = map[string]struct{}{}
		}
		tokens[token] = struct{}{}
	}
	return tokens
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		normalizedPort(a) == normalizedPort(b)
}

func normalizedPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func sanitizeCrossOriginRedirect(req *http.Request, tokens map[string]struct{}) {
	for name := range req.Header {
		if isCrossOriginSensitiveHeader(name) {
			delete(req.Header, name)
		}
	}
	if req.URL.RawQuery == "" {
		return
	}
	req.URL.RawQuery = removeSensitiveQuerySegments(req.URL.RawQuery, tokens)
}

func removeSensitiveQuerySegments(rawQuery string, tokens map[string]struct{}) string {
	segments := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(segments))
	removed := false
	for _, segment := range segments {
		name, value, hasValue := strings.Cut(segment, "=")
		key := name
		if decodedName, err := url.QueryUnescape(name); err == nil {
			key = decodedName
		}
		if isStrictQueryAuthKey(key) {
			removed = true
			continue
		}
		if key == genericQueryAuthKey && hasValue {
			decoded, err := url.QueryUnescape(value)
			if err == nil {
				if _, sensitive := tokens[decoded]; sensitive {
					removed = true
					continue
				}
			}
		}
		kept = append(kept, segment)
	}
	if !removed {
		return rawQuery
	}
	return strings.Join(kept, "&")
}

func isCrossOriginSensitiveHeader(name string) bool {
	switch {
	case strings.EqualFold(name, "Authorization"),
		strings.EqualFold(name, "Proxy-Authorization"),
		strings.EqualFold(name, "WWW-Authenticate"),
		strings.EqualFold(name, "Proxy-Authenticate"),
		strings.EqualFold(name, "Cookie"),
		strings.EqualFold(name, "Cookie2"),
		strings.EqualFold(name, "Referer"):
		return true
	default:
		return strings.HasPrefix(strings.ToLower(name), "x-emby-") ||
			strings.HasPrefix(strings.ToLower(name), "x-mediabrowser-")
	}
}

func redirectTokens(req *http.Request) map[string]struct{} {
	tokens := map[string]struct{}{}
	for name, values := range req.Header {
		if strings.EqualFold(name, "X-Emby-Token") || strings.EqualFold(name, "X-MediaBrowser-Token") {
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					tokens[value] = struct{}{}
				}
			}
			continue
		}
		if !strings.EqualFold(name, "Authorization") && !strings.EqualFold(name, "X-Emby-Authorization") && !strings.EqualFold(name, "X-MediaBrowser-Authorization") {
			continue
		}
		for _, value := range values {
			auth := ParseEmbyAuthHeader(value)
			if (strings.EqualFold(auth.Scheme, "Emby") || strings.EqualFold(auth.Scheme, "MediaBrowser")) && auth.Token != "" {
				tokens[auth.Token] = struct{}{}
			}
		}
	}
	return tokens
}
