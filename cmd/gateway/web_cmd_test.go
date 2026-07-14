package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxxbrian/emby-auth-gateway/internal/embyweb"
)

func TestWebInstallOptionValidation(t *testing.T) {
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "")

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing_assets",
			args: []string{"install", "--catalog-id", "x", "--from-dir", "/tmp/src"},
			want: "--assets-dir",
		},
		{
			name: "missing_catalog",
			args: []string{"install", "--assets-dir", t.TempDir(), "--from-dir", "/tmp/src"},
			want: "--catalog-id",
		},
		{
			name: "no_source",
			args: []string{"install", "--assets-dir", t.TempDir(), "--catalog-id", "x"},
			want: "exactly one",
		},
		{
			name: "two_sources",
			args: []string{
				"install",
				"--assets-dir", t.TempDir(),
				"--catalog-id", "x",
				"--from-dir", "/tmp/a",
				"--from-archive", "/tmp/b.tar.gz",
			},
			want: "exactly one",
		},
		{
			name: "three_sources",
			args: []string{
				"install",
				"--assets-dir", t.TempDir(),
				"--catalog-id", "x",
				"--from-dir", "/tmp/a",
				"--from-archive", "/tmp/b.tar.gz",
				"--from-url", "https://example.test/",
			},
			want: "exactly one",
		},
		{
			name: "http_flag_without_url",
			args: []string{
				"install",
				"--assets-dir", t.TempDir(),
				"--catalog-id", "x",
				"--from-dir", "/tmp/a",
				"--allow-http-url",
			},
			want: "--allow-http-url",
		},
		{
			name: "private_flag_without_url",
			args: []string{
				"install",
				"--assets-dir", t.TempDir(),
				"--catalog-id", "x",
				"--from-archive", "/tmp/a.tar.gz",
				"--allow-private-url",
			},
			want: "--allow-private-url",
		},
		{
			name: "private_flag_with_remote_archive_ok_then_legal_gate",
			args: []string{
				"install",
				"--assets-dir", t.TempDir(),
				"--catalog-id", "x",
				"--from-archive", "https://cdn.example.test/web.tar.gz",
				"--allow-private-url",
			},
			// Options accepted; production catalog legal gate fires.
			want: "catalog",
		},
		{
			name: "extra_args",
			args: []string{
				"install",
				"--assets-dir", t.TempDir(),
				"--catalog-id", "x",
				"--from-dir", "/tmp/a",
				"extra",
			},
			want: "unknown command",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newWebCommand()
			var out, errBuf bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errBuf)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error() + errBuf.String()
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("err=%q want substring %q", msg, tc.want)
			}
		})
	}
}

func TestWebInstallLegalGateNoSideEffects(t *testing.T) {
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "")

	assetsRoot := filepath.Join(t.TempDir(), "assets-not-created")
	srcDir := filepath.Join(t.TempDir(), "src-not-created")

	cmd := newWebCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{
		"install",
		"--assets-dir", assetsRoot,
		"--catalog-id", "any-production-id",
		"--from-dir", srcDir,
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected legal-gate error")
	}
	if !errors.Is(err, embyweb.ErrCatalogLegalGate) {
		t.Fatalf("err=%v want ErrCatalogLegalGate", err)
	}
	if !strings.Contains(err.Error(), "any-production-id") {
		t.Fatalf("err should mention catalog id: %v", err)
	}
	if _, statErr := os.Lstat(assetsRoot); !os.IsNotExist(statErr) {
		t.Fatalf("assets root must remain absent: %v", statErr)
	}
	// Parent of assets root must not gain install.lock or other install artifacts.
	parentEntries, _ := os.ReadDir(filepath.Dir(assetsRoot))
	for _, e := range parentEntries {
		if strings.Contains(e.Name(), "install") || strings.Contains(e.Name(), "lock") {
			t.Fatalf("unexpected install artifact %q under parent", e.Name())
		}
	}

	// URL mode also gates before any network/root creation.
	cmd = newWebCommand()
	out.Reset()
	errBuf.Reset()
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{
		"install",
		"--assets-dir", assetsRoot,
		"--catalog-id", "url-catalog-id",
		"--from-url", "https://example.invalid/",
	})
	err = cmd.Execute()
	if !errors.Is(err, embyweb.ErrCatalogLegalGate) {
		t.Fatalf("url mode err=%v", err)
	}
	if _, statErr := os.Lstat(assetsRoot); !os.IsNotExist(statErr) {
		t.Fatal("assets root created despite legal gate (url)")
	}
}

