package telemetry

import (
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
)

func TestByteMeterLiveTotalsBeforeEnd(t *testing.T) {
	m := NewByteMeter()
	id := m.BeginTransfer(TransferMeta{SessionID: "s1", MediaMode: "direct", Method: "GET"})
	if id == 0 {
		t.Fatal("expected non-zero transfer id")
	}
	m.AddTransferEgress(id, 1_000_000)
	m.AddTransferIngress(id, 1_000_000)
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
	m.EndTransfer(id, nil)
	if m.ActiveTransferCount() != 0 {
		t.Fatal("expected no active transfers")
	}
	if m.CompletedEgress() != 1_000_000 {
		t.Fatalf("completed=%d", m.CompletedEgress())
	}
}

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
