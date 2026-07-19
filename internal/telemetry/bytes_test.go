package telemetry

import (
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
)

func TestByteMeterLiveTotalsBeforeEnd(t *testing.T) {
	m := NewByteMeter()
	h := m.BeginTransfer(TransferMeta{SessionID: "s1", MediaMode: "direct", Method: "GET"})
	if h == nil || h.ID() == 0 {
		t.Fatal("expected non-zero transfer handle")
	}
	h.AddEgress(1_000_000)
	h.AddIngress(1_000_000)
	in, out := m.Totals()
	if in != 1_000_000 || out != 1_000_000 {
		t.Fatalf("totals in=%d out=%d", in, out)
	}
	active := m.ActiveTransfers()
	if len(active) != 1 || active[0].BytesOut != 1_000_000 {
		t.Fatalf("active=%+v", active)
	}
	if m.ActiveTransferCount() != 1 {
		t.Fatal("expected active transfer")
	}
	// LastSeen should advance with I/O.
	if !active[0].LastSeen.After(active[0].StartedAt) && !active[0].LastSeen.Equal(active[0].StartedAt) {
		// Allow equal if clock resolution is coarse; require non-zero.
		if active[0].LastSeen.IsZero() {
			t.Fatal("expected LastSeen set")
		}
	}
	h.End(nil)
	if in, out := h.Bytes(); in != 1_000_000 || out != 1_000_000 {
		t.Fatalf("retained bytes in=%d out=%d", in, out)
	}
	if m.ActiveTransferCount() != 0 {
		t.Fatal("expected no active transfers")
	}
	if m.CompletedEgress() != 1_000_000 {
		t.Fatalf("completed=%d", m.CompletedEgress())
	}
	// End is idempotent.
	h.End(nil)
	if m.CompletedEgress() != 1_000_000 {
		t.Fatalf("double end completed=%d", m.CompletedEgress())
	}
}

func TestByteMeterNoteError(t *testing.T) {
	m := NewByteMeter()
	m.NoteError()
	h := m.BeginTransfer(TransferMeta{SessionID: "s"})
	h.End(ioStubErr{})
	if m.ErrorTotal() != 2 {
		t.Fatalf("errors=%d want 2", m.ErrorTotal())
	}
}

type ioStubErr struct{}

func (ioStubErr) Error() string { return "stub" }

func TestLiveBytesDriveSnapshotMbpsWithoutPhaseEnd(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	// Simulate sampler delta from live meter (stream still open).
	r.mu.Lock()
	r.sec.add(base, func(c *counters) { c.BytesOut += 2_500_000 })
	r.mu.Unlock()

	snap := r.Snapshot()
	// 2.5e6 bytes * 8 / 1e6 / 10s = 2 Mbps
	if snap.Traffic.MbpsOut < 1.9 || snap.Traffic.MbpsOut > 2.1 {
		t.Fatalf("MbpsOut=%v want ~2", snap.Traffic.MbpsOut)
	}

	// PhaseEnd must not add more bytes to rings.
	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base,
		Phase:     observe.PhaseEnd,
		SessionID: "s",
		ItemID:    "i",
		MediaMode: observe.MediaDirect,
		Method:    "GET",
		Outcome:   observe.OutcomeOK,
		BytesOut:  2_500_000,
	})
	snap2 := r.Snapshot()
	if snap2.Traffic.MbpsOut != snap.Traffic.MbpsOut {
		t.Fatalf("PhaseEnd double-counted: before=%v after=%v", snap.Traffic.MbpsOut, snap2.Traffic.MbpsOut)
	}
}

func TestSampleLiveBytesOnceWhileTransferOpen(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	h := r.Meter().BeginTransfer(TransferMeta{SessionID: "live", MediaMode: "direct", Method: "GET"})
	h.AddEgress(2_500_000)
	h.AddIngress(1_000_000)

	var prevIn, prevOut, prevErr uint64
	r.SampleLiveBytesOnce(base, &prevIn, &prevOut, &prevErr)

	// Stream still open after sample.
	if r.Meter().ActiveTransferCount() != 1 {
		t.Fatal("transfer should remain open")
	}
	snap := r.Snapshot()
	if snap.Traffic.MbpsOut < 1.9 || snap.Traffic.MbpsOut > 2.1 {
		t.Fatalf("MbpsOut=%v want ~2 while stream open", snap.Traffic.MbpsOut)
	}
	if snap.Traffic.MbpsIn < 0.7 || snap.Traffic.MbpsIn > 0.9 {
		t.Fatalf("MbpsIn=%v want ~0.8", snap.Traffic.MbpsIn)
	}

	// Second sample with no new bytes is a no-op.
	r.SampleLiveBytesOnce(base.Add(time.Second), &prevIn, &prevOut, &prevErr)
	snap2 := r.Snapshot()
	if snap2.Traffic.MbpsOut != snap.Traffic.MbpsOut {
		t.Fatalf("idle sample changed MbpsOut: %v -> %v", snap.Traffic.MbpsOut, snap2.Traffic.MbpsOut)
	}

	// Errors from meter are sampled into rings (series count, not rate alone).
	r.Meter().NoteError()
	r.SampleLiveBytesOnce(base.Add(2*time.Second), &prevIn, &prevOut, &prevErr)
	snap3 := r.Snapshot()
	foundErr := false
	for _, p := range snap3.Series.Errors {
		if p.V >= 1 {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Fatal("expected errors series point from meter sample")
	}

	h.End(nil)
}
