//go:build !(linux || darwin)

package embyweb

import "fmt"

func renameNoReplace(oldpath, newpath string) error {
	return fmt.Errorf("%w: atomic no-replace rename", ErrInstallUnsupported)
}
