package gateway

import (
	"net/http"
	"strings"
	"time"
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

func resourceImagePath(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 4 && len(parts) != 5 {
		return false
	}
	if (!strings.EqualFold(parts[0], "Items") && !strings.EqualFold(parts[0], "Users")) || parts[1] == "" || !strings.EqualFold(parts[2], "Images") || parts[3] == "" {
		return false
	}
	if len(parts) == 5 {
		if parts[4] == "" {
			return false
		}
		for _, r := range parts[4] {
			if r < '0' || r > '9' {
				return false
			}
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
	for _, name := range append(append([]string{}, strictQueryAuthKeys...), genericQueryAuthKey) {
		for _, value := range q[name] {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func resourceCookieToken(r *http.Request, rel string) (string, bool) {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || !resourceImagePath(rel) || hasExplicitCredentialInput(r) {
		return "", false
	}
	var token string
	for _, cookie := range r.Cookies() {
		if cookie.Name != resourceCookieName {
			continue
		}
		if token != "" || strings.TrimSpace(cookie.Value) == "" {
			return "", false
		}
		token = cookie.Value
	}
	return token, token != ""
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
