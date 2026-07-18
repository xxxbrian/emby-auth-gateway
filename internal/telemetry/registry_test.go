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
	// Active transfers are meter-backed (unique handles), not event-derived keys.
	h := r.Meter().BeginTransfer(TransferMeta{SessionID: "s1", ItemID: "i1", MediaMode: observe.MediaDirect})
	if got := len(r.ActiveTransfers()); got != 1 {
		t.Fatalf("open transfers: %d", got)
	}
	if !r.HasActiveMediaLoad() {
		t.Fatal("expected media load from transfer")
	}
	h.AddEgress(5000)
	h.End(nil)
	if got := len(r.ActiveTransfers()); got != 0 {
		t.Fatalf("completed transfers: %d", got)
	}
}

func TestMediaTransferPhaseStartEnd(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	h := r.Meter().BeginTransfer(TransferMeta{SessionID: "s1", ItemID: "i1", MediaMode: observe.MediaDirect, Method: "GET"})
	if got := len(r.ActiveTransfers()); got != 1 {
		t.Fatalf("active during transfer: got %d want 1", got)
	}
	if !r.HasActiveMediaLoad() {
		t.Fatal("HasActiveMediaLoad should be true while transfer is open")
	}
	h.AddEgress(9000)
	// Live bytes visible before end.
	if _, out := r.Meter().Totals(); out != 9000 {
		t.Fatalf("live egress=%d", out)
	}

	// PhaseEnd events must not inflate RPS or Mbps rings.
	r.handle(observe.Event{
		Kind:      observe.KindMediaTransfer,
		At:        base,
		Phase:     observe.PhaseEnd,
		SessionID: "s1",
		ItemID:    "i1",
		MediaMode: observe.MediaDirect,
		Method:    "GET",
		Outcome:   observe.OutcomeOK,
		BytesOut:  9000,
	})
	snap := r.Snapshot()
	if snap.Traffic.RPS != 0 || snap.Traffic.MbpsOut != 0 {
		t.Fatalf("PhaseEnd must not affect RPS/Mbps: rps=%v mbps=%v", snap.Traffic.RPS, snap.Traffic.MbpsOut)
	}
	h.End(nil)
	if r.HasActiveMediaLoad() {
		t.Fatal("expected no media load after end")
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
	// Transfer events alone must not inflate RPS or Mbps (live meter owns bandwidth).
	snap := r.Snapshot()
	if snap.Traffic.RPS != 0 {
		t.Fatalf("rps=%v want 0 (media transfer must not increment Requests)", snap.Traffic.RPS)
	}
	if snap.Traffic.MbpsOut != 0 {
		t.Fatalf("PhaseEnd must not count bytes into Mbps: mbpsOut=%v", snap.Traffic.MbpsOut)
	}
}

func TestMediaTransferUniqueHandlesWithoutItemID(t *testing.T) {
	r := New(nil)
	// Concurrent HLS segments get unique handles (no session+mode collapse).
	h1 := r.Meter().BeginTransfer(TransferMeta{SessionID: "sess-a", MediaMode: observe.MediaHLS, Method: "GET"})
	h2 := r.Meter().BeginTransfer(TransferMeta{SessionID: "sess-a", MediaMode: observe.MediaHLS, Method: "GET"})
	if h1.ID() == h2.ID() {
		t.Fatal("expected unique transfer ids")
	}
	if got := len(r.ActiveTransfers()); got != 2 {
		t.Fatalf("open concurrent: %d", got)
	}
	h1.End(nil)
	if got := len(r.ActiveTransfers()); got != 1 {
		t.Fatalf("after one end: %d", got)
	}
	h2.End(nil)
	if got := len(r.ActiveTransfers()); got != 0 {
		t.Fatalf("after both end: %d", got)
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
	if snap.Upstream.AuthState != AuthStateHealthy {
		t.Fatalf("auth_state: %q want %q", snap.Upstream.AuthState, AuthStateHealthy)
	}
	if snap.Upstream.LastAuthAt == nil {
		t.Fatal("expected last_auth_at")
	}
	if snap.Upstream.LastAuthError != "" {
		t.Fatalf("last_auth_error: %q want empty", snap.Upstream.LastAuthError)
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

func TestAuthStateThreeStateTransitions(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }

	// Startup: unknown, always serialized.
	snap := r.Snapshot()
	if snap.Upstream.AuthState != AuthStateUnknown {
		t.Fatalf("initial auth_state: %q want %q", snap.Upstream.AuthState, AuthStateUnknown)
	}
	if snap.Upstream.LastAuthAt != nil || snap.Upstream.LastAuthError != "" {
		t.Fatalf("initial auth fields: at=%v err=%q", snap.Upstream.LastAuthAt, snap.Upstream.LastAuthError)
	}

	// 2xx managed upstream request -> healthy + timestamp, clears error.
	r.handle(observe.Event{
		Kind:        observe.KindUpstreamRequest,
		At:          base,
		Outcome:     observe.OutcomeOK,
		StatusClass: observe.Status2xx,
		DurationMS:  10,
	})
	snap = r.Snapshot()
	if snap.Upstream.AuthState != AuthStateHealthy {
		t.Fatalf("after 2xx: auth_state=%q", snap.Upstream.AuthState)
	}
	if snap.Upstream.LastAuthAt == nil || !snap.Upstream.LastAuthAt.Equal(base) {
		t.Fatalf("after 2xx: last_auth_at=%v want %v", snap.Upstream.LastAuthAt, base)
	}
	if snap.Upstream.LastAuthError != "" {
		t.Fatalf("after 2xx: last_auth_error=%q", snap.Upstream.LastAuthError)
	}

	// 4xx / 5xx / network (status0) must not change auth state.
	prevAuthAt := *snap.Upstream.LastAuthAt
	for i, class := range []string{observe.Status4xx, observe.Status5xx, observe.Status0} {
		at := base.Add(time.Duration(i+1) * time.Second)
		outcome := observe.OutcomeOK
		if class != observe.Status4xx {
			outcome = observe.OutcomeError
		}
		r.handle(observe.Event{
			Kind:        observe.KindUpstreamRequest,
			At:          at,
			Outcome:     outcome,
			StatusClass: class,
		})
		snap = r.Snapshot()
		if snap.Upstream.AuthState != AuthStateHealthy {
			t.Fatalf("after %s: auth_state=%q want healthy", class, snap.Upstream.AuthState)
		}
		if snap.Upstream.LastAuthAt == nil || !snap.Upstream.LastAuthAt.Equal(prevAuthAt) {
			t.Fatalf("after %s: last_auth_at changed to %v", class, snap.Upstream.LastAuthAt)
		}
		if snap.Upstream.LastAuthError != "" {
			t.Fatalf("after %s: last_auth_error=%q", class, snap.Upstream.LastAuthError)
		}
	}

	// Explicit refresh failure -> failing with stable code.
	failAt := base.Add(10 * time.Second)
	r.handle(observe.Event{
		Kind:      observe.KindUpstreamAuthRefresh,
		At:        failAt,
		Outcome:   observe.OutcomeError,
		ErrorKind: AuthErrorRefreshFailed,
	})
	snap = r.Snapshot()
	if snap.Upstream.AuthState != AuthStateFailing {
		t.Fatalf("after refresh fail: auth_state=%q", snap.Upstream.AuthState)
	}
	if snap.Upstream.LastAuthError != AuthErrorRefreshFailed {
		t.Fatalf("after refresh fail: last_auth_error=%q", snap.Upstream.LastAuthError)
	}
	// last_auth_at remains last successful auth evidence.
	if snap.Upstream.LastAuthAt == nil || !snap.Upstream.LastAuthAt.Equal(prevAuthAt) {
		t.Fatalf("after refresh fail: last_auth_at=%v want %v", snap.Upstream.LastAuthAt, prevAuthAt)
	}

	// auth_unavailable is also a stable failing code.
	r.handle(observe.Event{
		Kind:      observe.KindUpstreamAuthRefresh,
		At:        failAt.Add(time.Second),
		Outcome:   observe.OutcomeError,
		ErrorKind: AuthErrorAuthUnavailable,
	})
	snap = r.Snapshot()
	if snap.Upstream.AuthState != AuthStateFailing || snap.Upstream.LastAuthError != AuthErrorAuthUnavailable {
		t.Fatalf("auth_unavailable: state=%q err=%q", snap.Upstream.AuthState, snap.Upstream.LastAuthError)
	}

	// Non-stable ErrorKind is bounded to refresh_failed.
	r.handle(observe.Event{
		Kind:      observe.KindUpstreamAuthRefresh,
		At:        failAt.Add(2 * time.Second),
		Outcome:   observe.OutcomeError,
		ErrorKind: "connection reset by peer and a very long raw string",
	})
	snap = r.Snapshot()
	if snap.Upstream.LastAuthError != AuthErrorRefreshFailed {
		t.Fatalf("raw error kind: last_auth_error=%q want %q", snap.Upstream.LastAuthError, AuthErrorRefreshFailed)
	}

	// Recovery via successful refresh: healthy, clears error, updates timestamp.
	okAt := base.Add(20 * time.Second)
	r.handle(observe.Event{
		Kind:        observe.KindUpstreamAuthRefresh,
		At:          okAt,
		Outcome:     observe.OutcomeOK,
		StatusClass: observe.Status2xx,
	})
	snap = r.Snapshot()
	if snap.Upstream.AuthState != AuthStateHealthy {
		t.Fatalf("after refresh ok: auth_state=%q", snap.Upstream.AuthState)
	}
	if snap.Upstream.LastAuthError != "" {
		t.Fatalf("after refresh ok: last_auth_error=%q", snap.Upstream.LastAuthError)
	}
	if snap.Upstream.LastAuthAt == nil || !snap.Upstream.LastAuthAt.Equal(okAt) {
		t.Fatalf("after refresh ok: last_auth_at=%v want %v", snap.Upstream.LastAuthAt, okAt)
	}

	// Fail again then recover via managed 2xx request.
	r.handle(observe.Event{
		Kind:      observe.KindUpstreamAuthRefresh,
		At:        okAt.Add(time.Second),
		Outcome:   observe.OutcomeError,
		ErrorKind: AuthErrorRefreshFailed,
	})
	recoverAt := okAt.Add(2 * time.Second)
	r.handle(observe.Event{
		Kind:        observe.KindUpstreamRequest,
		At:          recoverAt,
		Outcome:     observe.OutcomeOK,
		StatusClass: observe.Status2xx,
	})
	snap = r.Snapshot()
	if snap.Upstream.AuthState != AuthStateHealthy || snap.Upstream.LastAuthError != "" {
		t.Fatalf("2xx recovery: state=%q err=%q", snap.Upstream.AuthState, snap.Upstream.LastAuthError)
	}
	if snap.Upstream.LastAuthAt == nil || !snap.Upstream.LastAuthAt.Equal(recoverAt) {
		t.Fatalf("2xx recovery: last_auth_at=%v want %v", snap.Upstream.LastAuthAt, recoverAt)
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
	if len(snap.Series.RPS) != series15mSec {
		t.Fatalf("rps series len: %d want %d", len(snap.Series.RPS), series15mSec)
	}
	if snap.Series.Window != string(Window15m) {
		t.Fatalf("default window: %q", snap.Series.Window)
	}
}

func TestParseSeriesWindow(t *testing.T) {
	cases := []struct {
		in   string
		want SeriesWindow
	}{
		{"", Window15m},
		{"15m", Window15m},
		{"1h", Window1h},
		{"6h", Window6h},
		{"24h", Window24h},
		{"1H", Window1h},
		{" bogus ", Window15m},
	}
	for _, tc := range cases {
		if got := ParseSeriesWindow(tc.in); got != tc.want {
			t.Fatalf("ParseSeriesWindow(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestSnapshotWindowSeries(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

	// 3 requests in the same second (one error), plus a playback sample.
	r.now = func() time.Time { return base }
	r.handle(observe.Event{
		Kind:        observe.KindRequest,
		At:          base,
		Outcome:     observe.OutcomeOK,
		StatusClass: observe.Status2xx,
		BytesIn:     1_000_000,
		BytesOut:    2_000_000,
	})
	r.handle(observe.Event{
		Kind:          observe.KindPlayback,
		At:            base,
		SessionID:     "s1",
		ItemID:        "i1",
		PlaybackEvent: observe.PlaybackPlaying,
	})
	r.handle(observe.Event{Kind: observe.KindRequest, At: base, Outcome: observe.OutcomeOK, StatusClass: observe.Status2xx})
	r.handle(observe.Event{Kind: observe.KindRequest, At: base, Outcome: observe.OutcomeError, StatusClass: observe.Status5xx})

	// Later minute: 60 MB out → 8 Mbps average over the 1m bucket.
	later := base.Add(5 * time.Minute)
	r.now = func() time.Time { return later }
	r.handle(observe.Event{
		Kind:        observe.KindRequest,
		At:          later,
		Outcome:     observe.OutcomeOK,
		StatusClass: observe.Status2xx,
		BytesOut:    60_000_000,
	})

	type want struct {
		window   SeriesWindow
		interval string
		points   int
	}
	for _, w := range []want{
		{Window15m, "1s", series15mSec},
		{Window1h, "1m", series1hMin},
		{Window6h, "1m", series6hMin},
		{Window24h, "1m", series24hMin},
	} {
		snap := r.SnapshotWindow(w.window)
		if snap.Series.Window != string(w.window) {
			t.Fatalf("%s: window=%q", w.window, snap.Series.Window)
		}
		if snap.Series.Interval != w.interval {
			t.Fatalf("%s: interval=%q want %q", w.window, snap.Series.Interval, w.interval)
		}
		if len(snap.Series.RPS) != w.points ||
			len(snap.Series.MbpsIn) != w.points ||
			len(snap.Series.MbpsOut) != w.points ||
			len(snap.Series.Errors) != w.points ||
			len(snap.Series.Playbacks) != w.points {
			t.Fatalf("%s: series lengths rps=%d mbps_in=%d mbps_out=%d errors=%d playbacks=%d want %d",
				w.window,
				len(snap.Series.RPS), len(snap.Series.MbpsIn), len(snap.Series.MbpsOut),
				len(snap.Series.Errors), len(snap.Series.Playbacks), w.points)
		}
		if !snap.Series.RPS[0].T.Before(snap.Series.RPS[len(snap.Series.RPS)-1].T) {
			t.Fatalf("%s: series not oldest-first", w.window)
		}
	}

	// 15m @ 1s: base second has 3 requests → rps=3; 1 error count; playbacks max >= 1.
	snap15 := r.SnapshotWindow(Window15m)
	lastRPS := snap15.Series.RPS[len(snap15.Series.RPS)-1].V
	if lastRPS < 0.9 || lastRPS > 1.1 {
		t.Fatalf("15m last rps: got %v want ~1", lastRPS)
	}
	foundRPS, foundErr, foundPB := false, false, false
	for i, pt := range snap15.Series.RPS {
		if pt.V > 2.5 && pt.V < 3.5 {
			foundRPS = true
		}
		if snap15.Series.Errors[i].V >= 1 {
			foundErr = true
		}
		if snap15.Series.Playbacks[i].V >= 1 {
			foundPB = true
		}
	}
	if !foundRPS {
		t.Fatal("15m series missing ~3 rps bucket")
	}
	if !foundErr {
		t.Fatal("15m series missing error count >= 1")
	}
	if !foundPB {
		t.Fatal("15m series missing playbacks max >= 1")
	}

	// 1h @ 1m: last minute has 1 request → rps=1/60; 60MB → 8 Mbps.
	snap1h := r.SnapshotWindow(Window1h)
	lastMinRPS := snap1h.Series.RPS[len(snap1h.Series.RPS)-1].V
	wantRPS := 1.0 / 60.0
	if lastMinRPS < wantRPS*0.9 || lastMinRPS > wantRPS*1.1 {
		t.Fatalf("1h last rps: got %v want ~%v", lastMinRPS, wantRPS)
	}
	lastMbps := snap1h.Series.MbpsOut[len(snap1h.Series.MbpsOut)-1].V
	if lastMbps < 7.5 || lastMbps > 8.5 {
		t.Fatalf("1h last mbps_out: got %v want ~8", lastMbps)
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
