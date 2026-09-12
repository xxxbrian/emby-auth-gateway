package webcontext

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func manager(t *testing.T) *Manager {
	t.Helper()
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return m
}

func index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("ETag", `"document"`)
	w.Header().Set("Vary", "Host")
	_, _ = io.WriteString(w, "<!doctype html><title>Emby</title>")
}

func issuedCookie(t *testing.T, m *Manager) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	m.Wrap(http.HandlerFunc(index)).ServeHTTP(w, httptest.NewRequest("GET", "https://gateway.example/emby/web/", nil))
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %#v", cookies)
	}
	return cookies[0]
}

func TestMarkerIsStableAndBoundToDocumentHost(t *testing.T) {
	m := manager(t)
	cookie := issuedCookie(t, m)
	if cookie.Name != CookieName || cookie.Path != "/emby" || cookie.Domain != "" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 86400 {
		t.Fatalf("unsafe cookie attributes: %#v", cookie)
	}
	for _, target := range []string{"https://gateway.example/emby/web/", "https://gateway.example/emby/web/index.html?cache=1", "https://GATEWAY.EXAMPLE/emby/Videos/1/Subtitles/4/0/Stream.vtt"} {
		r := httptest.NewRequest("GET", target, nil)
		r.AddCookie(cookie)
		if !m.Valid(r) {
			t.Fatalf("valid marker rejected at %s", target)
		}
		w := httptest.NewRecorder()
		m.Wrap(http.HandlerFunc(index)).ServeHTTP(w, r)
		if len(w.Result().Cookies()) != 0 {
			t.Fatal("valid context rotated on another tab/request")
		}
	}
	r := httptest.NewRequest("GET", "https://other.example/emby/web/", nil)
	r.AddCookie(cookie)
	if m.Valid(r) {
		t.Fatal("marker crossed host boundary")
	}
}

func TestOnlySuccessfulCanonicalHTMLGetsIssueMarker(t *testing.T) {
	m := manager(t)
	for _, tc := range []struct {
		name, method, path, contentType string
		status                          int
		want                            bool
	}{
		{"root", "GET", "/emby/web/", "text/html", 200, true},
		{"index", "GET", "/emby/web/index.html?x=1", "text/html; charset=utf-8", 200, true},
		{"revalidate", "GET", "/emby/web/index.html", "", 304, true},
		{"non-html", "GET", "/emby/web/", "application/json", 200, false},
		{"redirect", "GET", "/emby/web", "text/html", 308, false},
		{"head", "HEAD", "/emby/web/", "text/html", 200, false},
		{"post", "POST", "/emby/web/", "text/html", 200, false},
		{"options", "OPTIONS", "/emby/web/index.html", "text/html", 200, false},
		{"missing", "GET", "/emby/web/", "text/html", 404, false},
		{"unavailable", "GET", "/emby/web/", "text/html", 503, false},
		{"partial", "GET", "/emby/web/index.html", "text/html", 206, false},
		{"auth", "POST", "/emby/Users/AuthenticateByName", "text/html", 200, false},
		{"playback", "GET", "/emby/Items/1/PlaybackInfo", "text/html", 200, false},
		{"asset", "GET", "/emby/web/modules/app.js", "text/html", 200, false},
		{"encoded-segment", "GET", "/emby/%77eb/index.html", "text/html", 200, false},
		{"encoded-file", "GET", "/emby/web/%69ndex.html", "text/html", 200, false},
		{"case", "GET", "/emby/Web/index.html", "text/html", 200, false},
		{"traversal", "GET", "/emby/web/a/../index.html", "text/html", 200, false},
		{"double-slash", "GET", "/emby/web//index.html", "text/html", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
			})).ServeHTTP(w, httptest.NewRequest(tc.method, "https://gateway.example"+tc.path, nil))
			if got := len(w.Result().Cookies()) == 1; got != tc.want {
				t.Fatalf("issued = %v, want %v", got, tc.want)
			}
			if w.Code != tc.status {
				t.Fatalf("status changed: %d", w.Code)
			}
		})
	}
}

func TestInvalidMarkerNeverConfirmsWeb(t *testing.T) {
	m := manager(t)
	cookie := issuedCookie(t, m)
	for _, tc := range []struct{ name, value string }{
		{"empty", ""}, {"junk", "not-a-marker"}, {"length", cookie.Value + "x"},
		{"tampered", strings.Repeat("A", len(cookie.Value))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://gateway.example/emby/web/", nil)
			r.AddCookie(&http.Cookie{Name: CookieName, Value: tc.value})
			if m.Valid(r) {
				t.Fatal("invalid marker accepted")
			}
		})
	}
	for _, sameHeader := range []bool{false, true} {
		r := httptest.NewRequest("GET", "https://gateway.example/emby/web/", nil)
		r.AddCookie(cookie)
		if sameHeader {
			r.Header.Set("Cookie", r.Header.Get("Cookie")+"; "+CookieName+"="+cookie.Value)
		} else {
			r.Header.Add("Cookie", CookieName+"="+cookie.Value)
		}
		if m.Valid(r) {
			t.Fatal("ambiguous duplicate cookies accepted")
		}
	}
	r := httptest.NewRequest("GET", "https://gateway.example/emby/web/", nil)
	r.AddCookie(&http.Cookie{Name: "other", Value: "keep"})
	r.AddCookie(cookie)
	if !m.Valid(r) {
		t.Fatal("unrelated cookie affected validation")
	}
	other := manager(t)
	if other.Valid(r) {
		t.Fatal("marker survived signing key change")
	}
	m.now = func() time.Time { return cookie.Expires }
	if m.Valid(r) {
		t.Fatal("expired marker accepted")
	}
	if (*Manager)(nil).Valid(r) || (&Manager{}).Valid(r) || m.Valid(nil) {
		t.Fatal("uninitialized manager accepted marker")
	}
}

