package embyweb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// GuardHandler wraps next with a package-owned raw-path guard for composition
// layers (e.g. cmd/gateway) so ServeMux cannot clean/redirect traversal under
// /emby/web into API routes.
//
// Behavior:
//   - Non-Web paths (including lookalikes /emby/webX and /emby/websocket) pass
//     through to next unchanged.
//   - Canonical /emby/web, /emby/web/, and canonical descendants pass to next.
//   - Literal or encoded invalid/traversal/separator/backslash/NUL/empty-interior
//     paths that are Web-prefixed on either the escaped or decoded form return
//     404 with Cache-Control: no-store (including encoded "web" segments such as
//     /emby/%77eb/...).
//
// Query strings are preserved (the request is not rewritten). Validation rules
// match Server path checks; do not reimplement them at the composition layer.
func GuardHandler(next http.Handler) http.Handler {
	if next == nil {
		next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isRequestWebPrefixed(r) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok, _, _ := parseWebPath(r); !ok {
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) serveUnavailable(w http.ResponseWriter, r *http.Request) {
	rel, ok, isRoot, isRootSlash := parseWebPath(r)
	if !ok {
		// Path is not a well-formed web path: still classify methods for
		// non-canary OPTIONS / unsupported methods when the path shape is
		// under the web prefix; otherwise 404-like unavailability is fine.
		// Prefer state-independent method classification where the path is
		// parseable enough to know canary-ness.
		if methodUnsupported(r.Method) {
			writeAllow(w, false)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodOptions {
			// Non-canary OPTIONS (or unparseable): 405.
			writeAllow(w, false)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	canary := isRootSlash || isCanaryPath(rel)
	// Root without slash is not a canary resource path for OPTIONS purposes;
	// only exact canary file paths and the slash-root (index) count.
	if isRoot {
		canary = false
	}
	if isRootSlash {
		canary = isCanaryPath("index.html")
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	case http.MethodOptions:
		if canary {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		writeAllow(w, false)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	default:
		writeAllow(w, canary)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) serveReady(w http.ResponseWriter, r *http.Request) {
	rel, ok, isRoot, isRootSlash := parseWebPath(r)
	if !ok {
		if methodUnsupported(r.Method) {
			writeAllow(w, false)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodOptions {
			writeAllow(w, false)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		http.NotFound(w, r)
		return
	}

	// /emby/web -> 308 /emby/web/ preserving RawQuery (GET/HEAD only).
	if isRoot {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			target := webPrefixSlash
			if r.URL.RawQuery != "" {
				target = target + "?" + r.URL.RawQuery
			}
			w.Header().Set("Location", target)
			w.WriteHeader(http.StatusPermanentRedirect) // 308
			return
		case http.MethodOptions:
			writeAllow(w, false)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		default:
			writeAllow(w, false)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	}

	// Map slash-root to index.html.
	assetPath := rel
	if isRootSlash {
		assetPath = "index.html"
	}

	canary := isCanaryPath(assetPath)

	switch r.Method {
	case http.MethodOptions:
		if !canary {
			writeAllow(w, false)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveCanaryOPTIONS(w, r)
		return
	case http.MethodGet, http.MethodHead:
		// continue
	default:
		writeAllow(w, canary)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Directory-like non-root paths are never served (no listing, no SPA).
	if strings.HasSuffix(rel, "/") && !isRootSlash {
		http.NotFound(w, r)
		return
	}

	s.serveDiskAsset(w, r, assetPath, canary)
}

func (s *Server) serveDiskAsset(w http.ResponseWriter, r *http.Request, assetPath string, canary bool) {
	full, err := s.resolveAssetPath(assetPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Host injection is serve-time only: on-disk bytes are never modified.
	if needsHostInject(assetPath) {
		data, err := readRegularFileLimited(full, maxInjectFileBytes)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		host := injectHostForRequest(r, s.fallbackHost)
		body := rewriteHostPlaceholder(data, host)
		etag := etagForBytes(body)

		h := w.Header()
		h.Set("Content-Type", contentTypeFor(assetPath))
		h.Set("ETag", etag)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		h.Set("Vary", "Host")
		if canary {
			applySimpleCORS(w, r)
		}
		http.ServeContent(w, r, path.Base(assetPath), time.Time{}, bytes.NewReader(body))
		return
	}

	f, err := openRegularFileNoFollow(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	h := w.Header()
	h.Set("Content-Type", contentTypeFor(assetPath))
	h.Set("ETag", etagForFileInfo(st))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	switch cacheClassFor(assetPath) {
	case cacheImmutable:
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		h.Set("Cache-Control", "no-cache")
	}
	if canary {
		applySimpleCORS(w, r)
	}

	// ServeContent handles Range, If-Range, If-None-Match, HEAD, Accept-Ranges.
	http.ServeContent(w, r, path.Base(assetPath), st.ModTime(), f)
}

// resolveAssetPath joins a validated relative asset path under the ready root.
func (s *Server) resolveAssetPath(assetPath string) (string, error) {
	if s == nil || s.root == "" || !validAssetPath(assetPath) {
		return "", os.ErrNotExist
	}
	full := filepath.Join(s.root, filepath.FromSlash(assetPath))
	// filepath.Join cleaned the path; require it still lives under root.
	rel, err := filepath.Rel(s.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", os.ErrNotExist
	}
	return full, nil
}

func readRegularFileLimited(path string, limit int64) ([]byte, error) {
	f, err := openRegularFileNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds size limit")
	}
	return data, nil
}

func etagForFileInfo(st os.FileInfo) string {
	// Weak ETag from size + mtime; content is trusted operator-supplied disk.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())))
	return `W/"` + hex.EncodeToString(sum[:8]) + `"`
}

// canaryPreflightVary lists Vary tokens required on every canary OPTIONS so
// caches never mix grants across origin/method/headers/PNA.
var canaryPreflightVary = []string{
	"Origin",
	"Access-Control-Request-Method",
	"Access-Control-Request-Headers",
	"Access-Control-Request-Private-Network",
}

func (s *Server) serveCanaryOPTIONS(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	// Always no-store + full preflight Vary, including no-grant responses.
	h.Set("Cache-Control", "no-store")
	mergeVary(h, canaryPreflightVary...)

	origin := r.Header.Get("Origin")
	reqMethod := strings.TrimSpace(r.Header.Get("Access-Control-Request-Method"))
	reqHdrs := strings.TrimSpace(r.Header.Get("Access-Control-Request-Headers"))

	// Require an explicit GET or HEAD request method; missing/other => no grant.
	if reqMethod != http.MethodGet && reqMethod != http.MethodHead {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// V1 empty header allowlist: any non-empty requested headers => no grant.
	if reqHdrs != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if origin != allowedCORSOrig {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	h.Set("Access-Control-Allow-Origin", allowedCORSOrig)
	h.Set("Access-Control-Allow-Methods", "GET, HEAD")
	// No Access-Control-Allow-Credentials.

	if strings.EqualFold(r.Header.Get("Access-Control-Request-Private-Network"), "true") {
		h.Set("Access-Control-Allow-Private-Network", "true")
	}

	w.WriteHeader(http.StatusNoContent)
}

func applySimpleCORS(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	// Always Vary: Origin on canary GET/HEAD, including missing/disallowed Origin.
	mergeVary(h, "Origin")
	if r.Header.Get("Origin") != allowedCORSOrig {
		return
	}
	h.Set("Access-Control-Allow-Origin", allowedCORSOrig)
}

func mergeVary(h http.Header, tokens ...string) {
	existing := h.Get("Vary")
	parts := make([]string, 0, 8)
	if existing != "" {
		for _, p := range strings.Split(existing, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				parts = append(parts, p)
			}
		}
	}
	for _, tok := range tokens {
		if tok == "" || varyHasList(parts, tok) {
			continue
		}
		parts = append(parts, tok)
	}
	if len(parts) > 0 {
		h.Set("Vary", strings.Join(parts, ", "))
	}
}

func varyHasList(parts []string, token string) bool {
	for _, p := range parts {
		if strings.EqualFold(p, token) {
			return true
		}
	}
	return false
}

func writeAllow(w http.ResponseWriter, canary bool) {
	if canary {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
	} else {
		w.Header().Set("Allow", "GET, HEAD")
	}
}

func methodUnsupported(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func requestEscapedPath(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	// Prefer EscapedPath for encoded-form checks; fall back to RawPath then Path.
	escaped := r.URL.EscapedPath()
	if escaped == "" {
		escaped = r.URL.RawPath
	}
	if escaped == "" {
		escaped = r.URL.Path
	}
	return escaped
}

// isRequestWebPrefixed reports whether the request targets the Web tree on
// either the escaped wire form or the decoded URL.Path (including partially
// encoded prefixes such as /emby/%77eb/...). Lookalikes /emby/webX and
// /emby/websocket remain non-Web on both forms.
func isRequestWebPrefixed(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	escaped := requestEscapedPath(r)
	if isPathWebPrefixed(escaped) {
		return true
	}
	if r.URL.Path != "" && isPathWebPrefixed(r.URL.Path) {
		return true
	}
	if decoded, err := url.PathUnescape(escaped); err == nil && decoded != escaped && isPathWebPrefixed(decoded) {
		return true
	}
	return false
}

// isPathWebPrefixed reports whether p is under the exact /emby/web path boundary.
// Encoded separators immediately after /emby/web are treated as Web-prefixed
// attack surface. Lookalikes such as /emby/webX and /emby/websocket are not Web.
func isPathWebPrefixed(p string) bool {
	if p == "" {
		return false
	}
	if p == webPrefix || p == webPrefixSlash {
		return true
	}
	if strings.HasPrefix(p, webPrefixSlash) {
		return true
	}
	if !strings.HasPrefix(p, webPrefix) {
		// Also detect decoded-equivalent prefixes where "web" was percent-encoded
		// in the escaped form: handled by caller via PathUnescape + re-check.
		return false
	}
	rest := p[len(webPrefix):]
	if rest == "" {
		return true
	}
	switch rest[0] {
	case '/', '\\':
		return true
	}
	lower := strings.ToLower(rest)
	if strings.HasPrefix(lower, "%2f") || strings.HasPrefix(lower, "%5c") || strings.HasPrefix(lower, "%00") {
		return true
	}
	return false
}

// parseWebPath validates the request URL against the exact /emby/web prefix
// without cleaning-then-serving. It inspects Path and EscapedPath/RawPath for
// encoded traversal and separator ambiguity.
//
// Returns:
//   - rel: path relative to /emby/web/ (empty when isRootSlash)
//   - ok: well-formed under the web prefix
//   - isRoot: exact /emby/web
//   - isRootSlash: exact /emby/web/
func parseWebPath(r *http.Request) (rel string, ok bool, isRoot bool, isRootSlash bool) {
	if r == nil || r.URL == nil {
		return "", false, false, false
	}

	// Serving only accepts fully canonical escaped + decoded agreement under
	// the literal /emby/web prefix. Encoded-prefix attacks are Web-prefixed for
	// the guard but never ok for serving.
	escaped := requestEscapedPath(r)
	if !isPathWebPrefixed(escaped) {
		return "", false, false, false
	}
	if !safeEscapedWebPath(escaped) {
		return "", false, false, false
	}

	// Decode for logical path checks; reject if re-escape semantics differ in
	// dangerous ways (already covered by safeEscapedWebPath for separators).
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return "", false, false, false
	}
	if !safeDecodedWebPath(decoded) {
		return "", false, false, false
	}

	// Also require r.URL.Path (Go's decoded path) to agree with our decode for
	// the web prefix region when Path is set.
	if r.URL.Path != "" && r.URL.Path != decoded {
		// Allow only if Path is the request path Go already decoded equivalently.
		// Mismatch indicates ambiguous encoding; reject.
		if path.Clean(r.URL.Path) != path.Clean(decoded) || r.URL.Path != decoded {
			return "", false, false, false
		}
	}

	switch decoded {
	case webPrefix:
		return "", true, true, false
	case webPrefixSlash:
		return "", true, false, true
	}

	if !strings.HasPrefix(decoded, webPrefixSlash) {
		return "", false, false, false
	}

	rel = strings.TrimPrefix(decoded, webPrefixSlash)
	// Trailing slash on a non-root path: keep for directory detection, but the
	// relative form with trailing slash is never a valid asset key.
	if rel == "" {
		return "", true, false, true
	}

	// Reject non-canonical relative forms (dot segments already rejected).
	if strings.Contains(rel, "\\") || strings.Contains(rel, "\x00") {
		return "", false, false, false
	}

	// If it ends with '/', treat as directory request (caller 404s).
	if strings.HasSuffix(rel, "/") {
		// Validate interior without trailing slash.
		trim := strings.TrimSuffix(rel, "/")
		if trim == "" || !validAssetPath(trim) {
			return "", false, false, false
		}
		return rel, true, false, false
	}

	if !validAssetPath(rel) {
		return "", false, false, false
	}
	return rel, true, false, false
}

// safeEscapedWebPath rejects encoded traversal, backslashes, NULs, empty
// interior segments, and other separator ambiguity before any clean/serve.
// Caller must already establish isRawWebPrefixed.
func safeEscapedWebPath(escaped string) bool {
	if escaped == "" {
		return false
	}

	lower := strings.ToLower(escaped)

	// Reject backslash and encoded backslash.
	if strings.Contains(escaped, `\`) || strings.Contains(lower, "%5c") {
		return false
	}
	// Reject NUL and encoded NUL.
	if strings.Contains(escaped, "\x00") || strings.Contains(lower, "%00") {
		return false
	}
	// Reject encoded slash (path separator ambiguity).
	if strings.Contains(lower, "%2f") {
		return false
	}
	// Reject encoded dots that form dot segments.
	if strings.Contains(lower, "%2e") {
		return false
	}

	// Reject empty interior segments ("//") and dot segments in escaped form.
	rest := ""
	switch {
	case escaped == webPrefix || escaped == webPrefixSlash:
		return true
	case strings.HasPrefix(escaped, webPrefixSlash):
		rest = escaped[len(webPrefixSlash):]
	default:
		// /emby/web%2f... already rejected above via %2f; other non-slash
		// continuations are not valid web paths.
		return false
	}

	if rest == "" {
		return true
	}

	// Trailing slash is ok (directory form); interior empty is not.
	parts := strings.Split(rest, "/")
	for i, p := range parts {
		if p == "" {
			// Allow only a final empty part (trailing slash).
			if i != len(parts)-1 {
				return false
			}
			continue
		}
		if p == "." || p == ".." {
			return false
		}
		// Require that unescaping each segment succeeds and does not introduce
		// separators or dots.
		seg, err := url.PathUnescape(p)
		if err != nil {
			return false
		}
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		if strings.ContainsAny(seg, `/\`+"\x00") {
			return false
		}
	}
	return true
}

func safeDecodedWebPath(decoded string) bool {
	if decoded == "" {
		return false
	}
	if strings.ContainsAny(decoded, `\`+"\x00") {
		return false
	}
	if decoded != webPrefix && decoded != webPrefixSlash && !strings.HasPrefix(decoded, webPrefixSlash) {
		if !strings.HasPrefix(decoded, webPrefix) {
			return false
		}
		if len(decoded) > len(webPrefix) && decoded[len(webPrefix)] != '/' {
			return false
		}
		return decoded == webPrefix
	}

	// path.Clean must not change the path (no . / .. / empty segments).
	if path.Clean(decoded) != decoded && decoded != webPrefix {
		// path.Clean("/emby/web/") => "/emby/web" — allow the slash root.
		if decoded == webPrefixSlash && path.Clean(decoded) == webPrefix {
			return true
		}
		// Allow trailing-slash directory forms: clean strips trailing slash.
		if strings.HasSuffix(decoded, "/") {
			trimmed := strings.TrimSuffix(decoded, "/")
			if path.Clean(trimmed) == trimmed && strings.HasPrefix(trimmed, webPrefixSlash) {
				return true
			}
		}
		return false
	}
	return true
}
