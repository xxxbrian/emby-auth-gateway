package telemetry

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func TestMediaBufferMinutePeaksPreserveShortFaultAndCoherentSnapshot(t *testing.T) {
	base := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ring := newMediaBufferGaugeRing(time.Minute, 3)
	fault := MediaBufferAggregate{Health: MediaBufferHealthWarning, HardBudgetBytes: 100, AllocatedBytes: 80, OwnedBytes: 50, FreeBytes: 30, UnallocatedOptionalBytes: 20, ActiveRequests: 8, QueuedBytes: 64, PoolContentionCount: 1, UpstreamStallCount: 2, WarningStreams: 2}
	healthy := MediaBufferAggregate{Health: MediaBufferHealthHealthy, HardBudgetBytes: 100, AllocatedBytes: 40, OwnedBytes: 10, FreeBytes: 30, UnallocatedOptionalBytes: 60, ActiveRequests: 2, QueuedBytes: 8}
	ring.put(base.Add(5*time.Second), fault)
	ring.put(base.Add(55*time.Second), healthy)
	ring.put(base.Add(2*time.Minute), healthy)
	points := ring.series(base.Add(2*time.Minute), 3)
	first := points[0]
	if first.Aggregate.OwnedBytes != 10 || first.Aggregate.AllocatedBytes != 40 || first.Aggregate.Health != MediaBufferHealthHealthy {
		t.Fatalf("last coherent snapshot changed: %+v", first.Aggregate)
	}
	if first.Peaks == nil || first.Peaks.Health != MediaBufferHealthWarning || first.Peaks.PoolContentionCount != 1 || first.Peaks.UpstreamStallCount != 2 {
		t.Fatalf("lost short fault: %+v", first.Peaks)
	}
	if points[1].Present || points[1].Peaks != nil || points[1].Aggregate != nil {
		t.Fatalf("gap synthesized: %+v", points[1])
	}
	if points[2].Peaks.Health != MediaBufferHealthHealthy || points[2].Peaks.PoolContentionCount != 0 {
		t.Fatalf("carried peaks into next bucket: %+v", points[2].Peaks)
	}
	ring.put(base.Add(3*time.Minute), healthy)
	if p := ring.series(base.Add(3*time.Minute), 1)[0]; p.Peaks.Health != MediaBufferHealthHealthy {
		t.Fatalf("wrapped peaks retained: %+v", p)
	}
}