func TestSignedFutureAndUnknownVersionAreRejected(t *testing.T) {
	m := manager(t)
	cookie := issuedCookie(t, m)
	token, _ := base64.RawURLEncoding.DecodeString(cookie.Value)
	for _, kind := range []string{"future", "version"} {
		payload := bytes.Clone(token[:payloadSize])
		if kind == "future" {
			binary.BigEndian.PutUint64(payload[1:9], uint64(m.now().Add(2*lifetime).Unix()))
		} else {
			payload[0]++
		}
		value := base64.RawURLEncoding.EncodeToString(append(payload, m.sign(payload, "gateway.example")...))
		r := httptest.NewRequest("GET", "https://gateway.example/emby/web/", nil)
		r.AddCookie(&http.Cookie{Name: CookieName, Value: value})
		if m.Valid(r) {
			t.Fatalf("%s marker accepted", kind)
		}
	}
}

func TestSecureExceptLoopbackDevelopment(t *testing.T) {
	m := manager(t)
	for _, tc := range []struct {
		target, forwarded string
		tls, secure       bool
	}{
		{"http://localhost:18090/emby/web/", "", false, false},
		{"http://127.0.0.1:18090/emby/web/", "", false, false},
		{"http://[::1]:18090/emby/web/", "", false, false},
		{"https://localhost:18090/emby/web/", "", false, true},
		{"http://127.0.0.1:18090/emby/web/", "https", false, true},
		{"http://localhost:18090/emby/web/", "", true, true},
		{"http://gateway.example/emby/web/", "", false, true},
	} {
		r := httptest.NewRequest("GET", tc.target, nil)
		r.Header.Set("X-Forwarded-Proto", tc.forwarded)
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		w := httptest.NewRecorder()
		m.Wrap(http.HandlerFunc(index)).ServeHTTP(w, r)
		if cookies := w.Result().Cookies(); len(cookies) != 1 || cookies[0].Secure != tc.secure {
			t.Fatalf("%s: cookies = %#v", tc.target, cookies)
		}
	}
}

func TestHeaderBodyAndRevalidationSemantics(t *testing.T) {
	m := manager(t)
	r := httptest.NewRequest("GET", "https://gateway.example/emby/web/", nil)
	w := httptest.NewRecorder()
	m.Wrap(http.HandlerFunc(index)).ServeHTTP(w, r)
	if w.Body.String() != "<!doctype html><title>Emby</title>" || w.Header().Get("ETag") != `"document"` || w.Header().Get("Cache-Control") != "private, no-cache" || !varyContains(w.Header(), "Host") || !varyContains(w.Header(), "Cookie") {
		t.Fatalf("unexpected document response: %s %v", w.Body.String(), w.Header())
	}
	for _, etag := range []string{"", `"document"`} {
		w = httptest.NewRecorder()
		r = httptest.NewRequest("GET", "https://gateway.example/emby/web/", nil)
		r.Header.Set("If-None-Match", etag)
		m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("ETag", `"document"`)
			http.ServeContent(w, r, "index.html", time.Time{}, strings.NewReader("<!doctype html>document"))
		})).ServeHTTP(w, r)
		want := 200
		if etag != "" {
			want = 304
		}
		if w.Code != want || len(w.Result().Cookies()) != 1 {
			t.Fatalf("revalidation status/cookies = %d/%#v", w.Code, w.Result().Cookies())
		}
	}
}

func TestResponseControllerAndFailureSemantics(t *testing.T) {
	m := manager(t)
	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "https://gateway.example/emby/web/", nil).WithContext(ctx)
	m.Wrap(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		if req != r || !errors.Is(req.Context().Err(), context.Canceled) {
			t.Fatal("request context changed")
		}
		out.Header().Set("Content-Type", "text/html")
		if err := http.NewResponseController(out).Flush(); err != nil {
			t.Fatal(err)
		}
		if _, ok := out.(http.Flusher); !ok {
			t.Fatal("legacy flush interface lost")
		}
		_, _ = out.Write([]byte("body"))
	})).ServeHTTP(w, r)
	if !w.Flushed || w.Body.String() != "body" || len(w.Result().Cookies()) != 1 {
		t.Fatal("flush/body changed")
	}
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("abort panic changed: %v", got)
		}
	}()
	m.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })).ServeHTTP(httptest.NewRecorder(), r)
}

func TestNonDocumentBypassesWriterAndRequestWrapping(t *testing.T) {
	m := manager(t)
	for _, target := range []string{"/emby/Videos/1/stream.mkv", "/emby/Users/AuthenticateByName", "/emby/web/app.js", "/admin/"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "https://gateway.example"+target, nil)
		m.Wrap(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
			if out != w || req != r {
				t.Fatal("unrelated traffic was wrapped")
			}
		})).ServeHTTP(w, r)
	}
}
