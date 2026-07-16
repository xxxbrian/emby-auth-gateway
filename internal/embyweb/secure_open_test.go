//go:build linux || darwin

package embyweb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenRegularFileNoFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.dat")
	if err := os.WriteFile(real, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.dat")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := openRegularFileNoFollow(link)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
	f, err := openRegularFileNoFollow(real)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	nb, err := fileNonblockFlag(f)
	if err != nil {
		t.Fatal(err)
	}
	if nb {
		t.Fatal("O_NONBLOCK must be cleared on returned regular fd")
	}
}

func TestOpenAnchoredDirSymlinkRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := openAnchoredDir(link)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
	d, err := openAnchoredDir(real)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
}

func TestAnchoredOpenRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "modules")); err != nil {
		t.Fatal(err)
	}
	d, err := openAnchoredDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	_, err = d.Open("modules/secret.txt")
	if err == nil {
		t.Fatal("expected intermediate symlink rejection")
	}
	if !strings.Contains(err.Error(), "symlink") && !os.IsNotExist(err) {
		// ELOOP maps to "path is a symlink"; some platforms may surface not-exist.
		t.Fatalf("err=%v", err)
	}
}

func TestAnchoredOpenRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real.js")
	if err := os.WriteFile(real, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link.js")); err != nil {
		t.Fatal(err)
	}
	d, err := openAnchoredDir(root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	_, err = d.Open("link.js")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}

	f, err := d.Open("real.js")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Ensure returned fd is usable and nonblocking cleared.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, err := f.Read(buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read hung")
	}
}
