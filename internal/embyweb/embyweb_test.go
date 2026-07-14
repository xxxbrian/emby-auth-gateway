package embyweb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCanaryPathsDefensiveCopy(t *testing.T) {
	a := CanaryPaths()
	b := CanaryPaths()
	if len(a) != 3 {
		t.Fatalf("len=%d", len(a))
	}
	a[0] = "mutated"
	if CanaryPaths()[0] != "manifest.json" {
		t.Fatal("CanaryPaths is not defensive")
	}
	if b[0] != "manifest.json" {
		t.Fatal("prior copy mutated shared state")
	}
}

func TestNewDisabled(t *testing.T) {
	for _, root := range []string{"", "   ", "\t"} {
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatalf("root %q: %v", root, err)
		}
		if s.Status().State != StateDisabled {
			t.Fatalf("root %q: state=%s", root, s.Status().State)
		}
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("root %q: status=%d", root, rr.Code)
		}
	}
}

func TestNewEnabledRequiresEmbyBase(t *testing.T) {
	root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	for _, base := range []string{"", "/api", "/Emby", "/EMBY", "/emby/extra", "/emby/web"} {
		_, err := New(Config{GatewayBasePath: base, AssetsRoot: root})
		if err == nil {
			t.Fatalf("base %q: expected error", base)
		}
	}
	// Exact /emby after normalization (leading slash, strip trailing slash).
	for _, base := range []string{"/emby", "/emby/", "emby"} {
		s, err := New(Config{GatewayBasePath: base, AssetsRoot: root})
		if err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		if s.Status().State != StateReady {
			t.Fatalf("base %q: state=%s err=%v", base, s.Status().State, s.Status().Err)
		}
	}
}

func TestMissingStates(t *testing.T) {
	cases := []struct {
		name string
		opts fixtureOpts
	}{
		{"no_current", fixtureOpts{Files: readyMinimalFiles(), SkipCurrent: true}},
		{"no_release", fixtureOpts{Files: readyMinimalFiles(), SkipReleaseDir: true}},
		{"no_install", fixtureOpts{Files: readyMinimalFiles(), SkipInstall: true}},
		{"no_files_dir", fixtureOpts{Files: nil, SkipFilesDir: true}},
		{"missing_root", fixtureOpts{Files: readyMinimalFiles()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var assetsRoot string
			if tc.name == "missing_root" {
				assetsRoot = filepath.Join(t.TempDir(), "does-not-exist")
			} else {
				assetsRoot = buildFixture(t, tc.opts)
			}
			s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: assetsRoot})
			if err != nil {
				t.Fatal(err)
			}
			if s.Status().State != StateMissing {
				t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
			}
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
			if rr.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d", rr.Code)
			}
		})
	}
}

func TestCorruptStrictJSONAndBounds(t *testing.T) {
	validFiles := readyMinimalFiles()
	cases := []struct {
		name string
		opts fixtureOpts
	}{
		{"unknown_field_current", fixtureOpts{Files: validFiles, CurrentRaw: []byte(`{"schema":1,"release":"1.0.0-deadbeef","catalog_sha256":"` + defaultCatalogSHA() + `","extra":true}`)}},
		{"trailing_current", fixtureOpts{Files: validFiles, CurrentRaw: []byte(`{"schema":1,"release":"1.0.0-deadbeef","catalog_sha256":"` + defaultCatalogSHA() + `"}{"x":1}`)}},
		{"bad_schema", fixtureOpts{Files: validFiles, CurrentOverride: map[string]any{"schema": 2}}},
		{"bad_release_path", fixtureOpts{Files: validFiles, CurrentOverride: map[string]any{"release": "../x"}}},
		{"bad_catalog_case", fixtureOpts{Files: validFiles, CurrentOverride: map[string]any{"catalog_sha256": strings.ToUpper(defaultCatalogSHA())}}},
		{"identity_mismatch", fixtureOpts{Files: validFiles, InstallOverride: map[string]any{"release": "other-release"}}},
		{"bad_media_type", fixtureOpts{Files: validFiles, EntryOverrides: []map[string]any{
			{"path": "index.html", "size": 10, "sha256": sha256Hex([]byte("0123456789")), "media_type": "text/plain", "cache_class": "revalidate"},
		}, MutateAfter: func(root string) {
			// ensure file exists with matching size/hash for media type failure path
			p := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "index.html")
			_ = os.WriteFile(p, []byte("0123456789"), 0o644)
		}}},
		{"bad_cache_class", fixtureOpts{
			Files: []fixtureFile{{Path: "modules/app.js", Data: []byte("x"), CacheClass: "no-store"}},
		}},
		{"hash_mismatch", fixtureOpts{Files: validFiles, MutateAfter: func(root string) {
			// rewrite install entry hash after write by rebuilding install with wrong hash
			rel := filepath.Join(root, "releases", "1.0.0-deadbeef")
			raw, _ := os.ReadFile(filepath.Join(rel, "install.json"))
			var man map[string]any
			_ = json.Unmarshal(raw, &man)
			entries := man["entries"].([]any)
			e0 := entries[0].(map[string]any)
			e0["sha256"] = strings.Repeat("ab", 32)
			data, _ := json.Marshal(man)
			_ = os.WriteFile(filepath.Join(rel, "install.json"), data, 0o644)
		}}},
		{"undeclared_file", fixtureOpts{Files: validFiles, ExtraDiskFiles: map[string][]byte{"extra.txt": []byte("x")}}},
		{"undeclared_dir", fixtureOpts{Files: validFiles, ExtraDiskDirs: []string{"orphan-empty"}}},
		{"pointer_too_large", fixtureOpts{Files: validFiles, CurrentRaw: []byte(strings.Repeat("a", maxPointerBytes+1))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := buildFixture(t, tc.opts)
			s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			if s.Status().State != StateCorrupt {
				t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
			}
		})
	}
}

