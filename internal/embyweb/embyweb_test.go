package embyweb

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeReadyTree(t *testing.T, root string, extra map[string]string) {
	t.Helper()
	files := map[string]string{
		"index.html":         "<!doctype html><title>emby</title>",
		"manifest.json":      `{"name":"emby"}`,
		"strings/en-US.json": `{"Ok":"Ok"}`,
		"modules/other.js":   "console.log('other')",
	}
	for k, v := range extra {
		files[k] = v
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewStates(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		s, err := New(Config{})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateDisabled {
			t.Fatalf("state=%s", s.Status().State)
		}
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rr.Code)
		}
	})

	t.Run("missing_root", func(t *testing.T) {
		s, err := New(Config{
			GatewayBasePath: "/emby",
			AssetsRoot:      filepath.Join(t.TempDir(), "absent"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateMissing {
			t.Fatalf("state=%s", s.Status().State)
		}
	})

	t.Run("missing_canary", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateMissing {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})

	t.Run("corrupt_file_root", func(t *testing.T) {
		root := t.TempDir()
		p := filepath.Join(root, "not-a-dir")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: p})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateCorrupt {
			t.Fatalf("state=%s", s.Status().State)
		}
	})

	t.Run("bad_base_path", func(t *testing.T) {
		_, err := New(Config{GatewayBasePath: "/other", AssetsRoot: t.TempDir()})
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("ready", func(t *testing.T) {
		root := t.TempDir()
		writeReadyTree(t, root, nil)
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateReady {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})
}

func TestServeReadyBasics(t *testing.T) {
	root := t.TempDir()
	writeReadyTree(t, root, map[string]string{
		"modules/emby-apiclient/connectionmanager.js": `u="https://mb3admin.com/api"`,
		"embypremiere/embypremiere.js":                `h="mb3admin.com"`,
	})
	s, err := New(Config{
		GatewayBasePath: "/emby",
		AssetsRoot:      root,
		PublicBaseURL:   "https://fallback.example/emby",
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("index", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/emby/web/", nil)
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "emby") {
			t.Fatalf("body=%q", rr.Body.String())
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("content-type=%q", ct)
		}
	})

	t.Run("root_redirect", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/emby/web?x=1", nil)
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusPermanentRedirect {
			t.Fatalf("code=%d", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/emby/web/?x=1" {
			t.Fatalf("location=%q", loc)
		}
	})

	t.Run("canary_cors", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != allowedCORSOrig {
			t.Fatalf("acao=%q", got)
		}
	})

	t.Run("non_canary_no_cors", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/other.js", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("unexpected acao=%q", got)
		}
	})

	t.Run("host_inject", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/emby-apiclient/connectionmanager.js", nil)
		req.Host = "media.xvv.net"
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
		body, _ := io.ReadAll(rr.Body)
		if !strings.Contains(string(body), "media.xvv.net") || strings.Contains(string(body), "mb3admin.com") {
			t.Fatalf("body=%q", body)
		}
		// Disk must remain original.
		onDisk, err := os.ReadFile(filepath.Join(root, "modules/emby-apiclient/connectionmanager.js"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(onDisk), "mb3admin.com") {
			t.Fatalf("disk mutated: %q", onDisk)
		}
	})

	t.Run("missing_file", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/nope.js", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("code=%d", rr.Code)
		}
	})

	t.Run("traversal_rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/../secret", nil))
		if rr.Code == http.StatusOK {
			t.Fatal("traversal must not serve")
		}
	})
}

func TestGuardHandler(t *testing.T) {
	var nextHits int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextHits++
		w.WriteHeader(http.StatusTeapot)
	})
	h := GuardHandler(next)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/Users", nil))
	if rr.Code != http.StatusTeapot || nextHits != 1 {
		t.Fatalf("api path should pass through code=%d hits=%d", rr.Code, nextHits)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/..%2fsecret", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("encoded traversal code=%d", rr.Code)
	}
	if nextHits != 1 {
		t.Fatalf("guard must not forward bad web path hits=%d", nextHits)
	}
}

func TestUnavailable503(t *testing.T) {
	s, err := New(Config{
		GatewayBasePath: "/emby",
		AssetsRoot:      filepath.Join(t.TempDir(), "gone"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d", rr.Code)
	}
}
