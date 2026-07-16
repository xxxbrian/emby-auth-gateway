//go:build !(linux || darwin)

package embyweb

import (
	"fmt"
	"io/fs"
	"os"
	"runtime"
)

// Secure no-follow open is required for serve-time file access. Unsupported
// platforms fail closed rather than using an insecure Lstat+Open fallback.

func openRegularFileNoFollow(path string) (*os.File, error) {
	return nil, fmt.Errorf("embyweb: secure no-follow file open unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}

func openAnchoredDir(path string) (*anchoredDir, error) {
	return nil, fmt.Errorf("embyweb: secure no-follow directory open unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}

// anchoredDir is a stub type so call sites compile on all platforms.
type anchoredDir struct{}

func (d *anchoredDir) Close() error { return nil }

func (d *anchoredDir) Name() string { return "" }

func (d *anchoredDir) Lstat(rel string) (fs.FileInfo, error) {
	return nil, fmt.Errorf("embyweb: secure no-follow Lstat unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}

func (d *anchoredDir) Open(rel string) (*os.File, error) {
	return nil, fmt.Errorf("embyweb: secure no-follow Open unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}

func fileNonblockFlag(f *os.File) (bool, error) {
	return false, fmt.Errorf("embyweb: fcntl unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}