func TestCorruptDuplicatePath(t *testing.T) {
	data := []byte("hello")
	sum := sha256Hex(data)
	mt, _ := expectedMediaType("modules/app.js")
	entry := map[string]any{
		"path": "modules/app.js", "size": len(data), "sha256": sum,
		"media_type": mt, "cache_class": cacheImmutable,
	}
	root := buildFixture(t, fixtureOpts{
		Files:          []fixtureFile{{Path: "modules/app.js", Data: data}},
		EntryOverrides: []map[string]any{entry, entry},
	})
	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateCorrupt {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}
}

func TestSymlinkRejectionEveryLevel(t *testing.T) {
	// Each case introduces a symlink at a different level.
	type linkSpec struct {
		name string
		// linkPath is absolute path to create as symlink; target is link target.
		setup func(t *testing.T, root string)
	}
	cases := []linkSpec{
		{"assets_root_symlink", func(t *testing.T, root string) {
			// Replace usage: create real tree elsewhere and point AssetsRoot at symlink.
			// Handled specially below.
		}},
		{"current_json_symlink", func(t *testing.T, root string) {
			cur := filepath.Join(root, "current.json")
			real := filepath.Join(root, "current.real.json")
			if err := os.Rename(cur, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, cur); err != nil {
				t.Fatal(err)
			}
		}},
		{"releases_dir_symlink", func(t *testing.T, root string) {
			rel := filepath.Join(root, "releases")
			real := filepath.Join(root, "releases.real")
			if err := os.Rename(rel, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, rel); err != nil {
				t.Fatal(err)
			}
		}},
		{"release_dir_symlink", func(t *testing.T, root string) {
			rel := filepath.Join(root, "releases", "1.0.0-deadbeef")
			real := filepath.Join(root, "releases", "1.0.0-deadbeef.real")
			if err := os.Rename(rel, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, rel); err != nil {
				t.Fatal(err)
			}
		}},
		{"install_json_symlink", func(t *testing.T, root string) {
			p := filepath.Join(root, "releases", "1.0.0-deadbeef", "install.json")
			real := p + ".real"
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"files_dir_symlink", func(t *testing.T, root string) {
			p := filepath.Join(root, "releases", "1.0.0-deadbeef", "files")
			real := p + ".real"
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"nested_dir_symlink", func(t *testing.T, root string) {
			p := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "strings")
			real := p + ".real"
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"file_symlink", func(t *testing.T, root string) {
			p := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "index.html")
			real := p + ".real"
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "assets_root_symlink" {
				real := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
				link := filepath.Join(t.TempDir(), "assets-link")
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
				return
			}
			root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
			tc.setup(t, root)
			s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			if s.Status().State != StateCorrupt {
				t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
			}
		})
	}
}

func TestReadyServeBasics(t *testing.T) {
	root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	s := mustNewReady(t, root)

	t.Run("redirect_root", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/emby/web?x=1", nil)
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusPermanentRedirect {
			t.Fatalf("code=%d", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/emby/web/?x=1" {
			t.Fatalf("Location=%q", loc)
		}
	})

	t.Run("index", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("ct=%q", ct)
		}
		if rr.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("cache=%q", rr.Header().Get("Cache-Control"))
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("missing nosniff")
		}
		if rr.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("missing referrer-policy")
		}
		if rr.Header().Get("Last-Modified") != "" {
			t.Fatal("unexpected Last-Modified")
		}
		if !strings.Contains(rr.Body.String(), "<!doctype html>") {
			t.Fatalf("body=%q", rr.Body.String())
		}
	})

	t.Run("immutable_js", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil))
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("cache=%q", rr.Header().Get("Cache-Control"))
		}
		etag := rr.Header().Get("ETag")
		if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
			t.Fatalf("etag=%q", etag)
		}
	})

	t.Run("unknown_no_spa", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/nope.js", nil))
		if rr.Code != 404 {
			t.Fatalf("code=%d", rr.Code)
		}
	})

	t.Run("directory_no_listing", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/modules/", nil))
		if rr.Code != 404 {
			t.Fatalf("code=%d", rr.Code)
		}
	})

	t.Run("head", func(t *testing.T) {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/emby/web/modules/app.js", nil))
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("head body len=%d", rr.Body.Len())
		}
		if rr.Header().Get("Content-Length") == "" && rr.Header().Get("ETag") == "" {
			t.Fatal("expected headers on HEAD")
		}
	})
}

