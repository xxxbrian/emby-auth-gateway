package transcode

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// catalog owns both complete artifacts and in-flight byte reservations. Media
// reaches disk through its writer, so limits do not rely on directory polling.
type catalog struct {
	mu                      sync.Mutex
	root                    string
	budget, used, reserved  int64
	entries                 map[string]*artifact
	wanted                  map[string]int
	hits, misses, evictions uint64
}
type artifact struct {
	path    string
	size    int64
	pins    int
	used    time.Time
	retired bool
}
type cacheWriter struct {
	cache *catalog
	file  *os.File
	key   string
	bytes int64
	done  bool
}
type lease struct {
	*os.File
	cache *catalog
	entry *artifact
	once  sync.Once
}

const maxCacheArtifacts = 16384

func (c *catalog) protect(key string, delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wanted == nil {
		c.wanted = map[string]int{}
	}
	c.wanted[key] += delta
	if c.wanted[key] <= 0 {
		delete(c.wanted, key)
	}
}

func (l *lease) Close() error {
	var err error
	l.once.Do(func() {
		err = l.File.Close()
		c := l.cache
		c.mu.Lock()
		defer c.mu.Unlock()
		l.entry.pins--
		if l.entry.retired && l.entry.pins == 0 {
			c.used -= l.entry.size
			_ = os.Remove(l.entry.path)
		}
	})
	return err
}
func (c *catalog) open(key string) (*lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.entries[key]
	if a == nil || a.retired {
		c.misses++
		return nil, ErrNotFound
	}
	f, err := os.Open(a.path)
	if err != nil {
		return nil, ErrNotFound
	}
	a.pins++
	a.used = time.Now()
	c.hits++
	return &lease{File: f, cache: c, entry: a}, nil
}
func (c *catalog) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.entries[key]
	return a != nil && !a.retired
}
func (c *catalog) begin(key string) (*cacheWriter, error) {
	f, err := os.CreateTemp(c.root, "part-")
	if err != nil {
		return nil, ErrCapacity
	}
	return &cacheWriter{cache: c, file: f, key: key}, nil
}
func (w *cacheWriter) Write(p []byte) (int, error) {
	c := w.cache
	c.mu.Lock()
	err := c.reserve(int64(len(p)))
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	n, err := w.file.Write(p)
	c.mu.Lock()
	c.reserved -= int64(len(p) - n)
	w.bytes += int64(n)
	c.mu.Unlock()
	return n, err
}

// reserve is called under c.mu, before any bytes are written.
func (c *catalog) reserve(n int64) error {
	if n < 0 || n > c.budget {
		return ErrCapacity
	}
	for c.used+c.reserved+n > c.budget {
		if err := c.evictOne(); err != nil {
			return err
		}
	}
	c.reserved += n
	return nil
}

func (c *catalog) evictOne() error {
	var key string
	var oldest *artifact
	for k, a := range c.entries {
		if !a.retired && a.pins == 0 && c.wanted[k] == 0 && (oldest == nil || a.used.Before(oldest.used)) {
			key, oldest = k, a
		}
	}
	if oldest == nil {
		return ErrCapacity
	}
	if err := os.Remove(oldest.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrCapacity
	}
	delete(c.entries, key)
	c.used -= oldest.size
	c.evictions++
	return nil
}
func (w *cacheWriter) abort() {
	if w.done {
		return
	}
	w.done = true
	_ = w.file.Close()
	c := w.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserved -= w.bytes
	_ = os.Remove(w.file.Name())
}
func (w *cacheWriter) commit() error {
	if w.done {
		return ErrWorker
	}
	if err := w.file.Close(); err != nil {
		w.abort()
		return err
	}
	c := w.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous := c.entries[w.key]; previous != nil {
		// Another request may already have published the same immutable segment.
		c.reserved -= w.bytes
		_ = os.Remove(w.file.Name())
		w.done = true
		return nil
	}
	if len(c.entries) >= maxCacheArtifacts {
		if err := c.evictOne(); err != nil {
			return err
		}
	}
	path := filepath.Join(c.root, filepath.Base(w.file.Name())+".m4s")
	if err := os.Rename(w.file.Name(), path); err != nil {
		return err
	}
	c.reserved -= w.bytes
	c.used += w.bytes
	c.entries[w.key] = &artifact{path: path, size: w.bytes, used: time.Now()}
	w.done = true
	return nil
}
func (c *catalog) dropPrefix(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, a := range c.entries {
		if len(key) < len(prefix) || key[:len(prefix)] != prefix {
			continue
		}
		delete(c.entries, key)
		a.retired = true
		if a.pins == 0 {
			c.used -= a.size
			_ = os.Remove(a.path)
		}
	}
}
func (c *catalog) stats() (used, reserved, pinned int64, hits, misses, evictions uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.entries {
		if a.pins > 0 {
			pinned += a.size
		}
	}
	return c.used, c.reserved, pinned, c.hits, c.misses, c.evictions
}
func (c *catalog) keys(prefix string) map[string]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int64{}
	for key, a := range c.entries {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix && !a.retired {
			out[key] = a.size
		}
	}
	return out
}
func (c *catalog) store(key string, data []byte) error {
	w, err := c.begin(key)
	if err != nil {
		return err
	}
	defer w.abort()
	if _, err = w.Write(data); err != nil {
		return err
	}
	return w.commit()
}

var _ io.ReadSeekCloser = (*lease)(nil)
