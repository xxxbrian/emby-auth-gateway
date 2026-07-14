package embyweb

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeDirSourceTree(t *testing.T, files []fixtureFile) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func dirSourceStaging(t *testing.T, files []fixtureFile) (*directorySource, *stagingWriter, *trustedCatalog, string) {
	t.Helper()
	srcDir := writeDirSourceTree(t, files)
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, stageFiles := newTestStagingWriter(t, files)
	return src, w, tc, stageFiles
}

func assertStagedExact(t *testing.T, stageFiles string, files []fixtureFile, w *stagingWriter) {
	t.Helper()
	if err := w.complete(); err != nil {
		t.Fatalf("writer complete: %v", err)
	}
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(stageFiles, filepath.FromSlash(f.Path)))
		if err != nil {
			t.Fatalf("read staged %q: %v", f.Path, err)
		}
		if string(got) != string(f.Data) {
			t.Fatalf("staged %q mismatch: got %q want %q", f.Path, got, f.Data)
		}
	}
	// No extras under staged files/.
	var extras []string
	err := filepath.WalkDir(stageFiles, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(stageFiles, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		found := false
		for _, f := range files {
			if f.Path == rel {
				found = true
				break
			}
		}
		if !found {
			extras = append(extras, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(extras) > 0 {
		t.Fatalf("unexpected staged files: %v", extras)
	}
}

func TestDirectorySourceSuccessNestedTree(t *testing.T) {
	files := readyMinimalFiles()
	src, w, tc, stageFiles := dirSourceStaging(t, files)

	if src.kind() != "directory" {
		t.Fatalf("kind=%q want directory", src.kind())
	}
	if err := src.acquire(context.Background(), tc, w); err != nil {
		t.Fatal(err)
	}
	assertStagedExact(t, stageFiles, files, w)
}

func TestDirectorySourceMissingFile(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	// Remove one declared file.
	if err := os.Remove(filepath.Join(srcDir, "modules", "app.js")); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("err=%v want missing", err)
	}
}

func TestDirectorySourceMissingDir(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	if err := os.RemoveAll(filepath.Join(srcDir, "strings")); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected missing directory error")
	}
}

func TestDirectorySourceExtraFile(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	if err := os.WriteFile(filepath.Join(srcDir, "extra.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("err=%v want undeclared", err)
	}
}

func TestDirectorySourceExtraDir(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	if err := os.Mkdir(filepath.Join(srcDir, "bonus"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("err=%v want undeclared", err)
	}
}

func TestDirectorySourceRejectsReleaseLayoutExtras(t *testing.T) {
	// Pointing at a release-style wrapper must fail: install.json/current.json
	// are undeclared extras relative to the raw catalog tree.
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	for _, name := range []string{"install.json", "current.json"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("err=%v want undeclared release-layout files", err)
	}
}

func TestDirectorySourceRootSymlink(t *testing.T) {
	files := readyMinimalFiles()
	real := writeDirSourceTree(t, files)
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := newDirectorySource(link)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v want symlink", err)
	}
}

func TestDirectorySourceFileSymlink(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	target := filepath.Join(srcDir, "modules", "app.js")
	// Replace regular file with symlink to itself-content via temp.
	tmp := filepath.Join(t.TempDir(), "app.js")
	if err := os.WriteFile(tmp, []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tmp, target); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v want symlink", err)
	}
}

func TestDirectorySourceDirSymlink(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	// Replace modules/ with a symlink to a real modules tree elsewhere.
	realModules := filepath.Join(t.TempDir(), "modules")
	if err := os.MkdirAll(realModules, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realModules, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(srcDir, "modules")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realModules, filepath.Join(srcDir, "modules")); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v want symlink", err)
	}
}

func TestDirectorySourceFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fifo not supported on windows")
	}
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	// Replace a catalog file with a FIFO.
	target := filepath.Join(srcDir, "css", "site.css")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(target); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected nonregular/fifo rejection")
	}
	if !strings.Contains(err.Error(), "not a regular") && !strings.Contains(err.Error(), "regular file or directory") {
		t.Fatalf("err=%v want nonregular", err)
	}
}

func TestDirectorySourceSizeMismatch(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	// Truncate a file so Lstat size != catalog.
	p := filepath.Join(srcDir, "modules", "app.js")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	// First scan only checks presence; size fails at copy Lstat/open.
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("err=%v want size", err)
	}
}

func TestDirectorySourceHashMismatch(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	// Same size, wrong bytes.
	p := filepath.Join(srcDir, "modules", "app.js")
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), orig...)
	if len(bad) == 0 {
		t.Fatal("empty app.js")
	}
	bad[0] ^= 0xff
	if err := os.WriteFile(p, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySource(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err=%v want sha256", err)
	}
}

func TestDirectorySourceRootNotDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := newDirectorySource(f)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err=%v want not a directory", err)
	}
}

func TestDirectorySourceEmptyPath(t *testing.T) {
	_, err := newDirectorySource("  ")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err=%v want empty", err)
	}
}

func TestDirectorySourceContextCancelBeforeAcquire(t *testing.T) {
	files := readyMinimalFiles()
	src, w, tc, _ := dirSourceStaging(t, files)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := src.acquire(ctx, tc, w)
	if err == nil || !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "cancelled") {
		// context.Canceled message is "context canceled"
		if err == nil || !strings.Contains(err.Error(), "context") {
			t.Fatalf("err=%v want context canceled", err)
		}
	}
}

func TestDirectorySourceContextCancelBetweenFiles(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	ctx, cancel := context.WithCancel(context.Background())
	var opened int
	src, err := newDirectorySourceWithDeps(srcDir, directorySourceDeps{
		BeforeOpenFile: func(rel string) error {
			opened++
			if opened == 2 {
				cancel()
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(ctx, tc, w)
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Fatalf("err=%v want context", err)
	}
}

func TestDirectorySourceMutationAfterFirstScanExtraFile(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	src, err := newDirectorySourceWithDeps(srcDir, directorySourceDeps{
		AfterFirstScan: func() error {
			return os.WriteFile(filepath.Join(srcDir, "mutated-extra.txt"), []byte("x"), 0o644)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	// Extra appears after first scan; copy may succeed; final scan must fail.
	if err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("err=%v want undeclared on final scan", err)
	}
}

func TestDirectorySourceMutationBeforeOpenRemoveFile(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	target := path.Clean("modules/app.js")
	src, err := newDirectorySourceWithDeps(srcDir, directorySourceDeps{
		BeforeOpenFile: func(rel string) error {
			if rel == target {
				return os.Remove(filepath.Join(srcDir, filepath.FromSlash(rel)))
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil {
		t.Fatal("expected failure after remove")
	}
}

func TestDirectorySourceMutationBeforeOpenReplaceContent(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	target := "modules/app.js"
	src, err := newDirectorySourceWithDeps(srcDir, directorySourceDeps{
		BeforeOpenFile: func(rel string) error {
			if rel != target {
				return nil
			}
			p := filepath.Join(srcDir, filepath.FromSlash(rel))
			orig, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			bad := append([]byte(nil), orig...)
			bad[0] ^= 0xff
			return os.WriteFile(p, bad, 0o644)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err=%v want sha256", err)
	}
}

func TestDirectorySourceMutationAfterCopiesSecondScan(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	src, err := newDirectorySourceWithDeps(srcDir, directorySourceDeps{
		AfterAllCopies: func() error {
			// Remove a declared file so final exact-tree scan fails.
			return os.Remove(filepath.Join(srcDir, "index.html"))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, tc, stageFiles := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	if err == nil || !strings.Contains(err.Error(), "missing") && !strings.Contains(err.Error(), "final tree") {
		t.Fatalf("err=%v want final tree missing", err)
	}
	// Writer may be complete for catalog bytes already staged, but acquire failed.
	_ = stageFiles
}

func TestDirectorySourceMutationBeforeOpenToSymlink(t *testing.T) {
	files := readyMinimalFiles()
	srcDir := writeDirSourceTree(t, files)
	target := "css/site.css"
	alt := filepath.Join(t.TempDir(), "site.css")
	if err := os.WriteFile(alt, []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := newDirectorySourceWithDeps(srcDir, directorySourceDeps{
		BeforeOpenFile: func(rel string) error {
			if rel != target {
				return nil
			}
			p := filepath.Join(srcDir, filepath.FromSlash(rel))
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.Symlink(alt, p)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, tc, _ := newTestStagingWriter(t, files)
	err = src.acquire(context.Background(), tc, w)
	// Root.Open rejects out-of-root symlink targets as escapes; in-tree symlink
	// swaps may surface as symlink or non-regular depending on timing.
	if err == nil {
		t.Fatal("expected failure after symlink swap")
	}
	msg := err.Error()
	if !strings.Contains(msg, "symlink") && !strings.Contains(msg, "escapes") && !strings.Contains(msg, "not a regular") {
		t.Fatalf("err=%v want symlink/escape/nonregular", err)
	}
}

func TestDirectorySourceNilCatalogAndWriter(t *testing.T) {
	files := readyMinimalFiles()
	src, w, tc, _ := dirSourceStaging(t, files)
	if err := src.acquire(context.Background(), nil, w); err == nil {
		t.Fatal("expected nil catalog error")
	}
	if err := src.acquire(context.Background(), tc, nil); err == nil {
		t.Fatal("expected nil writer error")
	}
}
