package embyweb

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

	t.Run("corrupt_symlink_root", func(t *testing.T) {
		real := t.TempDir()
		writeReadyTree(t, real, nil)
		link := filepath.Join(t.TempDir(), "root-link")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: link})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateCorrupt {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})
}

func TestServeRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	writeReadyTree(t, root, nil)

	// Outside secret that must never be served via intermediate or final symlink.
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Intermediate component: modules -> outside dir. Request modules/secret.txt
	// would escape if open followed intermediate symlinks.
	modulesPath := filepath.Join(root, "modules")
	if err := os.RemoveAll(modulesPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, modulesPath); err != nil {
		t.Fatal(err)
	}

	// Final-component symlink under a real directory.
	if err := os.MkdirAll(filepath.Join(root, "strings"), 0o755); err != nil {
		t.Fatal(err)
	}
	finalLink := filepath.Join(root, "strings", "leak.json")
	if err := os.Symlink(secretPath, finalLink); err != nil {
		t.Fatal(err)
	}

	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		// Canaries still present; intermediate modules symlink must not break Ready.
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}

	t.Run("intermediate_symlink_escape", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/modules/secret.txt", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "TOPSECRET") {
			t.Fatal("leaked outside content via intermediate symlink")
		}
	})

	t.Run("final_component_symlink", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/strings/leak.json", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "TOPSECRET") {
			t.Fatal("leaked outside content via final-component symlink")
		}
	})

	t.Run("regular_canary_still_served", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/manifest.json", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
	})
}

func TestServeReadyBasics(t *testing.T) {
	root := t.TempDir()
	writeReadyTree(t, root, map[string]string{
		"modules/emby-apiclient/connectionmanager.js": `u="https://mb3admin.com/api"`,
		"embypremiere/embypremiere.js":                `h="mb3admin.com"`,
		"modules/app.js":                              "console.log(1)",
		"css/site.css":                                "body{}",
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

	t.Run("head_canary_content_type", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/emby/web/manifest.json", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("content-type=%q", ct)
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("HEAD must not write body, got %d bytes", rr.Body.Len())
		}
	})

	t.Run("inject_cache_headers", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/emby-apiclient/connectionmanager.js", nil)
		req.Host = "media.xvv.net"
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("cache-control=%q", got)
		}
		if got := rr.Header().Get("Vary"); !strings.Contains(got, "Host") {
			t.Fatalf("vary=%q want Host", got)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
			t.Fatalf("content-type=%q", ct)
		}
	})

	t.Run("non_document_assets_revalidate_not_immutable", func(t *testing.T) {
		// Emby uses stable names; JS/CSS must not get year-long immutable cache.
		for _, path := range []string{
			"/emby/web/modules/app.js",
			"/emby/web/css/site.css",
		} {
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s: code=%d", path, rr.Code)
			}
			got := rr.Header().Get("Cache-Control")
			if got != "no-cache" {
				t.Fatalf("%s: cache-control=%q want no-cache (revalidate)", path, got)
			}
			if strings.Contains(got, "immutable") {
				t.Fatalf("%s: must not use immutable cache: %q", path, got)
			}
		}
	})
}

func TestServeRejectsFIFO(t *testing.T) {
	root := t.TempDir()
	writeReadyTree(t, root, nil)
	fifoPath := filepath.Join(root, "modules", "fifo.js")
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		t.Skipf("mkfifo not available: %v", err)
	}

	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}

	type result struct {
		code int
		err  string
	}
	done := make(chan result, 1)
	go func() {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/modules/fifo.js", nil))
		done <- result{code: rr.Code}
	}()

	select {
	case res := <-done:
		if res.code != http.StatusNotFound {
			t.Fatalf("FIFO GET code=%d want 404", res.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GET on FIFO hung (timeout); open must not block on non-regular nodes")
	}
}

func TestTraversalMatrix(t *testing.T) {
	root := t.TempDir()
	writeReadyTree(t, root, nil)
	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{
		"/emby/web/../secret",
		"/emby/web/..%2fsecret",
		"/emby/web/%2e%2e/secret",
		"/emby/web/modules/%2e%2e/%2e%2e/secret",
		"/emby/web/modules/..%2f..%2fsecret",
		"/emby/web/foo/../../etc/passwd",
		"/emby/web/%2fetc/passwd",
		"/emby/web/..",
		"/emby/web/./../../secret",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, p, nil)
			// Preserve raw path for encoded forms when possible.
			if req.URL.RawPath == "" && strings.Contains(p, "%") {
				req.URL.RawPath = p
			}
			s.ServeHTTP(rr, req)
			if rr.Code == http.StatusOK {
				t.Fatalf("traversal path must not be 200: code=%d body=%q", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestInjectHostMatrix(t *testing.T) {
	root := t.TempDir()
	injectRel := "modules/emby-apiclient/connectionmanager.js"
	orig := `u="https://mb3admin.com/api"`
	writeReadyTree(t, root, map[string]string{injectRel: orig})

	s, err := New(Config{
		GatewayBasePath: "/emby",
		AssetsRoot:      root,
		PublicBaseURL:   "https://fallback.example/emby",
	})
	if err != nil {
		t.Fatal(err)
	}

	urlPath := "/emby/web/" + injectRel

	t.Run("request_host_wins", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, urlPath, nil)
		req.Host = "req.example:8443"
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "req.example:8443") || strings.Contains(body, "mb3admin.com") || strings.Contains(body, "fallback.example") {
			t.Fatalf("body=%q", body)
		}
	})

	t.Run("empty_host_uses_public_fallback", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, urlPath, nil)
		req.Host = ""
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "fallback.example") || strings.Contains(body, "mb3admin.com") {
			t.Fatalf("body=%q", body)
		}
	})

	t.Run("invalid_host_no_inject", func(t *testing.T) {
		// Server with no usable fallback so invalid Host cannot rewrite.
		s2, err := New(Config{
			GatewayBasePath: "/emby",
			AssetsRoot:      root,
			PublicBaseURL:   "",
		})
		if err != nil {
			t.Fatal(err)
		}
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, urlPath, nil)
		req.Host = "user@evil.example"
		s2.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d", rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "mb3admin.com") {
			t.Fatalf("invalid host must leave placeholder, body=%q", body)
		}
		if strings.Contains(body, "evil.example") {
			t.Fatalf("must not inject invalid host, body=%q", body)
		}
	})

	t.Run("disk_unchanged", func(t *testing.T) {
		onDisk, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(injectRel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(onDisk) != orig {
			t.Fatalf("disk mutated: %q", onDisk)
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
