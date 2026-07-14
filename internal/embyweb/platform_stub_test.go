//go:build !(linux || darwin)

package embyweb

import (
	"errors"
	"os"
	"testing"
)

// Compile/assertion coverage for unsupported-platform stubs.
func TestUnsupportedPlatformStubs(t *testing.T) {
	err := flockExclusiveNonblock(nil)
	if !errors.Is(err, ErrInstallUnsupported) {
		t.Fatalf("flock stub: %v", err)
	}
	err = renameNoReplace("/tmp/a", "/tmp/b")
	if !errors.Is(err, ErrInstallUnsupported) {
		t.Fatalf("rename stub: %v", err)
	}
	// openInstallLockFile still works for regular files; flock fails later.
	dir := t.TempDir()
	f, err := openInstallLockFile(dir + "/install.lock")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	_ = os.Remove(dir + "/install.lock")
}
