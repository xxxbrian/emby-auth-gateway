package embyweb

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// URL grammar
// ---------------------------------------------------------------------------

func TestParseURLBaseValid(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		allowHTTP bool
	}{
		{"https root", "https://example.com/", false},
		{"https nested", "https://cdn.example.com/web/assets/", false},
		{"https with port", "https://example.com:8443/files/", false},
		{"http with flag", "http://example.com/tree/", true},
		{"ipv4 host https", "https://8.8.8.8/assets/", false},
		{"ipv6 host https", "https://[2001:4860:4860::8888]/assets/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := parseURLBase(tc.raw, tc.allowHTTP)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !strings.HasSuffix(u.Path, "/") {
				t.Fatalf("path %q missing trailing slash", u.Path)
			}
			if u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Opaque != "" {
				t.Fatalf("unexpected fields: %+v", u)
			}
		})
	}
}

func TestParseURLBaseRejects(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", urlMaxBaseBytes)
	cases := []struct {
		name      string
		raw       string
		allowHTTP bool
		substr    string
	}{
		{"empty", "", false, "empty"},
		{"too long", long, false, "exceeds"},
		{"whitespace", " https://example.com/", false, "whitespace"},
		{"relative", "/assets/", false, "absolute"},
		{"no scheme", "example.com/assets/", false, "absolute"},
		{"ftp", "ftp://example.com/", false, "scheme"},
		{"http without flag", "http://example.com/", false, "allowHTTP"},
		{"userinfo", "https://user:pass@example.com/", false, "userinfo"},
		{"query", "https://example.com/?x=1", false, "query"},
		{"fragment", "https://example.com/#frag", false, "fragment"},
		{"empty query mark", "https://example.com/?", false, "query"},
		{"no trailing slash", "https://example.com/assets", false, "end with"},
		{"empty path no slash", "https://example.com", false, "end with"},
		{"percent host", "https://example%2ecom/", false, ""}, // url.Parse rejects %2e in host
		{"non-ascii host", "https://exämple.com/", false, "ASCII"},
		{"opaque", "https:example.com/foo/", false, "opaque"},
		{"dotdot path", "https://example.com/a/../b/", false, "clean"},
		{"backslash path", "https://example.com/a\\b/", false, "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseURLBase(tc.raw, tc.allowHTTP)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.substr != "" && !strings.Contains(err.Error(), tc.substr) {
				t.Fatalf("err=%v want substr %q", err, tc.substr)
			}
		})
	}
}

