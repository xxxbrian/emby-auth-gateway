package gateway

import (
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

const resourceCookieName = "__Secure-EmbyGatewayResource"

func (s *Server) resourceCookiePath() string {
	base := "/" + strings.Trim(s.cfg.GatewayBasePath, "/")
	if base == "/" {
		return base
	}
	return strings.TrimRight(base, "/")
}

func (s *Server) setResourceCookie(w http.ResponseWriter, token string, expires time.Time) {
	remaining := time.Until(expires)
	maxAge := int(remaining.Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{Name: resourceCookieName, Value: token, Path: s.resourceCookiePath(), Expires: expires, MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (s *Server) clearResourceCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: resourceCookieName, Value: "", Path: s.resourceCookiePath(), MaxAge: -1, Expires: time.Unix(1, 0), Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

type resourceRouteKind uint8

const (
	resourceRouteNone resourceRouteKind = iota
	resourceRouteImage
	resourceRouteMedia
)

type resourceCookieContextKey struct{}

func resourceRoute(r *http.Request, rel string) resourceRouteKind {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return resourceRouteNone
	}
	if isUpgradeRequest(r) {
		return resourceRouteNone
	}
	if !canonicalResourceRoute(r, rel) {
		return resourceRouteNone
	}
	return resourceRouteKindForRel(r.Method, rel)
}

// resourceRouteKindForRel selects image vs media kind. Media admission is
// authoritative via routeclass.Classify so cookie recognition cannot drift from
// the finite classifier allowlists (including shallow and nested HLS forms).
func resourceRouteKindForRel(method, rel string) resourceRouteKind {
	parts := strings.Split(strings.TrimPrefix(rel, "/"), "/")
	// Image kind first: exact binary image shapes. Users images remain
	// cookie-classifiable for existing tokenless image cache policy even when
	// routeclass does not admit them as MediaProxy.
	if resourceImageParts(parts) {
		return resourceRouteImage
	}
	if resourceMediaAdmittedByRouteclass(method, rel) {
		return resourceRouteMedia
	}
	return resourceRouteNone
}

func resourceMediaAdmittedByRouteclass(method, rel string) bool {
	d := routeclass.Classify(method, rel)
	return d.Ownership == routeclass.MediaProxy &&
		d.Operation == routeclass.OperationMediaProxy &&
		d.MethodAllowed
}

func canonicalResourceRoute(r *http.Request, rel string) bool {
	if len(rel) < 2 || rel[0] != '/' || rel[1] == '/' || path.Clean(rel) != rel || strings.HasSuffix(rel, "/") || strings.Contains(rel, "\\") || hasURLControls(rel) {
		return false
	}
	escaped := strings.ToLower(r.URL.EscapedPath())
	return !strings.Contains(escaped, "%2f") && !strings.Contains(escaped, "%5c") && !strings.Contains(escaped, "%2e")
}

// resourceImageParts matches exact binary image templates:
//
//	/Items/{ItemId}/Images/{ImageType}[/{Index}]
//	/Users/{UserId}/Images/{ImageType}[/{Index}]  (decimal index only)
//
// Rejects Images list metadata, non-decimal indexes, and deeper descendants.
func resourceImageParts(parts []string) bool {
	if len(parts) != 4 && len(parts) != 5 {
		return false
	}
	if (!strings.EqualFold(parts[0], "Items") && !strings.EqualFold(parts[0], "Users")) ||
		parts[1] == "" || !strings.EqualFold(parts[2], "Images") || parts[3] == "" {
		return false
	}
	if len(parts) == 5 {
		return isDecimalPathSegment(parts[4])
	}
	return true
}

func isDecimalPathSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for _, r := range seg {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hasExplicitCredentialInput(r *http.Request) bool {
	for _, name := range []string{"X-Emby-Token", "X-MediaBrowser-Token", "X-Emby-Authorization", "Authorization"} {
		for _, value := range r.Header.Values(name) {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	q := r.URL.Query()
	for name, values := range q {
		if !isEgressCredentialQueryKey(name) {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

// hasAuthControlOccurrence preserves authentication precedence for anonymous
// routes: an empty reserved control is still an explicit authentication attempt.
func hasAuthControlOccurrence(r *http.Request) bool {
	for key := range r.Header {
		for _, name := range []string{"X-Emby-Token", "X-MediaBrowser-Token", "X-Emby-Authorization", "Authorization"} {
			if strings.EqualFold(key, name) {
				return true
			}
		}
	}
	q, err := parseRawQuery(r.URL.RawQuery)
	if err != nil {
		return true
	}
	for name := range q {
		if isEgressCredentialQueryKey(name) {
			return true
		}
	}
	return false
}

func resourceCookieToken(r *http.Request, rel string) (string, resourceRouteKind, bool) {
	kind := resourceRoute(r, rel)
	if kind == resourceRouteNone || hasExplicitCredentialInput(r) {
		return "", resourceRouteNone, false
	}
	var token string
	for _, cookie := range r.Cookies() {
		if cookie.Name != resourceCookieName {
			continue
		}
		if token != "" || strings.TrimSpace(cookie.Value) == "" {
			return "", resourceRouteNone, false
		}
		token = cookie.Value
	}
	return token, kind, token != ""
}

func resourceRouteFromContext(r *http.Request) resourceRouteKind {
	kind, _ := r.Context().Value(resourceCookieContextKey{}).(resourceRouteKind)
	return kind
}

func isResourceRedirectPath(rel string) bool {
	if len(rel) < 2 || rel[0] != '/' || strings.Contains(rel, "\\") || hasURLControls(rel) {
		return false
	}
	parts := strings.Split(rel[1:], "/")
	for _, part := range parts {
		if part == "" {
			return false
		}
	}
	if resourceImageParts(parts) {
		return true
	}
	// Path-only redirect ownership uses GET admission (media templates are GET/HEAD).
	return resourceMediaAdmittedByRouteclass(http.MethodGet, rel)
}

func applyResourceCachePolicy(h http.Header, kind resourceRouteKind, status int) {
	if kind == resourceRouteNone {
		return
	}
	mergeVaryCookie(h)
	if kind == resourceRouteImage || (status != http.StatusOK && status != http.StatusPartialContent && status != http.StatusNotModified) {
		h.Set("Cache-Control", "private, no-store")
		return
	}
	if hasNoStoreDirective(h.Values("Cache-Control")) {
		h.Set("Cache-Control", "private, no-store")
		return
	}
	h.Set("Cache-Control", "private")
}

func hasNoStoreDirective(values []string) bool {
	for _, value := range values {
		for _, directive := range strings.Split(value, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(directive), "=")
			if strings.EqualFold(strings.TrimSpace(name), "no-store") {
				return true
			}
		}
	}
	return false
}

func mergeVaryCookie(h http.Header) {
	values := h.Values("Vary")
	for _, value := range values {
		if strings.TrimSpace(value) == "*" {
			return
		}
	}
	seen := map[string]bool{}
	parts := []string{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" || seen[strings.ToLower(part)] {
				continue
			}
			seen[strings.ToLower(part)] = true
			parts = append(parts, part)
		}
	}
	if !seen["cookie"] {
		parts = append(parts, "Cookie")
	}
	h.Del("Vary")
	h.Set("Vary", strings.Join(parts, ", "))
}

func resourceCookieMatches(r *http.Request, token string) bool {
	value, ok := resourceCookieTokenForLogout(r)
	return ok && value == token
}

func resourceCookieTokenForLogout(r *http.Request) (string, bool) {
	var token string
	for _, cookie := range r.Cookies() {
		if cookie.Name != resourceCookieName {
			continue
		}
		if token != "" || cookie.Value == "" {
			return "", false
		}
		token = cookie.Value
	}
	return token, token != ""
}

func stripResourceCookie(h http.Header) {
	values := h.Values("Cookie")
	if len(values) == 0 {
		return
	}
	var kept []string
	for _, value := range values {
		for _, part := range strings.Split(value, ";") {
			name, _, found := strings.Cut(strings.TrimSpace(part), "=")
			if found && name == resourceCookieName {
				continue
			}
			if strings.TrimSpace(part) != "" {
				kept = append(kept, strings.TrimSpace(part))
			}
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}