func TestMethodMatrix(t *testing.T) {
	readyRoot := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	ready := mustNewReady(t, readyRoot)

	missingRoot := buildFixture(t, fixtureOpts{Files: readyMinimalFiles(), SkipCurrent: true})
	missing, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: missingRoot})
	if err != nil {
		t.Fatal(err)
	}

	corruptRoot := buildFixture(t, fixtureOpts{Files: readyMinimalFiles(), CurrentOverride: map[string]any{"schema": 99}})
	corrupt, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: corruptRoot})
	if err != nil {
		t.Fatal(err)
	}

	disabled, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: ""})
	if err != nil {
		t.Fatal(err)
	}

	type exp struct {
		code  int
		allow string // if non-empty, expect Allow header
	}
	// path, method -> expected per state
	check := func(t *testing.T, s *Server, method, path string, want exp) {
		t.Helper()
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
		if rr.Code != want.code {
			t.Fatalf("%s %s: code=%d want=%d body=%q", method, path, rr.Code, want.code, rr.Body.String())
		}
		if want.allow != "" && rr.Header().Get("Allow") != want.allow {
			t.Fatalf("%s %s: Allow=%q want %q", method, path, rr.Header().Get("Allow"), want.allow)
		}
	}

	t.Run("disabled_always_404", func(t *testing.T) {
		for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut} {
			check(t, disabled, m, "/emby/web/", exp{code: 404})
			check(t, disabled, m, "/emby/web/manifest.json", exp{code: 404})
		}
	})

	for _, st := range []struct {
		name string
		s    *Server
	}{
		{"missing", missing},
		{"corrupt", corrupt},
	} {
		t.Run(st.name, func(t *testing.T) {
			check(t, st.s, http.MethodGet, "/emby/web/", exp{code: 503})
			check(t, st.s, http.MethodHead, "/emby/web/manifest.json", exp{code: 503})
			check(t, st.s, http.MethodOptions, "/emby/web/manifest.json", exp{code: 503})
			check(t, st.s, http.MethodOptions, "/emby/web/modules/app.js", exp{code: 405, allow: "GET, HEAD"})
			check(t, st.s, http.MethodPost, "/emby/web/", exp{code: 405, allow: "GET, HEAD, OPTIONS"})
			check(t, st.s, http.MethodPost, "/emby/web/modules/app.js", exp{code: 405, allow: "GET, HEAD"})
		})
	}

	t.Run("ready", func(t *testing.T) {
		check(t, ready, http.MethodGet, "/emby/web/", exp{code: 200})
		check(t, ready, http.MethodOptions, "/emby/web/manifest.json", exp{code: 204})
		check(t, ready, http.MethodOptions, "/emby/web/modules/app.js", exp{code: 405, allow: "GET, HEAD"})
		check(t, ready, http.MethodPost, "/emby/web/manifest.json", exp{code: 405, allow: "GET, HEAD, OPTIONS"})
		check(t, ready, http.MethodDelete, "/emby/web/modules/app.js", exp{code: 405, allow: "GET, HEAD"})
		check(t, ready, http.MethodOptions, "/emby/web", exp{code: 405, allow: "GET, HEAD"})
	})
}

