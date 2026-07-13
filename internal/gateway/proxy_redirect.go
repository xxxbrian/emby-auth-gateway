package gateway

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

var errCrossOriginRedirectBody = errors.New("cross-origin redirect with request body rejected")

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
			sanitizeCrossOriginRedirect(req, redirectTokens(via[0]))
			if req.Body != nil && req.Body != http.NoBody {
				return errCrossOriginRedirectBody
			}
		}
		return nil
	}
	return client
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
	if len(tokens) == 0 {
		return
	}
	req.URL.RawQuery = removeSensitiveQuerySegments(req.URL.RawQuery, tokens)
}

func removeSensitiveQuerySegments(rawQuery string, tokens map[string]struct{}) string {
	segments := strings.Split(rawQuery, "&")
	kept := make([]string, 0, len(segments))
	removed := false
	for _, segment := range segments {
		_, value, hasValue := strings.Cut(segment, "=")
		decoded, err := url.QueryUnescape(value)
		if hasValue && err == nil {
			if _, sensitive := tokens[decoded]; sensitive {
				removed = true
				continue
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
