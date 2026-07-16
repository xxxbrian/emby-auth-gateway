//go:build !(linux || darwin)

package embyweb

import (
	"fmt"
	"os"
	"runtime"
)

// Secure no-follow open is required for serve-time file access. Unsupported
// platforms fail closed rather than using an insecure Lstat+Open fallback.
func openRegularFileNoFollow(path string) (*os.File, error) {
	return nil, fmt.Errorf("embyweb: secure no-follow file open unsupported on %s/%s", runtime.GOOS, runtime.GOARCH)
}
