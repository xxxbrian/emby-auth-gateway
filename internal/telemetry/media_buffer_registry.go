package telemetry

import (
	"context"
	"time"
)

const mediaBufferRecentAge = 15 * time.Minute

// MediaBufferAggregate is the complete bounded aggregate contract.
type MediaBufferAggregate struct {
	Enabled                  bool                   `json:"enabled"`
	Health                   MediaBufferHealth      `json:"health"`
	HealthReasons            []string               `json:"health_reasons"`
	HardBudgetBytes          int64                  `json:"hard_budget_bytes"`
	AllocatedBytes           int64                  `json:"allocated_bytes"`
	OwnedBytes               int64                  `json:"owned_bytes"`
	FreeBytes                int64                  `json:"free_bytes"`
	UnallocatedOptionalBytes int64                  `json:"unallocated_optional_bytes"`
	PrivateBaseBytes         int64                  `json:"private_base_bytes"`
	QueuedBytes              int64                  `json:"queued_bytes"`
	WritingBytes             int64                  `json:"writing_bytes"`
	ActiveRequests           int                    `json:"active_requests"`
	BaseOnlyRequests         int                    `json:"base_only_requests"`
	IndebtedRequests         int                    `json:"indebted_requests"`
	RequestDebtBytes         int64                  `json:"request_debt_bytes"`
	BufferAcquireCount       int                    `json:"buffer_acquire_count"`
	PoolContentionCount      int                    `json:"pool_contention_count"`
	ConsumerStarvationCount  int                    `json:"consumer_starvation_count"`
	UpstreamStallCount       int                    `json:"upstream_stall_count"`
	DownstreamStallCount     int                    `json:"downstream_stall_count"`
	CloseJoinStallCount      int                    `json:"close_join_stall_count"`
	WarningStreams           int                    `json:"warning_streams"`
	CriticalStreams          int                    `json:"critical_streams"`
	CompletionDrops          uint64                 `json:"completion_drops"`
	ObservedActiveRequests   int                    `json:"observed_active_requests"`
	UnobservedActiveRequests int                    `json:"unobserved_active_requests"`
	LiveRegistrationDrops    uint64                 `json:"live_registration_drops"`
	ObservationCompleteness  MediaBufferObservation `json:"observation_completeness"`
}

type MediaBufferDomains struct {
	Pool    string `json:"pool"`
	Sidecar string `json:"sidecar"`
}
type MediaBufferSeriesPoint struct {
	T         time.Time             `json:"t"`
	Present   bool                  `json:"present"`
	Domains   *MediaBufferDomains   `json:"domains"`
	Aggregate *MediaBufferAggregate `json:"aggregate"`
}
type MediaBufferSeries struct {
	BootID   string                   `json:"boot_id"`
	Window   string                   `json:"window"`
	Interval string                   `json:"interval"`
	Points   []MediaBufferSeriesPoint `json:"points"`
}

type mediaBufferGaugeSlot struct {
	unit      int64
	present   bool
	aggregate MediaBufferAggregate
}
type mediaBufferGaugeRing struct {
	interval time.Duration
	slots    []mediaBufferGaugeSlot
}

func newMediaBufferGaugeRing(interval time.Duration, size int) *mediaBufferGaugeRing {
	return &mediaBufferGaugeRing{interval: interval, slots: make([]mediaBufferGaugeSlot, size)}
}
func (r *mediaBufferGaugeRing) unit(t time.Time) int64 {
	n := t.UnixNano()
	if n < 0 {
		n = 0
	}
	return n / int64(r.interval)
}
func (r *mediaBufferGaugeRing) put(t time.Time, a MediaBufferAggregate) {
	u := r.unit(t)
	i := int(u % int64(len(r.slots)))
	r.slots[i] = mediaBufferGaugeSlot{unit: u, present: true, aggregate: cloneAggregate(a)}
}
func cloneAggregate(a MediaBufferAggregate) MediaBufferAggregate {
	if len(a.HealthReasons) > 0 {
		a.HealthReasons = append([]string(nil), a.HealthReasons...)
	} else {
		a.HealthReasons = []string{}
	}
	return a
}
func (r *mediaBufferGaugeRing) series(now time.Time, n int) []MediaBufferSeriesPoint {
	if n > len(r.slots) {
		n = len(r.slots)
	}
	out := make([]MediaBufferSeriesPoint, 0, n)
	head := r.unit(now)
	for age := n - 1; age >= 0; age-- {
		u := head - int64(age)
		t := time.Unix(0, u*int64(r.interval)).UTC()
		slot := r.slots[int(u%int64(len(r.slots)))]
		p := MediaBufferSeriesPoint{T: t}
		if slot.present && slot.unit == u {
			a := cloneAggregate(slot.aggregate)
			p.Present = true
			p.Domains = &MediaBufferDomains{Pool: "coherent", Sidecar: "eventual"}
			p.Aggregate = &a
		}
		out = append(out, p)
	}
	return out
}

