package gateway

import (
	"io"
	"net/http"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

// TrafficMeter records live forwarded body bytes. Implemented by *telemetry.ByteMeter.
// Nil-safe: all methods no-op on a nil receiver implementation check at call sites.
type TrafficMeter interface {
	AddEgress(n int64)
	AddIngress(n int64)
	BeginTransfer(meta telemetry.TransferMeta) uint64
	AddTransferEgress(id uint64, n int64)
	AddTransferIngress(id uint64, n int64)
	EndTransfer(id uint64, err error)
}

// countedWriter counts successful downstream body writes into a TrafficMeter.
type countedWriter struct {
	http.ResponseWriter
	meter      TrafficMeter
	transferID uint64
}

func newCountedWriter(w http.ResponseWriter, meter TrafficMeter, transferID uint64) http.ResponseWriter {
	if w == nil || meter == nil {
		return w
	}
	return &countedWriter{ResponseWriter: w, meter: meter, transferID: transferID}
}

func (w *countedWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 && w.meter != nil {
		if w.transferID != 0 {
			w.meter.AddTransferEgress(w.transferID, int64(n))
		} else {
			w.meter.AddEgress(int64(n))
		}
	}
	return n, err
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (w *countedWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// countedReader counts successful upstream body reads into a TrafficMeter.
type countedReader struct {
	r          io.Reader
	meter      TrafficMeter
	transferID uint64
}

func newCountedReader(r io.Reader, meter TrafficMeter, transferID uint64) io.Reader {
	if r == nil || meter == nil {
		return r
	}
	return &countedReader{r: r, meter: meter, transferID: transferID}
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 && r.meter != nil {
		if r.transferID != 0 {
			r.meter.AddTransferIngress(r.transferID, int64(n))
		} else {
			r.meter.AddIngress(int64(n))
		}
	}
	return n, err
}