func TestWebInstallAssetsDirFromEnv(t *testing.T) {
	assetsRoot := filepath.Join(t.TempDir(), "from-env")
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "  "+assetsRoot+"  ")

	cmd := newWebCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"install",
		"--catalog-id", "env-catalog",
		"--from-dir", filepath.Join(t.TempDir(), "src"),
	})
	err := cmd.Execute()
	if !errors.Is(err, embyweb.ErrCatalogLegalGate) {
		t.Fatalf("err=%v want legal gate (proves options accepted)", err)
	}
	if _, statErr := os.Lstat(assetsRoot); !os.IsNotExist(statErr) {
		t.Fatal("env assets root must remain absent after legal gate")
	}
}

func TestWebStatusOutputsAndExit(t *testing.T) {
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "")

	t.Run("disabled_blank", func(t *testing.T) {
		cmd := newWebCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"status"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("disabled should exit nonzero")
		}
		text := out.String()
		if !strings.Contains(text, "state: disabled") {
			t.Fatalf("output=%q", text)
		}
		if !strings.Contains(text, "verified: false") {
			t.Fatalf("output=%q", text)
		}
		if strings.Contains(text, "release:") || strings.Contains(text, "catalog_sha256:") {
			t.Fatalf("disabled should omit identity fields: %q", text)
		}
	})

	t.Run("missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent")
		cmd := newWebCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"status", "--assets-dir", missing})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("missing should exit nonzero")
		}
		if !strings.Contains(out.String(), "state: missing") {
			t.Fatalf("output=%q", out.String())
		}
	})

	t.Run("corrupt_untrusted", func(t *testing.T) {
		root := writeUntrustedWebAssets(t)
		cmd := newWebCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"status", "--assets-dir", root})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("untrusted should exit nonzero")
		}
		text := out.String()
		if !strings.Contains(text, "state: corrupt") {
			t.Fatalf("output=%q", text)
		}
		if !strings.Contains(text, "verified: false") {
			t.Fatalf("output=%q", text)
		}
		if !strings.Contains(text, "release:") || !strings.Contains(text, "catalog_sha256:") {
			t.Fatalf("corrupt with pointer should print identity: %q", text)
		}
		if !strings.Contains(text, "error:") {
			t.Fatalf("expected error line: %q", text)
		}
	})

	t.Run("verify_corrupt", func(t *testing.T) {
		root := writeUntrustedWebAssets(t)
		cmd := newWebCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"status", "--assets-dir", root, "--verify"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("verify untrusted should exit nonzero")
		}
		if !strings.Contains(out.String(), "state: corrupt") {
			t.Fatalf("output=%q", out.String())
		}
	})

	t.Run("status_extra_args", func(t *testing.T) {
		cmd := newWebCommand()
		var errBuf bytes.Buffer
		cmd.SetErr(&errBuf)
		cmd.SetArgs([]string{"status", "extra"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("status_from_env", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "env-missing")
		t.Setenv("GATEWAY_WEB_ASSETS_DIR", missing)
		cmd := newWebCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"status"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("expected nonzero")
		}
		if !strings.Contains(out.String(), "state: missing") {
			t.Fatalf("output=%q", out.String())
		}
	})
}