type retainedMediaBufferCompletion struct {
	sequence    uint64
	completedAt time.Time
	value       MediaBufferCompletionDTO
}
type mediaBufferCompletionRing struct {
	slots        []retainedMediaBufferCompletion
	start, count int
	next         uint64
}

func newMediaBufferCompletionRing() *mediaBufferCompletionRing {
	return &mediaBufferCompletionRing{slots: make([]retainedMediaBufferCompletion, MediaBufferCompletionCapacity)}
}
func (r *mediaBufferCompletionRing) expire(now time.Time) {
	cutoff := now.Add(-mediaBufferRecentAge)
	for r.count > 0 {
		x := r.slots[r.start]
		if !x.completedAt.Before(cutoff) {
			break
		}
		r.slots[r.start] = retainedMediaBufferCompletion{}
		r.start = (r.start + 1) % len(r.slots)
		r.count--
	}
}
func (r *mediaBufferCompletionRing) add(now time.Time, v MediaBufferCompletionDTO) {
	r.expire(now)
	r.next++
	if r.next == 0 {
		r.next++
	}
	x := retainedMediaBufferCompletion{sequence: r.next, completedAt: v.CompletedAt, value: v}
	if r.count == len(r.slots) {
		r.slots[r.start] = x
		r.start = (r.start + 1) % len(r.slots)
		return
	}
	i := (r.start + r.count) % len(r.slots)
	r.slots[i] = x
	r.count++
}
func (r *mediaBufferCompletionRing) recent(now time.Time, limit int) []MediaBufferCompletionDTO {
	r.expire(now)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if limit > r.count {
		limit = r.count
	}
	out := make([]MediaBufferCompletionDTO, 0, limit)
	for i := 0; i < limit; i++ {
		idx := (r.start + r.count - 1 - i) % len(r.slots)
		out = append(out, r.slots[idx].value)
	}
	return out
}

// SetMediaBufferProvider installs the controller's O(1) snapshot callback.
// It must be configured before Start; the callback is never invoked under a
// Registry, live-registry, ByteMeter, or completion lock.
func (r *Registry) SetMediaBufferProvider(provider MediaBufferControllerProvider) {
	if r == nil {
		return
	}
	r.mediaMu.Lock()
	r.mediaProvider = provider
	r.mediaMu.Unlock()
}

// SetMediaBufferTicker injects the one-second cadence for deterministic tests.
func (r *Registry) SetMediaBufferTicker(factory func() (<-chan time.Time, func())) {
	if r == nil || factory == nil {
		return
	}
	r.mediaMu.Lock()
	r.mediaTicker = factory
	r.mediaMu.Unlock()
}

func (r *Registry) sampleMediaBuffer(ctx context.Context) {
	r.mediaMu.RLock()
	factory := r.mediaTicker
	r.mediaMu.RUnlock()
	if factory == nil {
		return
	}
	ticks, stop := factory()
	if stop != nil {
		defer stop()
	}
	r.SampleMediaBufferOnce(r.sampleTime(time.Now()))
	for {
		select {
		case <-ctx.Done():
			return
		case at, ok := <-ticks:
			if !ok {
				return
			}
			r.SampleMediaBufferOnce(r.sampleTime(at))
		}
	}
}

