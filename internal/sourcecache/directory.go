package sourcecache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const directoryName = "eag-source-cache-v1"
const markerName = ".gateway-source-cache"
const markerContent = "gateway-source-range-cache-v1\n"

type cacheDirectory struct {
	root      string
	blocks    string
	lock      *os.File
	temporary bool
}

// Only blocks beneath an ownership-verified, exclusively locked directory may
// be removed. Neither the configured parent nor its other files are cache data.
func openDirectory(parent string) (*cacheDirectory, error) {
	var root string
	var err error
	temporary := parent == ""
	if temporary {
		root, err = os.MkdirTemp("", "eag-source-cache-")
	} else {
		if err = os.MkdirAll(parent, 0700); err == nil {
			root = filepath.Join(parent, directoryName)
			err = os.Mkdir(root, 0700)
			if errors.Is(err, os.ErrExist) {
				err = nil
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create source cache directory: %w", err)
	}
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("source cache must be a dedicated directory, not a symlink")
	}
	fd, err := unix.Open(filepath.Join(root, ".lock"), unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open source cache lock: %w", err)
	}
	lock := os.NewFile(uintptr(fd), "source-cache-lock")
	fail := func(err error) (*cacheDirectory, error) {
		_ = lock.Close()
		return nil, err
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("source cache directory is already in use: %w", err))
	}
	marker := filepath.Join(root, markerName)
	st, err = os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		items, readErr := os.ReadDir(root)
		if readErr != nil {
			return fail(readErr)
		}
		for _, item := range items {
			if item.Name() != ".lock" {
				return fail(fmt.Errorf("source cache directory is not empty or gateway-owned"))
			}
		}
		if err = os.WriteFile(marker, []byte(markerContent), 0600); err != nil {
			return fail(err)
		}
	} else {
		if err != nil || !st.Mode().IsRegular() || st.Size() != int64(len(markerContent)) {
			return fail(fmt.Errorf("source cache ownership marker is invalid"))
		}
		content, readErr := os.ReadFile(marker)
		if readErr != nil || string(content) != markerContent {
			return fail(fmt.Errorf("source cache ownership marker is invalid"))
		}
	}
	if err = os.Chmod(root, 0700); err != nil {
		return fail(err)
	}
	blocks := filepath.Join(root, "blocks")
	if err = os.RemoveAll(blocks); err != nil {
		return fail(err)
	}
	if err = os.Mkdir(blocks, 0700); err != nil {
		return fail(err)
	}
	return &cacheDirectory{root: root, blocks: blocks, lock: lock, temporary: temporary}, nil
}

func (d *cacheDirectory) close() error {
	err := os.RemoveAll(d.blocks)
	// Keep a configured directory's marker and lock inode across restarts.
	// Removing its lock while held would allow another process a second lock.
	if d.temporary {
		err = errors.Join(err, os.RemoveAll(d.root))
	}
	return errors.Join(err, d.lock.Close())
}