func TestTraversalRejection(t *testing.T) {
	root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	s := mustNewReady(t, root)

	paths := []string{
		"/emby/web/../web/index.html",
		"/emby/web/./index.html",
		"/emby/web//index.html",
		"/emby/web/modules/../../index.html",
		"/emby/web/modules/%2e%2e/index.html",
		"/emby/web/%2e%2e/web/index.html",
		"/emby/web/index.html%00.js",
		"/emby/web/..\\/index.html",
		"/emby/web/modules/%2fetc/passwd",
		"/emby/web/modules/%5c..%5cindex.html",
		"/emby/web/%2e/index.html",
		"/emby/webX/index.html",
		"/other/web/index.html",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, p, nil)
			// httptest may clean paths; force RawPath/Path for encoded cases.
			req.URL.Path = p
			if strings.Contains(p, "%") {
				req.URL.RawPath = p
			}
			s.ServeHTTP(rr, req)
			if rr.Code == 200 {
				t.Fatalf("unexpected 200 for %q body=%q", p, rr.Body.String())
			}
		})
	}
}

func TestETagRangeConditionals(t *testing.T) {
	data := []byte("0123456789abcdef")
	root := buildFixture(t, fixtureOpts{Files: []fixtureFile{
		{Path: "index.html", Data: []byte("<html></html>")},
		{Path: "manifest.json", Data: []byte(`{}`)},
		{Path: "strings/en-US.json", Data: []byte(`{}`)},
		{Path: "modules/app.js", Data: data, CacheClass: cacheImmutable},
	}})
	s := mustNewReady(t, root)

	// Baseline
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil))
	if rr.Code != 200 {
		t.Fatalf("code=%d", rr.Code)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing etag")
	}
	if rr.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges=%q", rr.Header().Get("Accept-Ranges"))
	}

	t.Run("304", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil)
		req.Header.Set("If-None-Match", etag)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotModified {
			t.Fatalf("code=%d", rr.Code)
		}
	})

	t.Run("206", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil)
		req.Header.Set("Range", "bytes=0-3")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusPartialContent {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Body.String() != "0123" {
			t.Fatalf("body=%q", rr.Body.String())
		}
		if rr.Header().Get("Content-Range") == "" {
			t.Fatal("missing Content-Range")
		}
	})

	t.Run("416", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil)
		req.Header.Set("Range", "bytes=100-200")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("code=%d", rr.Code)
		}
	})

	t.Run("if_range_match", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil)
		req.Header.Set("Range", "bytes=0-3")
		req.Header.Set("If-Range", etag)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != http.StatusPartialContent {
			t.Fatalf("code=%d", rr.Code)
		}
	})

	t.Run("if_range_mismatch", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil)
		req.Header.Set("Range", "bytes=0-3")
		req.Header.Set("If-Range", `"deadbeef"`)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		// Mismatch should serve full 200.
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Body.String() != string(data) {
			t.Fatalf("body=%q", rr.Body.String())
		}
	})
}

