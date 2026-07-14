package embyweb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseArchiveURLValid(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		allowHTTP bool
	}{
		{"https root file", "https://example.com/emby-web.tar.gz", false},
		{"https nested", "https://cdn.example.com/web/emby-web-prepared-4.9.5.0.tar.gz", false},
		{"https with port", "https://example.com:8443/files/a.tar.gz", false},
		{"http with flag", "http://example.com/a.tar.gz", true},
		{"ipv4 host", "https://8.8.8.8/a.tar.gz", false},
		{"ipv6 host", "https://[2001:4860:4860::8888]/a.tar.gz", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := parseArchiveURL(tc.raw, tc.allowHTTP)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if !strings.HasSuffix(u.Path, archiveSuffix) {
				t.Fatalf("path %q missing .tar.gz", u.Path)
			}
			if u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Opaque != "" {
				t.Fatalf("unexpected fields: %+v", u)
			}
		})
	}
}

func TestParseArchiveURLRejects(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", urlMaxBaseBytes) + ".tar.gz"
	cases := []struct {
		name      string
		raw       string
		allowHTTP bool
		substr    string
	}{
		{"empty", "", false, "empty"},
		{"too long", long, false, "exceeds"},
		{"whitespace", " https://example.com/a.tar.gz", false, "whitespace"},
		{"relative", "/a.tar.gz", false, "absolute"},
		{"no scheme", "example.com/a.tar.gz", false, "absolute"},
		{"ftp", "ftp://example.com/a.tar.gz", false, "scheme"},
		{"http without flag", "http://example.com/a.tar.gz", false, "allowHTTP"},
		{"userinfo", "https://user:pass@example.com/a.tar.gz", false, "userinfo"},
		{"query", "https://example.com/a.tar.gz?x=1", false, "query"},
		{"fragment", "https://example.com/a.tar.gz#frag", false, "fragment"},
		{"empty query mark", "https://example.com/a.tar.gz?", false, "query"},
		{"missing suffix", "https://example.com/a.tgz", false, ".tar.gz"},
		{"wrong case suffix", "https://example.com/a.TAR.GZ", false, ".tar.gz"},
		{"trailing slash", "https://example.com/a.tar.gz/", false, ".tar.gz"},
		{"no path", "https://example.com", false, "path"},
		{"dotdot path", "https://example.com/a/../b.tar.gz", false, "clean"},
		{"backslash path", "https://example.com/a\\b.tar.gz", false, "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArchiveURL(tc.raw, tc.allowHTTP)
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.substr != "" && !strings.Contains(err.Error(), tc.substr) {
				t.Fatalf("err=%v want substr %q", err, tc.substr)
			}
		})
	}
}

func TestIsRemoteArchiveRef(t *testing.T) {
	if !isRemoteArchiveRef("https://x/a.tar.gz") || !isRemoteArchiveRef("http://x/a.tar.gz") {
		t.Fatal("expected remote")
	}
	if isRemoteArchiveRef("/tmp/a.tar.gz") || isRemoteArchiveRef("HTTP://x/a.tar.gz") || isRemoteArchiveRef("") {
		t.Fatal("expected local/non-remote")
	}
}

func TestNewArchiveSourceRejectsRemoteURL(t *testing.T) {
	_, err := newArchiveSource("https://example.com/a.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "remote") {
		t.Fatalf("err=%v", err)
	}
}

func TestArchiveURLSourceInstallSuccess(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "archive-url-ok", "1.0.0")
	reg := registryFromTrusted(t, tc)

	raw := gzipBytes(t, craftUSTARBytes(t, catalogFilesWithDirs(files)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/web/prepared.tar.gz" {
			http.NotFound(w, r)
			return
		}
		if ae := r.Header.Get("Accept-Encoding"); ae != "identity" {
			t.Errorf("Accept-Encoding=%q", ae)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(raw)))
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	host, port := mustSplitHostPort(t, srv.Listener.Addr().String())
	ip := netip.MustParseAddr(host)
	archiveURL := "http://files.example.test:" + port + "/web/prepared.tar.gz"

	var resolveCalls, dialCalls atomic.Int32
	udeps := urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			resolveCalls.Add(1)
			if h != "files.example.test" {
				t.Errorf("resolve host=%q", h)
			}
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialCalls.Add(1)
			// Must dial literal IP, not hostname.
			h, p, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if h != ip.String() {
				t.Errorf("dial host=%q want literal %s", h, ip)
			}
			if p != port {
				t.Errorf("dial port=%q want %s", p, port)
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
	}

	assets := t.TempDir()
	res, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot:      assets,
		CatalogID:       "archive-url-ok",
		FromArchive:     archiveURL,
		AllowHTTPURL:    true,
		AllowPrivateURL: true,
	}, reg, installDeps{}, udeps)
	if err != nil {
		t.Fatal(err)
	}
	if res.CatalogSHA256 != tc.Digest {
		t.Fatalf("%+v", res)
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolve=%d want 1", resolveCalls.Load())
	}
	if dialCalls.Load() < 1 {
		t.Fatalf("dial=%d", dialCalls.Load())
	}

	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: assets}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}
}

