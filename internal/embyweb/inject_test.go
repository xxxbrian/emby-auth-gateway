package embyweb

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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
		{"https://user:pass@evil.example/emby", "evil.example"}, // url.Host strips userinfo
		{"https://evil.example/path@mb3admin.com", "evil.example"},
		{"http://[::1]:8090/emby", "[::1]:8090"},
		{"https://bad host.com/emby", ""},
	}
	for _, tc := range cases {
		if got := hostFromPublicURL(tc.in); got != tc.want {
			t.Fatalf("hostFromPublicURL(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidInjectHost(t *testing.T) {
	valid := []string{
		"example.com",
		"media.xvv.net",
		"localhost",
		"localhost:8090",
		"cdn.example.com:443",
		"192.168.1.1",
		"192.168.1.1:8080",
		"[::1]",
		"[::1]:8090",
		"2001:db8::1",
	}
	for _, h := range valid {
		if !validInjectHost(h) {
			t.Fatalf("expected valid: %q", h)
		}
	}
	invalid := []string{
		"",
		" ",
		"  example.com  ",
		"example.com/path",
		"example.com\\x",
		"user@example.com",
		`ex"ample.com`,
		"ex'ample.com",
		"host with space",
		"http://example.com",
		"example.com:0",
		"example.com:99999",
		"example.com:abc",
		"exam ple.com",
		"host\nname",
		"host\x00name",
		"../evil",
		"a/b",
		"a@b",
		"[not-an-ip]",
		"example.com:8080/extra",
	}
	for _, h := range invalid {
		if validInjectHost(h) {
			t.Fatalf("expected invalid: %q", h)
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

	// Invalid request Host falls back to valid PublicBaseURL host.
	req.Host = "bad host"
	if got := injectHostForRequest(req, "fallback.example"); got != "fallback.example" {
		t.Fatalf("invalid Host should fall back, got %q", got)
	}
	// Invalid Host and invalid fallback => no inject host.
	req.Host = "user@evil"
	if got := injectHostForRequest(req, "also bad"); got != "" {
		t.Fatalf("both invalid should yield empty, got %q", got)
	}
	// Empty Host uses fallback only when fallback is valid.
	req.Host = ""
	if got := injectHostForRequest(req, "user@evil"); got != "" {
		t.Fatalf("invalid fallback should yield empty, got %q", got)
	}
}

func TestRewriteHostPlaceholder(t *testing.T) {
	in := []byte(`u="https://mb3admin.com/api"`)
	got := rewriteHostPlaceholder(in, "media.xvv.net")
	if !bytes.Equal(got, []byte(`u="https://media.xvv.net/api"`)) {
		t.Fatalf("got %q", got)
	}
	if !bytes.Equal(rewriteHostPlaceholder(in, ""), in) {
		t.Fatal("empty host must leave bytes unchanged")
	}
	if !bytes.Equal(rewriteHostPlaceholder(in, "user@evil"), in) {
		t.Fatal("invalid host must leave bytes unchanged")
	}
	if !bytes.Equal(rewriteHostPlaceholder(in, "evil/path"), in) {
		t.Fatal("slash host must leave bytes unchanged")
	}
}

func TestNeedsHostInject(t *testing.T) {
	if !needsHostInject("modules/emby-apiclient/connectionmanager.js") {
		t.Fatal("expected inject path")
	}
	if needsHostInject("modules/other.js") {
		t.Fatal("unexpected inject path")
	}
}
