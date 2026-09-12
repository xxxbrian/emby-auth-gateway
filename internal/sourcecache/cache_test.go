package sourcecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestCache(t *testing.T, budget int64, entries int) *Cache {
	t.Helper()
	c, err := New(Config{Dir: t.TempDir(), BudgetBytes: budget, MaxEntries: entries})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func readAndClose(t *testing.T, r io.ReadCloser, err error) []byte {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline:
			t.Fatal("condition did not become true")
		case <-tick.C:
		}
	}
}

// A logical 50GiB source requires no large allocation or disk fixture. Each
// byte is determined by its absolute offset; reads expose any over-fetching.
type patternedSource struct {
	offset int64
	reads  *atomic.Int64
	closes *atomic.Int64
}

func (r *patternedSource) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte((r.offset + int64(i)) % 251)
	}
	r.offset += int64(len(p))
	r.reads.Add(int64(len(p)))
	return len(p), nil
}

func (r *patternedSource) Close() error { r.closes.Add(1); return nil }

func pattern(offset, length int64) []byte {
	result := make([]byte, length)
	for i := range result {
		result[i] = byte((offset + int64(i)) % 251)
	}
	return result
}

func TestBoundedRangeOn50GiBSourceAndCoveredReuse(t *testing.T) {
	c := newTestCache(t, 1<<20, 10)
	var opens, reads, closes atomic.Int64
	const offset = 50<<30 - 100
	fetch := func(ctx context.Context, off, length int64) (io.ReadCloser, error) {
		opens.Add(1)
		if off != offset || length != 31 {
			t.Errorf("cache expanded the source range: %d + %d", off, length)
		}
		return &patternedSource{off, &reads, &closes}, nil
	}
	r, err := c.Open(context.Background(), "generation-1", offset, 31, fetch)
	if got := readAndClose(t, r, err); !bytes.Equal(got, pattern(offset, 31)) {
		t.Fatal("initial range differs from source")
	}
	r, err = c.Open(context.Background(), "generation-1", offset+3, 9, fetch)
	if got := readAndClose(t, r, err); !bytes.Equal(got, pattern(offset+3, 9)) {
		t.Fatal("covered subrange differs from source")
	}
	if opens.Load() != 1 || reads.Load() != 31 || closes.Load() != 1 {
		t.Fatalf("source work: opens=%d reads=%d closes=%d", opens.Load(), reads.Load(), closes.Load())
	}
	s := c.Snapshot()
	if s.CachedBytes != 31 || s.Entries != 1 || s.ReservedBytes != 0 || s.Hits != 1 || s.SourceReadBytes != 31 || s.CacheHitBytes != 9 {
		t.Fatalf("snapshot: %+v", s)
	}
	items, err := os.ReadDir(c.directory.blocks)
	if err != nil || len(items) != 1 {
		t.Fatalf("cache files: %v %v", items, err)
	}
	st, err := items[0].Info()
	if err != nil || st.Size() != 31 {
		t.Fatalf("cache allocated more than requested bytes: %v %v", st, err)
	}
}

func TestLargeRangeStreamsWithoutMaterializing(t *testing.T) {
	c := newTestCache(t, 8<<20, 2)
	var reads, closes atomic.Int64
	fetch := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		return &patternedSource{offset, &reads, &closes}, nil
	}
	for _, length := range []int64{50 << 30, -1} {
		r, err := c.Open(context.Background(), "generation", 0, length, fetch)
		if err != nil {
			t.Fatal(err)
		}
		before := reads.Load()
		got := make([]byte, 123)
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatal(err)
		}
		if reads.Load()-before != 123 || !bytes.Equal(got, pattern(0, 123)) {
			t.Fatal("stream read ahead")
		}
		_ = r.Close()
		_ = r.Close()
	}
	s := c.Snapshot()
	if s.Entries != 0 || s.CachedBytes != 0 || s.ReservedBytes != 0 || s.SourceReadBytes != 246 || s.Bypasses != 2 || closes.Load() != 2 {
		t.Fatalf("large stream allocated cache or double-closed source: %+v closes=%d", s, closes.Load())
	}
}