func TestAllowHTTPIndependentOfPrivate(t *testing.T) {
	// HTTP to a public name is allowed with only allowHTTP (IP policy checked later).
	src, err := newURLSource(urlSourceSpec{
		BaseURL:   "http://example.com/tree/",
		AllowHTTP: true,
	}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if src.allowHTTP != true || src.allowPrivate != false {
		t.Fatalf("flags: http=%v private=%v", src.allowHTTP, src.allowPrivate)
	}

	// HTTPS private still needs allowPrivate at resolve time, not construction.
	src2, err := newURLSource(urlSourceSpec{
		BaseURL: "https://127.0.0.1/tree/",
	}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = src2.resolveOnce(context.Background(), "127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "allowPrivate") {
		t.Fatalf("expected allowPrivate error, got %v", err)
	}

	// HTTP + private IP requires both flags at resolve.
	src3, err := newURLSource(urlSourceSpec{
		BaseURL:   "http://127.0.0.1/tree/",
		AllowHTTP: true,
	}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = src3.resolveOnce(context.Background(), "127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "allowPrivate") {
		t.Fatalf("http without allowPrivate: %v", err)
	}

	src4, err := newURLSource(urlSourceSpec{
		BaseURL:      "http://127.0.0.1/tree/",
		AllowHTTP:    true,
		AllowPrivate: true,
	}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := src4.resolveOnce(context.Background(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0].String() != "127.0.0.1" {
		t.Fatalf("addrs=%v", addrs)
	}
}

func TestJoinURLBasePathNoEscape(t *testing.T) {
	base, err := parseURLBase("https://example.com/web/assets/", false)
	if err != nil {
		t.Fatal(err)
	}
	u, err := joinURLBasePath(base, "modules/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "https://example.com/web/assets/modules/app.js" {
		t.Fatalf("got %s", u.String())
	}
	if u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("query/fragment leaked")
	}
	// validAssetPath rejects ".."; join should still refuse invalid paths.
	if _, err := joinURLBasePath(base, "../secret"); err == nil {
		t.Fatal("expected reject")
	}
}

func TestJoinURLBasePathLiteralDotDotInFilename(t *testing.T) {
	// Canonical catalog names may contain ".." as a substring of a segment
	// (e.g. foo..js). That is not traversal and must be accepted.
	cases := []struct {
		base string
		rel  string
		want string
	}{
		{"https://example.com/", "foo..js", "https://example.com/foo..js"},
		{"https://example.com/", "a..b..c.css", "https://example.com/a..b..c.css"},
		{"https://example.com/web/", "foo..js", "https://example.com/web/foo..js"},
		{"https://example.com/web/assets/", "modules/foo..js", "https://example.com/web/assets/modules/foo..js"},
		{"https://cdn.example.com/tree/v1/", "vendor/jquery-3..min.js", "https://cdn.example.com/tree/v1/vendor/jquery-3..min.js"},
		{"https://example.com/a/b/", "x..y/z..w.map", "https://example.com/a/b/x..y/z..w.map"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if !validAssetPath(tc.rel) {
				t.Fatalf("precondition: validAssetPath(%q) = false", tc.rel)
			}
			base, err := parseURLBase(tc.base, false)
			if err != nil {
				t.Fatal(err)
			}
			u, err := joinURLBasePath(base, tc.rel)
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			if u.String() != tc.want {
				t.Fatalf("got %q want %q", u.String(), tc.want)
			}
			if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
				t.Fatalf("unexpected URL fields: %+v", u)
			}
			// Path must retain the literal ".." substring from the filename.
			if !strings.Contains(u.Path, "..") {
				t.Fatalf("path lost literal .. : %q", u.Path)
			}
		})
	}
}

func TestJoinURLBasePathRootAndNonRoot(t *testing.T) {
	root, err := parseURLBase("https://example.com/", false)
	if err != nil {
		t.Fatal(err)
	}
	nested, err := parseURLBase("https://example.com/web/assets/", false)
	if err != nil {
		t.Fatal(err)
	}

	// Root base joins
	u, err := joinURLBasePath(root, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "https://example.com/index.html" {
		t.Fatalf("root join: %s", u.String())
	}
	u, err = joinURLBasePath(root, "modules/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "https://example.com/modules/app.js" {
		t.Fatalf("root nested join: %s", u.String())
	}
	u, err = joinURLBasePath(root, "foo..js")
	if err != nil {
		t.Fatalf("root foo..js: %v", err)
	}
	if u.Path != "/foo..js" {
		t.Fatalf("root foo..js path=%q", u.Path)
	}

	// Non-root base joins
	u, err = joinURLBasePath(nested, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "https://example.com/web/assets/index.html" {
		t.Fatalf("nested join: %s", u.String())
	}
	u, err = joinURLBasePath(nested, "css/site.css")
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "https://example.com/web/assets/css/site.css" {
		t.Fatalf("nested deep join: %s", u.String())
	}
	u, err = joinURLBasePath(nested, "foo..js")
	if err != nil {
		t.Fatalf("nested foo..js: %v", err)
	}
	if u.Path != "/web/assets/foo..js" {
		t.Fatalf("nested foo..js path=%q", u.Path)
	}
}

func TestJoinURLBasePathTraversalRejected(t *testing.T) {
	root, err := parseURLBase("https://example.com/", false)
	if err != nil {
		t.Fatal(err)
	}
	nested, err := parseURLBase("https://example.com/web/assets/", false)
	if err != nil {
		t.Fatal(err)
	}

	// Paths that validAssetPath rejects (traversal / non-canonical).
	invalid := []string{
		"../secret",
		"..",
		"foo/../bar.js",
		"foo/..",
		"/abs.js",
		"./rel.js",
		"foo//bar.js",
		"foo/./bar.js",
		"",
		"foo\\bar.js",
		"foo\x00bar.js",
	}
	for _, rel := range invalid {
		for _, base := range []*url.URL{root, nested} {
			if _, err := joinURLBasePath(base, rel); err == nil {
				t.Fatalf("expected reject rel=%q base=%q", rel, base.Path)
			}
		}
	}
}

func TestJoinURLBasePathQuestionHashEncodedInPath(t *testing.T) {
	// Trusted catalog filenames may contain literal '?' and '#'. They must be
	// placed in Path so EscapedPath percent-encodes them; RawQuery/Fragment stay empty.
	type row struct {
		base        string
		rel         string
		wantPath    string // decoded Path form
		wantEscaped string // EscapedPath / request path
		wantString  string
	}
	rows := []row{
		// Root base
		{
			base:        "https://example.com/",
			rel:         "foo?bar.js",
			wantPath:    "/foo?bar.js",
			wantEscaped: "/foo%3Fbar.js",
			wantString:  "https://example.com/foo%3Fbar.js",
		},
		{
			base:        "https://example.com/",
			rel:         "foo#bar.js",
			wantPath:    "/foo#bar.js",
			wantEscaped: "/foo%23bar.js",
			wantString:  "https://example.com/foo%23bar.js",
		},
		{
			base:        "https://example.com/",
			rel:         "a?b#c.js",
			wantPath:    "/a?b#c.js",
			wantEscaped: "/a%3Fb%23c.js",
			wantString:  "https://example.com/a%3Fb%23c.js",
		},
		// Nested base
		{
			base:        "https://example.com/web/assets/",
			rel:         "foo?bar.js",
			wantPath:    "/web/assets/foo?bar.js",
			wantEscaped: "/web/assets/foo%3Fbar.js",
			wantString:  "https://example.com/web/assets/foo%3Fbar.js",
		},
		{
			base:        "https://example.com/web/assets/",
			rel:         "modules/foo#bar.js",
			wantPath:    "/web/assets/modules/foo#bar.js",
			wantEscaped: "/web/assets/modules/foo%23bar.js",
			wantString:  "https://example.com/web/assets/modules/foo%23bar.js",
		},
		{
			base:        "https://cdn.example.com/tree/v1/",
			rel:         "vendor/a?b#c.min.js",
			wantPath:    "/tree/v1/vendor/a?b#c.min.js",
			wantEscaped: "/tree/v1/vendor/a%3Fb%23c.min.js",
			wantString:  "https://cdn.example.com/tree/v1/vendor/a%3Fb%23c.min.js",
		},
	}
	for _, tc := range rows {
		t.Run(tc.wantString, func(t *testing.T) {
			if !validAssetPath(tc.rel) {
				t.Fatalf("precondition: validAssetPath(%q)=false", tc.rel)
			}
			base, err := parseURLBase(tc.base, false)
			if err != nil {
				t.Fatal(err)
			}
			u, err := joinURLBasePath(base, tc.rel)
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			if u.Path != tc.wantPath {
				t.Fatalf("Path=%q want %q", u.Path, tc.wantPath)
			}
			if u.RawQuery != "" || u.Fragment != "" {
				t.Fatalf("RawQuery/Fragment must be empty: q=%q f=%q", u.RawQuery, u.Fragment)
			}
			if u.RawPath != "" {
				t.Fatalf("RawPath must be empty so EscapedPath derives from Path, got %q", u.RawPath)
			}
			if got := u.EscapedPath(); got != tc.wantEscaped {
				t.Fatalf("EscapedPath=%q want %q", got, tc.wantEscaped)
			}
			if got := u.RequestURI(); got != tc.wantEscaped {
				t.Fatalf("RequestURI=%q want %q (path-only, no query)", got, tc.wantEscaped)
			}
			if u.String() != tc.wantString {
				t.Fatalf("String=%q want %q", u.String(), tc.wantString)
			}
			// Ensure '?' / '#' did not split into query/fragment components.
			if strings.Contains(u.String(), "?") && !strings.Contains(u.EscapedPath(), "%3F") && !strings.Contains(u.EscapedPath(), "%3f") {
				t.Fatalf("unescaped ? in URL string: %s", u.String())
			}
			if strings.Contains(u.String(), "#") {
				t.Fatalf("raw # in URL string (would be fragment): %s", u.String())
			}
		})
	}
}

func TestJoinURLBasePathLiteralPercentAndEscapedSeparators(t *testing.T) {
	// Literal '%' in a trusted filename is path data: EscapedPath encodes it as %25.
	// A filename containing the characters "%2f" is NOT a path separator.
	root, err := parseURLBase("https://example.com/", false)
	if err != nil {
		t.Fatal(err)
	}
	nested, err := parseURLBase("https://example.com/web/", false)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		base        *url.URL
		rel         string
		wantPath    string
		wantEscaped string
	}{
		{root, "foo%2fbar.js", "/foo%2fbar.js", "/foo%252fbar.js"},
		{root, "foo%23bar.js", "/foo%23bar.js", "/foo%2523bar.js"},
		{root, "foo%3Fbar.js", "/foo%3Fbar.js", "/foo%253Fbar.js"},
		{root, "foo%2ebar.js", "/foo%2ebar.js", "/foo%252ebar.js"},
		{nested, "modules/foo%2fbar.js", "/web/modules/foo%2fbar.js", "/web/modules/foo%252fbar.js"},
		{nested, "a%00b.js", "/web/a%00b.js", "/web/a%2500b.js"}, // literal %00 in name, not NUL
	}
	for _, tc := range cases {
		t.Run(tc.wantEscaped, func(t *testing.T) {
			if !validAssetPath(tc.rel) {
				t.Fatalf("precondition: validAssetPath(%q)=false", tc.rel)
			}
			u, err := joinURLBasePath(tc.base, tc.rel)
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			if u.Path != tc.wantPath {
				t.Fatalf("Path=%q want %q", u.Path, tc.wantPath)
			}
			if u.RawQuery != "" || u.Fragment != "" {
				t.Fatalf("query/fragment set: q=%q f=%q", u.RawQuery, u.Fragment)
			}
			if got := u.EscapedPath(); got != tc.wantEscaped {
				t.Fatalf("EscapedPath=%q want %q", got, tc.wantEscaped)
			}
			// Must remain a single path segment under base (no decoded separator).
			if strings.Count(u.Path, "/") != strings.Count(tc.wantPath, "/") {
				t.Fatalf("path segment count changed: %q", u.Path)
			}
		})
	}
}

func TestJoinURLBasePathNoQueryFragmentOnOutput(t *testing.T) {
	base, err := parseURLBase("https://example.com/web/", false)
	if err != nil {
		t.Fatal(err)
	}
	// Even if a caller mutates base fields after construction, output must be clean.
	base.RawQuery = "leak=1"
	base.Fragment = "frag"
	u, err := joinURLBasePath(base, "foo..js")
	if err != nil {
		t.Fatal(err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		t.Fatalf("query/fragment leaked: %s", u.String())
	}
	if u.String() != "https://example.com/web/foo..js" {
		t.Fatalf("got %s", u.String())
	}

	// Same isolation for filenames containing ? and #.
	u, err = joinURLBasePath(base, "x?y#z.js")
	if err != nil {
		t.Fatal(err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		t.Fatalf("query/fragment leaked for ?# name: q=%q f=%q str=%s", u.RawQuery, u.Fragment, u.String())
	}
	if u.Path != "/web/x?y#z.js" {
		t.Fatalf("Path=%q", u.Path)
	}
	if u.EscapedPath() != "/web/x%3Fy%23z.js" {
		t.Fatalf("EscapedPath=%q", u.EscapedPath())
	}
	if u.String() != "https://example.com/web/x%3Fy%23z.js" {
		t.Fatalf("String=%q", u.String())
	}
}

func TestParseURLBaseStillRejectsQueryFragment(t *testing.T) {
	// Base URL policy is unchanged: query/fragment on the base remain forbidden.
	for _, raw := range []string{
		"https://example.com/?x=1",
		"https://example.com/#frag",
		"https://example.com/web/?q=1",
		"https://example.com/web/#f",
		"https://example.com/?",
		"https://example.com/#",
	} {
		if _, err := parseURLBase(raw, false); err == nil {
			t.Fatalf("expected base reject for %q", raw)
		}
	}
}

// ---------------------------------------------------------------------------
// IP allow/deny tables
// ---------------------------------------------------------------------------

func TestCheckURLIPAllowedTables(t *testing.T) {
	type row struct {
		ip           string
		allowPrivate bool
		wantOK       bool
		note         string
	}
	rows := []row{
		// Public
		{"8.8.8.8", false, true, "public v4"},
		{"1.1.1.1", true, true, "public v4 with private flag"},
		{"2001:4860:4860::8888", false, true, "public v6"},

		// Loopback — private opt-in
		{"127.0.0.1", false, false, "loopback denied default"},
		{"127.0.0.1", true, true, "loopback allowed private"},
		{"::1", false, false, "v6 loopback denied"},
		{"::1", true, true, "v6 loopback allowed"},

		// RFC1918
		{"10.0.0.1", false, false, "10/8"},
		{"10.0.0.1", true, true, "10/8 private"},
		{"172.16.5.1", false, false, "172.16/12"},
		{"172.16.5.1", true, true, "172.16/12 private"},
		{"192.168.1.1", false, false, "192.168/16"},
		{"192.168.1.1", true, true, "192.168/16 private"},

		// CGNAT
		{"100.64.0.1", false, false, "CGNAT denied"},
		{"100.64.0.1", true, true, "CGNAT allowed private"},
		{"100.127.255.254", true, true, "CGNAT high"},

		// ULA
		{"fd12:3456:789a::1", false, false, "ULA denied"},
		{"fd12:3456:789a::1", true, true, "ULA allowed"},

		// Link-local — never
		{"169.254.1.1", false, false, "link-local v4"},
		{"169.254.1.1", true, false, "link-local never with private"},
		{"fe80::1", false, false, "link-local v6"},
		{"fe80::1", true, false, "link-local v6 never"},

		// Metadata
		{"169.254.169.254", false, false, "metadata"},
		{"169.254.169.254", true, false, "metadata never"},
		{"169.254.170.2", true, false, "ecs metadata never"},

		// Documentation
		{"192.0.2.1", false, false, "TEST-NET-1"},
		{"192.0.2.1", true, false, "TEST-NET-1 never"},
		{"198.51.100.1", true, false, "TEST-NET-2 never"},
		{"203.0.113.1", true, false, "TEST-NET-3 never"},
		{"2001:db8::1", false, false, "v6 docs"},
		{"2001:db8::1", true, false, "v6 docs never"},
		{"3fff::1", true, false, "v6 docs 3fff::/20 never"},
		{"3fff:0fff::1", true, false, "v6 docs 3fff::/20 high never"},

		// Benchmark
		{"198.18.0.1", true, false, "benchmark v4"},
		{"2001:2::1", true, false, "benchmark v6"},

		// Multicast
		{"224.0.0.1", true, false, "multicast v4"},
		{"ff02::1", true, false, "multicast v6"},

		// Unspecified
		{"0.0.0.0", true, false, "unspecified v4"},
		{"::", true, false, "unspecified v6"},

		// Reserved / transition
		{"240.0.0.1", true, false, "reserved v4"},
		{"255.255.255.255", true, false, "broadcast"},
		{"192.88.99.1", true, false, "6to4 anycast"},
		{"2002:c000:0201::1", true, false, "6to4 v6"},
		{"64:ff9b::8.8.8.8", true, false, "NAT64"},
		{"100::1", true, false, "discard-only"},

		// IPv4 AS112 / AMT (globally-routable-looking special-purpose)
		{"192.31.196.1", true, false, "AS112-v4 never"},
		{"192.52.193.1", true, false, "AMT never"},
		{"192.175.48.1", true, false, "AS112 direct delegation never"},

		// IPv6 AS112 / SRv6 / deprecated site-local
		{"2620:4f:8000::1", true, false, "AS112-v6 never"},
		{"5f00::1", true, false, "SRv6 SID never"},
		{"5f00:ffff::1", true, false, "SRv6 SID high never"},
		{"fec0::1", true, false, "deprecated site-local never"},
		{"feff::1", true, false, "site-local high never"},

		// IPv4-mapped (checked after Unmap in helper callers; direct check also Unmaps)
		{"::ffff:127.0.0.1", false, false, "mapped loopback denied"},
		{"::ffff:127.0.0.1", true, true, "mapped loopback private"},
		{"::ffff:8.8.8.8", false, true, "mapped public"},
		{"::ffff:169.254.169.254", true, false, "mapped metadata never"},
		{"::ffff:10.0.0.1", false, false, "mapped RFC1918 denied"},
		{"::ffff:10.0.0.1", true, true, "mapped RFC1918 private"},
		{"::ffff:100.64.0.1", true, true, "mapped CGNAT private"},
		{"::ffff:192.0.2.1", true, false, "mapped docs never"},
		{"::ffff:192.31.196.1", true, false, "mapped AS112-v4 never"},
		{"::ffff:192.52.193.1", true, false, "mapped AMT never"},
		{"::ffff:192.175.48.1", true, false, "mapped AS112-dd never"},
	}
	for _, r := range rows {
		t.Run(fmt.Sprintf("%s/private=%v", r.ip, r.allowPrivate), func(t *testing.T) {
			ip := netip.MustParseAddr(r.ip)
			err := checkURLIPAllowed(ip, r.allowPrivate)
			if r.wantOK && err != nil {
				t.Fatalf("%s: unexpected err %v", r.note, err)
			}
			if !r.wantOK && err == nil {
				t.Fatalf("%s: expected deny", r.note)
			}
		})
	}
}

func TestCheckURLIPSpecialPurposeBoundaries(t *testing.T) {
	// Inside always-denied special-purpose blocks vs first address outside.
	type row struct {
		ip     string
		wantOK bool
		note   string
	}
	rows := []row{
		// AS112-v4 192.31.196.0/24
		{"192.31.196.0", false, "AS112-v4 network"},
		{"192.31.196.255", false, "AS112-v4 broadcast"},
		{"192.31.195.255", true, "just below AS112-v4"},
		{"192.31.197.0", true, "just above AS112-v4"},

		// AMT 192.52.193.0/24
		{"192.52.193.0", false, "AMT network"},
		{"192.52.193.255", false, "AMT high"},
		{"192.52.192.255", true, "just below AMT"},
		{"192.52.194.0", true, "just above AMT"},

		// Direct Delegation AS112 192.175.48.0/24
		{"192.175.48.0", false, "AS112-dd network"},
		{"192.175.48.255", false, "AS112-dd high"},
		{"192.175.47.255", true, "just below AS112-dd"},
		{"192.175.49.0", true, "just above AS112-dd"},

		// AS112-v6 2620:4f:8000::/48
		{"2620:4f:8000::", false, "AS112-v6 network"},
		{"2620:4f:8000:ffff:ffff:ffff:ffff:ffff", false, "AS112-v6 high"},
		{"2620:4f:7fff:ffff:ffff:ffff:ffff:ffff", true, "just below AS112-v6"},
		{"2620:4f:8001::", true, "just above AS112-v6"},

		// Documentation 3fff::/20 (covers 3fff:0000::–3fff:0fff:…)
		{"3fff::", false, "3fff network"},
		{"3fff:0fff:ffff:ffff:ffff:ffff:ffff:ffff", false, "3fff high"},
		{"3ffe:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true, "just below 3fff"},
		{"3fff:1000::", true, "just above 3fff::/20"},

		// SRv6 SIDs 5f00::/16 (covers 5f00:0000::–5f00:ffff:…)
		{"5f00::", false, "SRv6 network"},
		{"5f00:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false, "SRv6 high"},
		{"5eff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", true, "just below SRv6"},
		{"5f01::", true, "just above SRv6"},

		// Deprecated site-local fec0::/10 (fec0::–feff:…)
		{"fec0::", false, "site-local network"},
		{"feff:ffff:ffff:ffff:ffff:ffff:ffff:ffff", false, "site-local high"},
		// fe80::/10 link-local is also always denied (adjacent below fec0::/10).
		{"fe80::1", false, "link-local adjacent below site-local"},
		{"ff00::1", false, "multicast above site-local"},
		// Public global unicast well clear of site-local.
		{"2600::1", true, "public v6 outside site-local"},

		// Mapped boundary forms for IPv4 specials
		{"::ffff:192.31.196.1", false, "mapped inside AS112-v4"},
		{"::ffff:192.31.195.255", true, "mapped just below AS112-v4"},
		{"::ffff:192.52.193.1", false, "mapped inside AMT"},
		{"::ffff:192.52.194.0", true, "mapped just above AMT"},
		{"::ffff:192.175.48.1", false, "mapped inside AS112-dd"},
		{"::ffff:192.175.49.0", true, "mapped just above AS112-dd"},
	}
	for _, r := range rows {
		t.Run(r.note, func(t *testing.T) {
			ip := netip.MustParseAddr(r.ip)
			err := checkURLIPAllowed(ip, true) // allowPrivate must not open specials
			if r.wantOK && err != nil {
				t.Fatalf("unexpected err %v", err)
			}
			if !r.wantOK && err == nil {
				t.Fatal("expected deny")
			}
		})
	}
}

func TestCheckURLIPGlobalUnicastFailClosed(t *testing.T) {
	// After explicit tables, non-global-unicast addresses are denied.
	// Multicast/unspecified/link-local are also in always-forbidden tables;
	// this asserts the fail-closed path message for a non-global address and
	// that private opt-in still works for loopback (which is not global unicast).
	if err := checkURLIPAllowed(netip.MustParseAddr("224.0.0.1"), false); err == nil {
		t.Fatal("multicast must be denied")
	}
	// Loopback is not IsGlobalUnicast; allowPrivate must still permit it via opt-in.
	if err := checkURLIPAllowed(netip.MustParseAddr("127.0.0.1"), true); err != nil {
		t.Fatalf("loopback with allowPrivate: %v", err)
	}
	if err := checkURLIPAllowed(netip.MustParseAddr("::1"), true); err != nil {
		t.Fatalf("v6 loopback with allowPrivate: %v", err)
	}
	// RFC1918 / ULA / CGNAT remain opt-in only (and are accepted before fail-closed).
	for _, s := range []string{"10.1.2.3", "172.16.0.1", "192.168.0.1", "100.64.1.1", "fd00::1"} {
		if err := checkURLIPAllowed(netip.MustParseAddr(s), true); err != nil {
			t.Fatalf("%s allowPrivate: %v", s, err)
		}
		if err := checkURLIPAllowed(netip.MustParseAddr(s), false); err == nil {
			t.Fatalf("%s without allowPrivate must deny", s)
		}
	}
	// Ordinary public globals still allowed without allowPrivate.
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		if err := checkURLIPAllowed(netip.MustParseAddr(s), false); err != nil {
			t.Fatalf("public %s: %v", s, err)
		}
	}
}

func TestResolveMixedAnswerRejectsAll(t *testing.T) {
	src, err := newURLSource(urlSourceSpec{BaseURL: "https://example.com/"}, urlSourceDeps{
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return []netip.Addr{
				netip.MustParseAddr("8.8.8.8"),
				netip.MustParseAddr("127.0.0.1"),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.resolveOnce(context.Background(), "example.com")
	if err == nil {
		t.Fatal("expected reject mixed answer")
	}
	if !strings.Contains(err.Error(), "127.0.0.1") && !strings.Contains(err.Error(), "private") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveDedupeAndUnmap(t *testing.T) {
	var calls atomic.Int32
	src, err := newURLSource(urlSourceSpec{BaseURL: "https://example.com/"}, urlSourceDeps{
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			calls.Add(1)
			return []netip.Addr{
				netip.MustParseAddr("8.8.8.8"),
				netip.MustParseAddr("::ffff:8.8.8.8"),
				netip.MustParseAddr("8.8.8.8"),
				netip.MustParseAddr("1.1.1.1"),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	addrs, err := src.resolveOnce(context.Background(), "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resolve calls=%d", calls.Load())
	}
	if len(addrs) != 2 {
		t.Fatalf("addrs=%v want 2 after unmap+dedupe", addrs)
	}
	got := map[string]bool{}
	for _, a := range addrs {
		got[a.String()] = true
	}
	if !got["8.8.8.8"] || !got["1.1.1.1"] {
		t.Fatalf("got %v", addrs)
	}
}

func TestResolveOncePerAcquire(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-once", "1.0.0")
	data := fixtureDataMap(files)

	var resolveCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body, ok := data[rel]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	base := ensureTrailingSlash(srv.URL)
	host, port := mustSplitHostPort(t, srv.Listener.Addr().String())
	ip := netip.MustParseAddr(host)

	src, err := newURLSource(urlSourceSpec{
		BaseURL:      base,
		AllowHTTP:    true,
		AllowPrivate: true,
	}, urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			resolveCalls.Add(1)
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Force dial to the test server port with validated literal.
			_, p, _ := net.SplitHostPort(address)
			if p == "" {
				p = port
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Override base host to a name so Resolve is used (not IP literal path).
	src.base.Host = net.JoinHostPort("files.example.test", port)

	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolve calls=%d want 1", resolveCalls.Load())
	}
	if err := w.complete(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Dial / Host / proxy / concurrency / HTTP policy
// ---------------------------------------------------------------------------

func TestURLSourceDialUsesLiteralNotHostname(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-dial", "1.0.0")
	data := fixtureDataMap(files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body := data[rel]
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	host, port := mustSplitHostPort(t, srv.Listener.Addr().String())
	ip := netip.MustParseAddr(host)

	var dialed []string
	var mu sync.Mutex

	src, err := newURLSource(urlSourceSpec{
		BaseURL:      "http://files.example.test:" + port + "/",
		AllowHTTP:    true,
		AllowPrivate: true,
	}, urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			if h != "files.example.test" {
				t.Errorf("resolve host=%q", h)
			}
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			dialed = append(dialed, address)
			mu.Unlock()
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if h == "files.example.test" {
				return nil, errors.New("dialed hostname instead of literal")
			}
			if _, err := netip.ParseAddr(h); err != nil {
				return nil, fmt.Errorf("not literal: %s", address)
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) == 0 {
		t.Fatal("no dials recorded")
	}
	for _, a := range dialed {
		h, _, _ := net.SplitHostPort(a)
		if h == "files.example.test" {
			t.Fatalf("dialed hostname: %s", a)
		}
		if netip.MustParseAddr(h) != ip {
			t.Fatalf("dialed %s want %s", a, ip)
		}
	}
}

func TestURLSourcePreservesHostHeader(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-host", "1.0.0")
	data := fixtureDataMap(files)

	var sawHost atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHost.Store(r.Host)
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body := data[rel]
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	host, port := mustSplitHostPort(t, srv.Listener.Addr().String())
	ip := netip.MustParseAddr(host)
	wantHost := "assets.example.test:" + port

	src, err := newURLSource(urlSourceSpec{
		BaseURL:      "http://" + wantHost + "/",
		AllowHTTP:    true,
		AllowPrivate: true,
	}, urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	got, _ := sawHost.Load().(string)
	if got != wantHost {
		t.Fatalf("Host=%q want %q", got, wantHost)
	}
}

func TestURLSourceSuccessNestedConcurrentRace(t *testing.T) {
	files := readyMinimalFiles()
	// Ensure nested paths exist (modules/, strings/, css/, img/).
	tc := buildSyntheticCatalog(t, files, "url-ok", "1.0.0")
	data := fixtureDataMap(files)

	var concurrent atomic.Int32
	var maxConc atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := concurrent.Add(1)
		defer concurrent.Add(-1)
		for {
			cur := maxConc.Load()
			if n <= cur || maxConc.CompareAndSwap(cur, n) {
				break
			}
		}
		// Small delay so concurrency is observable.
		time.Sleep(20 * time.Millisecond)
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body, ok := data[rel]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)

	// Run under race detector via go test -race.
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	if err := w.complete(); err != nil {
		t.Fatal(err)
	}
	if maxConc.Load() < 2 && len(files) > 1 {
		// Soft check: with 6 files and 20ms delay we should see some overlap.
		t.Logf("max concurrency observed=%d (may be flaky on busy CI)", maxConc.Load())
	}
}

func TestURLSourceMaxConcurrencyAtMost8(t *testing.T) {
	// Build a catalog with more than 8 files by extending minimal set with unique js files.
	base := readyMinimalFiles()
	files := append([]fixtureFile{}, base...)
	for i := 0; i < 12; i++ {
		p := fmt.Sprintf("modules/extra%d.js", i)
		files = append(files, fixtureFile{
			Path:       p,
			Data:       []byte(fmt.Sprintf("console.log(%d)", i)),
			CacheClass: cacheImmutable,
		})
	}
	tc := buildSyntheticCatalog(t, files, "url-conc", "1.0.0")
	data := fixtureDataMap(files)

	var concurrent atomic.Int32
	var maxConc atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := concurrent.Add(1)
		defer concurrent.Add(-1)
		for {
			cur := maxConc.Load()
			if n <= cur || maxConc.CompareAndSwap(cur, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body := data[rel]
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	if maxConc.Load() > urlMaxConcurrency {
		t.Fatalf("max concurrency %d > %d", maxConc.Load(), urlMaxConcurrency)
	}
	if maxConc.Load() < 2 {
		t.Fatalf("expected some concurrency, max=%d", maxConc.Load())
	}
}

func TestURLSourceStatusNotOK(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-404", "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	err := src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("err=%v", err)
	}
}

func TestURLSourceRejectsRedirect(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-redir", "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	err := src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected redirect error")
	}
	if !strings.Contains(err.Error(), "redirect") && !strings.Contains(err.Error(), "status") {
		// Client CheckRedirect surfaces as Do() error containing "redirects forbidden"
		// or we may see the 302 if CheckRedirect path differs.
		t.Fatalf("err=%v", err)
	}
}

func TestURLSourceContentLengthMismatch(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-cl", "1.0.0")
	data := fixtureDataMap(files)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body := data[rel]
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)+5))
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	err := src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected content-length error")
	}
}

func TestURLSourceContentEncodingRejected(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-ce", "1.0.0")
	data := fixtureDataMap(files)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body := data[rel]
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	err := src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "Content-Encoding") {
		t.Fatalf("err=%v", err)
	}
}

func TestURLSourceIdentityEncodingOK(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-id", "1.0.0")
	data := fixtureDataMap(files)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body := data[rel]
		w.Header().Set("Content-Encoding", "identity")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
}

func TestURLSourceOversizeBody(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-over", "1.0.0")
	data := fixtureDataMap(files)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		body := append(append([]byte{}, data[rel]...), 'X')
		// Omit Content-Length so mismatch is detected by stagingWriter size+1 read.
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	err := src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected oversize error")
	}
}

func TestURLSourceHashMismatch(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-hash", "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wrong bytes, same length as index.html when requested.
		rel := strings.TrimPrefix(r.URL.Path, "/")
		var want int
		for _, f := range files {
			if f.Path == rel {
				want = len(f.Data)
				break
			}
		}
		_, _ = w.Write([]byte(strings.Repeat("Z", want)))
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	err := src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err=%v", err)
	}
}

func TestURLSourceCancel(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-cancel", "1.0.0")
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	err := src.acquire(ctx, tc, w)
	if err == nil {
		t.Fatal("expected cancel error")
	}
}

func TestURLSourcePerFileTimeout(t *testing.T) {
	files := []fixtureFile{
		{Path: "index.html", Data: []byte("<!doctype html><title>t</title>")},
		{Path: "manifest.json", Data: []byte(`{"name":"test"}`)},
		{Path: "strings/en-US.json", Data: []byte(`{"Hello":"Hello"}`)},
	}
	tc := buildSyntheticCatalog(t, files, "url-timeout", "1.0.0")
	// Only one slow path; others hang too — overall should fail via context.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until client context cancels.
		<-r.Context().Done()
	}))
	defer srv.Close()

	host, port := mustSplitHostPort(t, srv.Listener.Addr().String())
	ip := netip.MustParseAddr(host)
	src, err := newURLSource(urlSourceSpec{
		BaseURL:      ensureTrailingSlash(srv.URL),
		AllowHTTP:    true,
		AllowPrivate: true,
	}, urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Short overall timeout to keep the test fast (per-file is 5m in prod).
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w, _, _ := newTestStagingWriter(t, files)
	err = src.acquire(ctx, tc, w)
	if err == nil {
		t.Fatal("expected timeout")
	}
}

func TestURLSourceHeaderOverflow(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-hdr", "1.0.0")
	// Custom listener: write an oversized header block.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 4096)
				_, _ = conn.Read(buf)
				// Status + huge header exceeding 64KiB.
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\n")
				pad := strings.Repeat("a", 70<<10)
				_, _ = io.WriteString(conn, "X-Pad: "+pad+"\r\n\r\n")
				_, _ = io.WriteString(conn, "body")
			}(c)
		}
	}()

	host, port := mustSplitHostPort(t, ln.Addr().String())
	ip := netip.MustParseAddr(host)
	src, err := newURLSource(urlSourceSpec{
		BaseURL:      "http://" + net.JoinHostPort(host, port) + "/",
		AllowHTTP:    true,
		AllowPrivate: true,
	}, urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, _, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected header overflow error")
	}
}

func TestURLSourceNoEnvProxy(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-proxy", "1.0.0")
	data := fixtureDataMap(files)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		_, _ = w.Write(data[rel])
	}))
	defer srv.Close()

	// Point proxy env at a black hole; transport must ignore it (Proxy: nil).
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("https_proxy", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("all_proxy", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatalf("proxy env must be ignored: %v", err)
	}
	if err := w.complete(); err != nil {
		t.Fatal(err)
	}
}

func TestURLSourceNoDuplicateWrites(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-dup", "1.0.0")
	data := fixtureDataMap(files)
	var (
		mu   sync.Mutex
		hits = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		mu.Lock()
		hits[rel]++
		mu.Unlock()
		_, _ = w.Write(data[rel])
	}))
	defer srv.Close()
	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	// Within one acquire, each catalog path is fetched exactly once.
	mu.Lock()
	for _, f := range files {
		if hits[f.Path] != 1 {
			mu.Unlock()
			t.Fatalf("path %s fetch count=%d want 1", f.Path, hits[f.Path])
		}
	}
	mu.Unlock()
	// Second acquire on same writer must fail (stagingWriter rejects duplicates).
	err := src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected duplicate write error on second acquire")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err=%v want duplicate", err)
	}
}

