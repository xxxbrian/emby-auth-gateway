package gateway

import (
	"bytes"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

func TestCountedWriterRecordsEgressBeforeCopyCompletes(t *testing.T) {
	meter := telemetry.NewByteMeter()
	id := meter.BeginTransfer(telemetry.TransferMeta{SessionID: "s", Method: "GET", MediaMode: "direct"})

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(strings.Repeat("x", 64*1024)))
		// leave pipe open briefly so "during stream" is observable
		time.Sleep(20 * time.Millisecond)
		_ = pw.Close()
	}()

	rec := httptest.NewRecorder()
	dst := newCountedWriter(rec, meter, id)
	src := newCountedReader(pr, meter, id)

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
	meter.EndTransfer(id, result.Err)
	if _, total := meter.Totals(); total < 64*1024 {
		t.Fatalf("total egress=%d", total)
	}
}

func TestCountedWriterNilMeterPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := newCountedWriter(rec, nil, 0)
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write n=%d err=%v", n, err)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body=%q", rec.Body.String())
	}
	r := newCountedReader(bytes.NewReader([]byte("abc")), nil, 0)
	buf := make([]byte, 8)
	n, err = r.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("n=%d", n)
	}
}
