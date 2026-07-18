package telemetry

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
)

func TestPlaybackTTLAndStopped(t *testing.T) {
	em := observe.NewEmitter(64)
	defer em.Close()
	r := New(em)

	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	r.handle(observe.Event{
		Kind:          observe.KindPlayback,
		At:            base,
		SessionID:     "sess-1",
		UserID:        "u1",
		Username:      "alice",
		ItemID:        "item-1",
		ItemName:      "Secret Movie",
		PlaybackEvent: observe.PlaybackPlaying,
		PositionTicks: 1_000_000,
	})
	if got := len(r.ActivePlaybacks()); got != 1 {
		t.Fatalf("active playbacks: got %d want 1", got)
	}
	if !r.HasActiveMediaLoad() {
		t.Fatal("expected media load")
	}

	// Within TTL
	r.now = func() time.Time { return base.Add(60 * time.Second) }
	if got := len(r.ActivePlaybacks()); got != 1 {
		t.Fatalf("within TTL: got %d", got)
	}

	// Past 90s TTL
	r.now = func() time.Time { return base.Add(91 * time.Second) }
	if got := len(r.ActivePlaybacks()); got != 0 {
		t.Fatalf("expired: got %d want 0", got)
	}

	// Stopped removes immediately
	r.now = func() time.Time { return base.Add(2 * time.Minute) }
	r.handle(observe.Event{
		Kind:          observe.KindPlayback,
		At:            base.Add(2 * time.Minute),
		SessionID:     "sess-1",
		ItemID:        "item-2",
		PlaybackEvent: observe.PlaybackProgress,
	})
	if got := len(r.ActivePlaybacks()); got != 1 {
		t.Fatalf("progress: got %d", got)
	}
	r.handle(observe.Event{
		Kind:          observe.KindPlayback,
		At:            base.Add(2 * time.Minute),
		SessionID:     "sess-1",
		ItemID:        "item-2",
		PlaybackEvent: observe.PlaybackStopped,
	})
	if got := len(r.ActivePlaybacks()); got != 0 {
		t.Fatalf("stopped: got %d want 0", got)
	}
}

func TestSessionTTL(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	r.NoteSessionActivity("s1", "u1", "alice", "iPhone", "203.0.113.1")
	if r.ActiveSessionCount() != 1 {
		t.Fatalf("active sessions: %d", r.ActiveSessionCount())
	}

	r.now = func() time.Time { return base.Add(4 * time.Minute) }
	if r.ActiveSessionCount() != 1 {
		t.Fatal("should still be active at 4m")
	}

	r.now = func() time.Time { return base.Add(5*time.Minute + time.Second) }
	if r.ActiveSessionCount() != 0 {
		t.Fatalf("should expire after 5m, got %d", r.ActiveSessionCount())
	}
}

func TestMediaTransferOpenAndComplete(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	// Legacy open: empty phase, no outcome.
	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base,
		SessionID: "s1",
		ItemID:    "i1",
		MediaMode: observe.MediaDirect,
		BytesOut:  100,
	})
	if got := len(r.ActiveTransfers()); got != 1 {
		t.Fatalf("open transfers: %d", got)
	}
	if !r.HasActiveMediaLoad() {
		t.Fatal("expected media load from transfer")
	}

	// Legacy complete: empty phase with terminal outcome.
	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base.Add(time.Second),
		SessionID: "s1",
		ItemID:    "i1",
		MediaMode: observe.MediaDirect,
		BytesOut:  5000,
		Outcome:   observe.OutcomeOK,
	})
	if got := len(r.ActiveTransfers()); got != 0 {
		t.Fatalf("completed transfers: %d", got)
	}
}