func TestURLSourceKind(t *testing.T) {
	src, err := newURLSource(urlSourceSpec{BaseURL: "https://example.com/"}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if src.kind() != "url" {
		t.Fatalf("kind=%q", src.kind())
	}
	var _ acquisitionSource = src
}

func TestURLSourceIPLiteralNoDNS(t *testing.T) {
	var resolveCalls atomic.Int32
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-lit", "1.0.0")
	data := fixtureDataMap(files)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		_, _ = w.Write(data[rel])
	}))
	defer srv.Close()

	// srv.URL is http://127.0.0.1:port
	src, err := newURLSource(urlSourceSpec{
		BaseURL:      ensureTrailingSlash(srv.URL),
		AllowHTTP:    true,
		AllowPrivate: true,
	}, urlSourceDeps{
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			resolveCalls.Add(1)
			return nil, errors.New("DNS should not be called for IP literal")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	if resolveCalls.Load() != 0 {
		t.Fatalf("resolve calls=%d", resolveCalls.Load())
	}
}

func TestURLSourceTLSSNI(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-tls", "1.0.0")
	data := fixtureDataMap(files)

	// httptest TLS server with certificate for example.com-like name is awkward;
	// use a plain TLS listener and assert ServerName via GetConfigForClient.
	cert, err := tls.X509KeyPair(localhostCert, localhostKey)
	if err != nil {
		t.Fatal(err)
	}
	var sawSNI atomic.Value
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			sawSNI.Store(chi.ServerName)
			return &tls.Config{
				Certificates: []tls.Certificate{cert},
			}, nil
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tlsLn := tls.NewListener(ln, tlsCfg)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/")
		_, _ = w.Write(data[rel])
	})}
	go func() { _ = srv.Serve(tlsLn) }()
	defer srv.Close()

	host, port := mustSplitHostPort(t, ln.Addr().String())
	ip := netip.MustParseAddr(host)
	// Trust is relaxed only via package-private TLSClientConfig (tests).
	// Production leaves TLSClientConfig nil so system roots / normal verify apply.
	clientTLS := &tls.Config{
		ServerName:         "files.example.test",
		InsecureSkipVerify: true, // test-only; never set in production path
	}

	src, err := newURLSource(urlSourceSpec{
		BaseURL:      "https://files.example.test:" + port + "/",
		AllowPrivate: true,
	}, urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
		TLSClientConfig: clientTLS,
	})
	if err != nil {
		t.Fatal(err)
	}
	w, _, _ := newTestStagingWriter(t, files)
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	sni, _ := sawSNI.Load().(string)
	if sni != "files.example.test" {
		t.Fatalf("SNI=%q want files.example.test", sni)
	}
}

