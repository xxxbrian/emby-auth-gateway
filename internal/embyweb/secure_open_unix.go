//go:build linux || darwin

package embyweb

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openRegularFileNoFollow opens path without following a final-component symlink.
// O_NONBLOCK avoids hanging on a FIFO; nonblocking is cleared before return.
func openRegularFileNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, classifyNoFollowOpenErr(path, err)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("clear nonblock: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, errors.New("opened handle is not a regular file")
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("NewFile failed")
	}
	return f, nil
}

func classifyNoFollowOpenErr(path string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%s: path is a symlink", path)
	}
	if errors.Is(err, unix.ENOENT) {
		return os.ErrNotExist
	}
	return err
}
