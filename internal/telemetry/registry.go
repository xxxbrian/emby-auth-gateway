package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
)

const (
	sessionTTL  = 5 * time.Minute
	playbackTTL = 90 * time.Second
	secBuckets  = 3600
	minBuckets  = 1440

	// Series point counts by window.
	series15mSec = 900  // 15m @ 1s
	series1hMin  = 60   // 1h @ 1m
	series6hMin  = 360  // 6h @ 1m
	series24hMin = 1440 // 24h @ 1m

	// Rate windows.
	rpsWindowSec       = 10
	trafficWindowSec   = 10
	rejectWindowMin    = 5
	errorWindowMin     = 15
	reliabilityWindowM = 5
)

// Registry consumes observe events and maintains in-memory metrics.
// Live forwarded bandwidth is driven by ByteMeter (atomic totals + 1s sampler),
// not by end-of-transfer observe events.
type Registry struct {
	emitter         *observe.Emitter
	meter           *ByteMeter
	mediaBufferLive *MediaBufferLiveRegistry
	bootID          string
	started         time.Time
	now             func() time.Time

	mu sync.RWMutex

	sessions  map[string]*sessionState
	playbacks map[string]*playbackState
	transfers map[string]*transferState // legacy event-based; prefer meter handles
	upstream  upstreamState

	sec *timeRing
	min *timeRing

	startOnce sync.Once

	mediaMu            sync.RWMutex
	mediaProvider      MediaBufferControllerProvider
	mediaSec           *mediaBufferGaugeRing
	mediaMin           *mediaBufferGaugeRing
	mediaRecent        *mediaBufferCompletionRing
	mediaTicker        func() (<-chan time.Time, func())
	mediaLatest        MediaBufferAggregate
	mediaLatestPresent bool
}

// New creates a Registry bound to emitter. A nil emitter is allowed.
// ByteMeter is always created for live bandwidth sampling.
func New(emitter *observe.Emitter) *Registry {
	bootID := newBootID()
	return &Registry{
		emitter:         emitter,
		meter:           NewByteMeter(),
		mediaBufferLive: newMediaBufferLiveRegistry(bootID),
		bootID:          bootID,
		started:         time.Now().UTC(),
		now:             func() time.Time { return time.Now().UTC() },
		sessions:        make(map[string]*sessionState),
		playbacks:       make(map[string]*playbackState),
		transfers:       make(map[string]*transferState),
		sec:             newTimeRing(time.Second, secBuckets),
		min:             newTimeRing(time.Minute, minBuckets),
		mediaSec:        newMediaBufferGaugeRing(time.Second, secBuckets),
		mediaMin:        newMediaBufferGaugeRing(time.Minute, minBuckets),
		mediaRecent:     newMediaBufferCompletionRing(),
		mediaTicker: func() (<-chan time.Time, func()) {
			ticker := time.NewTicker(time.Second)
			return ticker.C, ticker.Stop
		},
	}
}

// BootID returns the immutable process telemetry boot identity.
func (r *Registry) BootID() string {
	if r == nil {
		return ""
	}
	return r.bootID
}

// MediaBufferLive returns the preallocated Phase 1 live registry. Merely
// constructing it does not enable gateway observation.
func (r *Registry) MediaBufferLive() *MediaBufferLiveRegistry {
	if r == nil {
		return nil
	}
	return r.mediaBufferLive
}

// Meter returns the live byte meter (never nil for a non-nil Registry).
func (r *Registry) Meter() *ByteMeter {
	if r == nil {
		return nil
	}
	return r.meter
}

func newBootID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}

// Start runs the live-byte sampler and optional observe consumer until ctx is done.
// Safe to call once; subsequent calls are no-ops.
func (r *Registry) Start(ctx context.Context) {
	if r == nil {
		return
	}
	r.startOnce.Do(func() {
		go r.sampleLiveBytes(ctx)
		go r.sampleMediaBuffer(ctx)
		if r.emitter == nil {
			return
		}
		ch := r.emitter.Events()
		if ch == nil {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				r.handle(ev)
			}
		}
	})
}

