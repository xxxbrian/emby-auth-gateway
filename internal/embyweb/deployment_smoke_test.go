package embyweb

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestSyntheticReadyDeploymentSmoke starts a real HTTP server over a fixture
// web root and runs scripts/smoke.sh in SMOKE_WEB_ONLY=ready mode.
func TestSyntheticReadyDeploymentSmoke(t *testing.T) {
	root := t.TempDir()
	writeReadyTree(t, root, map[string]string{
		"modules/app.js": "console.log(1)",
	})

	s, err := New(Config{GatewayBasePath: "/emby", AssetsRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status().State != StateReady {
		t.Fatalf("state=%s err=%v", s.Status().State, s.Status().Err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: GuardHandler(s)}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		_ = srv.Close()
	}()

	// Locate repo-root scripts/smoke.sh from this package directory.
	smoke := filepath.Join("..", "..", "scripts", "smoke.sh")
	if _, err := os.Stat(smoke); err != nil {
		t.Fatalf("smoke script: %v", err)
	}

	addr := "http://" + ln.Addr().String()
	cmd := exec.Command("bash", smoke)
	cmd.Env = append(os.Environ(),
		"SMOKE_WEB_ONLY=1",
		"SMOKE_WEB=ready",
		"SMOKE_WEB_NON_CANARY_PATH=modules/app.js",
		"GATEWAY_URL="+addr,
		"PB_URL="+addr,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("smoke failed: %v\n%s", err, out)
	}
	t.Logf("smoke ok (%s)", time.Now().UTC().Format(time.RFC3339))
}
