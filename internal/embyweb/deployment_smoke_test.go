package embyweb

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const deploymentSmokeTimeout = 90 * time.Second

// TestSmokeHeaderParserSelftest exercises scripts/smoke.sh header parsing
// fixtures (final-block selection, duplicates, empty-first, CRLF multi-Vary)
// without network I/O.
func TestSmokeHeaderParserSelftest(t *testing.T) {
	requireBash(t)
	smokePath := smokeScriptPath(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", smokePath)
	cmd.Env = smokeCleanEnv(
		"SMOKE_HEADER_SELFTEST=1",
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("header selftest timed out:\n%s", out)
	}
	if err != nil {
		t.Fatalf("header selftest failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "smoke header selftest passed") {
		t.Fatalf("header selftest missing success marker:\n%s", out)
	}
	t.Logf("header selftest output:\n%s", out)
}

// TestSyntheticReadyDeploymentSmoke is the hermetic CI deployment smoke:
// install a synthetic trusted catalog into a new assets root, serve it with the
// real package handler + GuardHandler, and drive scripts/smoke.sh in
// SMOKE_WEB=ready SMOKE_WEB_ONLY=1 mode. Production registry stays empty;
// package-private registry injection is test-only.
func TestSyntheticReadyDeploymentSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("deployment smoke invokes scripts/smoke.sh")
	}
	requireBash(t)
	requireCurl(t)

	smokePath := smokeScriptPath(t)
	files := readyMinimalFiles()
	tc := buildSyntheticCatalog(t, files, "deploy-smoke", "1.0.0")
	reg := registryFromTrusted(t, tc)
	srcDir := writeDirSourceTree(t, files)

	// Install into a path that does not exist yet (installer creates the root).
	assetsRoot := filepath.Join(t.TempDir(), "web_assets")
	if _, err := os.Lstat(assetsRoot); !os.IsNotExist(err) {
		t.Fatalf("assets root must start absent: %v", err)
	}

	res, err := installWithRegistry(context.Background(), InstallOptions{
		AssetsRoot: assetsRoot,
		CatalogID:  "deploy-smoke",
		FromDir:    srcDir,
	}, reg, installDeps{}, urlSourceDeps{})
	if err != nil {
		t.Fatalf("installWithRegistry: %v", err)
	}
	if res.Release != tc.Release || res.CatalogSHA256 != tc.Digest {
		t.Fatalf("install result %+v want release=%s digest=%s", res, tc.Release, tc.Digest)
	}
	if res.Reactivated {
		t.Fatal("first install must not report reactivated")
	}

	// Layout: current.json, release install.json + files, no abandoned staging.
	if st, err := os.Stat(filepath.Join(assetsRoot, "current.json")); err != nil || st.IsDir() {
		t.Fatalf("current.json: st=%v err=%v", st, err)
	}
	relDir := filepath.Join(assetsRoot, "releases", tc.Release)
	if st, err := os.Stat(filepath.Join(relDir, "install.json")); err != nil || st.IsDir() {
		t.Fatalf("install.json: st=%v err=%v", st, err)
	}
	for _, f := range files {
		p := filepath.Join(relDir, "files", filepath.FromSlash(f.Path))
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("release file %s: %v", f.Path, err)
		}
		if string(got) != string(f.Data) {
			t.Fatalf("release file %s content mismatch", f.Path)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(assetsRoot, "staging")); err == nil && len(entries) != 0 {
		t.Fatalf("abandoned staging entries: %v", entries)
	}

	st := inspectInstallation(assetsRoot, true, reg)
	if st.State != InstallStateReady || !st.Verified {
		t.Fatalf("inspect verify: state=%s verified=%v err=%v", st.State, st.Verified, st.Err)
	}
	if st.Release != tc.Release || st.CatalogSHA256 != tc.Digest {
		t.Fatalf("inspect identity %+v", st)
	}

	srvHandler, err := newWithRegistry(Config{GatewayBasePath: "/emby", AssetsRoot: assetsRoot}, reg)
	if err != nil {
		t.Fatalf("newWithRegistry: %v", err)
	}
	if srvHandler.Status().State != StateReady {
		t.Fatalf("serve state=%s err=%v", srvHandler.Status().State, srvHandler.Status().Err)
	}

	httpSrv := httptest.NewServer(GuardHandler(srvHandler))
	t.Cleanup(httpSrv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), deploymentSmokeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", smokePath)
	cmd.Env = smokeCleanEnv(
		"GATEWAY_URL="+httpSrv.URL,
		"SMOKE_WEB=ready",
		"SMOKE_WEB_ONLY=1",
		// Non-canary path present in readyMinimalFiles; must not grant CORS.
		"SMOKE_WEB_NON_CANARY_PATH=modules/app.js",
		// Avoid inheriting operator credentials/media flags into web-only mode.
		"SMOKE_OPTIONAL_MEDIA=0",
		"SMOKE_M3U8_PATH=",
		"USERNAME=",
		"PASSWORD=",
		"SMOKE_USERNAME=",
		"SMOKE_PASSWORD=",
	)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("smoke.sh timed out after %s:\n%s", deploymentSmokeTimeout, out)
	}
	if err != nil {
		t.Fatalf("smoke.sh failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "smoke passed") {
		t.Fatalf("smoke.sh missing success marker:\n%s", out)
	}
	t.Logf("smoke.sh output:\n%s", out)
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			t.Fatalf("bash required on %s for scripts/smoke.sh: %v", runtime.GOOS, err)
		}
		t.Skipf("bash not available on %s: %v", runtime.GOOS, err)
	}
}

func requireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			t.Fatalf("curl required on %s for scripts/smoke.sh: %v", runtime.GOOS, err)
		}
		t.Skipf("curl not available on %s: %v", runtime.GOOS, err)
	}
}

// smokeCleanEnv builds a subprocess environment that clears CURL_OPTS, proxy
// variables, and common curl config knobs, then forces NO_PROXY bypass and
// applies overrides. PATH/HOME/TMPDIR and other process env are preserved except
// for the cleared keys.
func smokeCleanEnv(overrides ...string) []string {
	clearKeys := map[string]struct{}{
		"CURL_OPTS": {}, "curl_opts": {},
		"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "ALL_PROXY": {}, "NO_PROXY": {},
		"http_proxy": {}, "https_proxy": {}, "all_proxy": {}, "no_proxy": {},
		"FTP_PROXY": {}, "ftp_proxy": {},
		"SOCKS_PROXY": {}, "socks_proxy": {},
		"SOCKS5_PROXY": {}, "socks5_proxy": {},
		"CURL_HOME": {}, "curl_home": {},
		"CURL_CA_BUNDLE": {}, "SSL_CERT_FILE": {}, "SSL_CERT_DIR": {},
		"REQUESTS_CA_BUNDLE": {},
	}
	out := make([]string, 0, len(os.Environ())+len(overrides)+4)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if _, drop := clearKeys[key]; drop {
			continue
		}
		// Also drop case variants of proxy keys that may appear oddly.
		lk := strings.ToLower(key)
		if strings.HasSuffix(lk, "_proxy") || lk == "curl_opts" || lk == "curl_home" || lk == "curl_ca_bundle" {
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"NO_PROXY=*",
		"no_proxy=*",
		"CURL_OPTS=",
	)
	out = append(out, overrides...)
	return out
}

func smokeScriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/embyweb/deployment_smoke_test.go -> repo root
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	path := filepath.Join(root, "scripts", "smoke.sh")
	if st, err := os.Stat(path); err != nil || st.IsDir() {
		t.Fatalf("scripts/smoke.sh not found at %s: %v", path, err)
	}
	return path
}
