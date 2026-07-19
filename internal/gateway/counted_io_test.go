package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

func TestCountedWriterRecordsEgressBeforeCopyCompletes(t *testing.T) {
	meter := telemetry.NewByteMeter()
	handle := meter.BeginTransfer(telemetry.TransferMeta{SessionID: "s", Method: "GET", MediaMode: "direct"})

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(strings.Repeat("x", 64*1024)))
		// leave pipe open briefly so "during stream" is observable
		time.Sleep(20 * time.Millisecond)
		_ = pw.Close()
	}()

	rec := httptest.NewRecorder()
	dst := newCountedWriter(rec, meter, handle)
	src := newCountedReader(pr, meter, handle)

	done := make(chan mediaCopyResult, 1)
	go func() {
		done <- copyMediaBody(dst, src, -1)
	}()

	// Poll until live egress is visible while copy may still be running.
	deadline := time.Now().Add(2 * time.Second)
	var out uint64
	for time.Now().Before(deadline) {
		_, out = meter.Totals()
		if out > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if out == 0 {
		t.Fatal("expected live egress > 0 before/during copy completion")
	}
	if meter.ActiveTransferCount() != 1 {
		t.Fatalf("active transfers=%d", meter.ActiveTransferCount())
	}

	result := <-done
	if result.Err != nil {
		t.Fatalf("copy: %v", result.Err)
	}
	handle.End(result.Err)
	if _, total := meter.Totals(); total < 64*1024 {
		t.Fatalf("total egress=%d", total)
	}
}

func TestCountedWriterNilMeterPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newCountedWriter(rec, nil, nil)
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body=%q", rec.Body.String())
	}
	r := newCountedReader(bytes.NewReader([]byte("abc")), nil, nil)
	buf := make([]byte, 8)
	n, err = r.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("n=%d", n)
	}
}

func TestCountedIONilHandleLiveFallback(t *testing.T) {
	var read, written atomic.Int64
	w := newCountedWriterWithLive(httptest.NewRecorder(), nil, nil, &written)
	r := newCountedReaderWithLive(strings.NewReader("abc"), nil, nil, &read)
	if n, err := w.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	buf := make([]byte, 8)
	if n, err := r.Read(buf); err != nil && err != io.EOF || n != 3 {
		t.Fatalf("read n=%d err=%v", n, err)
	}
	if read.Load() != 3 || written.Load() != 5 {
		t.Fatalf("fallback bytes=%d/%d", read.Load(), written.Load())
	}
}

func TestCountedIOInvalidAndShortWriteParity(t *testing.T) {
	meter := telemetry.NewByteMeter()
	handle := meter.BeginTransfer(telemetry.TransferMeta{SessionID: "parity"})
	invalidWrite := newCountedWriter(countedResultWriter{n: 4}, meter, handle)
	if n, _ := invalidWrite.Write([]byte("abc")); n != 4 {
		t.Fatalf("invalid write n=%d", n)
	}
	invalidRead := newCountedReader(countedResultReader{n: 4}, meter, handle)
	if n, _ := invalidRead.Read(make([]byte, 3)); n != 4 {
		t.Fatalf("invalid read n=%d", n)
	}
	if in, out := handle.Bytes(); in != 0 || out != 0 {
		t.Fatalf("invalid counts in=%d out=%d", in, out)
	}
	shortWrite := newCountedWriter(countedResultWriter{n: 2}, meter, handle)
	if n, _ := shortWrite.Write([]byte("abc")); n != 2 {
		t.Fatalf("short write n=%d", n)
	}
	if in, out := handle.Bytes(); in != 0 || out != 2 {
		t.Fatalf("short counts in=%d out=%d", in, out)
	}
}

func TestCountEgressWriteFailureCountsOneError(t *testing.T) {
	meter := telemetry.NewByteMeter()
	w := &countedFailureWriter{err: errors.New("write failed")}

	if _, err := countEgressWrite(w, meter, nil, []byte("payload")); err == nil {
		t.Fatal("expected write error")
	}
	if got := meter.ErrorTotal(); got != 1 {
		t.Fatalf("errors=%d want 1", got)
	}
}

func TestCopyProxyBodyFailureCountsOneError(t *testing.T) {
	meter := telemetry.NewByteMeter()
	s := NewServer(Config{Meter: meter}, testStore("http://backend.invalid/emby"))
	r := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/test", nil)
	w := &countedFailureWriter{err: errors.New("write failed")}

	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("panic=%v want http.ErrAbortHandler", recovered)
			}
		}()
		s.copyProxyBodyOrAbort(w, r, "/test", strings.NewReader("payload"), nil)
	}()

	if got := meter.ErrorTotal(); got != 1 {
		t.Fatalf("errors=%d want 1", got)
	}
}

type countedFailureWriter struct {
	header http.Header
	err    error
}

type countedResultWriter struct{ n int }

func (countedResultWriter) Header() http.Header         { return make(http.Header) }
func (countedResultWriter) WriteHeader(int)             {}
func (w countedResultWriter) Write([]byte) (int, error) { return w.n, nil }

type countedResultReader struct{ n int }

func (r countedResultReader) Read([]byte) (int, error) { return r.n, nil }

func (w *countedFailureWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (*countedFailureWriter) WriteHeader(int) {}

func (w *countedFailureWriter) Write([]byte) (int, error) {
	return 0, w.err
}
