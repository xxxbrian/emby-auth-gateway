package gateway

import (
	"io"
	"net/http"
	"sync/atomic"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

// TrafficMeter records live forwarded body bytes. Implemented by *telemetry.ByteMeter.
type TrafficMeter interface {
	AddEgress(n int64)
	AddIngress(n int64)
	NoteError()
	BeginTransfer(meta telemetry.TransferMeta) *telemetry.TransferHandle
}

// countedWriter counts successful downstream body writes.
// Prefer handle (atomics-only); fall back to meter global AddEgress.
type countedWriter struct {
	http.ResponseWriter
	meter  TrafficMeter
	handle *telemetry.TransferHandle
	live   *atomic.Int64
}

func newCountedWriter(w http.ResponseWriter, meter TrafficMeter, handle *telemetry.TransferHandle) http.ResponseWriter {
	return newCountedWriterWithLive(w, meter, handle, nil)
}

func newCountedWriterWithLive(w http.ResponseWriter, meter TrafficMeter, handle *telemetry.TransferHandle, live *atomic.Int64) http.ResponseWriter {
	if w == nil {
		return w
	}
	if meter == nil && handle == nil && live == nil {
		return w
	}
	return &countedWriter{ResponseWriter: w, meter: meter, handle: handle, live: live}
}

func (w *countedWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && n <= len(p) {
		if w.handle != nil {
			w.handle.AddEgress(int64(n))
		} else {
			if w.meter != nil {
				w.meter.AddEgress(int64(n))
			}
			if w.live != nil {
				w.live.Add(int64(n))
			}
		}
	}
	return n, err
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (w *countedWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// countedReader counts successful upstream body reads.
type countedReader struct {
	r      io.Reader
	meter  TrafficMeter
	handle *telemetry.TransferHandle
	live   *atomic.Int64
}

func newCountedReader(r io.Reader, meter TrafficMeter, handle *telemetry.TransferHandle) io.Reader {
	return newCountedReaderWithLive(r, meter, handle, nil)
}

func newCountedReaderWithLive(r io.Reader, meter TrafficMeter, handle *telemetry.TransferHandle, live *atomic.Int64) io.Reader {
	if r == nil {
		return r
	}
	if meter == nil && handle == nil && live == nil {
		return r
	}
	return &countedReader{r: r, meter: meter, handle: handle, live: live}
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && n <= len(p) {
		if r.handle != nil {
			r.handle.AddIngress(int64(n))
		} else {
			if r.meter != nil {
				r.meter.AddIngress(int64(n))
			}
			if r.live != nil {
				r.live.Add(int64(n))
			}
		}
	}
	return n, err
}

// countEgressWrite writes p through w and records successful bytes.
func countEgressWrite(w http.ResponseWriter, meter TrafficMeter, handle *telemetry.TransferHandle, p []byte) (int, error) {
	if w == nil {
		return 0, io.ErrShortWrite
	}
	cw := newCountedWriter(w, meter, handle)
	n, err := cw.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	// Direct buffered writes have no transfer End hook to account for failure.
	if err != nil && meter != nil && handle == nil {
		meter.NoteError()
	}
	return n, err
}
