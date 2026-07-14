//go:build linux || darwin

package embyweb

import (
	"context"
	"io/fs"
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

func TestOpenRegularFileNoFollowFIFONoHang(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "x.fifo")
	if err := mkfifo(fifo); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := openRegularFileNoFollow(fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-regular rejection")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("openRegularFileNoFollow hung on FIFO")
	}
}

func TestOpenAnchoredDirSymlink(t *testing.T) {
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

func TestArchiveSourceFIFONoHang(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "evil.tar.gz")
	if err := mkfifo(fifo); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	files := readyMinimalFiles()
	tc, w, _ := newArchiveTestEnv(t, files)
	src, err := newArchiveSource(fifo)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- src.acquire(context.Background(), tc, w)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected FIFO rejection")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("archive acquire hung on FIFO path")
	}
}

func TestDirectorySourceRootSymlinkRejectedAtAcquire(t *testing.T) {
	files := readyMinimalFiles()
	link := filepath.Join(t.TempDir(), "src-link")
	real := writeDirSourceTree(t, files)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := newDirectorySource(link)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("constructor err=%v", err)
	}

	srcDir := writeDirSourceTree(t, files)
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	aside := srcDir + ".aside"
	if err := os.Rename(srcDir, aside); err != nil {
		t.Fatal(err)
	}
	other := writeDirSourceTree(t, files)
	if err := os.Symlink(other, srcDir); err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("acquire after root symlink swap: %v", err)
	}
}

func TestDirectorySourceRootAnchoredDespitePathReplace(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	src, err := newDirectorySourceWithDeps(srcDir, directorySourceDeps{
		AfterFirstScan: func() error {
			aside := srcDir + ".orig"
			if err := os.Rename(srcDir, aside); err != nil {
				return err
			}
			empty := t.TempDir()
			return os.Symlink(empty, srcDir)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	// Fd-anchored: original tree still visible → success + complete.
	if err != nil {
		t.Fatalf("anchored acquire should succeed on original inode: %v", err)
	}
	if err := w.complete(); err != nil {
		t.Fatalf("complete: %v", err)
	}
}

// lstatOpenMutator delegates to an inner treeWalker and runs mutate exactly once
// between a successful Lstat of target and the subsequent Open of the same path.
type lstatOpenMutator struct {
	inner     treeWalker
	target    string
	mutate    func() error
	sawLstat  bool
	mutated   bool
	mutateErr error
}

func (m *lstatOpenMutator) Lstat(name string) (fs.FileInfo, error) {
	fi, err := m.inner.Lstat(name)
	if err == nil && name == m.target {
		m.sawLstat = true
	}
	return fi, err
}

func (m *lstatOpenMutator) Open(name string) (*os.File, error) {
	if name == m.target && m.sawLstat && !m.mutated {
		m.mutated = true
		m.mutateErr = m.mutate()
		// Always proceed to Open so we exercise the post-mutation open path;
		// callers must still assert mutateErr == nil (setup succeeded).
	}
	return m.inner.Open(name)
}

func TestAnchoredDirFileLstatOpenFIFONoHang(t *testing.T) {
	// Replace a regular file with a FIFO precisely between treeWalker Lstat and Open.
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	d, err := openAnchoredDir(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	target := "modules/app.js"
	abs := filepath.Join(srcDir, filepath.FromSlash(target))
	m := &lstatOpenMutator{
		inner:  d,
		target: target,
		mutate: func() error {
			if err := os.Remove(abs); err != nil {
				return err
			}
			return mkfifo(abs)
		},
	}

	fi, err := m.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("pre-mutation Lstat want regular, got %v", fi.Mode())
	}

	done := make(chan error, 1)
	go func() {
		f, err := m.Open(target)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !m.mutated {
			t.Fatal("mutation callback never ran between Lstat and Open")
		}
		if m.mutateErr != nil {
			t.Fatalf("mutation setup failed (test invalid): %v", m.mutateErr)
		}
		if err == nil {
			t.Fatal("expected Open failure after FIFO swap")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open hung after file→FIFO swap between Lstat and Open")
	}
}

func TestAnchoredDirDirectoryLstatOpenFIFONoHang(t *testing.T) {
	// Replace a directory with a FIFO precisely between treeWalker Lstat and Open
	// (the verifyExactTree walk pattern for directories).
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	d, err := openAnchoredDir(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	target := "modules"
	abs := filepath.Join(srcDir, target)
	m := &lstatOpenMutator{
		inner:  d,
		target: target,
		mutate: func() error {
			if err := os.Rename(abs, abs+".aside"); err != nil {
				return err
			}
			return mkfifo(abs)
		},
	}

	fi, err := m.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() {
		t.Fatalf("pre-mutation Lstat want dir, got %v", fi.Mode())
	}

	done := make(chan error, 1)
	go func() {
		f, err := m.Open(target)
		if f != nil {
			_ = f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !m.mutated {
			t.Fatal("mutation callback never ran between Lstat and Open")
		}
		if m.mutateErr != nil {
			t.Fatalf("mutation setup failed (test invalid): %v", m.mutateErr)
		}
		if err == nil {
			t.Fatal("expected Open failure after directory→FIFO swap")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Open hung after directory→FIFO swap between Lstat and Open")
	}
}

func TestOpenDirectoryRetryReturnsErr2(t *testing.T) {
	// When the initial NONBLOCK open fails and the O_DIRECTORY retry also fails,
	// the returned error must reflect the retry (err2), not the stale first err.
	// A path that is neither openable as file nor as directory: empty name base
	// is rejected earlier; use a dangling name under an anchored dir.
	d, err := openAnchoredDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, err = d.Open("does-not-exist-at-all")
	if err == nil {
		t.Fatal("expected missing path error")
	}
	// Must be not-exist (from err2/mapUnixErr), not a confusing stale message.
	if !strings.Contains(err.Error(), "exist") && !os.IsNotExist(err) {
		// mapUnixErr maps ENOENT to fs.ErrNotExist
		if err != fs.ErrNotExist && !strings.Contains(strings.ToLower(err.Error()), "no such") {
			t.Fatalf("err=%v want not-exist from directory retry path", err)
		}
	}
}
