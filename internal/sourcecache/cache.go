// Package sourcecache optionally reuses bounded, immutable ranges of a media
// source. Callers retain authentication, source-version validation and routing.
// It never expands a requested range or preallocates an entire media file.
package sourcecache

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// MaxCachedRangeBytes bounds how much one cache miss can materialize. Larger
// requests and length=-1 requests retain the caller's streaming behavior.
const MaxCachedRangeBytes int64 = 4 << 20

var (
	ErrClosed       = errors.New("source range cache is closed")
	ErrInvalidRange = errors.New("invalid source byte range")
	ErrInvalidKey   = errors.New("invalid source generation key")
	errCacheIO      = errors.New("source range cache storage is unavailable")
)

type Config struct {
	// Dir is a parent directory. Only its dedicated eag-source-cache-v1 child
	// is cache-owned. Empty uses a private temporary directory.
	Dir         string
	BudgetBytes int64
	MaxEntries  int
}

// Snapshot contains aggregate counters, never source keys, paths or URLs.
type Snapshot struct {
	BudgetBytes       int64  `json:"budgetBytes"`
	CachedBytes       int64  `json:"cachedBytes"`
	ReservedBytes     int64  `json:"reservedBytes"`
	PinnedBytes       int64  `json:"pinnedBytes"`
	MaxEntries        int    `json:"maxEntries"`
	Entries           int    `json:"entries"`
	Inflight          int    `json:"inflight"`
	PinnedEntries     int    `json:"pinnedEntries"`
	Hits              uint64 `json:"hits"`
	Misses            uint64 `json:"misses"`
	Coalesced         uint64 `json:"coalesced"`
	CacheHitBytes     uint64 `json:"cacheHitBytes"`
	SourceReadBytes   uint64 `json:"sourceReadBytes"`
	Bypasses          uint64 `json:"bypasses"`
	Evictions         uint64 `json:"evictions"`
	StorageErrors     uint64 `json:"storageErrors"`
	CapturedBytes     uint64 `json:"capturedBytes"`
	CaptureDrops      uint64 `json:"captureDrops"`
	CaptureQueuePages int    `json:"captureQueuePages"`
	CaptureQueueBytes int64  `json:"captureQueueBytes"`
	CaptureQueueLimit int    `json:"captureQueueLimit"`
	Disabled          bool   `json:"disabled"`
}

type byteRange struct{ offset, length int64 }

func (r byteRange) covers(other byteRange) bool {
	return r.offset <= other.offset && other.offset-r.offset <= r.length-other.length
}

type entry struct {
	key  string
	span byteRange
	path string
	pins int // Includes readers promised to completed, still-waiting Open calls.
	lru  *list.Element
}

type flight struct {
	key      string
	span     byteRange
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	waiters  int
	finished bool
	result   *entry
	err      error
	capture  bool // Bytes already being consumed; never owns additional source work.
}

type Cache struct {
	mu             sync.Mutex
	cfg            Config
	directory      *cacheDirectory
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	closeOnce      sync.Once
	closeErr       error
	closed         bool
	disabled       bool // Storage failure bypasses the cache for the remaining lifetime.
	entries        map[string]map[byteRange]*entry
	flights        map[string]map[byteRange]*flight
	readers        map[*cachedReader]struct{}
	lru            list.List
	used           int64
	reserved       int64
	inflight       int // Includes canceled fills until their files have been removed.
	hits           uint64
	misses         uint64
	coalesced      uint64
	bypasses       uint64
	evictions      uint64
	storageErr     uint64
	captured       uint64
	captureMu      sync.Mutex // Only nonblocking enqueue; never held during file IO.
	captureStopped atomic.Bool
	captureQueue   chan capturePage
	captureDrops   atomic.Uint64
	// Initialized before the writer starts; tests may replace it before their
	// first enqueue to model slow disks without blocking real filesystem calls.
	writeCapturePage func([]byte) (string, error)
	hitBytes         atomic.Uint64
	sourceBytes      atomic.Uint64
}

