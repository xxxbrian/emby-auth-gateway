package sourcecache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCaptureOnlyCachesConsumedBytesAndCloseFlushesPartial(t *testing.T) {
	c := newTestCache(t, 1<<20, 16)
	var reads, closes atomic.Int64
	const offset = 50<<30 - 100
	r := c.Capture("generation", offset, &patternedSource{offset, &reads, &closes})
	data := make([]byte, 37)
	if _, err := io.ReadFull(r, data); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 37 || c.Snapshot().CapturedBytes != 0 {
		t.Fatal("capture read ahead or published an incomplete page before Close")
	}
	_ = r.Close()
	_ = r.Close()
	if reads.Load() != 37 || closes.Load() != 1 {
		t.Fatal("closing capture read source or closed it twice")
	}
	eventually(t, func() bool { return c.Snapshot().CapturedBytes == 37 })
	fetch := func(context.Context, int64, int64) (io.ReadCloser, error) {
		t.Fatal("captured source bytes were fetched again")
		return nil, nil
	}
	hit, err := c.Open(context.Background(), "generation", offset+7, 10, fetch)
	if got := readAndClose(t, hit, err); !bytes.Equal(got, pattern(offset+7, 10)) {
		t.Fatal("capture reused incorrect source offsets")
	}
	if s := c.Snapshot(); s.CapturedBytes != 37 || s.CachedBytes != 37 || s.SourceReadBytes != 37 || s.CacheHitBytes != 10 {
		t.Fatalf("capture accounting: %+v", s)
	}
}

func TestCaptureEvictsWithinBudgetWithoutChangingStream(t *testing.T) {
	c := newTestCache(t, capturePageBytes, 2)
	var reads, closes atomic.Int64
	r := c.Capture("generation", 0, &patternedSource{0, &reads, &closes})
	defer r.Close()
	data := make([]byte, capturePageBytes*5+12)
	if _, err := io.ReadFull(r, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, pattern(0, int64(len(data)))) || reads.Load() != int64(len(data)) {
		t.Fatal("bounded capture changed source bytes or read ahead")
	}
	eventually(t, func() bool { return c.Snapshot().CapturedBytes == capturePageBytes*5 })
	if s := c.Snapshot(); s.CachedBytes > capturePageBytes || s.ReservedBytes != 0 || s.Entries != 1 || s.Evictions != 4 {
		t.Fatalf("capture exceeded its quota: %+v", s)
	}
	_ = r.Close()
	eventually(t, func() bool { return c.Snapshot().CapturedBytes == uint64(len(data)) })
	if s := c.Snapshot(); s.CachedBytes > capturePageBytes || s.ReservedBytes != 0 || s.CapturedBytes != uint64(len(data)) {
		t.Fatalf("partial capture exceeded quota: %+v", s)
	}
}

func TestCaptureCapacityFailureAndCacheClosePreserveSource(t *testing.T) {
	c := newTestCache(t, capturePageBytes, 2)
	pinned, err := c.Open(context.Background(), "pinned", 0, capturePageBytes, constantFetch('p'))
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	var reads, closes atomic.Int64
	r := c.Capture("stream", 0, &patternedSource{0, &reads, &closes})
	defer r.Close()
	data := make([]byte, capturePageBytes)
	if _, err := io.ReadFull(r, data); err != nil || !bytes.Equal(data, pattern(0, capturePageBytes)) {
		t.Fatal("capacity exhaustion interrupted source consumption")
	}
	eventually(t, func() bool { return c.Snapshot().CaptureDrops == 1 })
	if s := c.Snapshot(); s.CaptureDrops != 1 || s.CachedBytes != capturePageBytes || s.CapturedBytes != 0 {
		t.Fatalf("capture did not respect pinned cache: %+v", s)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(r, data); err != nil || !bytes.Equal(data, pattern(capturePageBytes, capturePageBytes)) {
		t.Fatal("cache shutdown interfered with captured source")
	}
	if closes.Load() != 0 {
		t.Fatal("cache took ownership of captured source")
	}
	_ = r.Close()
	if closes.Load() != 1 {
		t.Fatal("capture leaked original body")
	}
}

func TestSlowCaptureWriterNeverBlocksMediaReadOrSourceClose(t *testing.T) {
	c := newTestCache(t, 8<<20, 256)
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	writePage := c.writeCapturePage
	c.writeCapturePage = func(data []byte) (string, error) {
		startOnce.Do(func() { close(started) })
		<-release
		return writePage(data)
	}
	var reads, closes atomic.Int64
	r := c.Capture("media", 0, &patternedSource{0, &reads, &closes})
	first := make([]byte, capturePageBytes)
	if _, err := io.ReadFull(r, first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("capture writer did not start")
	}
	const pages = captureQueuePages + 20
	done := make(chan error, 1)
	go func() {
		data := make([]byte, capturePageBytes*pages)
		_, err := io.ReadFull(r, data)
		if err == nil && !bytes.Equal(data, pattern(capturePageBytes, int64(len(data)))) {
			err = errors.New("slow cache changed original media data")
		}
		err = errors.Join(err, r.Close())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("media read or source Close waited for the stalled cache disk")
	}
	s := c.Snapshot()
	if closes.Load() != 1 || reads.Load() != capturePageBytes*(pages+1) || s.CapturedBytes != 0 {
		t.Fatalf("source work changed under backpressure: %+v reads=%d closes=%d", s, reads.Load(), closes.Load())
	}
	if s.CaptureQueuePages != captureQueuePages || s.CaptureQueueBytes != captureQueuePages*capturePageBytes || s.CaptureDrops != 20 || s.ReservedBytes != capturePageBytes {
		t.Fatalf("capture queue is not bounded under disk pressure: %+v", s)
	}
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	eventually(t, func() bool { return c.captureStopped.Load() })
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cache Close did not drain queued pages")
	}
	if s := c.Snapshot(); s.CaptureQueuePages != 0 || s.ReservedBytes != 0 {
		t.Fatalf("shutdown retained queued memory or a reservation: %+v", s)
	}
}

func TestBusyCatalogNeverBlocksCaptureHandoff(t *testing.T) {
	c := newTestCache(t, 1<<20, 16)
	var reads, closes atomic.Int64
	func() {
		// A slow eviction or unrelated cache write may hold the catalog lock.
		// The media path must not take that lock even to construct its wrapper.
		c.mu.Lock()
		defer c.mu.Unlock()
		done := make(chan error, 1)
		go func() {
			r := c.Capture("media", 0, &patternedSource{0, &reads, &closes})
			_, err := io.ReadFull(r, make([]byte, capturePageBytes))
			done <- errors.Join(err, r.Close())
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("capture waited for the cache catalog")
		}
	}()
	if reads.Load() != capturePageBytes || closes.Load() != 1 {
		t.Fatal("catalog pressure affected media consumption")
	}
}

func TestConcurrentCaptureShutdownHasNoLateQueueSends(t *testing.T) {
	c := newTestCache(t, 1<<20, 16)
	var wg sync.WaitGroup
	start := make(chan struct{})
	var closeCount atomic.Int64
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := c.Capture("media", 0, &patternedSource{0, &atomic.Int64{}, &closeCount})
			_, _ = io.ReadFull(r, make([]byte, capturePageBytes*2+17))
			_ = r.Close()
		}()
	}
	close(start)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if len(c.captureQueue) != 0 || closeCount.Load() != 32 {
		t.Fatal("shutdown allowed a late queue send or lost source ownership")
	}
}
