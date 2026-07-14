package embyweb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
)

// acquisitionSource materializes trusted catalog files into a stagingWriter.
// Concrete directory/archive/URL sources are later lanes; tests use fakes.
type acquisitionSource interface {
	// kind returns a short diagnostic label (e.g. "fake", "dir", "archive", "url").
	kind() string
	// acquire writes every catalog entry exactly once through w.
	acquire(ctx context.Context, tc *trustedCatalog, w *stagingWriter) error
}

// stagingWriter accepts exclusive catalog-entry writes under a rooted files tree.
// It pre-creates only catalog-required directories and rejects arbitrary paths.
// Distinct declared paths may be written concurrently (e.g. URL concurrency=8);
// the same path cannot. Network/body reads are not globally serialized.
type stagingWriter struct {
	filesRoot *os.Root
	tc        *trustedCatalog
	expected  map[string]installEntry // immutable after construction
	syncFile  func(*os.File) error

	mu       sync.Mutex
	written  map[string]struct{}
	inFlight map[string]struct{}
}

func newStagingWriter(filesRoot *os.Root, tc *trustedCatalog, syncFile func(*os.File) error) (*stagingWriter, error) {
	if filesRoot == nil || tc == nil {
		return nil, errors.New("stagingWriter: nil root or catalog")
	}
	if syncFile == nil {
		syncFile = defaultSyncFile
	}
	expected := make(map[string]installEntry, len(tc.Catalog.Entries))
	for _, e := range tc.Catalog.Entries {
		expected[e.Path] = e
	}
	w := &stagingWriter{
		filesRoot: filesRoot,
		tc:        tc,
		expected:  expected,
		written:   make(map[string]struct{}, len(expected)),
		inFlight:  make(map[string]struct{}, len(expected)),
		syncFile:  syncFile,
	}
	if err := w.precreateDirs(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *stagingWriter) precreateDirs() error {
	dirs := map[string]struct{}{".": {}}
	for _, e := range w.tc.Catalog.Entries {
		dir := path.Dir(e.Path)
		for dir != "." && dir != "/" && dir != "" {
			dirs[dir] = struct{}{}
			parent := path.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	// Create deepest paths by sorting length ascending so parents exist first.
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		if d != "." {
			ordered = append(ordered, d)
		}
	}
	// Simple insertion by segment count.
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if strings.Count(ordered[j], "/") < strings.Count(ordered[i], "/") {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, d := range ordered {
		if err := w.filesRoot.Mkdir(d, 0o755); err != nil && !os.IsExist(err) {
			// Mkdir may fail if parent missing; create parents.
			if err := mkdirAllRoot(w.filesRoot, d, 0o755); err != nil {
				return fmt.Errorf("mkdir %q: %w", d, err)
			}
		}
	}
	return nil
}

func mkdirAllRoot(root *os.Root, rel string, perm os.FileMode) error {
	if rel == "." || rel == "" {
		return nil
	}
	parts := strings.Split(rel, "/")
	cur := ""
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur = path.Join(cur, p)
		}
		if err := root.Mkdir(cur, perm); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// reservePath atomically validates rel and marks it in-flight. The caller must
// call releasePath (failure) or finishPath (success after sync+close).
func (w *stagingWriter) reservePath(rel string) (installEntry, error) {
	if !validAssetPath(rel) {
		return installEntry{}, fmt.Errorf("stagingWriter: invalid path %q", rel)
	}
	e, ok := w.expected[rel]
	if !ok {
		return installEntry{}, fmt.Errorf("stagingWriter: undeclared path %q", rel)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, done := w.written[rel]; done {
		return installEntry{}, fmt.Errorf("stagingWriter: duplicate write for %q", rel)
	}
	if _, inflight := w.inFlight[rel]; inflight {
		return installEntry{}, fmt.Errorf("stagingWriter: write already in flight for %q", rel)
	}
	w.inFlight[rel] = struct{}{}
	return e, nil
}

func (w *stagingWriter) releasePath(rel string) {
	w.mu.Lock()
	delete(w.inFlight, rel)
	w.mu.Unlock()
}

func (w *stagingWriter) finishPath(rel string) {
	w.mu.Lock()
	delete(w.inFlight, rel)
	w.written[rel] = struct{}{}
	w.mu.Unlock()
}

// writeFile writes a single catalog entry exclusively. r is read for exactly
// entry.Size+1 bytes; hash and size must match the catalog entry.
// Distinct paths may run concurrently; the same path is rejected if already
// written or currently in flight. Body reads are not held under the mutex.
func (w *stagingWriter) writeFile(rel string, r io.Reader) error {
	if r == nil {
		return errors.New("stagingWriter: nil reader")
	}
	e, err := w.reservePath(rel)
	if err != nil {
		return err
	}
	// On any failure after reserve, drop in-flight so a later retry can proceed.
	// Success path calls finishPath instead.
	success := false
	defer func() {
		if !success {
			w.releasePath(rel)
		}
	}()

	// Exclusive create (outside mutex so concurrent distinct paths proceed).
	f, err := w.filesRoot.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create %q: %w", rel, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	h := sha256.New()
	// Read at most size+1 to detect oversize.
	limited := io.LimitReader(r, e.Size+1)
	n, err := io.Copy(io.MultiWriter(f, h), limited)
	if err != nil {
		_ = w.filesRoot.Remove(rel)
		return fmt.Errorf("write %q: %w", rel, err)
	}
	if n != e.Size {
		_ = w.filesRoot.Remove(rel)
		return fmt.Errorf("write %q: size %d != declared %d", rel, n, e.Size)
	}
	// Ensure source has no trailing bytes (best-effort single-byte peek).
	var extra [1]byte
	if m, _ := r.Read(extra[:]); m > 0 {
		_ = w.filesRoot.Remove(rel)
		return fmt.Errorf("write %q: source longer than declared size", rel)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if sum != e.SHA256 {
		_ = w.filesRoot.Remove(rel)
		return fmt.Errorf("write %q: sha256 mismatch", rel)
	}
	if err := w.syncFile(f); err != nil {
		_ = w.filesRoot.Remove(rel)
		return fmt.Errorf("sync %q: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		closed = true
		_ = w.filesRoot.Remove(rel)
		return fmt.Errorf("close %q: %w", rel, err)
	}
	closed = true

	// Mark written only after durable sync+close.
	w.finishPath(rel)
	success = true
	return nil
}

// complete reports whether every catalog entry was written exactly once.
// It fails if any write is still in flight.
func (w *stagingWriter) complete() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.inFlight) > 0 {
		for p := range w.inFlight {
			return fmt.Errorf("stagingWriter: write still in flight for %q", p)
		}
	}
	if len(w.written) != len(w.expected) {
		for p := range w.expected {
			if _, ok := w.written[p]; !ok {
				return fmt.Errorf("stagingWriter: missing entry %q", p)
			}
		}
		return fmt.Errorf("stagingWriter: written count %d != expected %d", len(w.written), len(w.expected))
	}
	return nil
}

func defaultSyncFile(f *os.File) error {
	if f == nil {
		return errors.New("nil file")
	}
	return f.Sync()
}
