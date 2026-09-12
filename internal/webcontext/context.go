// Package webcontext marks visits to the gateway's own Emby Web document.
// A marker is not an authentication credential or a client identity: callers
// must also validate the gateway session and its Emby client/device identity.
package webcontext

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
)

// CookieName is reserved for this gateway and must never reach an upstream.
const CookieName = "EmbyGatewayWebContext"

const (
	lifetime    = 24 * time.Hour
	payloadSize = 1 + 8 + 16 // format version, expiry, random browser context
	tokenSize   = payloadSize + sha256.Size
)

// Manager validates markers using a process-local signing key. A restart
// invalidates existing markers; loading the Web document issues a fresh one.
// It has no state or lifecycle relationship with native client sessions.
type Manager struct {
	key [32]byte
	now func() time.Time
}

func New() (*Manager, error) {
	m := &Manager{now: time.Now}
	if _, err := rand.Read(m.key[:]); err != nil {
		return nil, err
	}
	return m, nil
}

// Valid reports whether exactly one unexpired marker belongs to this host.
// Missing, malformed, duplicate, and old-process markers all fail closed for
// the optional Web feature; callers must not reject ordinary playback for it.
func (m *Manager) Valid(r *http.Request) bool {
	if m == nil || m.now == nil || r == nil || r.Host == "" {
		return false
	}
	var value string
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == CookieName {
			count++
			value = c.Value
		}
	}
	if count != 1 || len(value) != base64.RawURLEncoding.EncodedLen(tokenSize) {
		return false
	}
	token, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(token) != tokenSize || token[0] != 1 {
		return false
	}
	until := binary.BigEndian.Uint64(token[1:9])
	now := m.now().Unix()
	if now < 0 || until <= uint64(now) || until > uint64(now+int64(lifetime/time.Second)) {
		return false
	}
	return hmac.Equal(token[payloadSize:], m.sign(token[:payloadSize], r.Host))
}

// Wrap should be mounted only around the ready local Emby Web handler. It
// issues markers on successful canonical document GETs, never on API traffic,
// assets, redirects, errors, partial content, or native authentication calls.
// Neither requests nor bodies are buffered, and panics/cancellation propagate.
func (m *Manager) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m == nil || m.now == nil || !documentRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		var cookie *http.Cookie
		if !m.Valid(r) {
			cookie = m.cookie(r)
		}
		mw := &markerWriter{ResponseWriter: w, cookie: cookie}
		// Preserve the legacy flushing interface where it existed. Other
		// response controls remain accessible through ResponseController.
		if _, ok := w.(http.Flusher); ok {
			next.ServeHTTP(flushingWriter{mw}, r)
		} else {
			next.ServeHTTP(mw, r)
		}
	})
}

func documentRequest(r *http.Request) bool {
	if r == nil || r.URL == nil || r.Host == "" || r.Method != http.MethodGet || r.Header.Get("Upgrade") != "" {
		return false
	}
	// Compare escaped and decoded paths: alternate encodings, traversal,
	// case variants and cleaned paths do not establish a Web context.
	p := r.URL.Path
	return (p == "/emby/web/" || p == "/emby/web/index.html") && r.URL.EscapedPath() == p
}

func (m *Manager) cookie(r *http.Request) *http.Cookie {
	until := m.now().Add(lifetime).Truncate(time.Second)
	token := make([]byte, payloadSize, tokenSize)
	token[0] = 1
	binary.BigEndian.PutUint64(token[1:9], uint64(until.Unix()))
	if _, err := rand.Read(token[9:]); err != nil {
		// Entropy failure must only disable the optional feature.
		return nil
	}
	token = append(token, m.sign(token, r.Host)...)
	return &http.Cookie{
		Name: CookieName, Value: base64.RawURLEncoding.EncodeToString(token),
		Path: "/emby", Expires: until, MaxAge: int(lifetime / time.Second),
		Secure: secureRequest(r), HttpOnly: true, SameSite: http.SameSiteStrictMode,
	}
}

func (m *Manager) sign(payload []byte, host string) []byte {
	mac := hmac.New(sha256.New, m.key[:])
	_, _ = mac.Write(payload)
	_, _ = mac.Write([]byte("\x00" + strings.ToLower(host)))
	return mac.Sum(nil)
}

func secureRequest(r *http.Request) bool {
	if r.TLS != nil || strings.EqualFold(r.URL.Scheme, "https") || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	// A cleartext cookie is allowed only for loopback development. Public
	// HTTP cannot establish a usable context accidentally through a proxy.
	return !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback())
}

type markerWriter struct {
	http.ResponseWriter
	cookie      *http.Cookie
	wroteHeader bool
}

func (w *markerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *markerWriter) WriteHeader(code int) {
	w.prepare(code, nil)
	w.ResponseWriter.WriteHeader(code)
}

func (w *markerWriter) Write(p []byte) (int, error) {
	w.prepare(http.StatusOK, p)
	return w.ResponseWriter.Write(p)
}

func (w *markerWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *markerWriter) prepare(code int, sniff []byte) {
	if w.wroteHeader || code < 200 && code != http.StatusSwitchingProtocols || code > 999 {
		return
	}
	w.wroteHeader = true
	contentType := w.Header().Get("Content-Type")
	if contentType == "" && len(sniff) > 0 {
		contentType = http.DetectContentType(sniff)
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if code != http.StatusNotModified && (code != http.StatusOK || mediaType != "text/html") {
		return
	}
	// A shared intermediary must not replay one visitor's context. Existing
	// ETag/Last-Modified validators and 304 revalidation continue to work.
	w.Header().Set("Cache-Control", "private, no-cache")
	if !varyContains(w.Header(), "Cookie") {
		w.Header().Add("Vary", "Cookie")
	}
	if w.cookie != nil {
		http.SetCookie(w, w.cookie)
	}
}

func varyContains(h http.Header, name string) bool {
	for _, value := range h.Values("Vary") {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part == "*" || strings.EqualFold(part, name) {
				return true
			}
		}
	}
	return false
}

type flushingWriter struct{ *markerWriter }

func (w flushingWriter) Flush() { _ = w.FlushError() }