type blockedSource struct {
	ctx     context.Context
	release <-chan struct{}
	closes  *atomic.Int64
	value   byte
}

func (r *blockedSource) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.release:
		for i := range p {
			p[i] = r.value
		}
		return len(p), nil
	}
}

func (r *blockedSource) Close() error { r.closes.Add(1); return nil }

func TestLeaderCancellationDoesNotPoisonSharedRange(t *testing.T) {
	c := newTestCache(t, 4096, 4)
	var opens, closes atomic.Int64
	release := make(chan struct{})
	started := make(chan context.Context, 1)
	fetch := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		opens.Add(1)
		started <- ctx
		return &blockedSource{ctx, release, &closes, 's'}, nil
	}
	leader, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaderResult := make(chan error, 1)
	go func() {
		r, err := c.Open(leader, "shared-generation", 0, 128, fetch)
		if r != nil {
			_ = r.Close()
		}
		leaderResult <- err
	}()
	fillContext := <-started
	followerResult := make(chan []byte, 1)
	followerError := make(chan error, 1)
	go func() {
		r, err := c.Open(context.Background(), "shared-generation", 32, 64, fetch)
		if err != nil {
			followerError <- err
			return
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			followerError <- err
			return
		}
		followerResult <- data
	}()
	eventually(t, func() bool { return c.Snapshot().Coalesced == 1 })
	cancel()
	if err := <-leaderResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader cancellation: %v", err)
	}
	if err := fillContext.Err(); err != nil {
		t.Fatalf("one user canceled another user's shared fill: %v", err)
	}
	close(release)
	select {
	case err := <-followerError:
		t.Fatal(err)
	case data := <-followerResult:
		if !bytes.Equal(data, bytes.Repeat([]byte{'s'}, 64)) {
			t.Fatal("shared subrange corrupt")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("follower did not finish")
	}
	eventually(t, func() bool { return closes.Load() == 1 && c.Snapshot().PinnedEntries == 0 })
	if opens.Load() != 1 || c.Snapshot().ReservedBytes != 0 {
		t.Fatal("shared range fetched twice or leaked a reservation")
	}
}

func TestLastWaiterCancellationReclaimsFill(t *testing.T) {
	c := newTestCache(t, 4096, 2)
	var closes atomic.Int64
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		r, err := c.Open(ctx, "generation", 0, 256, func(fill context.Context, offset, length int64) (io.ReadCloser, error) {
			close(started)
			return &blockedSource{fill, make(chan struct{}), &closes, 0}, nil
		})
		if r != nil {
			_ = r.Close()
		}
		done <- err
	}()
	<-started
	if s := c.Snapshot(); s.ReservedBytes != 256 || s.Inflight != 1 || s.CachedBytes != 0 {
		t.Fatalf("fill must reserve before writing: %+v", s)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	eventually(t, func() bool { return c.Snapshot().Inflight == 0 })
	if s := c.Snapshot(); s.ReservedBytes != 0 || s.Entries != 0 || closes.Load() != 1 {
		t.Fatalf("cancellation leaked work: %+v closes=%d", s, closes.Load())
	}
	items, err := os.ReadDir(c.directory.blocks)
	if err != nil || len(items) != 0 {
		t.Fatalf("canceled fill left files: %v %v", items, err)
	}
	r, err := c.Open(context.Background(), "generation", 0, 256, constantFetch('n'))
	if got := readAndClose(t, r, err); !bytes.Equal(got, bytes.Repeat([]byte{'n'}, 256)) {
		t.Fatal("canceled fill poisoned retry")
	}
}

func constantFetch(value byte) func(context.Context, int64, int64) (io.ReadCloser, error) {
	return func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{value}, int(length)))), nil
	}
}

