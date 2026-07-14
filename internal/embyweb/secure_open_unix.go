//go:build linux || darwin

package embyweb

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// openRegularFileNoFollow opens path without following a final-component symlink.
// O_NONBLOCK avoids hanging on a FIFO; nonblocking is cleared before return.
func openRegularFileNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, classifyNoFollowOpenErr(path, err)
	}
	return finishRegularFD(fd, path)
}

func finishRegularFD(fd int, name string) (*os.File, error) {
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
	f := os.NewFile(uintptr(fd), name)
	if f == nil {
		_ = unix.Close(fd)
		return nil, errors.New("NewFile failed")
	}
	return f, nil
}

// anchoredDir is a retained directory file descriptor. All descendant access is
// openat/fstatat-relative with O_NOFOLLOW; the original path is never re-resolved.
type anchoredDir struct {
	fd   int
	name string
}

// openAnchoredDir opens path with O_DIRECTORY|O_NOFOLLOW and retains the fd.
func openAnchoredDir(path string) (*anchoredDir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, classifyNoFollowOpenErr(path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return nil, errors.New("opened handle is not a directory")
	}
	return &anchoredDir{fd: fd, name: path}, nil
}

func (d *anchoredDir) Close() error {
	if d == nil || d.fd < 0 {
		return nil
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

func (d *anchoredDir) Name() string { return d.name }

// Lstat returns info for a path relative to the anchored directory without
// following any final-component symlink.
func (d *anchoredDir) Lstat(rel string) (fs.FileInfo, error) {
	if d == nil || d.fd < 0 {
		return nil, errors.New("anchoredDir closed")
	}
	if rel == "." || rel == "" {
		var st unix.Stat_t
		if err := unix.Fstat(d.fd, &st); err != nil {
			return nil, err
		}
		return newUnixFileInfo(".", &st), nil
	}
	if err := checkRelPath(rel); err != nil {
		return nil, err
	}
	parent, base, cleanup, err := d.walkToParent(rel)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	var st unix.Stat_t
	if err := unix.Fstatat(parent, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, mapUnixErr(err)
	}
	return newUnixFileInfo(base, &st), nil
}

// Open opens a path relative to the anchored directory. Directories use
// O_DIRECTORY|O_NOFOLLOW; regular files use O_NOFOLLOW|O_NONBLOCK then clear
// nonblock after fstat confirms a regular file.
func (d *anchoredDir) Open(rel string) (*os.File, error) {
	if d == nil || d.fd < 0 {
		return nil, errors.New("anchoredDir closed")
	}
	if rel == "." || rel == "" {
		fd, err := unix.Openat(d.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, mapUnixErr(err)
		}
		f := os.NewFile(uintptr(fd), d.name)
		if f == nil {
			_ = unix.Close(fd)
			return nil, errors.New("NewFile failed")
		}
		return f, nil
	}
	if err := checkRelPath(rel); err != nil {
		return nil, err
	}
	parent, base, cleanup, err := d.walkToParent(rel)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Probe with O_NOFOLLOW|O_NONBLOCK (safe for files and avoids FIFO hang).
	fd, err := unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		// Directory open may need O_DIRECTORY on some systems if NONBLOCK open fails.
		if errors.Is(err, unix.ELOOP) {
			return nil, errors.New("path is a symlink")
		}
		// Retry as directory without NONBLOCK if ENOTDIR/EISDIR variants.
		fd2, err2 := unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err2 != nil {
			// Prefer the directory-open error (authoritative for this retry path).
			return nil, mapUnixErr(err2)
		}
		f := os.NewFile(uintptr(fd2), path.Join(d.name, rel))
		if f == nil {
			_ = unix.Close(fd2)
			return nil, errors.New("NewFile failed")
		}
		return f, nil
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return finishRegularFD(fd, path.Join(d.name, rel))
	case unix.S_IFDIR:
		// Re-open as directory without NONBLOCK for reliable ReadDir.
		_ = unix.Close(fd)
		fd, err = unix.Openat(parent, base, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, mapUnixErr(err)
		}
		f := os.NewFile(uintptr(fd), path.Join(d.name, rel))
		if f == nil {
			_ = unix.Close(fd)
			return nil, errors.New("NewFile failed")
		}
		return f, nil
	case unix.S_IFLNK:
		_ = unix.Close(fd)
		return nil, errors.New("path is a symlink")
	default:
		_ = unix.Close(fd)
		return nil, errors.New("opened handle is not a regular file or directory")
	}
}

// walkToParent opens each intermediate directory component with
// O_NOFOLLOW|O_DIRECTORY relative to the anchored fd. Caller must cleanup.
func (d *anchoredDir) walkToParent(rel string) (parent int, base string, cleanup func(), err error) {
	parts := strings.Split(rel, "/")
	if len(parts) == 0 || parts[0] == "" {
		return -1, "", func() {}, errors.New("invalid relative path")
	}
	base = parts[len(parts)-1]
	if base == "" || base == "." || base == ".." {
		return -1, "", func() {}, errors.New("invalid path base")
	}
	parent = d.fd
	var opened []int
	cleanup = func() {
		for i := len(opened) - 1; i >= 0; i-- {
			_ = unix.Close(opened[i])
		}
	}
	for _, seg := range parts[:len(parts)-1] {
		if seg == "" || seg == "." || seg == ".." {
			cleanup()
			return -1, "", func() {}, errors.New("invalid path segment")
		}
		fd, err := unix.Openat(parent, seg, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			cleanup()
			if errors.Is(err, unix.ELOOP) {
				return -1, "", func() {}, errors.New("path is a symlink")
			}
			return -1, "", func() {}, mapUnixErr(err)
		}
		opened = append(opened, fd)
		parent = fd
	}
	return parent, base, cleanup, nil
}

func checkRelPath(rel string) error {
	if rel == "" || !fs.ValidPath(rel) || path.Clean(rel) != rel {
		return errors.New("non-canonical relative path")
	}
	if strings.ContainsAny(rel, `\`+"\x00") {
		return errors.New("unsafe relative path")
	}
	return nil
}

func mapUnixErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOENT) {
		return fs.ErrNotExist
	}
	if errors.Is(err, unix.ELOOP) {
		return errors.New("path is a symlink")
	}
	return err
}

func classifyNoFollowOpenErr(path string, err error) error {
	if errors.Is(err, unix.ELOOP) {
		return errors.New("path is a symlink")
	}
	if li, lerr := os.Lstat(path); lerr == nil && li.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is a symlink")
	}
	return err
}

// unixFileInfo implements fs.FileInfo from a Stat_t.
type unixFileInfo struct {
	name    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
	sys     unix.Stat_t
}

func newUnixFileInfo(name string, st *unix.Stat_t) *unixFileInfo {
	mode := fs.FileMode(st.Mode & 0o777)
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= fs.ModeDir
	case unix.S_IFLNK:
		mode |= fs.ModeSymlink
	case unix.S_IFIFO:
		mode |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= fs.ModeSocket
	case unix.S_IFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		mode |= fs.ModeDevice
	}
	if st.Mode&unix.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if st.Mode&unix.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if st.Mode&unix.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return &unixFileInfo{
		name:    name,
		size:    int64(st.Size),
		mode:    mode,
		modTime: time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)),
		sys:     *st,
	}
}

func (fi *unixFileInfo) Name() string       { return fi.name }
func (fi *unixFileInfo) Size() int64        { return fi.size }
func (fi *unixFileInfo) Mode() fs.FileMode  { return fi.mode }
func (fi *unixFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *unixFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi *unixFileInfo) Sys() any           { return &fi.sys }

// fileNonblockFlag reports whether O_NONBLOCK is set on f's descriptor (tests).
func fileNonblockFlag(f *os.File) (bool, error) {
	if f == nil {
		return false, errors.New("nil file")
	}
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return false, err
	}
	return flags&unix.O_NONBLOCK != 0, nil
}
