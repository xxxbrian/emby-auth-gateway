//go:build linux || darwin

package embyweb

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenInstallLockFileCLOEXEC(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, installLockName)
	f, err := openInstallLockFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("F_GETFD: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("install.lock fd missing FD_CLOEXEC (flags=%#x)", flags)
	}
}