func New(cfg Config) (*Cache, error) {
	if cfg.BudgetBytes <= 0 || cfg.MaxEntries < 0 || cfg.MaxEntries > 65536 {
		return nil, fmt.Errorf("source range cache requires a positive budget and at most 65536 entries")
	}
	if cfg.MaxEntries == 0 {
		cfg.MaxEntries = 4096
	}
	directory, err := openDirectory(cfg.Dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Cache{
		cfg: cfg, directory: directory, ctx: ctx, cancel: cancel,
		entries:      make(map[string]map[byteRange]*entry),
		flights:      make(map[string]map[byteRange]*flight),
		readers:      make(map[*cachedReader]struct{}),
		captureQueue: make(chan capturePage, captureQueuePages),
	}
	c.writeCapturePage = c.writePage
	c.wg.Add(1)
	go c.captureLoop()
	return c, nil
}

// Open returns exactly the requested range (or an unbounded stream for length
// -1). The caller must close the returned reader. fetch must obey its context
// and return the requested source range; this cache does not validate HTTP.
//
// key must identify immutable source bytes, including their generation. It
// must not contain URLs or credentials. The caller must check current access
// and source validity before every Open, including a cache hit.
//
// A missing range up to MaxCachedRangeBytes is filled before Open returns.
// Exact duplicates and covered subranges share that fill. Canceling one caller
// does not cancel another caller's fill. Capacity and storage failures bypass
// caching; source failures are returned without retrying the source.
func (c *Cache) Open(ctx context.Context, key string, offset, length int64, fetch func(context.Context, int64, int64) (io.ReadCloser, error)) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validKey(key) {
		return nil, ErrInvalidKey
	}
	if fetch == nil || offset < 0 || length < -1 || length > 0 && offset > math.MaxInt64-length {
		return nil, ErrInvalidRange
	}
	span := byteRange{offset, length}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if length == 0 {
		c.mu.Unlock()
		return io.NopCloser(strings.NewReader("")), nil
	}
	if length == -1 || length > MaxCachedRangeBytes || length > c.cfg.BudgetBytes || c.disabled {
		c.bypasses++
		c.mu.Unlock()
		return c.direct(ctx, offset, length, fetch)
	}
	for _, e := range c.entries[key] {
		if e.span.covers(span) {
			r, err := c.readerLocked(e, span, true, false)
			if err == nil {
				c.hits++
				c.mu.Unlock()
				return r, nil
			}
			c.disableLocked()
			c.bypasses++
			c.mu.Unlock()
			return c.direct(ctx, offset, length, fetch)
		}
	}
	for _, f := range c.flights[key] {
		if f.span.covers(span) {
			f.waiters++
			c.coalesced++
			c.mu.Unlock()
			return c.wait(ctx, f, span, true, fetch)
		}
	}
	c.misses++
	if !c.reserveLocked(length) {
		c.bypasses++
		c.mu.Unlock()
		return c.direct(ctx, offset, length, fetch)
	}
	// A fill belongs to its current waiters, not to the initiating request's
	// deadline. WithoutCancel preserves request values needed by source access.
	fillCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	f := &flight{key: key, span: span, ctx: fillCtx, cancel: cancel, done: make(chan struct{}), waiters: 1}
	if c.flights[key] == nil {
		c.flights[key] = make(map[byteRange]*flight)
	}
	c.flights[key][span] = f
	c.inflight++
	c.wg.Add(1)
	c.mu.Unlock()
	go c.fill(f, fetch)
	return c.wait(ctx, f, span, false, fetch)
}