func TestPinnedReadersAndReservationsPreventEviction(t *testing.T) {
	c := newTestCache(t, 32, 2)
	a, err := c.Open(context.Background(), "a", 0, 16, constantFetch('a'))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := c.Open(context.Background(), "b", 0, 16, constantFetch('b'))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	bypass, err := c.Open(context.Background(), "c", 0, 16, constantFetch('c'))
	_ = readAndClose(t, bypass, err)
	if s := c.Snapshot(); s.Bypasses != 1 || s.CachedBytes != 32 || s.PinnedEntries != 2 || s.Evictions != 0 {
		t.Fatalf("pinned cache should bypass without eviction: %+v", s)
	}
	_ = a.Close()
	third, err := c.Open(context.Background(), "c", 0, 16, constantFetch('c'))
	_ = readAndClose(t, third, err)
	if got := readAndClose(t, b, nil); !bytes.Equal(got, bytes.Repeat([]byte{'b'}, 16)) {
		t.Fatal("LRU eviction corrupted an active reader")
	}
	if s := c.Snapshot(); s.CachedBytes != 32 || s.Entries != 2 || s.Evictions != 1 || s.PinnedEntries != 0 {
		t.Fatalf("quota or reader pin leaked: %+v", s)
	}
}

func TestEntryCountLimitsTinyRanges(t *testing.T) {
	c := newTestCache(t, 4096, 2)
	for _, key := range []string{"a", "b", "c", "d"} {
		r, err := c.Open(context.Background(), key, 0, 1, constantFetch('x'))
		_ = readAndClose(t, r, err)
	}
	if s := c.Snapshot(); s.Entries != 2 || s.Evictions != 2 || s.CachedBytes != 2 {
		t.Fatalf("entry limit ignored: %+v", s)
	}
}

func TestStorageFailureFallsBackAndSourceFailureDoesNotRetry(t *testing.T) {
	t.Run("storage", func(t *testing.T) {
		c := newTestCache(t, 4096, 2)
		if err := os.Remove(c.directory.blocks); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.directory.blocks, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		var opens atomic.Int64
		fetch := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
			opens.Add(1)
			return constantFetch('s')(ctx, offset, length)
		}
		r, err := c.Open(context.Background(), "generation", 0, 64, fetch)
		if got := readAndClose(t, r, err); !bytes.Equal(got, bytes.Repeat([]byte{'s'}, 64)) {
			t.Fatal("optional cache failure affected source bytes")
		}
		if s := c.Snapshot(); !s.Disabled || s.StorageErrors != 1 || s.ReservedBytes != 0 || s.Entries != 0 || opens.Load() != 1 {
			t.Fatalf("cache failure was not isolated: %+v opens=%d", s, opens.Load())
		}
	})
	t.Run("source", func(t *testing.T) {
		c := newTestCache(t, 4096, 2)
		var opens atomic.Int64
		fetch := func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
			opens.Add(1)
			return io.NopCloser(bytes.NewReader([]byte("too short"))), nil
		}
		r, err := c.Open(context.Background(), "generation", 0, 64, fetch)
		if r != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("short source accepted: reader=%v err=%v", r, err)
		}
		if s := c.Snapshot(); s.Disabled || s.ReservedBytes != 0 || s.Entries != 0 || opens.Load() != 1 {
			t.Fatalf("source failure was retried or cached: %+v opens=%d", s, opens.Load())
		}
	})
}

func TestValidateRangesAndGenerationKeys(t *testing.T) {
	c := newTestCache(t, 4096, 2)
	fetch := func(context.Context, int64, int64) (io.ReadCloser, error) {
		t.Fatal("invalid or empty request fetched source")
		return nil, nil
	}
	for _, tc := range []struct {
		key            string
		offset, length int64
		err            error
	}{
		{"", 0, 1, ErrInvalidKey},
		{"https://media.example/video?token=secret", 0, 1, ErrInvalidKey},
		{"generation", -1, 1, ErrInvalidRange},
		{"generation", 0, -2, ErrInvalidRange},
		{"generation", math.MaxInt64, 1, ErrInvalidRange},
	} {
		r, err := c.Open(context.Background(), tc.key, tc.offset, tc.length, fetch)
		if r != nil || !errors.Is(err, tc.err) {
			t.Fatalf("validation result: %v %v", r, err)
		}
	}
	r, err := c.Open(context.Background(), "generation", math.MaxInt64, 0, fetch)
	if data := readAndClose(t, r, err); len(data) != 0 {
		t.Fatal("zero range not empty")
	}
}

