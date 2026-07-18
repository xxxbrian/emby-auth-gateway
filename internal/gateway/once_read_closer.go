package gateway

import (
	"io"
	"net/http"
	"sync"
)

type onceReadCloser struct {
	reader   io.Reader
	closer   io.Closer
	once     sync.Once
	closeErr error
}

func (r *onceReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *onceReadCloser) Close() error {
	r.once.Do(func() {
		r.closeErr = r.closer.Close()
	})
	return r.closeErr
}

func (r *onceReadCloser) replaceReader(reader io.Reader) {
	r.reader = reader
}

func wrapResponseBodyOnce(resp *http.Response) *onceReadCloser {
	if resp == nil || resp.Body == nil {
		return nil
	}
	if body, ok := resp.Body.(*onceReadCloser); ok {
		return body
	}
	body := &onceReadCloser{reader: resp.Body, closer: resp.Body}
	resp.Body = body
	return body
}