func (c *Cache) wait(ctx context.Context, f *flight, span byteRange, hit bool, fetch func(context.Context, int64, int64) (io.ReadCloser, error)) (io.ReadCloser, error) {
	select {
	case <-ctx.Done():
	case <-c.ctx.Done():
	case <-f.done:
	}
	c.mu.Lock()
	f.waiters--
	err := ctx.Err()
	if err == nil && c.closed {
		err = ErrClosed
	}
	if err != nil {
		if f.finished && f.result != nil {
			f.result.pins--
		} else if !f.finished && f.waiters == 0 && !f.capture {
			c.detachLocked(f)
			f.cancel()
		}
		c.mu.Unlock()
		return nil, err
	}
	if f.err != nil {
		err = f.err
		if errors.Is(err, errCacheIO) {
			c.bypasses++
		}
		c.mu.Unlock()
		if errors.Is(err, errCacheIO) {
			return c.direct(ctx, span.offset, span.length, fetch)
		}
		return nil, err
	}
	r, err := c.readerLocked(f.result, span, hit, true)
	if err != nil {
		c.disableLocked()
		c.bypasses++
	}
	c.mu.Unlock()
	if err != nil {
		return c.direct(ctx, span.offset, span.length, fetch)
	}
	return r, nil
}

func (c *Cache) direct(ctx context.Context, offset, length int64, fetch func(context.Context, int64, int64) (io.ReadCloser, error)) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := fetch(ctx, offset, length)
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, err
	}
	if body == nil {
		return nil, io.ErrUnexpectedEOF
	}
	return countSource(body, length, &c.sourceBytes), nil
}

// reserveLocked reserves bytes and an entry slot before creating any file.
func (c *Cache) reserveLocked(length int64) bool {
	for c.used+c.reserved > c.cfg.BudgetBytes-length || c.lru.Len()+c.inflight >= c.cfg.MaxEntries {
		var victim *entry
		for item := c.lru.Back(); item != nil; item = item.Prev() {
			candidate := item.Value.(*entry)
			if candidate.pins == 0 {
				victim = candidate
				break
			}
		}
		if victim == nil {
			return false
		}
		if err := os.Remove(victim.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			c.disableLocked()
			return false
		}
		delete(c.entries[victim.key], victim.span)
		if len(c.entries[victim.key]) == 0 {
			delete(c.entries, victim.key)
		}
		c.lru.Remove(victim.lru)
		c.used -= victim.span.length
		c.evictions++
	}
	c.reserved += length
	return true
}

func (c *Cache) detachLocked(f *flight) {
	if c.flights[f.key][f.span] == f {
		delete(c.flights[f.key], f.span)
		if len(c.flights[f.key]) == 0 {
			delete(c.flights, f.key)
		}
	}
}

func (c *Cache) disableLocked() {
	c.disabled = true
	c.storageErr++
	c.captureStopped.Store(true)
}

func (c *Cache) fill(f *flight, fetch func(context.Context, int64, int64) (io.ReadCloser, error)) {
	defer c.wg.Done()
	defer f.cancel()
	stop := context.AfterFunc(c.ctx, f.cancel)
	defer stop()
	path, err := c.writeRange(f, fetch)
	c.finish(f, path, err)
}

func (c *Cache) finish(f *flight, path string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.detachLocked(f)
	c.inflight--
	if err == nil && (c.closed || f.waiters == 0 && !f.capture) {
		err = context.Canceled
	}
	if err != nil {
		// An unremovable file keeps its reservation. Disable further writes so
		// an optional cache failure cannot grow disk usage past its budget.
		removed := true
		if path != "" {
			removeErr := os.Remove(path)
			removed = removeErr == nil || errors.Is(removeErr, os.ErrNotExist)
		}
		if removed {
			c.reserved -= f.span.length
		}
		if !removed || errors.Is(err, errCacheIO) {
			c.disableLocked()
		}
		f.err = err
	} else {
		c.reserved -= f.span.length
		c.used += f.span.length
		if f.capture {
			c.captured += uint64(f.span.length)
		}
		e := &entry{key: f.key, span: f.span, path: path, pins: f.waiters}
		e.lru = c.lru.PushFront(e)
		if c.entries[f.key] == nil {
			c.entries[f.key] = make(map[byteRange]*entry)
		}
		c.entries[f.key][f.span] = e
		f.result = e
	}
	f.finished = true
	close(f.done)
}

