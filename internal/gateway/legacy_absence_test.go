package gateway

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoLegacyProxyProductionSymbols is a Phase 8 W4 gate: production source under
// cmd/ and internal/ must not retain the deleted Legacy HTTP proxy surface.
func TestNoLegacyProxyProductionSymbols(t *testing.T) {
	forbidden := []string{
		"LegacyProxy",
		"OperationLegacyProxy",
		"LegacyHTTPUpstream",
		"RoundTripLegacy",
		"legacyUpstreamRequest",
		"responseProjectionLegacyCompatibility",
		"legacy_proxy_request",
		"upstreamPurposeLegacy",
		"legacyHTTPUpstream",
		"newLegacyHTTPUpstream",
		"rewriteLegacyHeaderIdentity",
	}

	root := repoRoot(t)
	// Prove scan scope reaches outside internal/gateway (this package's cwd).
	sentinelOutsideGateway := filepath.Join(root, "internal", "routeclass", "routeclass.go")
	if _, err := os.Stat(sentinelOutsideGateway); err != nil {
		t.Fatalf("scan scope sentinel missing: %v", err)
	}
	cmdMain := filepath.Join(root, "cmd", "gateway", "main.go")
	if _, err := os.Stat(cmdMain); err != nil {
		t.Fatalf("cmd/gateway/main.go missing from repo root %s: %v", root, err)
	}

	var scannedOutsideGateway int
	var scannedCmd int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := info.Name()
			// Skip dependency clones, VCS, testdata fixtures, and non-source trees.
			if base == "testdata" || base == "vendor" || base == "node_modules" ||
				base == ".git" || base == ".slim" || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			// Only walk cmd and internal production trees (plus root go files if any).
			if rel == "." {
				return nil
			}
			top := strings.Split(filepath.ToSlash(rel), "/")[0]
			if top != "cmd" && top != "internal" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if strings.HasPrefix(slash, "cmd/") {
			scannedCmd++
		}
		if strings.HasPrefix(slash, "internal/") && !strings.HasPrefix(slash, "internal/gateway/") {
			scannedOutsideGateway++
		}
		// Allow this gate file only.
		if strings.HasSuffix(path, "legacy_absence_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, sym := range forbidden {
			if strings.Contains(text, sym) {
				t.Errorf("production file %s still references deleted Legacy symbol %q", slash, sym)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scannedCmd == 0 {
		t.Fatal("scan did not visit any cmd/*.go production files")
	}
	if scannedOutsideGateway == 0 {
		t.Fatal("scan did not visit any internal/* production files outside gateway")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// .../internal/gateway/legacy_absence_test.go -> repo root
	dir := filepath.Dir(file)
	root := filepath.Clean(filepath.Join(dir, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s missing go.mod: %v", root, err)
	}
	return root
}