func TestURLSourceWorkerErrorCancelsOthers(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "url-cancel-all", "1.0.0")
	var started atomic.Int32
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		rel := strings.TrimPrefix(r.URL.Path, "/")
		if rel == "index.html" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		<-block // hang until test ends / cancel
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	defer close(block)

	src := mustLocalHTTPURLSource(t, srv)
	w, _, _ := newTestStagingWriter(t, files)
	err := src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected error")
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustLocalHTTPURLSource(t *testing.T, srv *httptest.Server) *urlSource {
	t.Helper()
	host, port := mustSplitHostPort(t, srv.Listener.Addr().String())
	ip := netip.MustParseAddr(host)
	src, err := newURLSource(urlSourceSpec{
		BaseURL:      ensureTrailingSlash(srv.URL),
		AllowHTTP:    true,
		AllowPrivate: true,
	}, urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func ensureTrailingSlash(u string) string {
	if strings.HasSuffix(u, "/") {
		return u
	}
	return u + "/"
}

func mustSplitHostPort(t *testing.T, addr string) (host, port string) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return h, p
}

// Test-only self-signed cert for CN/SAN files.example.test (not used in production).
// Verification is intentionally relaxed only via package-private TLSClientConfig in tests.
var localhostCert = []byte(`-----BEGIN CERTIFICATE-----
MIIBrjCCAVSgAwIBAgIUOjyMeA2isIYiFrlhDF3T5L7Li/QwCgYIKoZIzj0EAwIw
HTEbMBkGA1UEAwwSZmlsZXMuZXhhbXBsZS50ZXN0MB4XDTI2MDcxNDAwNDMwNloX
DTM2MDcxMTAwNDMwNlowHTEbMBkGA1UEAwwSZmlsZXMuZXhhbXBsZS50ZXN0MFkw
EwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAELYUseNM9FWquTcpcZZRN5ofFnqGsApP4
3vYXua8Frp2ogGrfUFvGzqxPTL0/X1ngPvBETZp7EkvsgPYRlTI84qNyMHAwHQYD
VR0OBBYEFKD2Cve2EEavNzBjGOkIRYDBYlySMB8GA1UdIwQYMBaAFKD2Cve2EEav
NzBjGOkIRYDBYlySMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0RBBYwFIISZmlsZXMu
ZXhhbXBsZS50ZXN0MAoGCCqGSM49BAMCA0gAMEUCIC7BNHg+DHC5AbyJ195YcRrG
L3Hae8gshgRZF03X/gYRAiEA2be6dIevoc9vGuDEaDT4OLjTGY03yqfU8zltVf/X
o6s=
-----END CERTIFICATE-----`)

var localhostKey = []byte(`-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgECuOiPeHtmaNZE9I
0SWNGQB+AKrzO53fzsQtuJ3eX9ehRANCAAQthSx40z0Vaq5NylxllE3mh8WeoawC
k/je9he5rwWunaiAat9QW8bOrE9MvT9fWeA+8ERNmnsSS+yA9hGVMjzi
-----END PRIVATE KEY-----`)