func TestCORS(t *testing.T) {
	root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	s := mustNewReady(t, root)

	wantPreflightVary := []string{
		"Origin",
		"Access-Control-Request-Method",
		"Access-Control-Request-Headers",
		"Access-Control-Request-Private-Network",
	}
	assertPreflightCache := func(t *testing.T, rr *httptest.ResponseRecorder) {
		t.Helper()
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control=%q", rr.Header().Get("Cache-Control"))
		}
		vary := rr.Header().Get("Vary")
		for _, tok := range wantPreflightVary {
			found := false
			for _, p := range strings.Split(vary, ",") {
				if strings.EqualFold(strings.TrimSpace(p), tok) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("Vary=%q missing %q", vary, tok)
			}
		}
	}

	t.Run("simple_allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Header().Get("Access-Control-Allow-Origin") != allowedCORSOrig {
			t.Fatalf("ACAO=%q", rr.Header().Get("Access-Control-Allow-Origin"))
		}
		if rr.Header().Get("Vary") != "Origin" {
			t.Fatalf("Vary=%q", rr.Header().Get("Vary"))
		}
		if rr.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("credentials must not be set")
		}
	})

	t.Run("simple_disallowed_origin_still_varies", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", "https://evil.example")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unexpected CORS grant")
		}
		if rr.Header().Get("Vary") != "Origin" {
			t.Fatalf("Vary=%q want Origin", rr.Header().Get("Vary"))
		}
	})

	t.Run("simple_missing_origin_still_varies", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/manifest.json", nil)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unexpected CORS grant")
		}
		if rr.Header().Get("Vary") != "Origin" {
			t.Fatalf("Vary=%q want Origin", rr.Header().Get("Vary"))
		}
	})

	t.Run("simple_head_allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodHead, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
		if rr.Header().Get("Access-Control-Allow-Origin") != allowedCORSOrig {
			t.Fatal("missing ACAO on HEAD")
		}
		if rr.Header().Get("Vary") != "Origin" {
			t.Fatalf("Vary=%q", rr.Header().Get("Vary"))
		}
	})

	t.Run("ordinary_asset_no_cors", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("ordinary asset must not grant CORS")
		}
	})

	t.Run("preflight_ok", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		req.Header.Set("Access-Control-Request-Method", "GET")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != 204 {
			t.Fatalf("code=%d", rr.Code)
		}
		assertPreflightCache(t, rr)
		if rr.Header().Get("Access-Control-Allow-Origin") != allowedCORSOrig {
			t.Fatal("missing ACAO")
		}
		if rr.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD" {
			t.Fatalf("methods=%q", rr.Header().Get("Access-Control-Allow-Methods"))
		}
	})

	t.Run("preflight_absent_method_no_grant", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != 204 {
			t.Fatalf("code=%d", rr.Code)
		}
		assertPreflightCache(t, rr)
		if rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("must not grant without request method")
		}
	})

	t.Run("preflight_rejected_method_no_grant", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		req.Header.Set("Access-Control-Request-Method", "POST")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != 204 {
			t.Fatalf("code=%d", rr.Code)
		}
		assertPreflightCache(t, rr)
		if rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("must not grant for POST preflight")
		}
	})

	t.Run("preflight_unapproved_headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/emby/web/manifest.json", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Headers", "X-Custom")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != 204 {
			t.Fatalf("code=%d", rr.Code)
		}
		assertPreflightCache(t, rr)
		if rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("must not grant CORS with unapproved headers")
		}
	})

	t.Run("pna", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/emby/web/index.html", nil)
		req.Header.Set("Origin", allowedCORSOrig)
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Private-Network", "true")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		assertPreflightCache(t, rr)
		if rr.Header().Get("Access-Control-Allow-Private-Network") != "true" {
			t.Fatal("expected PNA grant")
		}
	})

	t.Run("pna_disallowed_origin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/emby/web/index.html", nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Private-Network", "true")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		assertPreflightCache(t, rr)
		if rr.Header().Get("Access-Control-Allow-Private-Network") != "" {
			t.Fatal("PNA must not grant for disallowed origin")
		}
		if rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("must not grant ACAO for disallowed origin")
		}
	})
}

func TestPostStartMutationServesPinned(t *testing.T) {
	root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	s := mustNewReady(t, root)

	// Mutate on-disk index after load.
	indexPath := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "index.html")
	original, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("MUTATED"), 0o644); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
	if rr.Code != 200 {
		t.Fatalf("code=%d", rr.Code)
	}
	if rr.Body.String() != string(original) {
		t.Fatalf("served mutated bytes: %q", rr.Body.String())
	}
}

func TestConcurrentServeHTTP(t *testing.T) {
	root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	s := mustNewReady(t, root)

	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths := []string{
				"/emby/web/",
				"/emby/web/manifest.json",
				"/emby/web/modules/app.js",
				"/emby/web/nope",
			}
			p := paths[i%len(paths)]
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
			if rr.Code != 200 && rr.Code != 404 {
				errCh <- fmt.Errorf("path %s code %d", p, rr.Code)
			}
			_ = s.Status()
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestStatusFields(t *testing.T) {
	catalog := defaultCatalogSHA()
	root := buildFixture(t, fixtureOpts{
		Release:       "2.0.0-abc",
		CatalogSHA256: catalog,
		Files:         readyMinimalFiles(),
	})
	s := mustNewReady(t, root)
	st := s.Status()
	if st.Release != "2.0.0-abc" || st.CatalogSHA256 != catalog {
		t.Fatalf("status=%+v", st)
	}
	if st.Err != nil {
		t.Fatalf("err=%v", st.Err)
	}
}

func TestNonRegularFileRejected(t *testing.T) {
	// FIFO if supported.
	root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles()})
	fifo := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "modules", "pipe.fifo")
	if err := mkfifo(fifo); err != nil {
		t.Skipf("mkfifo not available: %v", err)
	}
	// Add to install as if declared — still non-regular.
	// Easier: leave undeclared FIFO in tree -> corrupt as non-regular.
	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateCorrupt {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}
}

