package sourcecache

import (
	"context"
	"io"
	"math"
	"os"
	"sync"
)

const (
	capturePageBytes  = 64 << 10
	captureQueuePages = 16
)

type capturePage struct {
	key  string
	span byteRange
	data []byte
}

// Capture returns a streaming reader that opportunistically caches only bytes
// its consumer reads. It never fetches, reads ahead, or waits for another fill.
// A nonblocking queue hands pages to one background writer. Storage failure,
// exhausted capacity, or a busy/full queue simply drops that page. Memory is at
// most 64KiB per reader plus 16 queued pages and one writer page per Cache.
//
// Only use Capture for a source already being consumed. Do not wrap an Open
// result: Open already counts upstream traffic and caches its bounded requests.
// Callers own access/version validation and must close the result. Closing the
// cache does not close a captured source or interfere with its consumption.
func (c *Cache) Capture(key string, offset int64, body io.ReadCloser) io.ReadCloser {
	if body == nil || offset < 0 || !validKey(key) {
		return body
	}
	if c.captureStopped.Load() {
		return body
	}
	return &captureReader{cache: c, key: key, offset: offset, body: countSource(body, -1, &c.sourceBytes)}
}

type captureReader struct {
	mu     sync.Mutex
	cache  *Cache
	key    string
	offset int64
	body   *countedSource
	buffer []byte
	closed bool
	once   sync.Once
	err    error
}

func (r *captureReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	n, err := r.body.Read(p)
	if r.cache.captureStopped.Load() {
		r.buffer = nil
		return n, err
	}
	remaining := p[:n]
	for len(remaining) > 0 {
		if r.buffer == nil {
			r.buffer = make([]byte, 0, capturePageBytes)
		}
		take := min(cap(r.buffer)-len(r.buffer), len(remaining))
		r.buffer = append(r.buffer, remaining[:take]...)
		remaining = remaining[take:]
		if len(r.buffer) == cap(r.buffer) {
			r.flush()
		}
	}
	if err != nil {
		r.flush()
	}
	return n, err
}

func (r *captureReader) flush() {
	if len(r.buffer) == 0 {
		return
	}
	length := int64(len(r.buffer))
	if r.offset >= 0 && r.offset <= math.MaxInt64-length {
		if r.cache.enqueue(capturePage{key: r.key, span: byteRange{r.offset, length}, data: r.buffer}) {
			// Transfer this allocation to the writer. A dropped page keeps its
			// buffer for reuse, avoiding allocations while the disk is stalled.
			r.buffer = nil
		}
		r.offset += length
	} else {
		r.offset = -1
	}
	r.buffer = r.buffer[:0]
}

func (c *Cache) enqueue(page capturePage) bool {
	if c.captureStopped.Load() || !c.captureMu.TryLock() {
		c.captureDrops.Add(1)
		return false
	}
	defer c.captureMu.Unlock()
	if c.captureStopped.Load() {
		c.captureDrops.Add(1)
		return false
	}
	select {
	case c.captureQueue <- page:
		return true
	default:
		c.captureDrops.Add(1)
		return false
	}
}

func (c *Cache) captureLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			// Close stopped admission before canceling the context. Queued data
			// can now be released without another reader racing a late enqueue.
			for {
				select {
				case <-c.captureQueue:
					c.captureDrops.Add(1)
				default:
					return
				}
			}
		case page := <-c.captureQueue:
			if c.captureStopped.Load() {
				c.captureDrops.Add(1)
				continue
			}
			c.offer(page.key, page.span, page.data)
		}
	}
}

func (r *captureReader) Close() error {
	r.once.Do(func() {
		// Close the body before taking mu so Close can unblock a pending Read.
		r.err = r.body.Close()
		r.mu.Lock()
		r.closed = true
		r.flush()
		r.buffer = nil
		r.mu.Unlock()
	})
	return r.err
}

func (c *Cache) offer(key string, span byteRange, data []byte) {
	c.mu.Lock()
	if c.closed || c.disabled || span.length > c.cfg.BudgetBytes {
		c.captureDrops.Add(1)
		c.mu.Unlock()
		return
	}
	for _, e := range c.entries[key] {
		if e.span.covers(span) {
			c.mu.Unlock()
			return
		}
	}
	for _, f := range c.flights[key] {
		if f.span.covers(span) {
			c.mu.Unlock()
			return
		}
	}
	if !c.reserveLocked(span.length) {
		c.captureDrops.Add(1)
		c.mu.Unlock()
		return
	}
	f := &flight{key: key, span: span, capture: true, ctx: context.Background(), cancel: func() {}, done: make(chan struct{})}
	if c.flights[key] == nil {
		c.flights[key] = make(map[byteRange]*flight)
	}
	c.flights[key][span] = f
	c.inflight++
	c.mu.Unlock()
	path, err := c.writeCapturePage(data)
	c.finish(f, path, err)
}

func (c *Cache) writePage(data []byte) (string, error) {
	file, err := os.CreateTemp(c.directory.blocks, "part-")
	if err != nil {
		return "", errCacheIO
	}
	path := file.Name()
	n, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || n != len(data) {
		return path, errCacheIO
	}
	complete := path + ".block"
	if err := os.Rename(path, complete); err != nil {
		return path, errCacheIO
	}
	return complete, nil
}
