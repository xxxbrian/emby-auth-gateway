//go:build !unix

package embyweb

import "fmt"

func mkfifo(path string) error {
	return fmt.Errorf("mkfifo not supported on this platform")
}