func TestHTMLForcedRevalidateEvenIfImmutableDeclared(t *testing.T) {
	root := buildFixture(t, fixtureOpts{Files: []fixtureFile{
		{Path: "index.html", Data: []byte("<html></html>"), CacheClass: cacheImmutable},
		{Path: "manifest.json", Data: []byte(`{}`)},
		{Path: "strings/en-US.json", Data: []byte(`{}`)},
		{Path: "other.html", Data: []byte("<html>x</html>"), CacheClass: cacheImmutable},
	}})
	s := mustNewReady(t, root)
	for _, p := range []string{"/emby/web/", "/emby/web/other.html"} {
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if rr.Header().Get("Cache-Control") != "no-cache" {
			t.Fatalf("%s cache=%q", p, rr.Header().Get("Cache-Control"))
		}
	}
}

func TestSizeMismatchCorrupt(t *testing.T) {
	root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles(), MutateAfter: func(root string) {
		// Truncate a file without updating manifest.
		p := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "modules", "app.js")
		_ = os.WriteFile(p, []byte("x"), 0o644)
	}})
	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateCorrupt {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}
}

func TestInstallUnknownFieldCorrupt(t *testing.T) {
	files := readyMinimalFiles()
	root := buildFixture(t, fixtureOpts{Files: files})
	// Rewrite install with unknown field.
	p := filepath.Join(root, "releases", "1.0.0-deadbeef", "install.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var man map[string]any
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	man["extra"] = true
	data, _ := json.Marshal(man)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateCorrupt {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}
}

func TestNilServer(t *testing.T) {
	var s *Server
	if s.Status().State != StateDisabled {
		t.Fatal("nil status")
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
	if rr.Code != 404 {
		t.Fatalf("code=%d", rr.Code)
	}
}

func TestStateString(t *testing.T) {
	if StateReady.String() != "ready" {
		t.Fatal(StateReady.String())
	}
}

func TestDeclaredAssetMissingIsMissing(t *testing.T) {
	t.Run("root_file", func(t *testing.T) {
		root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles(), MutateAfter: func(root string) {
			p := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "index.html")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}})
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateMissing {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})

	t.Run("nested_file", func(t *testing.T) {
		root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles(), MutateAfter: func(root string) {
			p := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "strings", "en-US.json")
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
		}})
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateMissing {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})

	t.Run("nested_directory", func(t *testing.T) {
		root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles(), MutateAfter: func(root string) {
			p := filepath.Join(root, "releases", "1.0.0-deadbeef", "files", "strings")
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
		}})
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateMissing {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})
}

