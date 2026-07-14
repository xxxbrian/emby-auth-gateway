//go:build linux || darwin

package embyweb

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func flockExclusiveNonblock(f *os.File) error {
	if f == nil {
		return errors.New("nil file")
	}
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
		return ErrInstallBusy
	}
	return err
}

func flockUnlock(f *os.File) error {
	if f == nil {
		return nil
	}
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// openInstallLockFile opens or creates install.lock without following symlinks.
// The opened handle is verified to be a regular file (best-effort against
// replacement races between Lstat and open).
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

	// O_NOFOLLOW rejects a symlink at the final path component (ELOOP).
	// O_CLOEXEC keeps the lock fd out of exec'd children.
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, lockMode)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, errors.New("install lock is a symlink")
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), lockPath)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("install lock: NewFile failed")
	}

	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	// Stat on an open fd follows the opened inode; ModeSymlink should not appear,
	// but reject non-regular nodes (devices, fifos) if the path was replaced.
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("install lock is not a regular file")
	}
	// Re-Lstat path: if it is now a symlink, another process replaced the name
	// after open; we still hold a regular inode, but refuse to use a lock whose
	// path is a symlink (operator confusion / attack surface).
	if lfi, err := os.Lstat(lockPath); err == nil {
		if lfi.Mode()&os.ModeSymlink != 0 {
			_ = f.Close()
			return nil, errors.New("install lock path became a symlink")
		}
	}
	return f, nil
}
