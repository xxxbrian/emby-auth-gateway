package embyweb

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHostFromPublicURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"https://media.example.com/emby", "media.example.com"},
		{"http://localhost:8090/emby", "localhost:8090"},
		{"  https://cdn.xvv.net:443/emby  ", "cdn.xvv.net:443"},
		{"localhost:8090/emby", "localhost:8090"},
		{"not a url", ""},
	}
	for _, tc := range cases {
		if got := hostFromPublicURL(tc.in); got != tc.want {
			t.Fatalf("hostFromPublicURL(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestInjectHostForRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/emby/web/x.js", nil)
	req.Host = "req.example:8443"
	if got := injectHostForRequest(req, "fallback.example"); got != "req.example:8443" {
		t.Fatalf("got %q", got)
	}
	req.Host = ""
	if got := injectHostForRequest(req, "fallback.example"); got != "fallback.example" {
		t.Fatalf("got %q", got)
	}
	if got := injectHostForRequest(nil, "fallback.example"); got != "fallback.example" {
		t.Fatalf("got %q", got)
	}
	if got := injectHostForRequest(nil, ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestRewriteHostPlaceholder(t *testing.T) {
	in := []byte(`u="https://mb3admin.co/api"`)
	got := rewriteHostPlaceholder(in, "media.xvv.net")
	if !bytes.Equal(got, []byte(`u="https://media.xvv.net/api"`)) {
		t.Fatalf("got %q", got)
	}
	if !bytes.Equal(rewriteHostPlaceholder(in, ""), in) {
		t.Fatal("empty host should not rewrite content")
	}
}

func TestServeHostInjectWhitelist(t *testing.T) {
	connBody := []byte(`var api="https://mb3admin.co/emby"`)
	premBody := []byte(`host:"mb3admin.co"`)
	otherBody := []byte(`var api="https://mb3admin.co/emby"`)
	tree := buildFixture(t, fixtureOpts{Files: []fixtureFile{
		{Path: "manifest.json", Data: []byte(`{}`)},
		{Path: "index.html", Data: []byte(`<!doctype html>`)},
		{Path: "strings/en-US.json", Data: []byte(`{}`)},
		{Path: "modules/emby-apiclient/connectionmanager.js", Data: connBody, CacheClass: cacheImmutable},
		{Path: "embypremiere/embypremiere.js", Data: premBody, CacheClass: cacheImmutable},
		{Path: "modules/app.js", Data: otherBody, CacheClass: cacheImmutable},
	}})
	s, err := newWithRegistry(Config{
		GatewayBasePath: "/emby",
		AssetsRoot:      tree.Root,
		PublicBaseURL:   "https://fallback.example/emby",
	}, tree.Registry)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%v err=%v", s.Status().State, s.Status().Err)
	}

	// Request Host wins.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/emby-apiclient/connectionmanager.js", nil)
	req.Host = "media.xvv.net"
	s.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("media.xvv.net")) || bytes.Contains(rr.Body.Bytes(), injectHostPlaceholder) {
		t.Fatalf("body=%q", rr.Body.Bytes())
	}
	if rr.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("cache %q", rr.Header().Get("Cache-Control"))
	}
	if rr.Header().Get("Vary") != "Host" {
		t.Fatalf("vary %q", rr.Header().Get("Vary"))
	}

	// Premiere path also rewritten.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/emby/web/embypremiere/embypremiere.js", nil)
	req.Host = "media.xvv.net"
	s.ServeHTTP(rr, req)
	if !bytes.Contains(rr.Body.Bytes(), []byte("media.xvv.net")) {
		t.Fatalf("premiere body=%q", rr.Body.Bytes())
	}

	// Non-whitelist path unchanged.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/emby/web/modules/app.js", nil)
	req.Host = "media.xvv.net"
	s.ServeHTTP(rr, req)
	if !bytes.Equal(rr.Body.Bytes(), otherBody) {
		t.Fatalf("other js rewritten: %q", rr.Body.Bytes())
	}

	// Empty request Host falls back to PublicBaseURL host.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/emby/web/modules/emby-apiclient/connectionmanager.js", nil)
	req.Host = ""
	s.ServeHTTP(rr, req)
	if !bytes.Contains(rr.Body.Bytes(), []byte("fallback.example")) {
		t.Fatalf("fallback body=%q", rr.Body.Bytes())
	}

	// Disk remains original.
	onDisk, err := os.ReadFile(filepath.Join(tree.Root, "releases", tree.Release, "files", "modules", "emby-apiclient", "connectionmanager.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, connBody) {
		t.Fatalf("disk mutated: %q", onDisk)
	}
}

func TestServeHostInjectNoHostLeavesPlaceholder(t *testing.T) {
	body := []byte(`u="https://mb3admin.co/x"`)
	tree := buildFixture(t, fixtureOpts{Files: []fixtureFile{
		{Path: "manifest.json", Data: []byte(`{}`)},
		{Path: "index.html", Data: []byte(`<!doctype html>`)},
		{Path: "strings/en-US.json", Data: []byte(`{}`)},
		{Path: "modules/emby-apiclient/connectionmanager.js", Data: body},
	}})
	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: tree.Root}, tree.Registry)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/emby/web/modules/emby-apiclient/connectionmanager.js", nil)
	req.Host = ""
	s.ServeHTTP(rr, req)
	if !bytes.Equal(rr.Body.Bytes(), body) {
		t.Fatalf("expected placeholder preserved, got %q", rr.Body.Bytes())
	}
}