func TestMediaBufferCompletionHistoryStablePagingFilteringAndExpiry(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ring := newMediaBufferCompletionRing()
	for i := 0; i < 7; i++ {
		outcome := OutcomeSuccess
		if i%2 == 0 {
			outcome = OutcomeUpstreamError
		}
		ring.add(now, MediaBufferCompletionDTO{StreamID: idString(uint64(i + 1)), CompletedAt: now.Add(-time.Hour), Outcome: outcome})
	}
	query := MediaBufferRecentQuery{From: now.Add(-2 * time.Hour), To: now, ErrorsOnly: true, Limit: 2}
	page, err := ring.page(now, now.Add(-3*time.Hour), query)
	if err != nil || len(page.Items) != 2 || page.Items[0].StreamID != "7" || page.Items[1].StreamID != "5" || !page.HasMore || page.NextCursor != "5" {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	ring.add(now, MediaBufferCompletionDTO{StreamID: "8", CompletedAt: now, Outcome: OutcomeUpstreamError})
	query.Before = 5
	page, err = ring.page(now, now.Add(-3*time.Hour), query)
	if err != nil || len(page.Items) != 2 || page.Items[0].StreamID != "3" || page.Items[1].StreamID != "1" || page.HasMore {
		t.Fatalf("stable second page=%+v err=%v", page, err)
	}
	if item, err := ring.detail(now, 3); err != nil || item.StreamID != "3" || item.CompletionID != "3" {
		t.Fatalf("detail=%+v err=%v", item, err)
	}
	if _, err := ring.detail(now, 99); !errors.Is(err, ErrMediaBufferCompletionNotFound) {
		t.Fatalf("unknown id err=%v", err)
	}
	page, err = ring.page(now.Add(24*time.Hour), now, MediaBufferRecentQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].StreamID != "8" || page.EvictedCount != 7 {
		t.Fatalf("age boundary=%+v %v", page, err)
	}
	if _, err := ring.page(now.Add(24*time.Hour), now, query); !errors.Is(err, ErrMediaBufferCursorExpired) {
		t.Fatalf("expired cursor err=%v", err)
	}
	if _, err := ring.detail(now.Add(24*time.Hour), 3); !errors.Is(err, ErrMediaBufferCompletionExpired) {
		t.Fatalf("expired detail err=%v", err)
	}
}

func TestMediaBufferCompletionHistoryCapacityAndOutOfOrderTime(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	ring := newMediaBufferCompletionRing()
	backing := &ring.slots[0]
	for i := 0; i < MediaBufferCompletionCapacity+3; i++ {
		ring.add(now, MediaBufferCompletionDTO{CompletedAt: now.Add(-time.Hour), Outcome: OutcomeSuccess})
	}
	page, err := ring.page(now, now.Add(-48*time.Hour), MediaBufferRecentQuery{Limit: 200})
	if err != nil || page.Capacity != 2048 || page.RetainedCount != 2048 || page.EvictedCount != 3 || page.RetentionSeconds != 86400 || !page.AvailableFrom.Equal(now.Add(-time.Hour)) {
		t.Fatalf("coverage=%+v err=%v", page, err)
	}
	if _, err := ring.page(now, now, MediaBufferRecentQuery{Before: 2}); !errors.Is(err, ErrMediaBufferCursorExpired) {
		t.Fatalf("capacity cursor err=%v", err)
	}
	ring.add(now, MediaBufferCompletionDTO{CompletedAt: now.Add(-23 * time.Hour), Outcome: OutcomeSuccess})
	ring.expire(now.Add(2 * time.Hour))
	if ring.count != 2047 || &ring.slots[0] != backing || cap(ring.slots) != 2048 {
		t.Fatalf("out of order expiry count=%d storage changed=%v", ring.count, &ring.slots[0] != backing)
	}
	if _, err := ring.detail(now.Add(2*time.Hour), ring.next); !errors.Is(err, ErrMediaBufferCompletionExpired) {
		t.Fatalf("late completion was not expired: %v", err)
	}
}

func TestMediaBufferFullDayHistoryEncodedBudget(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	r := New(nil)
	r.now = func() time.Time { return now }
	a := MediaBufferAggregate{Enabled: true, Health: MediaBufferHealthCritical, HealthReasons: []string{"close_join_stall", "consumer_starvation", "downstream_stall", "pool_contention", "upstream_stall"}, HardBudgetBytes: math.MaxInt64, AllocatedBytes: math.MaxInt64, OwnedBytes: math.MaxInt64, FreeBytes: math.MaxInt64, UnallocatedOptionalBytes: math.MaxInt64, PrivateBaseBytes: math.MaxInt64, QueuedBytes: math.MaxInt64, WritingBytes: math.MaxInt64, ActiveRequests: math.MaxInt, BaseOnlyRequests: math.MaxInt, IndebtedRequests: math.MaxInt, RequestDebtBytes: math.MaxInt64, BufferAcquireCount: 4096, PoolContentionCount: 4096, ConsumerStarvationCount: 4096, UpstreamStallCount: 4096, DownstreamStallCount: 4096, CloseJoinStallCount: 4096, WarningStreams: 4096, CriticalStreams: 4096, CompletionDrops: math.MaxUint64, ObservedActiveRequests: 4096, UnobservedActiveRequests: math.MaxInt, LiveRegistrationDrops: math.MaxUint64, ObservationCompleteness: ObservationUnavailable}
	for i := 0; i < 1440; i++ {
		r.mediaMin.put(now.Add(-time.Duration(i)*time.Minute), a)
	}
	data, err := json.Marshal(r.MediaBufferSeries(Window24h))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 2<<20 {
		t.Fatalf("full day history is %d bytes, over 2 MiB", len(data))
	}
	t.Logf("maximal full-day history: %d bytes", len(data))
}
