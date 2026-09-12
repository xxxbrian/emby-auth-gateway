package subtitleindex

import (
	"context"
	"errors"
	"io"
	"sort"
	"sync"
)

const pageSize int64 = 4096

type reader struct {
	ctx      context.Context
	source   Source
	limits   Limits
	used     int64
	requests int
	pages    map[int64][]byte
}

type interval struct{ start, end int64 }

func (r *reader) read(offset, size int64) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || size < 0 || offset > r.source.Size || size > r.source.Size-offset {
		return nil, ErrIndex
	}
	if size > r.limits.MaxReadBytes {
		return nil, ErrLimit
	}
	if size == 0 {
		return []byte{}, nil
	}
	if err := r.prefetch([]interval{{offset, offset + size}}); err != nil {
		return nil, err
	}
	b := make([]byte, int(size))
	for pos := offset; pos < offset+size; {
		base := pos / pageSize * pageSize
		page := r.pages[base]
		if len(page) == 0 {
			return nil, ErrSource
		}
		n := copy(b[pos-offset:], page[pos-base:])
		if n == 0 {
			return nil, ErrSource
		}
		pos += int64(n)
	}
	return b, nil
}

// prefetch merges nearby pages into bounded ranges. Parallelism is deliberately
// fixed: a subtitle operation cannot spawn one request per cue concurrently.
func (r *reader) prefetch(wanted []interval) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	missing := make(map[int64]struct{})
	for _, w := range wanted {
		if w.start < 0 || w.end < w.start || w.end > r.source.Size {
			return ErrIndex
		}
		if w.end-w.start > r.limits.MaxReadBytes {
			return ErrLimit
		}
		for p := w.start / pageSize * pageSize; p < w.end; {
			if _, ok := r.pages[p]; !ok {
				missing[p] = struct{}{}
			}
			if int64(len(missing))*pageSize > r.limits.MaxReadBytes+pageSize {
				return ErrLimit
			}
			if w.end-p <= pageSize {
				break
			}
			p += pageSize
		}
	}
	if len(missing) == 0 {
		return nil
	}
	positions := make([]int64, 0, len(missing))
	for p := range missing {
		positions = append(positions, p)
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i] < positions[j] })
	ranges := make([]interval, 0, len(positions))
	for _, p := range positions {
		end := p + min(pageSize, r.source.Size-p)
		if len(ranges) > 0 && p-ranges[len(ranges)-1].end <= 16<<10 && end-ranges[len(ranges)-1].start <= 256<<10 {
			ranges[len(ranges)-1].end = end
		} else {
			ranges = append(ranges, interval{p, end})
		}
	}
	var bytes int64
	for _, w := range ranges {
		bytes += w.end - w.start
	}
	if bytes > r.limits.MaxReadBytes-r.used || len(ranges) > r.limits.MaxRequests-r.requests {
		return ErrLimit
	}
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	type response struct {
		w   interval
		b   []byte
		n   int
		err error
	}
	results := make([]response, len(ranges))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < min(4, len(ranges)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				w := ranges[i]
				result := response{w: w}
				if err := ctx.Err(); err != nil {
					result.err = err
					results[i] = result
					continue
				}
				result.n = 1
				body, err := r.source.Open(ctx, w.start, w.end-w.start)
				if err == nil && body == nil {
					err = ErrSource
				}
				if err == nil {
					result.b = make([]byte, int(w.end-w.start))
					var n int
					n, err = io.ReadFull(body, result.b)
					result.b = result.b[:n]
					closeErr := body.Close()
					if err == nil {
						err = closeErr
					}
				} else if body != nil {
					_ = body.Close()
				}
				if err != nil {
					if ctx.Err() != nil {
						err = ctx.Err()
					} else {
						err = errors.Join(ErrSource, err)
					}
					cancel()
				}
				result.err = err
				results[i] = result
			}
		}()
	}
	for i := range ranges {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	var failure error
	for _, result := range results {
		r.requests += result.n
		r.used += int64(len(result.b))
		if result.err != nil {
			if failure == nil || errors.Is(result.err, ErrSource) {
				failure = result.err
			}
			continue
		}
		for off := int64(0); off < int64(len(result.b)); off += pageSize {
			r.pages[result.w.start+off] = result.b[off:min(off+pageSize, int64(len(result.b)))]
		}
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	return failure
}
