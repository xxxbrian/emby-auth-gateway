//go:build !(linux || darwin)

package embyweb

import (
	"errors"
	"fmt"
	"os"
)

func flockExclusiveNonblock(f *os.File) error {
	return fmt.Errorf("%w: flock", ErrInstallUnsupported)
}

func flockUnlock(f *os.File) error {
	return nil
}

// openInstallLockFile still rejects symlinks/non-regular paths on unsupported
// platforms, but flock will fail with ErrInstallUnsupported.
func openInstallLockFile(lockPath string) (*os.File, error) {
	if fi, err := os.Lstat(lockPath); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("install lock is a symlink")
		}
		if !fi.Mode().IsRegular() {
			return nil, errors.New("install lock is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, lockMode)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("install lock is not a regular file")
	}
	return f, nil
}