// SampleMediaBufferOnce commits at most one complete cycle to the actual bucket,
// drains bounded completions, and performs bounded terminal maintenance.
func (r *Registry) SampleMediaBufferOnce(at time.Time) bool {
	if r == nil {
		return false
	}
	at = at.UTC()
	r.mediaMu.RLock()
	provider := r.mediaProvider
	r.mediaMu.RUnlock()
	if provider == nil {
		r.drainMediaBufferCompletions(at)
		r.mediaBufferLive.CompactTerminal()
		return false
	}
	controller := provider()
	if !controller.Available {
		r.drainMediaBufferCompletions(at)
		r.mediaBufferLive.CompactTerminal()
		return false
	}
	aggregate := r.mediaBufferLive.aggregateCycle(controller, at)
	r.drainMediaBufferCompletions(at)
	r.mediaMu.Lock()
	r.mediaSec.put(at, aggregate)
	r.mediaMin.put(at, aggregate)
	r.mediaLatest = cloneAggregate(aggregate)
	r.mediaLatestPresent = true
	r.mediaMu.Unlock()
	r.mediaBufferLive.CompactTerminal()
	return true
}

func (r *Registry) drainMediaBufferCompletions(now time.Time) {
	for {
		c, ok := r.mediaBufferLive.TryCompletion()
		if !ok {
			break
		}
		dto := completionDTO(r.bootID, c, now)
		r.mediaMu.Lock()
		r.mediaRecent.add(now, dto)
		r.mediaMu.Unlock()
	}
}

func (r *MediaBufferLiveRegistry) aggregateCycle(c MediaBufferControllerSnapshot, now time.Time) MediaBufferAggregate {
	a := MediaBufferAggregate{Enabled: c.Enabled, Health: MediaBufferHealthDisabled, HardBudgetBytes: nonNegative(c.HardBudgetBytes), AllocatedBytes: nonNegative(c.AllocatedBytes), OwnedBytes: nonNegative(c.OwnedBytes), FreeBytes: nonNegative(c.FreeBytes), UnallocatedOptionalBytes: nonNegative(c.UnallocatedOptionalBytes), PrivateBaseBytes: nonNegative(c.PrivateBaseBytes), ActiveRequests: c.ActiveRequests, BaseOnlyRequests: c.BaseOnlyRequests, IndebtedRequests: c.IndebtedRequests, RequestDebtBytes: nonNegative(c.RequestDebtBytes), CompletionDrops: r.CompletionDrops(), LiveRegistrationDrops: r.RegistrationDrops(), ObservationCompleteness: ObservationUnavailable}
	if c.HardBudgetBytes >= c.AllocatedBytes && c.AllocatedBytes >= 0 {
		a.UnallocatedOptionalBytes = c.HardBudgetBytes - c.AllocatedBytes
	}
	if !c.Enabled {
		a.HealthReasons = []string{}
		return a
	}
	if a.ActiveRequests < 0 {
		a.ActiveRequests = 0
	}
	a.PrivateBaseBytes = int64(a.ActiveRequests) * 32768
	a.Health = MediaBufferHealthIdle
	a.ObservationCompleteness = ObservationComplete
	reasons := [5]bool{}
	r.mu.RLock()
	for _, state := range r.slots {
		if a.ObservedActiveRequests >= c.ActiveRequests {
			break
		}
		s := state.MediaBufferLiveSnapshot()
		if s.Terminal {
			continue
		}
		a.ObservedActiveRequests++
		a.QueuedBytes += nonNegative(s.QueuedBytes)
		a.WritingBytes += nonNegative(s.WritingBytes)
		h := streamHealth(s, now)
		if h == MediaBufferHealthCritical {
			a.CriticalStreams++
			a.Health = MediaBufferHealthCritical
		} else if h == MediaBufferHealthWarning {
			a.WarningStreams++
			if a.Health != MediaBufferHealthCritical {
				a.Health = MediaBufferHealthWarning
			}
		} else if a.Health == MediaBufferHealthIdle {
			a.Health = MediaBufferHealthHealthy
		}
		if conditionReached(s, now, WaitBufferAcquire) {
			a.BufferAcquireCount++
		}
		if conditionReached(s, now, WaitPoolContention) {
			a.PoolContentionCount++
			reasons[3] = true
		}
		if conditionReached(s, now, WaitConsumerStarvation) {
			a.ConsumerStarvationCount++
			reasons[1] = true
		}
		if conditionReached(s, now, WaitUpstreamStall) {
			a.UpstreamStallCount++
			reasons[4] = true
		}
		if conditionReached(s, now, WaitDownstreamStall) {
			a.DownstreamStallCount++
			reasons[2] = true
		}
		if conditionReached(s, now, WaitCloseJoinStall) {
			a.CloseJoinStallCount++
			reasons[0] = true
		}
	}
	r.mu.RUnlock()
	a.UnobservedActiveRequests = c.ActiveRequests - a.ObservedActiveRequests
	if a.UnobservedActiveRequests > 0 {
		a.ObservationCompleteness = ObservationLimited
	}
	if c.ActiveRequests == 0 {
		a.Health = MediaBufferHealthIdle
	}
	names := []string{"close_join_stall", "consumer_starvation", "downstream_stall", "pool_contention", "upstream_stall"}
	a.HealthReasons = make([]string, 0, 5)
	for i, on := range reasons {
		if on {
			a.HealthReasons = append(a.HealthReasons, names[i])
		}
	}
	return a
}

