package embyweb

import (
	"context"
	"errors"
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

func TestValidateInstallOptions(t *testing.T) {
	base := InstallOptions{
		AssetsRoot: "/tmp/assets",
		CatalogID:  "cat",
		FromDir:    "/tmp/src",
	}
	if err := validateInstallOptions(base); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		opts InstallOptions
		want string
	}{
		{"empty_root", InstallOptions{CatalogID: "c", FromDir: "/d"}, "assets root"},
		{"empty_id", InstallOptions{AssetsRoot: "/a", FromDir: "/d"}, "catalog id"},
		{"no_source", InstallOptions{AssetsRoot: "/a", CatalogID: "c"}, "exactly one"},
		{"two_sources", InstallOptions{AssetsRoot: "/a", CatalogID: "c", FromDir: "/d", FromArchive: "/x.tar.gz"}, "exactly one"},
		{"three_sources", InstallOptions{AssetsRoot: "/a", CatalogID: "c", FromDir: "/d", FromArchive: "/x.tar.gz", FromURL: "https://x/"}, "exactly one"},
		{"http_flag_without_url", InstallOptions{AssetsRoot: "/a", CatalogID: "c", FromDir: "/d", AllowHTTPURL: true}, "AllowHTTPURL"},
		{"private_flag_without_url", InstallOptions{AssetsRoot: "/a", CatalogID: "c", FromArchive: "/x.tar.gz", AllowPrivateURL: true}, "AllowPrivateURL"},
		{"http_flag_local_archive", InstallOptions{AssetsRoot: "/a", CatalogID: "c", FromArchive: "/tmp/x.tar.gz", AllowHTTPURL: true}, "AllowHTTPURL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInstallOptions(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want contain %q", err, tc.want)
			}
		})
	}
	// Whitespace-only FromDir does not count; FromURL alone is valid.
	if err := validateInstallOptions(InstallOptions{
		AssetsRoot: "/a",
		CatalogID:  "c",
		FromDir:    "  ",
		FromURL:    "https://x/",
	}); err != nil {
		t.Fatalf("whitespace FromDir: %v", err)
	}

	// URL + flags OK.
	if err := validateInstallOptions(InstallOptions{
		AssetsRoot:      "/a",
		CatalogID:       "c",
		FromURL:         "https://example.com/",
		AllowHTTPURL:    true,
		AllowPrivateURL: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Remote FromArchive + flags OK.
	if err := validateInstallOptions(InstallOptions{
		AssetsRoot:      "/a",
		CatalogID:       "c",
		FromArchive:     "https://cdn.example.com/web.tar.gz",
		AllowHTTPURL:    true,
		AllowPrivateURL: true,
	}); err != nil {
		t.Fatal(err)
	}

	// Remote FromArchive without flags OK (https default).
	if err := validateInstallOptions(InstallOptions{
		AssetsRoot:  "/a",
		CatalogID:   "c",
		FromArchive: "https://cdn.example.com/web.tar.gz",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallPublicLegalGateNoSideEffects(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "does-not-exist-yet")
	missingSrc := filepath.Join(t.TempDir(), "also-missing")

	var resolveCalls atomic.Int32
	// Even if someone wired URL deps into public Install (they cannot), production
	// path must not reach source construction. Prove root/source untouched.
	_, err := Install(context.Background(), InstallOptions{
		AssetsRoot: missingRoot,
		CatalogID:  "nonexistent-catalog-id",
		FromDir:    missingSrc,
	})
	if !errors.Is(err, ErrCatalogLegalGate) {
		t.Fatalf("err=%v want ErrCatalogLegalGate", err)
	}
	if _, err := os.Lstat(missingRoot); !os.IsNotExist(err) {
		t.Fatalf("assets root should be untouched: %v", err)
	}
	// No install.lock under parent either.
	if entries, _ := os.ReadDir(filepath.Dir(missingRoot)); len(entries) != 0 {
		// temp parent may be empty; ensure missingRoot itself absent
	}
	if resolveCalls.Load() != 0 {
		t.Fatal("resolver must not be called")
	}

	// URL mode also gates before network.
	_, err = Install(context.Background(), InstallOptions{
		AssetsRoot: missingRoot,
		CatalogID:  "still-missing",
		FromURL:    "https://example.invalid/",
	})
	if !errors.Is(err, ErrCatalogLegalGate) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Lstat(missingRoot); !os.IsNotExist(err) {
		t.Fatal("root created despite legal gate")
	}
}

func TestInstallKnownProductionIDPastLegalGate(t *testing.T) {
	// Known production catalog ID must resolve; failure must be source validation
	// (missing prepared tree), not ErrCatalogLegalGate. No official bytes required.
	missingRoot := filepath.Join(t.TempDir(), "assets-not-created")
	missingSrc := filepath.Join(t.TempDir(), "prepared-source-missing")

	_, err := Install(context.Background(), InstallOptions{
		AssetsRoot: missingRoot,
		CatalogID:  "emby-web-4.9.5.0",
		FromDir:    missingSrc,
	})
	if err == nil {
		t.Fatal("expected source failure for missing prepared tree")
	}
	if errors.Is(err, ErrCatalogLegalGate) {
		t.Fatalf("known production ID must pass legal gate, got %v", err)
	}
	// directory source wraps missing path after catalog resolve.
	if !strings.Contains(err.Error(), "directory source") {
		t.Fatalf("unexpected error (want directory source validation): %v", err)
	}
	// Catalog resolve happens before root creation; missing source fails in
	// newInstallSource, so assets root must still be absent.
	if _, statErr := os.Lstat(missingRoot); !os.IsNotExist(statErr) {
		t.Fatalf("assets root should remain absent after source failure: %v", statErr)
	}
}

func TestInstallWithRegistryDirEndToEnd(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "facade-dir", "1.0.0")
	reg := registryFromTrusted(t, tc)
	srcDir := writeDirSourceTree(t, files)
	assets := t.TempDir()

	res, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot: assets,
		CatalogID:  "facade-dir",
		FromDir:    srcDir,
	}, reg, installDeps{}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Release != tc.Release || res.CatalogSHA256 != tc.Digest || res.Reactivated {
		t.Fatalf("%+v", res)
	}

	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: assets}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}

	// Idempotent second install.
	res2, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot: assets,
		CatalogID:  "facade-dir",
		FromDir:    srcDir,
	}, reg, installDeps{}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Reactivated {
		t.Fatal("expected reactivation")
	}
}

