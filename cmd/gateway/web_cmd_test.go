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
	// Walk install flags; none may expose test-only registry/catalog injection.
	install, _, err := cmd.Find([]string{"install"})
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"catalog-file", "registry", "catalog-bytes", "digest-override",
		"trusted-catalog", "inject-registry", "catalog-sha256",
	}
	for _, name := range forbidden {
		if install.Flags().Lookup(name) != nil {
			t.Fatalf("forbidden flag present: %s", name)
		}
	}
	status, _, err := cmd.Find([]string{"status"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range forbidden {
		if status.Flags().Lookup(name) != nil {
			t.Fatalf("forbidden status flag: %s", name)
		}
	}
}