func TestMediaTransferPhaseStartEnd(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	// PhaseStart opens the transfer even if Outcome is accidentally set.
	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base,
		Phase:     observe.PhaseStart,
		SessionID: "s1",
		ItemID:    "i1",
		MediaMode: observe.MediaDirect,
		Method:    "GET",
		Outcome:   observe.OutcomeOK, // must not close on start
	})
	if got := len(r.ActiveTransfers()); got != 1 {
		t.Fatalf("active during transfer: got %d want 1", got)
	}
	if !r.HasActiveMediaLoad() {
		t.Fatal("HasActiveMediaLoad should be true while transfer is open")
	}
	// PhaseStart must not count a request (RPS stays 0).
	if snap := r.Snapshot(); snap.Traffic.RPS != 0 {
		t.Fatalf("PhaseStart must not increment Requests: rps=%v", snap.Traffic.RPS)
	}

	// Mid-stream: still active.
	r.now = func() time.Time { return base.Add(30 * time.Second) }
	if got := len(r.ActiveTransfers()); got != 1 {
		t.Fatalf("still active mid-transfer: got %d", got)
	}
	if !r.HasActiveMediaLoad() {
		t.Fatal("expected media load mid-transfer")
	}

	// PhaseEnd closes and counts bytes only (not Requests — KindRequest owns RPS).
	endAt := base.Add(30 * time.Second)
	r.handle(observe.Event{
		Kind:       observe.KindMediaTransfer,
		At:         endAt,
		Phase:      observe.PhaseEnd,
		SessionID:  "s1",
		ItemID:     "i1",
		MediaMode:  observe.MediaDirect,
		Method:     "GET",
		Outcome:    observe.OutcomeOK,
		BytesOut:   9000,
		DurationMS: 30000,
	})
	if got := len(r.ActiveTransfers()); got != 0 {
		t.Fatalf("empty after end: got %d want 0", got)
	}
	if r.HasActiveMediaLoad() {
		t.Fatal("HasActiveMediaLoad should be false after transfer ends")
	}
	// Media transfer events must not contribute to RPS, but still count bytes.
	r.now = func() time.Time { return endAt }
	snap := r.Snapshot()
	if snap.Traffic.RPS != 0 {
		t.Fatalf("start+end must not count Requests: rps=%v want 0", snap.Traffic.RPS)
	}
	// 9000 bytes over 10s window → MbpsOut > 0
	if snap.Traffic.MbpsOut <= 0 {
		t.Fatalf("PhaseEnd should still count bytes: mbpsOut=%v", snap.Traffic.MbpsOut)
	}
}

func TestMediaTransferStartEndDoesNotCountRequests(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base,
		Phase:     observe.PhaseStart,
		SessionID: "sess-count",
		ItemID:    "item-count",
		MediaMode: observe.MediaHLS,
		Method:    "GET",
	})
	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base,
		Phase:     observe.PhaseEnd,
		SessionID: "sess-count",
		ItemID:    "item-count",
		MediaMode: observe.MediaHLS,
		Method:    "GET",
		Outcome:   observe.OutcomeOK,
		BytesOut:  100,
	})
	// Transfer events alone must not inflate RPS (KindRequest already counts the HTTP request).
	snap := r.Snapshot()
	if snap.Traffic.RPS != 0 {
		t.Fatalf("rps=%v want 0 (media transfer must not increment Requests)", snap.Traffic.RPS)
	}
	if snap.Traffic.MbpsOut <= 0 {
		t.Fatalf("bytes still counted: mbpsOut=%v", snap.Traffic.MbpsOut)
	}
}

func TestMediaTransferKeyWithoutItemID(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	// Start/end without ItemID must share a stable key (session+mode+method).
	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base,
		Phase:     observe.PhaseStart,
		SessionID: "sess-a",
		MediaMode: observe.MediaHLS,
		Method:    "GET",
	})
	if got := len(r.ActiveTransfers()); got != 1 {
		t.Fatalf("open without item: %d", got)
	}
	if !r.HasActiveMediaLoad() {
		t.Fatal("expected active media load")
	}
	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base.Add(time.Second),
		Phase:     observe.PhaseEnd,
		SessionID: "sess-a",
		MediaMode: observe.MediaHLS,
		Method:    "GET",
		Outcome:   observe.OutcomeOK,
		BytesOut:  100,
	})
	if got := len(r.ActiveTransfers()); got != 0 {
		t.Fatalf("end without item should close: %d", got)
	}
}