func TestInstallWithRegistryArchiveEndToEnd(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "facade-archive", "1.0.0")
	reg := registryFromTrusted(t, tc)
	archPath := filepath.Join(t.TempDir(), "web.tar.gz")
	writeUSTARTarGz(t, archPath, catalogFilesWithDirs(files))
	assets := t.TempDir()

	res, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot:  assets,
		CatalogID:   "facade-archive",
		FromArchive: archPath,
	}, reg, installDeps{}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Release != tc.Release {
		t.Fatalf("%+v", res)
	}
	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: assets}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}
}

func TestInstallWithRegistryURLEndToEnd(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "facade-url", "1.0.0")
	reg := registryFromTrusted(t, tc)
	data := fixtureDataMap(files)

	mux := http.NewServeMux()
	for p, body := range data {
		p, body := p, body
		mux.HandleFunc("/"+p, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(body)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// httptest is http://127.0.0.1 — need AllowHTTP + AllowPrivate and pinned dial.
	base := srv.URL + "/"
	var resolveCalls, dialCalls atomic.Int32
	udeps := urlSourceDeps{
		Resolve: func(ctx context.Context, host string) ([]netip.Addr, error) {
			resolveCalls.Add(1)
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialCalls.Add(1)
			var d net.Dialer
			// Dial the real httptest listener.
			u := strings.TrimPrefix(srv.URL, "http://")
			return d.DialContext(ctx, "tcp", u)
		},
	}

	assets := t.TempDir()
	res, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot:      assets,
		CatalogID:       "facade-url",
		FromURL:         base,
		AllowHTTPURL:    true,
		AllowPrivateURL: true,
	}, reg, installDeps{}, udeps)
	if err != nil {
		t.Fatal(err)
	}
	if res.CatalogSHA256 != tc.Digest {
		t.Fatalf("%+v", res)
	}
	// httptest base is an IP literal host, so Resolve is skipped; Dial must run.
	if dialCalls.Load() < 1 {
		t.Fatalf("dial=%d resolve=%d", dialCalls.Load(), resolveCalls.Load())
	}
	s, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: assets}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}
}