func TestArchiveURLSourcePrivateIPDenied(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "archive-url-priv", "1.0.0")
	reg := registryFromTrusted(t, tc)

	// No server needed: resolve returns private IP and must fail before dial.
	var dialCalls atomic.Int32
	udeps := urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, fmt.Errorf("dial should not run")
		},
	}

	_, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot:  t.TempDir(),
		CatalogID:   "archive-url-priv",
		FromArchive: "https://files.example.test/web.tar.gz",
	}, reg, installDeps{}, udeps)
	if err == nil {
		t.Fatal("expected private IP denial")
	}
	if !strings.Contains(err.Error(), "allowPrivate") && !strings.Contains(err.Error(), "private") {
		t.Fatalf("err=%v", err)
	}
	if dialCalls.Load() != 0 {
		t.Fatalf("dial must not run, got %d", dialCalls.Load())
	}
}

func TestArchiveURLSourceContentLengthTooLarge(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "archive-url-clen", "1.0.0")
	reg := registryFromTrusted(t, tc)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Claim an oversized body without sending it.
		w.Header().Set("Content-Length", fmt.Sprintf("%d", maxArchiveCompressedBytes+1))
	}))
	defer srv.Close()

	host, port := mustSplitHostPort(t, srv.Listener.Addr().String())
	ip := netip.MustParseAddr(host)
	archiveURL := "http://files.example.test:" + port + "/big.tar.gz"

	udeps := urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			return []netip.Addr{ip}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		},
	}

	_, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot:      t.TempDir(),
		CatalogID:       "archive-url-clen",
		FromArchive:     archiveURL,
		AllowHTTPURL:    true,
		AllowPrivateURL: true,
	}, reg, installDeps{}, udeps)
	if err == nil {
		t.Fatal("expected Content-Length rejection")
	}
	if !strings.Contains(err.Error(), "Content-Length") {
		t.Fatalf("err=%v", err)
	}
}

func TestArchiveURLSourceLegalGateBeforeNetwork(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "assets-not-created")
	var resolveCalls atomic.Int32
	udeps := urlSourceDeps{
		Resolve: func(ctx context.Context, h string) ([]netip.Addr, error) {
			resolveCalls.Add(1)
			return nil, fmt.Errorf("resolve must not run")
		},
	}

	reg, err := newCatalogRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot:  missingRoot,
		CatalogID:   "unknown-remote-archive-catalog",
		FromArchive: "https://example.invalid/web.tar.gz",
	}, reg, installDeps{}, udeps)
	if !errors.Is(err, ErrCatalogLegalGate) {
		t.Fatalf("err=%v want ErrCatalogLegalGate", err)
	}
	if resolveCalls.Load() != 0 {
		t.Fatal("resolver must not be called before legal gate")
	}
	if _, statErr := os.Lstat(missingRoot); !os.IsNotExist(statErr) {
		t.Fatal("assets root must remain absent")
	}
}

func TestInstallPublicLegalGateRemoteArchiveNoNetwork(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "does-not-exist-yet")
	_, err := Install(context.Background(), InstallOptions{
		AssetsRoot:  missingRoot,
		CatalogID:   "nonexistent-catalog-id",
		FromArchive: "https://example.invalid/web.tar.gz",
	})
	if !errors.Is(err, ErrCatalogLegalGate) {
		t.Fatalf("err=%v want ErrCatalogLegalGate", err)
	}
	if _, statErr := os.Lstat(missingRoot); !os.IsNotExist(statErr) {
		t.Fatal("assets root should be untouched")
	}
}
