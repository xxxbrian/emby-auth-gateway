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
	sessionTTL   = 5 * time.Minute
	playbackTTL  = 90 * time.Second
	secBuckets   = 3600
	minBuckets   = 1440
	seriesPoints = 60

	// Rate windows.
	rpsWindowSec       = 10
	trafficWindowSec   = 10
	rejectWindowMin    = 5
	errorWindowMin     = 15
	reliabilityWindowM = 5
)

// Registry consumes observe events and maintains in-memory metrics.
type Registry struct {
	emitter *observe.Emitter
	bootID  string
	started time.Time
	now     func() time.Time

	mu sync.RWMutex

	sessions  map[string]*sessionState
	playbacks map[string]*playbackState
	transfers map[string]*transferState
	upstream  upstreamState

	sec *timeRing
	min *timeRing

	startOnce sync.Once
}

// New creates a Registry bound to emitter. A nil emitter is allowed (no-op Start).
func New(emitter *observe.Emitter) *Registry {
	return &Registry{
		emitter:   emitter,
		bootID:    newBootID(),
		started:   time.Now().UTC(),
		now:       func() time.Time { return time.Now().UTC() },
		sessions:  make(map[string]*sessionState),
		playbacks: make(map[string]*playbackState),
		transfers: make(map[string]*transferState),
		sec:       newTimeRing(time.Second, secBuckets),
		min:       newTimeRing(time.Minute, minBuckets),
	}
}

func newBootID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}

// Start runs the single consumer loop until ctx is done or the emitter closes.
// Safe to call once; subsequent calls are no-ops.
func (r *Registry) Start(ctx context.Context) {
	if r == nil {
		return
	}
	r.startOnce.Do(func() {
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

// ActiveTransfers returns open media transfers.
func (r *Registry) ActiveTransfers() []Transfer {
	if r == nil {
		return nil
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

// HasActiveMediaLoad reports whether any playbacks or transfers are active.
func (r *Registry) HasActiveMediaLoad() bool {
	if r == nil {
		return false
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
	return len(r.playbacks) > 0 || len(r.transfers) > 0
}

// Snapshot returns a race-safe metrics snapshot.
func (r *Registry) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
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
			ActiveMediaTransfers: len(r.transfers),
			ActiveSessions:       len(r.sessions),
			Rejects5m:            min5.Rejects,
			RejectRate5m:         rate(min5.Rejects, min5.Requests),
		},
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
		Series: SeriesData{
			RPS: r.sec.series(now, seriesPoints, func(c counters) float64 {
				return float64(c.Requests)
			}),
			MbpsOut: r.sec.series(now, seriesPoints, func(c counters) float64 {
				// bytes in this 1s bucket → Mbps
				return float64(c.BytesOut) * 8 / 1_000_000
			}),
			Errors: r.sec.series(now, seriesPoints, func(c counters) float64 {
				return float64(c.Errors)
			}),
			Playbacks: r.sec.series(now, seriesPoints, func(c counters) float64 {
				return float64(c.PlaybacksMax)
			}),
		},
	}
	return snap
}

func (r *Registry) upstreamSnapshotLocked() UpstreamStatus {
	u := UpstreamStatus{
		LastStatusClass: r.upstream.LastStatusClass,
		LastErrorKind:   r.upstream.LastErrorKind,
		LastLatencyMS:   r.upstream.LastLatencyMS,
		AuthOK:          r.upstream.AuthOK,
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
}

func (r *Registry) recordUpstreamAuthLocked(at time.Time, ev observe.Event) {
	r.upstream.LastAuthAt = at
	r.upstream.HasLastAuth = true
	if isErrorOutcome(ev) || isErrorStatus(ev.StatusClass) {
		r.upstream.AuthOK = false
		r.upstream.LastAuthError = firstNonEmpty(ev.ErrorKind, ev.Outcome, "auth_failed")
		r.upstream.LastErrorAt = at
		r.upstream.HasLastError = true
		r.upstream.LastErrorKind = r.upstream.LastAuthError
	} else {
		r.upstream.AuthOK = true
		r.upstream.LastAuthError = ""
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

func (r *Registry) recordTransferLocked(at time.Time, ev observe.Event) {
	key := transferKey(ev)
	if key == "" {
		return
	}
	// Completion (PhaseEnd or legacy terminal outcome) closes the transfer and
	// counts bytes/errors only. KindRequest/KindUpstreamRequest already count
	// the HTTP request for RPS — never increment Requests on media transfer events.
	if isTransferComplete(ev) {
		fn := func(c *counters) {
			c.BytesIn += ev.BytesIn
			c.BytesOut += ev.BytesOut
			if isErrorOutcome(ev) {
				c.Errors++
			}
		}
		r.sec.add(at, fn)
		r.min.add(at, fn)
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