func TestBoundedInventoryRejectsExcess(t *testing.T) {
	t.Run("extra_files_corrupt", func(t *testing.T) {
		root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles(), MutateAfter: func(root string) {
			dir := filepath.Join(root, "releases", "1.0.0-deadbeef", "files")
			// A handful of extras is enough: first undeclared stops the walk.
			for i := 0; i < 8; i++ {
				name := filepath.Join(dir, fmt.Sprintf("extra-%d.js", i))
				if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}})
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateCorrupt {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})

	t.Run("extra_directory_corrupt", func(t *testing.T) {
		root := buildFixture(t, fixtureOpts{Files: readyMinimalFiles(), ExtraDiskDirs: []string{"orphan-empty"}})
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateCorrupt {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})

	t.Run("deep_path_rejected", func(t *testing.T) {
		segs := make([]string, maxPathDepth+1)
		for i := range segs {
			segs[i] = "d"
		}
		deep := strings.Join(segs, "/") + ".js"
		root := buildFixture(t, fixtureOpts{
			Files: append(readyMinimalFiles(), fixtureFile{Path: deep, Data: []byte("x"), CacheClass: cacheImmutable}),
		})
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if s.Status().State != StateCorrupt {
			t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
		}
	})
}

func TestMarkdownMediaType(t *testing.T) {
	// Independent of mediaTypes map lookup: assert the canonical value literally.
	const wantType = "text/markdown; charset=utf-8"
	data := []byte("# hello\n")
	root := buildFixture(t, fixtureOpts{Files: []fixtureFile{
		{Path: "index.html", Data: []byte("<html></html>")},
		{Path: "manifest.json", Data: []byte(`{}`)},
		{Path: "strings/en-US.json", Data: []byte(`{}`)},
		{Path: "docs/readme.md", Data: data, MediaType: wantType, CacheClass: cacheImmutable},
	}})
	s := mustNewReady(t, root)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/docs/readme.md", nil))
	if rr.Code != 200 {
		t.Fatalf("code=%d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != wantType {
		t.Fatalf("Content-Type=%q want %q", ct, wantType)
	}
	if rr.Body.String() != string(data) {
		t.Fatalf("body=%q", rr.Body.String())
	}
}

func TestGuardEncodedWebPrefix(t *testing.T) {
	var hits int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusTeapot)
	})
	h := GuardHandler(next)

	// /emby/%77eb/../Users decodes to /emby/web/../Users — Web-prefixed attack.
	cases := []struct {
		path string
		raw  string
	}{
		{"/emby/web/../Users", "/emby/%77eb/../Users"},
		{"/emby/web/../Users", "/emby/%77EB/../Users"}, // hex case variant
		{"/emby/web/x", "/emby/%77eb/x"},
	}
	for _, tc := range cases {
		hits = 0
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
		req.URL.Path = tc.path
		req.URL.RawPath = tc.raw
		h.ServeHTTP(rr, req)
		if hits != 0 {
			t.Fatalf("raw=%q: hits=%d want blocked", tc.raw, hits)
		}
		if rr.Code != http.StatusNotFound {
			t.Fatalf("raw=%q: code=%d", tc.raw, rr.Code)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("raw=%q: Cache-Control=%q", tc.raw, rr.Header().Get("Cache-Control"))
		}
	}

	// Lookalikes still pass.
	for _, path := range []string{"/emby/websocket", "/emby/webX", "/emby/webX/index.html"} {
		hits = 0
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if hits != 1 || rr.Code != http.StatusTeapot {
			t.Fatalf("%s: hits=%d code=%d", path, hits, rr.Code)
		}
	}
}

func TestRequiredCanariesForReady(t *testing.T) {
	t.Run("zero_entries", func(t *testing.T) {
		root := buildFixture(t, fixtureOpts{Files: nil})
		s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		st := s.Status()
		if st.State != StateCorrupt {
			t.Fatalf("state=%s err=%v", st.State, st.Err)
		}
		if st.Err == nil || !strings.Contains(st.Err.Error(), "canary") {
			t.Fatalf("err=%v", st.Err)
		}
	})

	for _, missing := range CanaryPaths() {
		missing := missing
		t.Run("missing_"+strings.ReplaceAll(missing, "/", "_"), func(t *testing.T) {
			files := make([]fixtureFile, 0, 3)
			for _, c := range CanaryPaths() {
				if c == missing {
					continue
				}
				switch c {
				case "index.html":
					files = append(files, fixtureFile{Path: c, Data: []byte("<html></html>")})
				case "manifest.json":
					files = append(files, fixtureFile{Path: c, Data: []byte(`{}`)})
				case "strings/en-US.json":
					files = append(files, fixtureFile{Path: c, Data: []byte(`{}`)})
				}
			}
			// Keep a non-canary so the tree is non-empty and install is otherwise valid.
			files = append(files, fixtureFile{Path: "modules/app.js", Data: []byte("x"), CacheClass: cacheImmutable})
			root := buildFixture(t, fixtureOpts{Files: files})
			s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
			if err != nil {
				t.Fatal(err)
			}
			st := s.Status()
			if st.State != StateCorrupt {
				t.Fatalf("state=%s err=%v", st.State, st.Err)
			}
			if st.Err == nil || !strings.Contains(st.Err.Error(), missing) {
				t.Fatalf("err=%v want mention of %q", st.Err, missing)
			}
		})
	}

	t.Run("all_canaries_only_ready", func(t *testing.T) {
		root := buildFixture(t, fixtureOpts{Files: []fixtureFile{
			{Path: "index.html", Data: []byte("<html></html>")},
			{Path: "manifest.json", Data: []byte(`{}`)},
			{Path: "strings/en-US.json", Data: []byte(`{}`)},
		}})
		s := mustNewReady(t, root)
		// Canaries must force revalidate.
		for _, p := range []string{"/emby/web/", "/emby/web/manifest.json", "/emby/web/strings/en-US.json"} {
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
			if rr.Code != 200 {
				t.Fatalf("%s code=%d", p, rr.Code)
			}
			if rr.Header().Get("Cache-Control") != "no-cache" {
				t.Fatalf("%s cache=%q", p, rr.Header().Get("Cache-Control"))
			}
		}
	})
}

