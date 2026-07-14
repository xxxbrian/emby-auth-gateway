package embyweb

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStagingWriter(t *testing.T, files []fixtureFile) (*stagingWriter, *trustedCatalog, string) {
	t.Helper()
	tc := buildSyntheticCatalog(t, files, "writer-conc", "1.0.0")
	dir := t.TempDir()
	filesDir := filepath.Join(dir, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filesDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	w, err := newStagingWriter(root, tc, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w, tc, filesDir
}

func fixtureDataMap(files []fixtureFile) map[string][]byte {
	m := make(map[string][]byte, len(files))
	for _, f := range files {
		m[f.Path] = f.Data
	}
	return m
}

func TestStagingWriterConcurrentDistinctPaths(t *testing.T) {
	files := readyMinimalFiles()
	w, tc, _ := newTestStagingWriter(t, files)
	data := fixtureDataMap(files)

	var wg sync.WaitGroup
	errCh := make(chan error, len(tc.Catalog.Entries))
	for _, e := range tc.Catalog.Entries {
		e := e
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- w.writeFile(e.Path, bytes.NewReader(data[e.Path]))
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	}
	if err := w.complete(); err != nil {
		t.Fatal(err)
	}
}

func TestStagingWriterConcurrentSamePath(t *testing.T) {
	files := readyMinimalFiles()
	w, _, _ := newTestStagingWriter(t, files)
	data := files[0].Data // index.html typically
	path := files[0].Path
	for _, f := range files {
		if f.Path == "modules/app.js" {
			path = f.Path
			data = f.Data
			break
		}
	}

	// Gate so both goroutines attempt reserve around the same time.
	start := make(chan struct{})
	var wg sync.WaitGroup
	var success, fail atomic.Int64
	var failErrs []string
	var mu sync.Mutex

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := w.writeFile(path, bytes.NewReader(data))
			if err == nil {
				success.Add(1)
				return
			}
			fail.Add(1)
			mu.Lock()
			failErrs = append(failErrs, err.Error())
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if success.Load() != 1 {
		t.Fatalf("success=%d want 1", success.Load())
	}
	if fail.Load() != 1 {
		t.Fatalf("fail=%d want 1 errs=%v", fail.Load(), failErrs)
	}
	msg := failErrs[0]
	if !strings.Contains(msg, "in flight") && !strings.Contains(msg, "duplicate") {
		t.Fatalf("fail err=%q want in flight or duplicate", msg)
	}
}

func TestStagingWriterReservationReleaseRetry(t *testing.T) {
	files := readyMinimalFiles()
	w, _, _ := newTestStagingWriter(t, files)
	var path string
	var data []byte
	for _, f := range files {
		if f.Path == "modules/app.js" {
			path = f.Path
			data = f.Data
			break
		}
	}

	// First attempt: wrong hash fails after reserve; reservation must release.
	bad := append([]byte(nil), data...)
	if len(bad) > 0 {
		bad[0] ^= 0xff
	}
	err := w.writeFile(path, bytes.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err=%v", err)
	}

	// Retry with correct bytes must succeed (reservation released).
	if err := w.writeFile(path, bytes.NewReader(data)); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
}

func TestStagingWriterCompleteDuringInFlight(t *testing.T) {
	files := readyMinimalFiles()
	w, tc, _ := newTestStagingWriter(t, files)
	data := fixtureDataMap(files)

	// Write all but one path first.
	var holdPath string
	for _, e := range tc.Catalog.Entries {
		if e.Path == "modules/app.js" {
			holdPath = e.Path
			continue
		}
		if err := w.writeFile(e.Path, bytes.NewReader(data[e.Path])); err != nil {
			t.Fatal(err)
		}
	}
	if holdPath == "" {
		t.Fatal("missing hold path")
	}

	// Block body read so write stays in flight.
	started := make(chan struct{})
	release := make(chan struct{})
	blocked := &blockingReader{
		data:    data[holdPath],
		started: started,
		release: release,
	}

	var writeErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		writeErr = w.writeFile(holdPath, blocked)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("write did not start")
	}

	if err := w.complete(); err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("complete during in-flight: %v", err)
	}

	close(release)
	wg.Wait()
	if writeErr != nil {
		t.Fatalf("held write: %v", writeErr)
	}
	if err := w.complete(); err != nil {
		t.Fatal(err)
	}
}

// blockingReader signals when Read is first called, then waits for release
// before returning the full payload (simulates slow network body).
type blockingReader struct {
	data    []byte
	off     int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

func TestStagingWriterCompleteMissing(t *testing.T) {
	files := readyMinimalFiles()
	w, _, _ := newTestStagingWriter(t, files)
	if err := w.complete(); err == nil {
		t.Fatal("expected missing entries")
	}
}

func TestStagingWriterNilReader(t *testing.T) {
	files := readyMinimalFiles()
	w, _, _ := newTestStagingWriter(t, files)
	err := w.writeFile("index.html", nil)
	if err == nil || !strings.Contains(err.Error(), "nil reader") {
		t.Fatalf("err=%v", err)
	}
}
