package telemetry

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestMediaBufferWaitsJSONContract(t *testing.T) {
	v := MediaBufferWaits{
		BufferAcquire:      MediaBufferWaitStat{TotalMS: 1, MaxMS: 2},
		PoolContention:     MediaBufferWaitStat{TotalMS: 3, MaxMS: 4},
		ConsumerStarvation: MediaBufferWaitStat{TotalMS: 5, MaxMS: 6},
		UpstreamStall:      MediaBufferWaitStat{TotalMS: 7, MaxMS: 8},
		DownstreamStall:    MediaBufferWaitStat{TotalMS: 9, MaxMS: 10},
		CloseJoinStall:     MediaBufferWaitStat{TotalMS: 11, MaxMS: 12},
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"buffer_acquire":{"total":1,"max":2},"pool_contention":{"total":3,"max":4},"consumer_starvation":{"total":5,"max":6},"upstream_stall":{"total":7,"max":8},"downstream_stall":{"total":9,"max":10},"close_join_stall":{"total":11,"max":12}}`
	if string(raw) != want {
		t.Fatalf("waits JSON=%s want=%s", raw, want)
	}
}

func TestMediaBufferFiniteEnumsAndExactHealthThresholds(t *testing.T) {
	for _, health := range []MediaBufferHealth{MediaBufferHealthDisabled, MediaBufferHealthIdle, MediaBufferHealthHealthy, MediaBufferHealthWarning, MediaBufferHealthCritical} {
		if !health.Valid() {
			t.Fatalf("health %q invalid", health)
		}
	}
	for _, observation := range []MediaBufferObservation{ObservationComplete, ObservationLimited, ObservationUnavailable} {
		if !observation.Valid() {
			t.Fatalf("observation %q invalid", observation)
		}
	}
	for _, condition := range []MediaBufferWaitCondition{WaitNone, WaitBufferAcquire, WaitPoolContention, WaitConsumerStarvation, WaitUpstreamStall, WaitDownstreamStall, WaitCloseJoinStall} {
		if !condition.Valid() {
			t.Fatalf("wait condition %q invalid", condition)
		}
	}
	for _, outcome := range []MediaBufferOutcome{OutcomeSuccess, OutcomeCanceled, OutcomeUpstreamError, OutcomeDownstreamError, OutcomeShortWrite, OutcomeLengthMismatch, OutcomeInvalidRead, OutcomeInvalidWrite, OutcomeNoProgress, OutcomeInvariantError} {
		if !outcome.Valid() {
			t.Fatalf("outcome %q invalid", outcome)
		}
	}
	for _, mode := range []MediaBufferMediaMode{MediaBufferModeDirect, MediaBufferModeHLS, MediaBufferModeRange, MediaBufferModeUnknown} {
		if !mode.Valid() {
			t.Fatalf("media mode %q invalid", mode)
		}
	}
	for _, state := range []MediaBufferLifecycleName{LifecycleStarting, LifecycleActive, LifecycleClosing} {
		if !state.Valid() {
			t.Fatalf("lifecycle %q invalid", state)
		}
	}
	for _, state := range []MediaBufferProducerName{ProducerIdle, ProducerReadingBase, ProducerReadingOptional, ProducerWaitingForBuffer, ProducerDone} {
		if !state.Valid() {
			t.Fatalf("producer %q invalid", state)
		}
	}
	for _, state := range []MediaBufferConsumerName{ConsumerIdle, ConsumerWaitingForData, ConsumerWriting, ConsumerDone} {
		if !state.Valid() {
			t.Fatalf("consumer %q invalid", state)
		}
	}
	for _, blocker := range []MediaBufferBlockerName{BlockerNone, BlockerPoolExhausted, BlockerAtTarget, BlockerDebt} {
		if !blocker.Valid() {
			t.Fatalf("blocker %q invalid", blocker)
		}
	}
	if MediaBufferHealth("bad").Valid() || MediaBufferObservation("bad").Valid() || MediaBufferOutcome("bad").Valid() || MediaBufferWaitCondition("bad").Valid() || MediaBufferMediaMode("bad").Valid() || MediaBufferLifecycleName("bad").Valid() || MediaBufferProducerName("bad").Valid() || MediaBufferConsumerName("bad").Valid() || MediaBufferBlockerName("bad").Valid() {
		t.Fatal("unknown enum accepted")
	}
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		snapshot  MediaBufferLiveSnapshot
		threshold time.Duration
		want      MediaBufferHealth
	}{
		{"pool", MediaBufferLiveSnapshot{Producer: MediaBufferTimedValue{Value: uint8(MediaBufferProducerWaitingForBuffer)}, Blocker: MediaBufferTimedValue{Value: uint8(MediaBufferBlockerPoolExhausted)}}, 2 * time.Second, MediaBufferHealthWarning},
		{"consumer", MediaBufferLiveSnapshot{Consumer: MediaBufferTimedValue{Value: uint8(MediaBufferConsumerWaitingForData)}}, 2 * time.Second, MediaBufferHealthWarning},
		{"upstream", MediaBufferLiveSnapshot{Producer: MediaBufferTimedValue{Value: uint8(MediaBufferProducerReadingBase)}}, 10 * time.Second, MediaBufferHealthWarning},
		{"downstream", MediaBufferLiveSnapshot{Consumer: MediaBufferTimedValue{Value: uint8(MediaBufferConsumerWriting)}}, 10 * time.Second, MediaBufferHealthWarning},
		{"close", MediaBufferLiveSnapshot{Lifecycle: MediaBufferTimedValue{Value: uint8(MediaBufferLifecycleClosing)}}, 10 * time.Second, MediaBufferHealthCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.snapshot.StartedAt = base
			tc.snapshot.AgeMS = tc.threshold.Milliseconds() - 1
			if got := streamHealth(tc.snapshot, base.Add(tc.threshold)); got != MediaBufferHealthHealthy {
				t.Fatalf("before=%q", got)
			}
			tc.snapshot.AgeMS++
			if got := streamHealth(tc.snapshot, base.Add(tc.threshold)); got != tc.want {
				t.Fatalf("at threshold=%q want %q", got, tc.want)
			}
		})
	}
	info := MediaBufferLiveSnapshot{StartedAt: base, AgeMS: 2000, Producer: MediaBufferTimedValue{Value: uint8(MediaBufferProducerWaitingForBuffer)}, Blocker: MediaBufferTimedValue{Value: uint8(MediaBufferBlockerAtTarget)}}
	if got := streamHealth(info, base.Add(2*time.Second)); got != MediaBufferHealthHealthy {
		t.Fatalf("informational acquire health=%q", got)
	}
}

func TestSelectedConditionAlwaysUsesFinitePriority(t *testing.T) {
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	if got := selectedCondition(MediaBufferLiveSnapshot{StartedAt: base}, base); got != WaitNone {
		t.Fatalf("idle condition=%q want=%q", got, WaitNone)
	}
	zero := MediaBufferLiveSnapshot{
		StartedAt: base,
		AgeMS:     100,
		Consumer:  MediaBufferTimedValue{Value: uint8(MediaBufferConsumerWaitingForData), TransitionMS: 100},
	}
	if got := selectedCondition(zero, base.Add(100*time.Millisecond)); got != WaitConsumerStarvation {
		t.Fatalf("zero-duration active condition=%q", got)
	}
	equal := MediaBufferLiveSnapshot{
		StartedAt: base,
		AgeMS:     100,
		Lifecycle: MediaBufferTimedValue{Value: uint8(MediaBufferLifecycleClosing), TransitionMS: 50},
		Producer:  MediaBufferTimedValue{Value: uint8(MediaBufferProducerReadingBase), TransitionMS: 50},
		Consumer:  MediaBufferTimedValue{Value: uint8(MediaBufferConsumerWriting), TransitionMS: 50},
	}
	if got := selectedCondition(equal, base.Add(100*time.Millisecond)); got != WaitCloseJoinStall {
		t.Fatalf("equal-duration condition=%q want priority=%q", got, WaitCloseJoinStall)
	}
}

func TestMediaBufferGaugeAndCompletionStoresFixedBacking(t *testing.T) {
	r := New(nil)
	if len(r.mediaSec.slots) != secBuckets || cap(r.mediaSec.slots) != secBuckets || len(r.mediaMin.slots) != minBuckets || cap(r.mediaMin.slots) != minBuckets {
		t.Fatalf("gauge lengths sec=%d/%d min=%d/%d", len(r.mediaSec.slots), cap(r.mediaSec.slots), len(r.mediaMin.slots), cap(r.mediaMin.slots))
	}
	if len(r.mediaRecent.slots) != MediaBufferCompletionCapacity || cap(r.mediaRecent.slots) != MediaBufferCompletionCapacity {
		t.Fatalf("completion length=%d/%d", len(r.mediaRecent.slots), cap(r.mediaRecent.slots))
	}
	secBacking := unsafe.SliceData(r.mediaSec.slots)
	minBacking := unsafe.SliceData(r.mediaMin.slots)
	recentBacking := unsafe.SliceData(r.mediaRecent.slots)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	for i := 0; i < secBuckets+100; i++ {
		a := MediaBufferAggregate{Health: MediaBufferHealthHealthy, HealthReasons: []string{"pool_contention"}}
		r.mediaSec.put(base.Add(time.Duration(i)*time.Second), a)
		r.mediaMin.put(base.Add(time.Duration(i)*time.Minute), a)
	}
	for i := 0; i < MediaBufferCompletionCapacity+100; i++ {
		r.mediaRecent.add(base, MediaBufferCompletionDTO{StreamID: idString(uint64(i + 1)), CompletedAt: base})
	}
	_ = r.mediaSec.series(base.Add(secBuckets*time.Second), secBuckets)
	_ = r.mediaMin.series(base.Add(minBuckets*time.Minute), minBuckets)
	if unsafe.SliceData(r.mediaSec.slots) != secBacking || len(r.mediaSec.slots) != secBuckets || cap(r.mediaSec.slots) != secBuckets {
		t.Fatal("second gauge backing changed")
	}
	if unsafe.SliceData(r.mediaMin.slots) != minBacking || len(r.mediaMin.slots) != minBuckets || cap(r.mediaMin.slots) != minBuckets {
		t.Fatal("minute gauge backing changed")
	}
	if unsafe.SliceData(r.mediaRecent.slots) != recentBacking || len(r.mediaRecent.slots) != MediaBufferCompletionCapacity || cap(r.mediaRecent.slots) != MediaBufferCompletionCapacity || r.mediaRecent.count != MediaBufferCompletionCapacity {
		t.Fatal("completion backing or capacity changed")
	}
}

func TestMediaBufferAggregateNAnchorCompletenessAndComposition(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	for id, queued := range []int64{10, 20, 40} {
		if !r.mediaBufferLive.Register(&testMediaBufferLiveState{id: uint64(id + 1), snapshot: MediaBufferLiveSnapshot{StartedAt: base, QueuedBytes: queued, WritingBytes: queued / 2}}) {
			t.Fatal("register")
		}
	}
	r.SetMediaBufferProvider(func() MediaBufferControllerSnapshot {
		return MediaBufferControllerSnapshot{Available: true, Enabled: true, HardBudgetBytes: 131072, AllocatedBytes: 65536, OwnedBytes: 32768, FreeBytes: 32768, UnallocatedOptionalBytes: 65536, ActiveRequests: 2, BaseOnlyRequests: 1, IndebtedRequests: 1, RequestDebtBytes: 32768}
	})
	a := r.MediaBufferAggregateSnapshot()
	if a.ObservedActiveRequests != 2 || a.UnobservedActiveRequests != 0 || a.QueuedBytes != 30 || a.WritingBytes != 15 || a.PrivateBaseBytes != 65536 || a.ObservationCompleteness != ObservationComplete {
		t.Fatalf("aggregate=%+v", a)
	}
	if a.Health != MediaBufferHealthHealthy {
		t.Fatalf("health=%q", a.Health)
	}
	r.SetMediaBufferProvider(func() MediaBufferControllerSnapshot {
		return MediaBufferControllerSnapshot{Available: true, Enabled: true, ActiveRequests: 4}
	})
	a = r.MediaBufferAggregateSnapshot()
	if a.ObservedActiveRequests != 3 || a.UnobservedActiveRequests != 1 || a.ObservationCompleteness != ObservationLimited {
		t.Fatalf("limited=%+v", a)
	}
	r.SetMediaBufferProvider(nil)
	if got := r.MediaBufferAggregateSnapshot().ObservationCompleteness; got != ObservationUnavailable {
		t.Fatalf("unavailable=%q", got)
	}
}

func TestMediaBufferGaugeZeroDeltaGapsAndWindowNormalization(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	now := base.Add(2 * time.Second)
	r.now = func() time.Time { return now }
	calls := 0
	r.SetMediaBufferProvider(func() MediaBufferControllerSnapshot {
		calls++
		return MediaBufferControllerSnapshot{Available: true, Enabled: true}
	})
	if !r.SampleMediaBufferOnce(base) || !r.SampleMediaBufferOnce(base.Add(2*time.Second)) || calls != 2 {
		t.Fatalf("samples/calls=%d", calls)
	}
	series := r.MediaBufferSeries(Window15m)
	last := series.Points[len(series.Points)-3:]
	if !last[0].Present || last[1].Present || !last[2].Present || last[1].Aggregate != nil || last[1].Domains != nil {
		t.Fatalf("gap points=%+v", last)
	}
	for _, tc := range []struct {
		in               SeriesWindow
		window, interval string
		n                int
	}{{"bogus", "15m", "1s", 900}, {Window1h, "1h", "1m", 60}, {Window6h, "6h", "1m", 360}, {Window24h, "24h", "1m", 1440}} {
		got := r.MediaBufferSeries(tc.in)
		if got.Window != tc.window || got.Interval != tc.interval || len(got.Points) != tc.n {
			t.Fatalf("%q: %s/%s/%d", tc.in, got.Window, got.Interval, len(got.Points))
		}
	}
}

func TestMediaBufferCompletionOrderingRetentionAgeAndDrop(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	now := base
	r.now = func() time.Time { return now }
	for i := 0; i < MediaBufferCompletionCapacity+7; i++ {
		c := MediaBufferCompletion{Terminal: MediaBufferLiveSnapshot{StreamID: uint64(i + 1), StartedAt: base}, Outcome: string(OutcomeSuccess), CompletedAt: base.Add(time.Duration(i) * time.Millisecond)}
		if !r.mediaBufferLive.OfferCompletion(c) {
			t.Fatalf("offer %d", i)
		}
		r.drainMediaBufferCompletions(base.Add(time.Duration(i) * time.Millisecond))
	}
	if r.mediaRecent.count != MediaBufferCompletionCapacity || r.mediaRecent.next != MediaBufferCompletionCapacity+7 {
		t.Fatalf("count/seq=%d/%d", r.mediaRecent.count, r.mediaRecent.next)
	}
	recent := r.MediaBufferRecent(2)
	if recent.Items[0].StreamID != idString(MediaBufferCompletionCapacity+7) || recent.Items[1].StreamID != idString(MediaBufferCompletionCapacity+6) {
		t.Fatalf("ordering=%+v", recent.Items)
	}
	now = base.Add(mediaBufferRecentAge + 10*time.Second)
	_ = r.MediaBufferRecent(200)
	if r.mediaRecent.count != 0 {
		t.Fatalf("age retained=%d", r.mediaRecent.count)
	}
	for i := 0; i < mediaBufferCompletionCapacity; i++ {
		if !r.mediaBufferLive.OfferCompletion(MediaBufferCompletion{}) {
			t.Fatal("fill")
		}
	}
	if r.mediaBufferLive.OfferCompletion(MediaBufferCompletion{}) || r.mediaBufferLive.CompletionDrops() != 1 {
		t.Fatalf("drop=%d", r.mediaBufferLive.CompletionDrops())
	}
}

func TestMediaBufferSamplerCompactsTerminalAndSanitizesDTO(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base.Add(3 * time.Second) }
	state := &testMediaBufferLiveState{id: 1, snapshot: MediaBufferLiveSnapshot{StartedAt: base, AgeMS: 3000, TransferID: 42, UserID: "  user\x00\x01 ", Username: strings.Repeat("x", 300), MediaMode: "arbitrary", TargetBytes: -1, Producer: MediaBufferTimedValue{Value: uint8(MediaBufferProducerWaitingForBuffer)}}}
	r.mediaBufferLive.Register(state)
	detail, ok := r.MediaBufferStreamDetail(1)
	if !ok || detail.StreamID != "1" || detail.TransferID == nil || *detail.TransferID != "42" || detail.MediaMode != "unknown" || detail.TargetBytes != 0 || detail.UserID == nil || strings.ContainsRune(*detail.UserID, '\x00') || len(*detail.Username) > 256 {
		t.Fatalf("detail=%+v ok=%v", detail, ok)
	}
	state.snapshot.Terminal = true
	r.SetMediaBufferProvider(func() MediaBufferControllerSnapshot {
		return MediaBufferControllerSnapshot{Available: true, Enabled: true}
	})
	r.SampleMediaBufferOnce(base.Add(3 * time.Second))
	if len(r.mediaBufferLive.slots) != 0 {
		t.Fatalf("terminal slots=%d", len(r.mediaBufferLive.slots))
	}
}

func TestMediaBufferSamplerLifecycleStops(t *testing.T) {
	r := New(nil)
	ticks := make(chan time.Time)
	stopped := make(chan struct{})
	r.SetMediaBufferTicker(func() (<-chan time.Time, func()) { return ticks, func() { close(stopped) } })
	r.SetMediaBufferProvider(func() MediaBufferControllerSnapshot {
		return MediaBufferControllerSnapshot{Available: true, Enabled: true}
	})
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("sampler did not stop")
	}
}

func TestMediaBufferConcurrentSamplingAndReads(t *testing.T) {
	r := New(nil)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return base }
	r.SetMediaBufferProvider(func() MediaBufferControllerSnapshot {
		return MediaBufferControllerSnapshot{Available: true, Enabled: true, ActiveRequests: 1}
	})
	r.mediaBufferLive.Register(&testMediaBufferLiveState{id: 1, snapshot: MediaBufferLiveSnapshot{StartedAt: base}})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				at := base.Add(time.Duration(i) * time.Second)
				if id%2 == 0 {
					r.SampleMediaBufferOnce(at)
				} else {
					_ = r.MediaBufferSeries(Window15m)
					_ = r.MediaBufferAggregateSnapshot()
					_, _ = r.MediaBufferStreamDetail(1)
					_ = r.MediaBufferRecent(10)
				}
			}
		}(worker)
	}
	wg.Wait()
}