// sampleLiveBytes moves cumulative ByteMeter totals into 1s/1m rings once per second.
// This is the sole source of Traffic.Mbps* / series bandwidth (not PhaseEnd events).
// prev starts at 0 so bytes written before the sampler goroutine starts are not lost.
func (r *Registry) sampleLiveBytes(ctx context.Context) {
	if r == nil || r.meter == nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var prevIn, prevOut, prevErr uint64
	// First sample immediately so early traffic is not discarded until the first tick.
	r.SampleLiveBytesOnce(r.sampleTime(time.Now()), &prevIn, &prevOut, &prevErr)
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.SampleLiveBytesOnce(r.sampleTime(now), &prevIn, &prevOut, &prevErr)
		}
	}
}

func (r *Registry) sampleTime(fallback time.Time) time.Time {
	if r != nil && r.now != nil {
		return r.now()
	}
	return fallback.UTC()
}

// SampleLiveBytesOnce applies one meter→ring sample. Used by the sampler and tests.
// prev* are updated in place. Delayed ticks attribute the full multi-second delta
// to the sample timestamp (documented trade-off vs splitting buckets).
func (r *Registry) SampleLiveBytesOnce(at time.Time, prevIn, prevOut, prevErr *uint64) {
	if r == nil || r.meter == nil || prevIn == nil || prevOut == nil || prevErr == nil {
		return
	}
	in, out := r.meter.Totals()
	errs := r.meter.ErrorTotal()
	dIn := int64(in - *prevIn)
	dOut := int64(out - *prevOut)
	dErr := int64(errs - *prevErr)
	*prevIn, *prevOut, *prevErr = in, out, errs
	if dIn < 0 {
		dIn = 0
	}
	if dOut < 0 {
		dOut = 0
	}
	if dErr < 0 {
		dErr = 0
	}
	if dIn == 0 && dOut == 0 && dErr == 0 {
		return
	}
	at = at.UTC()
	r.mu.Lock()
	fn := func(c *counters) {
		c.BytesIn += dIn
		c.BytesOut += dOut
		c.Errors += dErr
	}
	r.sec.add(at, fn)
	r.min.add(at, fn)
	r.mu.Unlock()
}