func TestGuardHandler(t *testing.T) {
	var hits int
	var lastPath, lastRawQuery string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		lastPath = r.URL.Path
		lastRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("next"))
	})
	h := GuardHandler(next)

	// guardReq builds a request. When rawPath is non-empty it models a real
	// server request: Path is the decoded form, RawPath is the wire form.
	guardReq := func(method, path, rawPath, rawQuery string) *http.Request {
		r := httptest.NewRequest(method, "http://example.test/", nil)
		r.URL.Path = path
		if rawPath != "" {
			r.URL.RawPath = rawPath
		} else {
			r.URL.RawPath = ""
		}
		r.URL.RawQuery = rawQuery
		return r
	}

	t.Run("canonical_pass", func(t *testing.T) {
		for _, path := range []string{
			"/emby/web",
			"/emby/web/",
			"/emby/web/index.html",
			"/emby/web/modules/app.js",
			"/emby/web/strings/en-US.json",
		} {
			hits = 0
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, guardReq(http.MethodGet, path, "", "q=1"))
			if hits != 1 {
				t.Fatalf("%s: hits=%d", path, hits)
			}
			if rr.Code != http.StatusTeapot {
				t.Fatalf("%s: code=%d", path, rr.Code)
			}
			if lastRawQuery != "q=1" {
				t.Fatalf("%s: query not preserved: %q", path, lastRawQuery)
			}
			if rr.Header().Get("Cache-Control") == "no-store" {
				t.Fatalf("%s: unexpected no-store on pass-through", path)
			}
		}
	})

	t.Run("non_web_pass", func(t *testing.T) {
		for _, path := range []string{
			"/emby/Users",
			"/emby/websocket",
			"/emby/webX",
			"/emby/webX/index.html",
			"/embywebsocket",
			"/api/foo",
			"/",
			"/emby",
		} {
			hits = 0
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, guardReq(http.MethodGet, path, "", ""))
			if hits != 1 {
				t.Fatalf("%s: hits=%d want pass-through", path, hits)
			}
			if rr.Code != http.StatusTeapot {
				t.Fatalf("%s: code=%d", path, rr.Code)
			}
		}
	})

	t.Run("invalid_web_block", func(t *testing.T) {
		// path is decoded URL.Path; raw is wire RawPath (empty => path only).
		cases := []struct {
			path string
			raw  string
		}{
			{"/emby/web/../web/index.html", ""},
			{"/emby/web/./index.html", ""},
			{"/emby/web//index.html", ""},
			{"/emby/web/modules/../../index.html", ""},
			{"/emby/web/modules/../index.html", "/emby/web/modules/%2e%2e/index.html"},
			{"/emby/web/../x", "/emby/web/%2e%2e/x"},
			{"/emby/web/index.html\x00.js", "/emby/web/index.html%00.js"},
			{"/emby/web/..\\/index.html", ""},
			{"/emby/web/modules//etc/passwd", "/emby/web/modules/%2fetc/passwd"},
			{`/emby/web/modules/\..\index.html`, "/emby/web/modules/%5c..%5cindex.html"},
			{"/emby/web/./index.html", "/emby/web/%2e/index.html"},
			// Encoded separator immediately after /emby/web (no literal slash).
			{"/emby/web/index.html", "/emby/web%2findex.html"},
			{`/emby/web\index.html`, "/emby/web%5cindex.html"},
		}
		for _, tc := range cases {
			hits = 0
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, guardReq(http.MethodGet, tc.path, tc.raw, "keep=1"))
			if hits != 0 {
				t.Fatalf("path=%q raw=%q: hits=%d want blocked (lastPath=%q)", tc.path, tc.raw, hits, lastPath)
			}
			if rr.Code != http.StatusNotFound {
				t.Fatalf("path=%q raw=%q: code=%d", tc.path, tc.raw, rr.Code)
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("path=%q raw=%q: Cache-Control=%q", tc.path, tc.raw, rr.Header().Get("Cache-Control"))
			}
		}
	})

	t.Run("nil_next", func(t *testing.T) {
		g := GuardHandler(nil)
		rr := httptest.NewRecorder()
		g.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/emby/web/", nil))
		if rr.Code != 404 {
			t.Fatalf("code=%d", rr.Code)
		}
	})
}