func TestWriteInstallationStatusDeterministic(t *testing.T) {
	var buf bytes.Buffer
	st := embyweb.InstallationStatus{
		State:         embyweb.InstallStateCorrupt,
		Verified:      false,
		Release:       "1.0.0-deadbeef",
		CatalogSHA256: "abc",
		Err:           errors.New("untrusted"),
	}
	if err := writeInstallationStatus(&buf, st); err != nil {
		t.Fatal(err)
	}
	want := "state: corrupt\nverified: false\nrelease: 1.0.0-deadbeef\ncatalog_sha256: abc\nerror: untrusted\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

func TestInstallationStatusExitError(t *testing.T) {
	if err := installationStatusExitError(embyweb.InstallationStatus{State: embyweb.InstallStateReady}); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := installationStatusExitError(embyweb.InstallationStatus{State: embyweb.InstallStateInstalled}); err != nil {
		t.Fatalf("installed: %v", err)
	}
	if err := installationStatusExitError(embyweb.InstallationStatus{State: embyweb.InstallStateDisabled}); err == nil {
		t.Fatal("disabled should error")
	}
	if err := installationStatusExitError(embyweb.InstallationStatus{
		State: embyweb.InstallStateMissing,
		Err:   errors.New("gone"),
	}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("missing wrap: %v", err)
	}
}

func TestWriteInstallResult(t *testing.T) {
	var buf bytes.Buffer
	if err := writeInstallResult(&buf, embyweb.InstallResult{
		Release:       "rel",
		CatalogSHA256: "digest",
		Reactivated:   true,
	}); err != nil {
		t.Fatal(err)
	}
	want := "status: installed\nrelease: rel\ncatalog_sha256: digest\nreactivated: true\n"
	if buf.String() != want {
		t.Fatalf("got %q want %q", buf.String(), want)
	}
}

func TestWebCommandHasNoRegistryInjectionFlags(t *testing.T) {
	cmd := newWebCommand()
	// Walk install/init flags; none may expose test-only registry/catalog injection.
	forbidden := []string{
		"catalog-file", "registry", "catalog-bytes", "digest-override",
		"trusted-catalog", "inject-registry", "catalog-sha256",
	}
	for _, sub := range []string{"install", "init", "status"} {
		c, _, err := cmd.Find([]string{sub})
		if err != nil {
			t.Fatalf("find %s: %v", sub, err)
		}
		for _, name := range forbidden {
			if c.Flags().Lookup(name) != nil {
				t.Fatalf("forbidden %s flag present: %s", sub, name)
			}
		}
	}
}

func TestWebInitOptionValidation(t *testing.T) {
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "")
	assets := t.TempDir()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing_assets",
			args: []string{"init", "--catalog-id", "x", "--source-kind", "dir", "--source", "/tmp/src"},
			want: "--assets-dir",
		},
		{
			name: "missing_catalog",
			args: []string{"init", "--assets-dir", assets, "--source-kind", "dir", "--source", "/tmp/src"},
			want: "--catalog-id",
		},
		{
			name: "missing_source_kind",
			args: []string{"init", "--assets-dir", assets, "--catalog-id", "x", "--source", "/tmp/src"},
			want: "--source-kind",
		},
		{
			name: "missing_source",
			args: []string{"init", "--assets-dir", assets, "--catalog-id", "x", "--source-kind", "dir"},
			want: "--source",
		},
		{
			name: "bad_source_kind",
			args: []string{"init", "--assets-dir", assets, "--catalog-id", "x", "--source-kind", "tarball", "--source", "/tmp/x"},
			want: "dir, archive, or url",
		},
		{
			name: "http_flag_with_dir",
			args: []string{
				"init", "--assets-dir", assets, "--catalog-id", "x",
				"--source-kind", "dir", "--source", "/tmp/a", "--allow-http-url",
			},
			want: "--allow-http-url",
		},
		{
			name: "private_flag_with_archive",
			args: []string{
				"init", "--assets-dir", assets, "--catalog-id", "x",
				"--source-kind", "archive", "--source", "/tmp/a.tar.gz", "--allow-private-url",
			},
			want: "--allow-private-url",
		},
		{
			name: "extra_args",
			args: []string{
				"init", "--assets-dir", assets, "--catalog-id", "x",
				"--source-kind", "dir", "--source", "/tmp/a", "extra",
			},
			want: "unknown command",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newWebCommand()
			var out, errBuf bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errBuf)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error() + errBuf.String()
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("err=%q want substring %q", msg, tc.want)
			}
		})
	}
}

