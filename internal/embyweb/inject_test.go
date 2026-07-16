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
	in := []byte(`u="https://mb3admin.com/api"`)
	got := rewriteHostPlaceholder(in, "media.xvv.net")
	if !bytes.Equal(got, []byte(`u="https://media.xvv.net/api"`)) {
		t.Fatalf("got %q", got)
	}
	if !bytes.Equal(rewriteHostPlaceholder(in, ""), in) {
		t.Fatal("empty host must leave bytes unchanged")
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