func TestRequestRefreshesSessionLastSeen(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	r.handle(observe.Event{
		Kind:      observe.KindAuthLogin,
		At:        base,
		SessionID: "s1",
		UserID:    "u1",
		Username:  "alice",
		Outcome:   observe.OutcomeOK,
	})
	if r.ActiveSessionCount() != 1 {
		t.Fatal("expected session after login")
	}

	// Near expiry without refresh would drop; KindRequest should keep it alive.
	nearExpiry := base.Add(4*time.Minute + 50*time.Second)
	r.now = func() time.Time { return nearExpiry }
	r.handle(observe.Event{
		Kind:      observe.KindRequest,
		At:        nearExpiry,
		SessionID: "s1",
		UserID:    "u1",
		Username:  "alice",
		Outcome:   observe.OutcomeOK,
	})

	// 4m50s after the request refresh — still within 5m TTL from last_seen.
	r.now = func() time.Time { return nearExpiry.Add(4*time.Minute + 50*time.Second) }
	if r.ActiveSessionCount() != 1 {
		t.Fatal("KindRequest should refresh session last_seen")
	}

	// Past TTL from last request.
	r.now = func() time.Time { return nearExpiry.Add(5*time.Minute + time.Second) }
	if r.ActiveSessionCount() != 0 {
		t.Fatalf("should expire after 5m without further activity, got %d", r.ActiveSessionCount())
	}
}

func TestDropCounterInSnapshot(t *testing.T) {
	em := observe.NewEmitter(1)
	defer em.Close()
	r := New(em)

	// Fill buffer without consumer
	if !em.TryEmit(observe.Event{Kind: observe.KindRequest}) {
		t.Fatal("first emit")
	}
	if em.TryEmit(observe.Event{Kind: observe.KindRequest}) {
		t.Fatal("expected drop")
	}
	if em.Drops() != 1 {
		t.Fatalf("drops: %d", em.Drops())
	}

	snap := r.Snapshot()
	if snap.Reliability.TelemetryDrops != 1 {
		t.Fatalf("snapshot drops: %d", snap.Reliability.TelemetryDrops)
	}
	if snap.BootID == "" {
		t.Fatal("expected boot_id")
	}
}

func TestRateCalculations(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	// Process 20 requests over 10 seconds with known bytes and some errors.
	for i := 0; i < 20; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		r.now = func() time.Time { return at }
		outcome := observe.OutcomeOK
		status := observe.Status2xx
		if i%5 == 0 {
			outcome = observe.OutcomeError
			status = observe.Status5xx
		}
		r.handle(observe.Event{
			Kind:        observe.KindRequest,
			At:          at,
			Outcome:     outcome,
			StatusClass: status,
			BytesIn:     1_000_000, // 1 MB
			BytesOut:    2_000_000, // 2 MB
		})
	}
	// Snapshot at last second of the window
	last := base.Add(19 * time.Second)
	r.now = func() time.Time { return last }
	snap := r.Snapshot()

	// rps over last 10s: requests at t=10..19 = 10 requests → 1.0 rps
	if snap.Traffic.RPS < 0.9 || snap.Traffic.RPS > 1.1 {
		t.Fatalf("rps: got %v want ~1.0", snap.Traffic.RPS)
	}
	// mbps_out: 10 * 2MB * 8 / 10s / 1e6 = 16 Mbps
	if snap.Traffic.MbpsOut < 15 || snap.Traffic.MbpsOut > 17 {
		t.Fatalf("mbps_out: got %v want ~16", snap.Traffic.MbpsOut)
	}
	// error rate 15m: 4 errors / 20 requests = 0.2
	if snap.Traffic.ErrorRate15m < 0.15 || snap.Traffic.ErrorRate15m > 0.25 {
		t.Fatalf("error_rate_15m: got %v want ~0.2", snap.Traffic.ErrorRate15m)
	}

	// Capacity rejects
	r.handle(observe.Event{Kind: observe.KindCapacityReject, At: last, Outcome: observe.OutcomeDenied})
	r.handle(observe.Event{Kind: observe.KindCapacityReject, At: last, Outcome: observe.OutcomeDenied})
	snap = r.Snapshot()
	if snap.Capacity.Rejects5m != 2 {
		t.Fatalf("rejects_5m: %d", snap.Capacity.Rejects5m)
	}
}