func TestGenerationIsolationAndConcurrentEviction(t *testing.T) {
	c := newTestCache(t, 1024, 8)
	var wg sync.WaitGroup
	failed := make(chan error, 32)
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := string(rune('a' + worker))
			value := byte(worker)
			for range 30 {
				r, err := c.Open(context.Background(), key, 128, 128, constantFetch(value))
				if err != nil {
					failed <- err
					return
				}
				data, readErr := io.ReadAll(r)
				_ = r.Close()
				if readErr != nil || !bytes.Equal(data, bytes.Repeat([]byte{value}, 128)) {
					failed <- errors.New("concurrent eviction corrupted or confused a source generation")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failed)
	for err := range failed {
		t.Error(err)
	}
	if s := c.Snapshot(); s.CachedBytes+s.ReservedBytes > 1024 || s.Entries+s.Inflight > 8 || s.PinnedEntries != 0 {
		t.Fatalf("concurrency exceeded limits or leaked pins: %+v", s)
	}
}

func TestCloseCancelsFillAndLeavesUncachedReaderOwnedByCaller(t *testing.T) {
	c := newTestCache(t, 4096, 2)
	var reads, closes atomic.Int64
	direct, err := c.Open(context.Background(), "stream", 0, -1, func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		return &patternedSource{offset, &reads, &closes}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		r, err := c.Open(context.Background(), "pending", 0, 64, func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
			close(started)
			return &blockedSource{ctx, make(chan struct{}), &atomic.Int64{}, 0}, nil
		})
		if r != nil {
			_ = r.Close()
		}
		result <- err
	}()
	<-started
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("pending request after Close: %v", err)
	}
	if _, err := io.ReadFull(direct, make([]byte, 10)); err != nil || reads.Load() != 10 || closes.Load() != 0 {
		t.Fatal("cache shutdown interfered with uncached source ownership")
	}
	if r, err := c.Open(context.Background(), "new", 0, 1, constantFetch(0)); r != nil || !errors.Is(err, ErrClosed) {
		t.Fatal("closed cache accepted work")
	}
}

func TestDirectoryOwnershipLockAndCleanup(t *testing.T) {
	parent := t.TempDir()
	unrelated := filepath.Join(parent, "keep-me")
	if err := os.WriteFile(unrelated, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Dir: parent, BudgetBytes: 4096}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := New(cfg); err == nil {
		_ = other.Close()
		t.Fatal("two cache instances acquired the same directory")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, directoryName)
	if err := os.Mkdir(filepath.Join(root, "blocks"), 0700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "blocks", "stale-part")
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale owned blocks not cleaned: %v", err)
	}
	if data, err := os.ReadFile(unrelated); err != nil || string(data) != "original" {
		t.Fatal("cleanup affected unrelated files")
	}
}

func TestDirectoryRefusesUnownedDataAndSymlinks(t *testing.T) {
	for _, kind := range []string{"foreign", "root-symlink", "lock-symlink", "marker-symlink"} {
		t.Run(kind, func(t *testing.T) {
			parent, target := t.TempDir(), t.TempDir()
			root := filepath.Join(parent, directoryName)
			precious := filepath.Join(target, "precious")
			if err := os.WriteFile(precious, []byte(markerContent), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "root-symlink" {
				if err := os.Symlink(target, root); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "foreign":
					if err := os.WriteFile(filepath.Join(root, "user-data"), []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
				case "lock-symlink":
					if err := os.Symlink(precious, filepath.Join(root, ".lock")); err != nil {
						t.Fatal(err)
					}
				case "marker-symlink":
					if err := os.Symlink(precious, filepath.Join(root, markerName)); err != nil {
						t.Fatal(err)
					}
				}
			}
			c, err := New(Config{Dir: parent, BudgetBytes: 4096})
			if err == nil {
				_ = c.Close()
				t.Fatal("accepted foreign directory or symlink")
			}
			if data, err := os.ReadFile(precious); err != nil || string(data) != markerContent {
				t.Fatal("directory validation modified a symlink target")
			}
		})
	}
}
