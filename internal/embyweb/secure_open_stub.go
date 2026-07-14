//go:build !(linux || darwin)

package embyweb

import (
	"fmt"
	"io/fs"
	"os"
)

// Secure no-follow open is required for archive/directory sources. Unsupported
// platforms fail closed rather than using an insecure Lstat+Open fallback.
func openRegularFileNoFollow(path string) (*os.File, error) {
	return nil, fmt.Errorf("%w: secure no-follow file open", ErrInstallUnsupported)
}

func openAnchoredDir(path string) (*anchoredDir, error) {
	return nil, fmt.Errorf("%w: secure no-follow directory open", ErrInstallUnsupported)
}

// anchoredDir is a stub type so call sites compile on all platforms.
type anchoredDir struct{}

func (d *anchoredDir) Close() error { return nil }

func (d *anchoredDir) Lstat(rel string) (fs.FileInfo, error) {
	return nil, fmt.Errorf("%w: anchoredDir", ErrInstallUnsupported)
}

func (d *anchoredDir) Open(rel string) (*os.File, error) {
	return nil, fmt.Errorf("%w: anchoredDir", ErrInstallUnsupported)
}

func fileNonblockFlag(f *os.File) (bool, error) {
	return false, fmt.Errorf("%w: fcntl", ErrInstallUnsupported)
}