func conditionReached(s MediaBufferLiveSnapshot, now time.Time, c MediaBufferWaitCondition) bool {
	if c == WaitBufferAcquire {
		return s.Producer.Value == 3 && conditionDuration(s, now, c) >= 2*time.Second
	}
	return streamHealthFor(s, now, c)
}

// MediaBufferAggregateSnapshot executes a fresh bounded provider/live scan.
func (r *Registry) MediaBufferAggregateSnapshot() MediaBufferAggregate {
	if r == nil {
		return MediaBufferAggregate{Health: MediaBufferHealthDisabled, ObservationCompleteness: ObservationUnavailable, HealthReasons: []string{}}
	}
	r.mediaMu.RLock()
	p := r.mediaProvider
	r.mediaMu.RUnlock()
	if p == nil {
		return MediaBufferAggregate{Health: MediaBufferHealthDisabled, ObservationCompleteness: ObservationUnavailable, HealthReasons: []string{}}
	}
	c := p()
	if !c.Available {
		return MediaBufferAggregate{Health: MediaBufferHealthDisabled, ObservationCompleteness: ObservationUnavailable, HealthReasons: []string{}}
	}
	return r.mediaBufferLive.aggregateCycle(c, r.now())
}

func (r *Registry) MediaBufferSeries(window SeriesWindow) MediaBufferSeries {
	if r == nil {
		return MediaBufferSeries{Window: string(Window15m), Interval: "1s"}
	}
	window = ParseSeriesWindow(string(window))
	r.mediaMu.Lock()
	defer r.mediaMu.Unlock()
	out := MediaBufferSeries{BootID: r.bootID, Window: string(window), Interval: "1m"}
	now := r.now()
	switch window {
	case Window1h:
		out.Points = r.mediaMin.series(now, series1hMin)
	case Window6h:
		out.Points = r.mediaMin.series(now, series6hMin)
	case Window24h:
		out.Points = r.mediaMin.series(now, series24hMin)
	default:
		out.Interval = "1s"
		out.Points = r.mediaSec.series(now, series15mSec)
	}
	return out
}