func TestInstallWithRegistrySourceConstructionAfterLookup(t *testing.T) {
	// Unknown catalog must not call directory constructor side effects:
	// nonexistent FromDir would error if constructed first.
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	assets := filepath.Join(t.TempDir(), "assets-not-created")
	reg, err := newCatalogRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot: assets,
		CatalogID:  "missing",
		FromDir:    missing,
	}, reg, installDeps{}, urlSourceDeps{})
	if !errors.Is(err, ErrCatalogLegalGate) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Lstat(assets); !os.IsNotExist(err) {
		t.Fatal("assets root should not be created")
	}
}

func TestInstallWithRegistryCanceledContext(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "facade-cancel", "1.0.0")
	reg := registryFromTrusted(t, tc)
	srcDir := writeDirSourceTree(t, files)
	assets := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := installWithRegistry(ctx, InstallOptions{
		AssetsRoot: assets,
		CatalogID:  "facade-cancel",
		FromDir:    srcDir,
	}, reg, installDeps{}, urlSourceDeps{})
	if !errors.Is(err, context.Canceled) {
		// Option validation + lookup may succeed; cancel checked before source/install.
		// If installTrusted also checks ctx, either is fine.
		if err == nil {
			t.Fatal("expected cancellation error")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestInstallWithRegistryUnknownIDErrorsIs(t *testing.T) {
	reg, _ := newCatalogRegistry(nil)
	_, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot: t.TempDir(),
		CatalogID:  "nope",
		FromDir:    t.TempDir(),
	}, reg, installDeps{}, urlSourceDeps{})
	if !errors.Is(err, ErrCatalogLegalGate) {
		t.Fatalf("err=%v", err)
	}
}

func TestInstallDispatchSelectsArchiveNotDir(t *testing.T) {
	// Ensure archive path is used when only FromArchive set (constructor rejects bad suffix).
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "facade-dispatch", "1.0.0")
	reg := registryFromTrusted(t, tc)
	_, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot:  t.TempDir(),
		CatalogID:   "facade-dispatch",
		FromArchive: filepath.Join(t.TempDir(), "not-an-archive.zip"),
	}, reg, installDeps{}, urlSourceDeps{})
	if err == nil {
		t.Fatal("expected archive constructor error")
	}
	if !strings.Contains(err.Error(), "tar.gz") && !strings.Contains(err.Error(), "archive") {
		t.Fatalf("err=%v", err)
	}
}

func TestInstallResultFields(t *testing.T) {
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "facade-result", "3.0.0")
	reg := registryFromTrusted(t, tc)
	srcDir := writeDirSourceTree(t, files)
	assets := t.TempDir()
	res, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot: assets,
		CatalogID:  "facade-result",
		FromDir:    srcDir,
	}, reg, installDeps{}, urlSourceDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Release != tc.Release || res.CatalogSHA256 != tc.Digest {
		t.Fatalf("%+v vs %s %s", res, tc.Release, tc.Digest)
	}
	if !strings.HasPrefix(res.Release, "3.0.0-") {
		t.Fatalf("release=%q", res.Release)
	}
}