func (c *Cache) writeRange(f *flight, fetch func(context.Context, int64, int64) (io.ReadCloser, error)) (string, error) {
	if err := f.ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(c.directory.blocks, "part-")
	if err != nil {
		return "", errCacheIO
	}
	path := file.Name()
	body, err := c.direct(f.ctx, f.span.offset, f.span.length, fetch)
	if err != nil {
		_ = file.Close()
		return path, err
	}
	stop := context.AfterFunc(f.ctx, func() { _ = body.Close() })
	defer stop()
	defer body.Close()
	// Separate read and write failures: only storage failures may transparently
	// retry through the uncached path. An upstream failure must not be retried.
	buffer := make([]byte, 32<<10)
	remaining := f.span.length
	for remaining > 0 {
		if err = f.ctx.Err(); err != nil {
			break
		}
		n, readErr := body.Read(buffer[:min(int64(len(buffer)), remaining)])
		if n > 0 {
			written, writeErr := file.Write(buffer[:n])
			if writeErr != nil || written != n {
				err = errCacheIO
				break
			}
			remaining -= int64(n)
		}
		if readErr != nil && !(remaining == 0 && errors.Is(readErr, io.EOF)) {
			err = readErr
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			break
		}
		if n == 0 && readErr == nil {
			err = io.ErrNoProgress
			break
		}
	}
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = errCacheIO
	}
	if err != nil {
		return path, err
	}
	complete := filepath.Join(c.directory.blocks, filepath.Base(path)+".block")
	if err = os.Rename(path, complete); err != nil {
		return path, errCacheIO
	}
	return complete, nil
}

func (c *Cache) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	queued := len(c.captureQueue)
	snapshot := Snapshot{
		BudgetBytes: c.cfg.BudgetBytes, MaxEntries: c.cfg.MaxEntries,
		CachedBytes: c.used, ReservedBytes: c.reserved,
		Entries: c.lru.Len(), Inflight: c.inflight,
		Hits: c.hits, Misses: c.misses, Coalesced: c.coalesced,
		CacheHitBytes: c.hitBytes.Load(), SourceReadBytes: c.sourceBytes.Load(),
		Bypasses: c.bypasses, Evictions: c.evictions, StorageErrors: c.storageErr,
		CapturedBytes: c.captured, CaptureDrops: c.captureDrops.Load(),
		CaptureQueuePages: queued, CaptureQueueBytes: int64(queued) * capturePageBytes, CaptureQueueLimit: captureQueuePages,
		Disabled: c.disabled,
	}
	for item := c.lru.Front(); item != nil; item = item.Next() {
		e := item.Value.(*entry)
		if e.pins > 0 {
			snapshot.PinnedEntries++
			snapshot.PinnedBytes += e.span.length
		}
	}
	return snapshot
}

func validKey(key string) bool {
	return key != "" && len(key) <= 512 && !strings.ContainsAny(key, " \t\r\n/?\\\x00") && !strings.Contains(key, "://")
}

// Close cancels cache fills, closes cached readers, and removes only cache-owned
// raw blocks. Uncached readers remain owned by their callers.
func (c *Cache) Close() error {
	c.closeOnce.Do(func() {
		// The handoff lock is never held by a reader during disk IO. Once this
		// barrier completes, no producer can send behind the writer's drain.
		c.captureMu.Lock()
		c.captureStopped.Store(true)
		c.captureMu.Unlock()
		c.mu.Lock()
		c.closed = true
		c.cancel()
		readers := make([]*cachedReader, 0, len(c.readers))
		for reader := range c.readers {
			readers = append(readers, reader)
		}
		c.mu.Unlock()
		for _, reader := range readers {
			_ = reader.Close()
		}
		c.wg.Wait()
		c.closeErr = c.directory.close()
	})
	return c.closeErr
}
