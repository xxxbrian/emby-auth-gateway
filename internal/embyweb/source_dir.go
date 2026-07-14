package embyweb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// directorySource materializes catalog files from a prepared raw-files directory.
// Catalog paths are resolved directly under the source root (not a release-layout
// wrapper with install.json/current.json). Absolute source snapshot guarantees
// are impossible under active mutation; this source only fails closed or yields
// exact catalog bytes. The staged verifier remains authoritative in core.
type directorySource struct {
	dir  string
	deps directorySourceDeps
}

// directorySourceDeps are package-private test hooks. Production construction
// leaves every field nil.
type directorySourceDeps struct {
	// AfterFirstScan runs after the initial exact-tree scan succeeds.
	AfterFirstScan func() error
	// BeforeOpenFile runs after component Lstat rechecks and before Open(rel).
	BeforeOpenFile func(rel string) error
	// AfterAllCopies runs after every catalog entry is written and before the
	// final exact-tree scan.
	AfterAllCopies func() error
}

// newDirectorySource validates a non-blank prepared raw-files directory path:
// resolve absolute, Lstat the root, and reject symlinks and non-directories.
// The returned source is ready for acquire; tree contents are checked there.
func newDirectorySource(dir string) (*directorySource, error) {
	return newDirectorySourceWithDeps(dir, directorySourceDeps{})
}

func newDirectorySourceWithDeps(dir string, deps directorySourceDeps) (*directorySource, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, errors.New("directory source: empty path")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("directory source: resolve path: %w", err)
	}
	if err := validateDirectorySourceRoot(abs); err != nil {
		return nil, err
	}
	return &directorySource{dir: abs, deps: deps}, nil
}

func validateDirectorySourceRoot(abs string) error {
	fi, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("directory source: lstat root: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("directory source: root is a symlink")
	}
	if !fi.IsDir() {
		return errors.New("directory source: root is not a directory")
	}
	return nil
}

func (s *directorySource) kind() string { return "directory" }

func (s *directorySource) acquire(ctx context.Context, tc *trustedCatalog, w *stagingWriter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil {
		return errors.New("directory source: nil source")
	}
	if tc == nil {
		return errors.New("directory source: nil catalog")
	}
	if w == nil {
		return errors.New("directory source: nil writer")
	}

	// Open and retain a directory fd with O_NOFOLLOW|O_DIRECTORY. All descendant
	// Lstat/Open/ReadDir are openat-relative on that fd (no /dev/fd, no path re-walk).
	root, err := openAnchoredDir(s.dir)
	if err != nil {
		return fmt.Errorf("directory source: open root: %w", err)
	}
	defer root.Close()

	if err := scanDirectorySourceTree(root, tc); err != nil {
		return fmt.Errorf("directory source: initial tree: %w", err)
	}
	if s.deps.AfterFirstScan != nil {
		if err := s.deps.AfterFirstScan(); err != nil {
			return err
		}
	}

	// Catalog entries are path-sorted; copy only those declared paths.
	for _, e := range tc.Catalog.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.copyCatalogFile(root, e, w); err != nil {
			return err
		}
	}

	if s.deps.AfterAllCopies != nil {
		if err := s.deps.AfterAllCopies(); err != nil {
			return err
		}
	}

	// Fresh second scan so active mutation after the first inventory fails closed
	// unless the tree still exactly matches the catalog.
	if err := scanDirectorySourceTree(root, tc); err != nil {
		return fmt.Errorf("directory source: final tree: %w", err)
	}
	return nil
}

// scanDirectorySourceTree builds manifest-derived expected sets and runs the
// shared bounded exact-tree walk (path/depth/node limits, no symlinks/extras).
func scanDirectorySourceTree(root treeWalker, tc *trustedCatalog) error {
	files, dirs, err := expectedTreeFromManifest(tc.Catalog.Entries)
	if err != nil {
		return err
	}
	return verifyExactTree(root, files, dirs)
}

func (s *directorySource) copyCatalogFile(root treeWalker, e installEntry, w *stagingWriter) error {
	// Recheck every path component with Lstat before open.
	if err := ensurePathComponentsSafe(root, e.Path); err != nil {
		return fmt.Errorf("directory source: %q: %w", e.Path, err)
	}
	fi, err := root.Lstat(e.Path)
	if err != nil {
		return fmt.Errorf("directory source: lstat %q: %w", e.Path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("directory source: %q is a symlink", e.Path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("directory source: %q is not a regular file", e.Path)
	}
	if fi.Size() != e.Size {
		return fmt.Errorf("directory source: %q size %d != declared %d", e.Path, fi.Size(), e.Size)
	}

	if s.deps.BeforeOpenFile != nil {
		if err := s.deps.BeforeOpenFile(e.Path); err != nil {
			return err
		}
	}

	f, err := root.Open(e.Path)
	if err != nil {
		return fmt.Errorf("directory source: open %q: %w", e.Path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("directory source: stat %q: %w", e.Path, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("directory source: %q is not a regular file after open", e.Path)
	}
	if st.Size() != e.Size {
		return fmt.Errorf("directory source: %q size %d != declared %d after open", e.Path, st.Size(), e.Size)
	}

	// stagingWriter enforces exclusive create, exact size, hash, and sync.
	if err := w.writeFile(e.Path, f); err != nil {
		return fmt.Errorf("directory source: write %q: %w", e.Path, err)
	}
	return nil
}