func (r *Registry) MediaBufferLivePage(cursor uint64, limit int) MediaBufferLivePageDTO {
	if limit <= 0 {
		limit = 50
	}
	raw := r.mediaBufferLive.Page(cursor, limit)
	now := r.now()
	items := make([]MediaBufferStream, 0, len(raw.Items))
	for _, state := range raw.Items {
		s := state.MediaBufferLiveSnapshot()
		if s.Terminal {
			continue
		}
		var bytesRead, bytesWritten int64
		if byteState, ok := state.(MediaBufferLiveByteState); ok {
			bytesRead, bytesWritten = byteState.MediaBufferLiveBytes()
		}
		items = append(items, mediaBufferStreamDTO(r.bootID, s, bytesRead, bytesWritten, now, r.started))
	}
	completeness := ObservationUnavailable
	r.mediaMu.RLock()
	if r.mediaLatestPresent {
		completeness = r.mediaLatest.ObservationCompleteness
	}
	r.mediaMu.RUnlock()
	return MediaBufferLivePageDTO{BootID: r.bootID, Items: items, NextCursor: idString(raw.NextCursor), HasMore: raw.HasMore, ObservationCompleteness: completeness}
}
func (r *Registry) MediaBufferStreamDetail(streamID uint64) (MediaBufferStream, bool) {
	state, ok := r.mediaBufferLive.Detail(streamID)
	if !ok {
		return MediaBufferStream{}, false
	}
	s := state.MediaBufferLiveSnapshot()
	if s.Terminal {
		return MediaBufferStream{}, false
	}
	var bytesRead, bytesWritten int64
	if byteState, ok := state.(MediaBufferLiveByteState); ok {
		bytesRead, bytesWritten = byteState.MediaBufferLiveBytes()
	}
	return mediaBufferStreamDTO(r.bootID, s, bytesRead, bytesWritten, r.now(), r.started), true
}
func (r *Registry) MediaBufferRecent(limit int) MediaBufferRecentPage {
	if r == nil {
		return MediaBufferRecentPage{}
	}
	now := r.now()
	r.drainMediaBufferCompletions(now)
	r.mediaMu.Lock()
	items := r.mediaRecent.recent(now, limit)
	r.mediaMu.Unlock()
	return MediaBufferRecentPage{BootID: r.bootID, Items: items}
}

func completionDTO(boot string, c MediaBufferCompletion, now time.Time) MediaBufferCompletionDTO {
	s := c.Terminal
	completed := c.CompletedAt
	if completed.IsZero() {
		completed = now
	}
	outcome := MediaBufferOutcome(c.Outcome)
	if !validMediaBufferOutcome(outcome) {
		outcome = OutcomeSuccess
	}
	waits := MediaBufferWaits{BufferAcquire: s.Waits[0], PoolContention: s.Waits[1], ConsumerStarvation: s.Waits[2], UpstreamStall: s.Waits[3], DownstreamStall: s.Waits[4], CloseJoinStall: s.Waits[5]}
	duration := nonNegative(completed.Sub(s.StartedAt).Milliseconds())
	var transferID *string
	if s.TransferID != 0 {
		v := idString(s.TransferID)
		transferID = &v
	}
	return MediaBufferCompletionDTO{BootID: boot, StreamID: idString(s.StreamID), TransferID: transferID, UserID: sanitizeMediaBufferString(s.UserID), Username: sanitizeMediaBufferString(s.Username), Device: sanitizeMediaBufferString(s.Device), ItemID: sanitizeMediaBufferString(s.ItemID), MediaMode: finiteMediaMode(s.MediaMode), FinalState: LifecycleClosing, FinalProducerState: ProducerDone, FinalConsumerState: ConsumerDone, FinalAllocationBlocker: BlockerNone, Outcome: outcome, StartedAt: s.StartedAt.UTC(), CompletedAt: completed.UTC(), DurationMS: duration, BytesRead: nonNegative(c.BytesRead), BytesWritten: nonNegative(c.BytesWritten), PeakOwnedBytes: nonNegative(s.PeakOwnedBytes), PeakDebtBytes: nonNegative(s.PeakDebtBytes), PeakQueuedBytes: nonNegative(s.PeakQueuedBytes), PeakWritingBytes: nonNegative(s.PeakWritingBytes), WaitsMS: sanitizeWaits(waits), InvariantObserved: c.InvariantObserved}
}
func sanitizeWaits(w MediaBufferWaits) MediaBufferWaits {
	values := []*MediaBufferWaitStat{&w.BufferAcquire, &w.PoolContention, &w.ConsumerStarvation, &w.UpstreamStall, &w.DownstreamStall, &w.CloseJoinStall}
	for _, v := range values {
		v.TotalMS = nonNegative(v.TotalMS)
		v.MaxMS = nonNegative(v.MaxMS)
	}
	return w
}