func TestWebInitSourceKindMapping(t *testing.T) {
	assets := "/assets/root"
	src := "/prepared/source"
	url := "https://assets.example.test/tree/"

	cases := []struct {
		name string
		f    webInitFlags
		want embyweb.InstallOptions
	}{
		{
			name: "dir",
			f: webInitFlags{
				AssetsDir: assets, CatalogID: "cat",
				SourceKind: "dir", Source: src,
			},
			want: embyweb.InstallOptions{
				AssetsRoot: assets, CatalogID: "cat", FromDir: src,
			},
		},
		{
			name: "archive",
			f: webInitFlags{
				AssetsDir: assets, CatalogID: "cat",
				SourceKind: "archive", Source: src + ".tar.gz",
			},
			want: embyweb.InstallOptions{
				AssetsRoot: assets, CatalogID: "cat", FromArchive: src + ".tar.gz",
			},
		},
		{
			name: "archive_remote_url",
			f: webInitFlags{
				AssetsDir: assets, CatalogID: "cat",
				SourceKind: "archive", Source: "https://cdn.example.test/web.tar.gz",
				AllowHTTPURL: true, AllowPrivateURL: true,
			},
			want: embyweb.InstallOptions{
				AssetsRoot: assets, CatalogID: "cat",
				FromArchive:     "https://cdn.example.test/web.tar.gz",
				AllowHTTPURL:    true,
				AllowPrivateURL: true,
			},
		},
		{
			name: "url",
			f: webInitFlags{
				AssetsDir: assets, CatalogID: "cat",
				SourceKind: "url", Source: url,
				AllowHTTPURL: true, AllowPrivateURL: true,
			},
			want: embyweb.InstallOptions{
				AssetsRoot: assets, CatalogID: "cat", FromURL: url,
				AllowHTTPURL: true, AllowPrivateURL: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.f.toInstallOptions()
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestWebInitMetacharactersLiteral(t *testing.T) {
	// Source values must be treated as literal paths/URLs (no shell parsing).
	literal := `/tmp/prepared; rm -rf / $(whoami) | tee & "quotes" 'x'`
	f := webInitFlags{
		AssetsDir:  "/assets",
		CatalogID:  "cat",
		SourceKind: "dir",
		Source:     literal,
	}
	got, err := f.toInstallOptions()
	if err != nil {
		t.Fatal(err)
	}
	if got.FromDir != literal {
		t.Fatalf("FromDir=%q want exact literal", got.FromDir)
	}
	if got.FromArchive != "" || got.FromURL != "" {
		t.Fatalf("unexpected other sources: %+v", got)
	}

	urlLit := `https://example.test/tree/;$(id)/`
	f = webInitFlags{
		AssetsDir:  "/assets",
		CatalogID:  "cat",
		SourceKind: "url",
		Source:     urlLit,
	}
	got, err = f.toInstallOptions()
	if err != nil {
		t.Fatal(err)
	}
	if got.FromURL != urlLit {
		t.Fatalf("FromURL=%q want exact literal", got.FromURL)
	}
}

func TestWebInitAssetsDirFromEnv(t *testing.T) {
	assetsRoot := filepath.Join(t.TempDir(), "from-env-init")
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "  "+assetsRoot+"  ")

	cmd := newWebCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{
		"init",
		"--catalog-id", "env-catalog",
		"--source-kind", "dir",
		"--source", filepath.Join(t.TempDir(), "src"),
	})
	err := cmd.Execute()
	if !errors.Is(err, embyweb.ErrCatalogLegalGate) {
		t.Fatalf("err=%v want legal gate (proves options accepted)", err)
	}
	if _, statErr := os.Lstat(assetsRoot); !os.IsNotExist(statErr) {
		t.Fatal("env assets root must remain absent after legal gate")
	}
}

func TestWebInitLegalGateNoSideEffects(t *testing.T) {
	t.Setenv("GATEWAY_WEB_ASSETS_DIR", "")

	assetsRoot := filepath.Join(t.TempDir(), "assets-not-created-init")
	srcDir := filepath.Join(t.TempDir(), "src-not-created-init")

	cmd := newWebCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{
		"init",
		"--assets-dir", assetsRoot,
		"--catalog-id", "any-production-id",
		"--source-kind", "dir",
		"--source", srcDir,
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected legal-gate error")
	}
	if !errors.Is(err, embyweb.ErrCatalogLegalGate) {
		t.Fatalf("err=%v want ErrCatalogLegalGate", err)
	}
	if !strings.Contains(err.Error(), "any-production-id") {
		t.Fatalf("err should mention catalog id: %v", err)
	}
	if _, statErr := os.Lstat(assetsRoot); !os.IsNotExist(statErr) {
		t.Fatalf("assets root must remain absent: %v", statErr)
	}
	parentEntries, _ := os.ReadDir(filepath.Dir(assetsRoot))
	for _, e := range parentEntries {
		if strings.Contains(e.Name(), "install") || strings.Contains(e.Name(), "lock") {
			t.Fatalf("unexpected install artifact %q under parent", e.Name())
		}
	}

	// URL kind also gates before network/root creation.
	cmd = newWebCommand()
	out.Reset()
	errBuf.Reset()
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs([]string{
		"init",
		"--assets-dir", assetsRoot,
		"--catalog-id", "url-catalog-id",
		"--source-kind", "url",
		"--source", "https://example.invalid/",
	})
	err = cmd.Execute()
	if !errors.Is(err, embyweb.ErrCatalogLegalGate) {
		t.Fatalf("url mode err=%v", err)
	}
	if _, statErr := os.Lstat(assetsRoot); !os.IsNotExist(statErr) {
		t.Fatal("assets root created despite legal gate (url)")
	}
}