func TestUpstreamAndReliability(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	r.handle(observe.Event{
		Kind:        observe.KindUpstreamRequest,
		At:          base,
		Outcome:     observe.OutcomeOK,
		StatusClass: observe.Status2xx,
		DurationMS:  42,
	})
	r.handle(observe.Event{
		Kind:        observe.KindUpstreamAuthRefresh,
		At:          base,
		Outcome:     observe.OutcomeOK,
		StatusClass: observe.Status2xx,
	})
	r.handle(observe.Event{
		Kind:      observe.KindUserdataError,
		At:        base,
		ErrorKind: "userdata_write",
	})
	r.handle(observe.Event{
		Kind:      observe.KindReliability,
		At:        base,
		Outcome:   observe.OutcomeError,
		ErrorKind: "overlay_fail",
	})

	snap := r.Snapshot()
	if snap.Upstream.LastLatencyMS != 42 {
		t.Fatalf("latency: %d", snap.Upstream.LastLatencyMS)
	}
	if !snap.Upstream.AuthOK {
		t.Fatal("expected auth ok")
	}
	if snap.Upstream.LastOKAt == nil {
		t.Fatal("expected last_ok_at")
	}
	if snap.Reliability.UserdataWriteFail5m < 1 {
		t.Fatalf("userdata fails: %d", snap.Reliability.UserdataWriteFail5m)
	}
	if snap.Reliability.OverlayFail5m < 1 {
		t.Fatalf("overlay fails: %d", snap.Reliability.OverlayFail5m)
	}
}

func TestSnapshotRaceSafe(t *testing.T) {
	em := observe.NewEmitter(256)
	defer em.Close()
	r := New(em)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.Start(ctx)
	}()

	// producers
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				em.TryEmit(observe.Event{
					Kind:          observe.KindPlayback,
					SessionID:     "s",
					ItemID:        "i",
					PlaybackEvent: observe.PlaybackProgress,
					PositionTicks: int64(j),
				})
				em.TryEmit(observe.Event{
					Kind:        observe.KindRequest,
					Outcome:     observe.OutcomeOK,
					StatusClass: observe.Status2xx,
					BytesOut:    100,
				})
				r.NoteSessionActivity("s", "u", "user", "dev", "10.0.0.1")
				_ = r.Snapshot()
				_ = r.ActivePlaybacks()
				_ = r.ActiveTransfers()
				_ = r.ActiveSessionCount()
				_ = r.HasActiveMediaLoad()
			}
		}(i)
	}

	time.Sleep(50 * time.Millisecond)
	cancel()
	// Close emitter to unblock Start if still reading
	em.Close()
	wg.Wait()
}

func TestStartConsumesEvents(t *testing.T) {
	em := observe.NewEmitter(16)
	r := New(em)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.Start(ctx)
		close(done)
	}()

	if !em.TryEmit(observe.Event{
		Kind:          observe.KindPlayback,
		SessionID:     "s1",
		ItemID:        "i1",
		PlaybackEvent: observe.PlaybackPlaying,
	}) {
		t.Fatal("emit")
	}

	// wait for consumption
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.ActivePlaybacks()) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(r.ActivePlaybacks()) != 1 {
		t.Fatal("event not consumed")
	}

	cancel()
	em.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit")
	}
}

func TestSeriesPrivacyNoRawIdentity(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	r.handle(observe.Event{
		Kind:          observe.KindPlayback,
		At:            base,
		SessionID:     "sess-secret",
		UserID:        "user-secret",
		ItemID:        "item-secret",
		ItemName:      "Top Secret Film",
		Device:        "device-secret",
		PlaybackEvent: observe.PlaybackPlaying,
	})
	r.NoteSessionActivity("sess-secret", "user-secret", "alice", "iPhone", "198.51.100.9")

	snap := r.Snapshot()
	// Series only has numeric values and timestamps — no identity strings.
	for _, pt := range snap.Series.Playbacks {
		if pt.V < 0 {
			t.Fatalf("negative playback series: %v", pt.V)
		}
	}
	// Active playbacks may include item name for ops UI current-state, but series must not.
	// Ensure series points don't embed names (struct has only T,V).
	if len(snap.Series.RPS) != seriesPoints {
		t.Fatalf("rps series len: %d", len(snap.Series.RPS))
	}
}

func TestAuthLogoutRemovesSession(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	r.handle(observe.Event{
		Kind:      observe.KindAuthLogin,
		At:        base,
		SessionID: "s1",
		UserID:    "u1",
		Username:  "alice",
		Outcome:   observe.OutcomeOK,
	})
	if r.ActiveSessionCount() != 1 {
		t.Fatal("expected session after login")
	}
	r.handle(observe.Event{
		Kind:      observe.KindAuthLogout,
		At:        base,
		SessionID: "s1",
		Outcome:   observe.OutcomeOK,
	})
	if r.ActiveSessionCount() != 0 {
		t.Fatal("expected session removed on logout")
	}
}