// NoteSessionActivity records request activity for session liveness (5m TTL).
// IP is stored for current-state only and never used as a series label.
func (r *Registry) NoteSessionActivity(sessionID, userID, username, device, ip string) {
	if r == nil || sessionID == "" {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[sessionID]
	if s == nil {
		s = &sessionState{SessionID: sessionID}
		r.sessions[sessionID] = s
	}
	s.UserID = firstNonEmpty(userID, s.UserID)
	s.Username = firstNonEmpty(username, s.Username)
	s.Device = firstNonEmpty(device, s.Device)
	s.IP = firstNonEmpty(ip, s.IP)
	s.LastSeen = now
}

// ActiveSessionCount returns sessions with last_seen within 5 minutes.
func (r *Registry) ActiveSessionCount() int {
	if r == nil {
		return 0
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	return len(r.sessions)
}

// ActivePlaybacks returns currently active playbacks (90s TTL).
func (r *Registry) ActivePlaybacks() []Playback {
	if r == nil {
		return nil
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	out := make([]Playback, 0, len(r.playbacks))
	for _, p := range r.playbacks {
		out = append(out, p.Playback)
	}
	return out
}

// ActiveTransfers returns open media transfers from the live byte meter
// (authoritative). Falls back to legacy event-based map if meter is nil.
func (r *Registry) ActiveTransfers() []Transfer {
	if r == nil {
		return nil
	}
	if r.meter != nil {
		return r.meter.ActiveTransfers()
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	out := make([]Transfer, 0, len(r.transfers))
	for _, t := range r.transfers {
		out = append(out, t.Transfer)
	}
	return out
}

// HasActiveMediaLoad reports whether any playbacks or live transfers are active.
// Transfer activity is meter-only (legacy event map is not consulted).
func (r *Registry) HasActiveMediaLoad() bool {
	if r == nil {
		return false
	}
	if r.meter != nil && r.meter.ActiveTransferCount() > 0 {
		return true
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	return len(r.playbacks) > 0
}

// ParseSeriesWindow maps a query value to a supported series window.
// Unknown or empty values default to 15m.
func ParseSeriesWindow(s string) SeriesWindow {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(Window1h):
		return Window1h
	case string(Window6h):
		return Window6h
	case string(Window24h):
		return Window24h
	default:
		return Window15m
	}
}

// Snapshot returns a race-safe metrics snapshot with the default 15m series window.
func (r *Registry) Snapshot() Snapshot {
	return r.SnapshotWindow(Window15m)
}

// SnapshotWindow returns a race-safe metrics snapshot with series for the given window.
func (r *Registry) SnapshotWindow(window SeriesWindow) Snapshot {
	if r == nil {
		return Snapshot{}
	}
	window = ParseSeriesWindow(string(window))
	now := r.now()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	goroutines := runtime.NumGoroutine()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)

	sec10 := r.sec.sum(now, rpsWindowSec)
	min5 := r.min.sum(now, rejectWindowMin)
	min15 := r.min.sum(now, errorWindowMin)
	minRel := r.min.sum(now, reliabilityWindowM)

	var drops uint64
	if r.emitter != nil {
		drops = r.emitter.Drops()
	}

	uptime := int64(now.Sub(r.started).Seconds())
	if uptime < 0 {
		uptime = 0
	}

	snap := Snapshot{
		TS:        now,
		BootID:    r.bootID,
		UptimeSec: uptime,
		Upstream:  r.upstreamSnapshotLocked(),
		Capacity: CapacityStatus{
			ActivePlaybacks:      len(r.playbacks),
			ActiveMediaTransfers: r.activeTransferCountLocked(),
			ActiveSessions:       len(r.sessions),
			Rejects5m:            min5.Rejects,
			RejectRate5m:         rate(min5.Rejects, min5.Requests),
		},
		// Traffic Mbps* is a rolling ~10s average of live-sampled downstream/upstream
		// body bytes (from ByteMeter), not end-of-transfer totals.
		Traffic: TrafficStatus{
			RPS:          float64(sec10.Requests) / float64(rpsWindowSec),
			MbpsIn:       bitsPerSec(sec10.BytesIn, trafficWindowSec),
			MbpsOut:      bitsPerSec(sec10.BytesOut, trafficWindowSec),
			ErrorRate15m: rate(min15.Errors, min15.Requests),
		},
		Reliability: ReliabilityStatus{
			UserdataWriteFail5m: minRel.UserdataFails,
			OverlayFail5m:       minRel.OverlayFails,
			TelemetryDrops:      drops,
		},
		Runtime: RuntimeStatus{
			Goroutines: goroutines,
			HeapBytes:  ms.HeapAlloc,
		},
		Series: r.buildSeriesLocked(now, window),
	}
	return snap
}

// buildSeriesLocked selects ring resolution and point count for the window.
// Caller must hold r.mu.
func (r *Registry) buildSeriesLocked(now time.Time, window SeriesWindow) SeriesData {
	switch window {
	case Window1h:
		return seriesFromRing(r.min, now, series1hMin, time.Minute, window)
	case Window6h:
		return seriesFromRing(r.min, now, series6hMin, time.Minute, window)
	case Window24h:
		return seriesFromRing(r.min, now, series24hMin, time.Minute, window)
	default:
		// 15m: prefer 1s buckets for smoothness (900 points).
		return seriesFromRing(r.sec, now, series15mSec, time.Second, Window15m)
	}
}

func seriesFromRing(ring *timeRing, now time.Time, n int, bucket time.Duration, window SeriesWindow) SeriesData {
	bucketSec := bucket.Seconds()
	if bucketSec <= 0 {
		bucketSec = 1
	}
	interval := "1s"
	if bucket >= time.Minute {
		interval = "1m"
	}
	return SeriesData{
		Window:   string(window),
		Interval: interval,
		RPS: ring.series(now, n, func(c counters) float64 {
			return float64(c.Requests) / bucketSec
		}),
		MbpsIn: ring.series(now, n, func(c counters) float64 {
			return float64(c.BytesIn) * 8 / 1_000_000 / bucketSec
		}),
		MbpsOut: ring.series(now, n, func(c counters) float64 {
			return float64(c.BytesOut) * 8 / 1_000_000 / bucketSec
		}),
		Errors: ring.series(now, n, func(c counters) float64 {
			return float64(c.Errors) // count per bucket
		}),
		Playbacks: ring.series(now, n, func(c counters) float64 {
			return float64(c.PlaybacksMax)
		}),
	}
}

func (r *Registry) upstreamSnapshotLocked() UpstreamStatus {
	authState := r.upstream.AuthState
	if authState == "" {
		authState = AuthStateUnknown
	}
	u := UpstreamStatus{
		LastStatusClass: r.upstream.LastStatusClass,
		LastErrorKind:   r.upstream.LastErrorKind,
		LastLatencyMS:   r.upstream.LastLatencyMS,
		AuthState:       authState,
		LastAuthError:   r.upstream.LastAuthError,
	}
	if r.upstream.HasLastOK {
		t := r.upstream.LastOKAt
		u.LastOKAt = &t
	}
	if r.upstream.HasLastError {
		t := r.upstream.LastErrorAt
		u.LastErrorAt = &t
	}
	if r.upstream.HasLastAuth {
		t := r.upstream.LastAuthAt
		u.LastAuthAt = &t
	}
	return u
}

func (r *Registry) handle(ev observe.Event) {
	at := ev.At
	if at.IsZero() {
		at = r.now()
	} else {
		at = at.UTC()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(at)

	switch ev.Kind {
	case observe.KindRequest, observe.KindUpstreamRequest:
		r.recordTrafficLocked(at, ev)
		if ev.Kind == observe.KindUpstreamRequest {
			r.recordUpstreamLocked(at, ev)
		}
		// Refresh session liveness on requests that carry a session id.
		if ev.SessionID != "" {
			s := r.sessions[ev.SessionID]
			if s == nil {
				s = &sessionState{SessionID: ev.SessionID}
				r.sessions[ev.SessionID] = s
			}
			s.UserID = firstNonEmpty(ev.UserID, s.UserID)
			s.Username = firstNonEmpty(ev.Username, s.Username)
			s.Device = firstNonEmpty(ev.Device, s.Device)
			s.LastSeen = at
		}
	case observe.KindUpstreamAuthRefresh:
		r.recordUpstreamAuthLocked(at, ev)
		r.recordTrafficLocked(at, ev)
	case observe.KindCapacityReject:
		r.recordRejectLocked(at, ev)
	case observe.KindPlayback:
		r.recordPlaybackLocked(at, ev)
	case observe.KindMediaTransfer:
		r.recordTransferLocked(at, ev)
	case observe.KindUserdataError:
		r.recordUserdataFailLocked(at, ev)
	case observe.KindReliability:
		r.recordReliabilityLocked(at, ev)
	case observe.KindAuthLogin, observe.KindAuthLogout:
		if ev.SessionID != "" {
			if ev.Kind == observe.KindAuthLogout {
				delete(r.sessions, ev.SessionID)
			} else {
				s := r.sessions[ev.SessionID]
				if s == nil {
					s = &sessionState{SessionID: ev.SessionID}
					r.sessions[ev.SessionID] = s
				}
				s.UserID = firstNonEmpty(ev.UserID, s.UserID)
				s.Username = firstNonEmpty(ev.Username, s.Username)
				s.Device = firstNonEmpty(ev.Device, s.Device)
				s.LastSeen = at
			}
		}
		// count auth as request traffic
		r.recordTrafficLocked(at, ev)
	}

	// Sample active playback gauge into the current second bucket.
	active := int64(len(r.playbacks))
	r.sec.add(at, func(c *counters) {
		if active > c.PlaybacksMax {
			c.PlaybacksMax = active
		}
	})
	r.min.add(at, func(c *counters) {
		if active > c.PlaybacksMax {
			c.PlaybacksMax = active
		}
	})
}

func (r *Registry) recordTrafficLocked(at time.Time, ev observe.Event) {
	isErr := isErrorOutcome(ev)
	fn := func(c *counters) {
		c.Requests++
		c.BytesIn += ev.BytesIn
		c.BytesOut += ev.BytesOut
		if isErr {
			c.Errors++
		}
	}
	r.sec.add(at, fn)
	r.min.add(at, fn)
}

func (r *Registry) recordRejectLocked(at time.Time, ev observe.Event) {
	fn := func(c *counters) {
		c.Rejects++
		c.Requests++ // capacity reject is still a request attempt
		if isErrorOutcome(ev) || ev.Outcome == observe.OutcomeDenied || ev.Outcome == "" {
			c.Errors++
		}
	}
	r.sec.add(at, fn)
	r.min.add(at, fn)
}

func (r *Registry) recordUserdataFailLocked(at time.Time, ev observe.Event) {
	fn := func(c *counters) {
		c.UserdataFails++
		c.Errors++
		c.Requests++
	}
	r.sec.add(at, fn)
	r.min.add(at, fn)
	_ = ev
}

func (r *Registry) recordReliabilityLocked(at time.Time, ev observe.Event) {
	fn := func(c *counters) {
		c.Requests++
		if isErrorOutcome(ev) || ev.Outcome == "" {
			c.Errors++
		}
		if isOverlayFail(ev) {
			c.OverlayFails++
		}
		if isUserdataFail(ev) {
			c.UserdataFails++
		}
	}
	r.sec.add(at, fn)
	r.min.add(at, fn)
}

func (r *Registry) recordUpstreamLocked(at time.Time, ev observe.Event) {
	r.upstream.LastStatusClass = ev.StatusClass
	r.upstream.LastLatencyMS = ev.DurationMS
	if isErrorOutcome(ev) || isErrorStatus(ev.StatusClass) {
		r.upstream.LastErrorAt = at
		r.upstream.HasLastError = true
		r.upstream.LastErrorKind = firstNonEmpty(ev.ErrorKind, ev.Outcome)
	} else {
		r.upstream.LastOKAt = at
		r.upstream.HasLastOK = true
		r.upstream.LastErrorKind = ""
	}
	// Managed backend requests after Ensure/header rewrite: 2xx means the
	// configured identity was accepted. Network errors, 5xx, and ordinary 4xx
	// do not change auth state.
	if ev.StatusClass == observe.Status2xx {
		r.markAuthHealthyLocked(at)
	}
}

func (r *Registry) recordUpstreamAuthLocked(at time.Time, ev observe.Event) {
	if isErrorOutcome(ev) || isErrorStatus(ev.StatusClass) {
		// Explicit confirmed auth/refresh failure only.
		r.upstream.AuthState = AuthStateFailing
		r.upstream.LastAuthError = stableAuthError(ev.ErrorKind)
		r.upstream.LastErrorAt = at
		r.upstream.HasLastError = true
		r.upstream.LastErrorKind = r.upstream.LastAuthError
	} else {
		r.markAuthHealthyLocked(at)
		r.upstream.LastOKAt = at
		r.upstream.HasLastOK = true
	}
	if ev.DurationMS > 0 {
		r.upstream.LastLatencyMS = ev.DurationMS
	}
	if ev.StatusClass != "" {
		r.upstream.LastStatusClass = ev.StatusClass
	}
}

// markAuthHealthyLocked records confirmed managed-backend auth acceptance.
// Caller must hold r.mu.
func (r *Registry) markAuthHealthyLocked(at time.Time) {
	r.upstream.AuthState = AuthStateHealthy
	r.upstream.LastAuthAt = at
	r.upstream.HasLastAuth = true
	r.upstream.LastAuthError = ""
}

// stableAuthError maps an observation ErrorKind to a bounded wire code.
func stableAuthError(kind string) string {
	switch kind {
	case AuthErrorAuthUnavailable:
		return AuthErrorAuthUnavailable
	default:
		return AuthErrorRefreshFailed
	}
}

func (r *Registry) recordPlaybackLocked(at time.Time, ev observe.Event) {
	key := playbackKey(ev)
	if key == "" {
		return
	}
	event := strings.ToLower(strings.TrimSpace(ev.PlaybackEvent))
	if event == observe.PlaybackStopped || ev.Outcome == observe.OutcomeDenied {
		delete(r.playbacks, key)
		return
	}
	p := r.playbacks[key]
	if p == nil {
		p = &playbackState{Playback: Playback{
			SessionID: ev.SessionID,
			UserID:    ev.UserID,
			Username:  ev.Username,
			Device:    ev.Device,
			ItemID:    ev.ItemID,
			ItemName:  ev.ItemName,
			StartedAt: at,
		}}
		r.playbacks[key] = p
	}
	p.SessionID = firstNonEmpty(ev.SessionID, p.SessionID)
	p.UserID = firstNonEmpty(ev.UserID, p.UserID)
	p.Username = firstNonEmpty(ev.Username, p.Username)
	p.Device = firstNonEmpty(ev.Device, p.Device)
	p.ItemID = firstNonEmpty(ev.ItemID, p.ItemID)
	p.ItemName = firstNonEmpty(ev.ItemName, p.ItemName)
	if ev.PositionTicks > 0 || event == observe.PlaybackProgress || event == observe.PlaybackPlaying {
		p.PositionTicks = ev.PositionTicks
	}
	p.IsPaused = ev.IsPaused
	p.LastSeen = at

	if ev.SessionID != "" {
		s := r.sessions[ev.SessionID]
		if s == nil {
			s = &sessionState{SessionID: ev.SessionID}
			r.sessions[ev.SessionID] = s
		}
		s.UserID = firstNonEmpty(ev.UserID, s.UserID)
		s.Username = firstNonEmpty(ev.Username, s.Username)
		s.Device = firstNonEmpty(ev.Device, s.Device)
		s.LastSeen = at
	}
}

// activeTransferCountLocked returns live meter transfer count (preferred) or
// legacy event-map size. Caller may hold r.mu; meter uses its own lock.
func (r *Registry) activeTransferCountLocked() int {
	if r.meter != nil {
		return r.meter.ActiveTransferCount()
	}
	return len(r.transfers)
}

func (r *Registry) recordTransferLocked(at time.Time, ev observe.Event) {
	key := transferKey(ev)
	if key == "" {
		return
	}
	// Completion closes legacy event-based transfer tracking only.
	// Bytes for Mbps come exclusively from ByteMeter live sampling — never
	// add PhaseEnd BytesIn/BytesOut to traffic rings (would double-count).
	if isTransferComplete(ev) {
		if isErrorOutcome(ev) {
			fn := func(c *counters) { c.Errors++ }
			r.sec.add(at, fn)
			r.min.add(at, fn)
		}
		delete(r.transfers, key)
		return
	}

	// PhaseStart / open: track transfer only; do NOT increment Requests.
	tr := r.transfers[key]
	if tr == nil {
		tr = &transferState{Transfer: Transfer{
			SessionID: ev.SessionID,
			UserID:    ev.UserID,
			Username:  ev.Username,
			Device:    ev.Device,
			ItemID:    ev.ItemID,
			MediaMode: firstNonEmpty(ev.MediaMode, observe.MediaUnknown),
			StartedAt: at,
		}}
		r.transfers[key] = tr
	}
	tr.SessionID = firstNonEmpty(ev.SessionID, tr.SessionID)
	tr.UserID = firstNonEmpty(ev.UserID, tr.UserID)
	tr.Username = firstNonEmpty(ev.Username, tr.Username)
	tr.Device = firstNonEmpty(ev.Device, tr.Device)
	tr.ItemID = firstNonEmpty(ev.ItemID, tr.ItemID)
	if ev.MediaMode != "" {
		tr.MediaMode = ev.MediaMode
	}
	if ev.BytesIn > tr.BytesIn {
		tr.BytesIn = ev.BytesIn
	}
	if ev.BytesOut > tr.BytesOut {
		tr.BytesOut = ev.BytesOut
	}
	tr.LastSeen = at
}

func (r *Registry) expireLocked(now time.Time) {
	sessionCutoff := now.Add(-sessionTTL)
	for id, s := range r.sessions {
		if s.LastSeen.Before(sessionCutoff) {
			delete(r.sessions, id)
		}
	}
	playbackCutoff := now.Add(-playbackTTL)
	for id, p := range r.playbacks {
		if p.LastSeen.Before(playbackCutoff) {
			delete(r.playbacks, id)
		}
	}
	// Transfers have no TTL; they close on completion events only.
	// Defensive: drop transfers idle for a very long time (1h) to avoid leaks.
	transferCutoff := now.Add(-time.Hour)
	for id, tr := range r.transfers {
		if tr.LastSeen.Before(transferCutoff) {
			delete(r.transfers, id)
		}
	}
}

func playbackKey(ev observe.Event) string {
	switch {
	case ev.SessionID != "" && ev.ItemID != "":
		return ev.SessionID + "\x00" + ev.ItemID
	case ev.SessionID != "":
		return "s:" + ev.SessionID
	case ev.UserID != "" && ev.ItemID != "":
		return ev.UserID + "\x00" + ev.ItemID
	case ev.ItemID != "":
		return "i:" + ev.ItemID
	default:
		return ""
	}
}

func transferKey(ev observe.Event) string {
	mode := ev.MediaMode
	if mode == "" {
		mode = observe.MediaUnknown
	}
	switch {
	case ev.SessionID != "" && ev.ItemID != "":
		return ev.SessionID + "\x00" + ev.ItemID + "\x00" + mode
	case ev.SessionID != "":
		// Stable without ItemID: session + mode + method path class.
		method := ev.Method
		if method == "" {
			method = "_"
		}
		return "s:" + ev.SessionID + "\x00" + mode + "\x00" + method
	case ev.ItemID != "":
		return "i:" + ev.ItemID + "\x00" + mode
	default:
		return ""
	}
}

func isTransferComplete(ev observe.Event) bool {
	// Prefer explicit phase over outcome heuristics.
	switch ev.Phase {
	case observe.PhaseStart:
		return false
	case observe.PhaseEnd:
		return true
	}
	// Legacy: empty phase with a terminal outcome means complete.
	switch ev.Outcome {
	case observe.OutcomeOK, observe.OutcomeError, observe.OutcomeTimeout, observe.OutcomeDenied:
		return true
	default:
		return false
	}
}

func isErrorOutcome(ev observe.Event) bool {
	switch ev.Outcome {
	case observe.OutcomeError, observe.OutcomeTimeout:
		return true
	case observe.OutcomeDenied:
		return true
	default:
		return isErrorStatus(ev.StatusClass)
	}
}

func isErrorStatus(class string) bool {
	switch class {
	case observe.Status5xx, observe.Status0:
		return true
	default:
		return false
	}
}

func isOverlayFail(ev observe.Event) bool {
	k := strings.ToLower(ev.ErrorKind)
	return strings.Contains(k, "overlay")
}

func isUserdataFail(ev observe.Event) bool {
	k := strings.ToLower(ev.ErrorKind)
	return strings.Contains(k, "userdata") || strings.Contains(k, "user_data")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func rate(num, den int64) float64 {
	if den <= 0 {
		if num <= 0 {
			return 0
		}
		return 1
	}
	return float64(num) / float64(den)
}

func bitsPerSec(bytes int64, windowSec int) float64 {
	if windowSec <= 0 {
		return 0
	}
	return float64(bytes) * 8 / float64(windowSec) / 1_000_000
}
