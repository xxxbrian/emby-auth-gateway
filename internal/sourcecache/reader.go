package sourcecache

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
)

type countedSource struct {
	reader io.Reader
	body   io.Closer
	bytes  *atomic.Uint64
	once   sync.Once
	err    error
}

func countSource(body io.ReadCloser, length int64, counter *atomic.Uint64) *countedSource {
	var reader io.Reader = body
	if length >= 0 {
		reader = io.LimitReader(body, length)
	}
	return &countedSource{reader: reader, body: body, bytes: counter}
}

func (r *countedSource) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.bytes.Add(uint64(n))
	}
	return n, err
}

func (r *countedSource) Close() error {
	r.once.Do(func() { r.err = r.body.Close() })
	return r.err
}

type cachedReader struct {
	reader *io.SectionReader
	file   *os.File
	cache  *Cache
	entry  *entry
	hit    bool
	once   sync.Once
	err    error
}

func (c *Cache) readerLocked(e *entry, span byteRange, hit, promised bool) (*cachedReader, error) {
	file, err := os.Open(e.path)
	if err != nil {
		if promised {
			e.pins--
		}
		return nil, err
	}
	if !promised {
		e.pins++
	}
	c.lru.MoveToFront(e.lru)
	r := &cachedReader{reader: io.NewSectionReader(file, span.offset-e.span.offset, span.length), file: file, cache: c, entry: e, hit: hit}
	c.readers[r] = struct{}{}
	return r, nil
}

func (r *cachedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if r.hit && n > 0 {
		r.cache.hitBytes.Add(uint64(n))
	}
	return n, err
}

func (r *cachedReader) Close() error {
	r.once.Do(func() {
		r.err = r.file.Close()
		r.cache.mu.Lock()
		r.entry.pins--
		delete(r.cache.readers, r)
		r.cache.mu.Unlock()
	})
	return r.err
}
