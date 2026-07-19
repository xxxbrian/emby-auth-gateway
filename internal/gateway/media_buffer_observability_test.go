package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

func TestMediaBufferLivePackedWordsIdentityAndFakeClock(t *testing.T) {
	origin := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	clock := newFakeMediaBufferClock(origin, 10)
	live := newMediaBufferLiveState(mediaBufferLiveIdentity{
		BootID:    " boot\x00 ",
		StreamID:  7,
		UserID:    " user\n id ",
		Username:  strings.Repeat("x", 300),
		Device:    " device\tname ",
		ItemID:    " item ",
		MediaMode: " direct ",
	}, clock)
	live.projectAllocation(3*mediaBufferChunkSize, 2*mediaBufferChunkSize, mediaBufferChunkSize)
	live.setProducer(telemetry.MediaBufferProducerWaitingForBuffer)
	live.projectBlocker(telemetry.MediaBufferBlockerPoolExhausted)
	clock.advance(3 * time.Second)
	live.projectBlocker(telemetry.MediaBufferBlockerNone)
	live.setProducer(telemetry.MediaBufferProducerReadingOptional)
	live.setConsumer(telemetry.MediaBufferConsumerWaitingForData)
	clock.advance(2 * time.Second)
	live.setConsumer(telemetry.MediaBufferConsumerWriting)
	live.bindTransferID(11)
	live.bindTransferID(12)

	snapshot := live.MediaBufferLiveSnapshot()
	if snapshot.BootID != "boot" || snapshot.StreamID != 7 || snapshot.TransferID != 11 || snapshot.UserID != "user id" || snapshot.Device != "device name" || snapshot.ItemID != "item" {
		t.Fatalf("identity snapshot=%+v", snapshot)
	}
	if len(snapshot.Username) != 256 {
		t.Fatalf("username bytes=%d", len(snapshot.Username))
	}
	if snapshot.TargetBytes != 3*mediaBufferChunkSize || snapshot.OwnedBytes != 2*mediaBufferChunkSize || snapshot.DebtBytes != mediaBufferChunkSize {
		t.Fatalf("allocation snapshot=%+v", snapshot)
	}
	longWait := telemetry.MediaBufferWaitStat{TotalMS: 3000, MaxMS: 3000}
	consumerWait := telemetry.MediaBufferWaitStat{TotalMS: 2000, MaxMS: 2000}
	if snapshot.Blocker.Value != uint8(telemetry.MediaBufferBlockerNone) || snapshot.Waits[mediaBufferWaitPool] != longWait || snapshot.Waits[mediaBufferWaitAcquire] != longWait || snapshot.Waits[mediaBufferWaitConsumer] != consumerWait {
		t.Fatalf("timed snapshot=%+v", snapshot)
	}
	if snapshot.Producer.Value != uint8(telemetry.MediaBufferProducerReadingOptional) || snapshot.Consumer.Value != uint8(telemetry.MediaBufferConsumerWriting) {
		t.Fatalf("direct transitions=%+v", snapshot)
	}
}

func TestMediaBufferLiveOperationBoundariesRestartSameEnum(t *testing.T) {
	clock := newFakeMediaBufferClock(time.Unix(0, 0), 0)
	live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, clock)
	producerStarted := live.beginProducerOperation(telemetry.MediaBufferProducerReadingBase)
	clock.advance(time.Second)
	live.endProducerOperation(telemetry.MediaBufferProducerReadingBase, producerStarted)
	producerStarted = live.beginProducerOperation(telemetry.MediaBufferProducerReadingBase)
	clock.advance(2 * time.Second)
	live.endProducerOperation(telemetry.MediaBufferProducerReadingBase, producerStarted)
	consumerStarted := live.beginConsumerOperation(telemetry.MediaBufferConsumerWriting)
	clock.advance(3 * time.Second)
	live.endConsumerOperation(telemetry.MediaBufferConsumerWriting, consumerStarted)
	consumerStarted = live.beginConsumerOperation(telemetry.MediaBufferConsumerWriting)
	clock.advance(4 * time.Second)
	live.endConsumerOperation(telemetry.MediaBufferConsumerWriting, consumerStarted)
	snapshot := live.MediaBufferLiveSnapshot()
	if snapshot.Producer.Value != uint8(telemetry.MediaBufferProducerIdle) || snapshot.Producer.TransitionMS != 3000 || snapshot.Waits[mediaBufferWaitUpstream] != (telemetry.MediaBufferWaitStat{TotalMS: 3000, MaxMS: 2000}) {
		t.Fatalf("producer operations=%+v", snapshot)
	}
	if snapshot.Consumer.Value != uint8(telemetry.MediaBufferConsumerIdle) || snapshot.Consumer.TransitionMS != 10000 || snapshot.Waits[mediaBufferWaitDownstream] != (telemetry.MediaBufferWaitStat{TotalMS: 7000, MaxMS: 4000}) {
		t.Fatalf("consumer operations=%+v", snapshot)
	}
}

func TestMediaBufferPoolContentionMeasuresPredicateIntersection(t *testing.T) {
	t.Run("blocker first", func(t *testing.T) {
		clock := newFakeMediaBufferClock(time.Unix(0, 0), 0)
		live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, clock)
		live.projectBlocker(telemetry.MediaBufferBlockerPoolExhausted)
		clock.advance(2 * time.Second)
		started := live.beginProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer)
		clock.advance(3 * time.Second)
		live.endProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer, started)
		wait := live.MediaBufferLiveSnapshot().Waits[mediaBufferWaitPool]
		if wait != (telemetry.MediaBufferWaitStat{TotalMS: 3000, MaxMS: 3000}) {
			t.Fatalf("pool wait=%+v", wait)
		}
	})
	t.Run("producer first", func(t *testing.T) {
		clock := newFakeMediaBufferClock(time.Unix(0, 0), 0)
		live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, clock)
		started := live.beginProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer)
		clock.advance(2 * time.Second)
		live.projectBlocker(telemetry.MediaBufferBlockerPoolExhausted)
		clock.advance(4 * time.Second)
		live.projectBlocker(telemetry.MediaBufferBlockerNone)
		clock.advance(time.Second)
		live.endProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer, started)
		snapshot := live.MediaBufferLiveSnapshot()
		if snapshot.Waits[mediaBufferWaitPool] != (telemetry.MediaBufferWaitStat{TotalMS: 4000, MaxMS: 4000}) || snapshot.Waits[mediaBufferWaitAcquire] != (telemetry.MediaBufferWaitStat{TotalMS: 7000, MaxMS: 7000}) {
			t.Fatalf("intersection snapshot=%+v", snapshot)
		}
	})
	t.Run("terminal finishes once", func(t *testing.T) {
		clock := newFakeMediaBufferClock(time.Unix(0, 0), 0)
		live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, clock)
		live.beginProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer)
		live.projectBlocker(telemetry.MediaBufferBlockerPoolExhausted)
		clock.advance(5 * time.Second)
		live.markTerminal()
		live.markTerminal()
		if wait := live.MediaBufferLiveSnapshot().Waits[mediaBufferWaitPool]; wait != (telemetry.MediaBufferWaitStat{TotalMS: 5000, MaxMS: 5000}) {
			t.Fatalf("terminal pool wait=%+v", wait)
		}
	})
	t.Run("concurrent predicate clears finish once", func(t *testing.T) {
		clock := newFakeMediaBufferClock(time.Unix(0, 0), 0)
		live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, clock)
		started := live.beginProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer)
		live.projectBlocker(telemetry.MediaBufferBlockerPoolExhausted)
		clock.advance(5 * time.Second)
		start := make(chan struct{})
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			live.endProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer, started)
		}()
		go func() {
			defer group.Done()
			<-start
			live.projectBlocker(telemetry.MediaBufferBlockerNone)
		}()
		close(start)
		group.Wait()
		if wait := live.MediaBufferLiveSnapshot().Waits[mediaBufferWaitPool]; wait != (telemetry.MediaBufferWaitStat{TotalMS: 5000, MaxMS: 5000}) {
			t.Fatalf("concurrent clear wait=%+v", wait)
		}
	})
}

func TestMediaBufferLiveProjectionAllocatesNothingAndStopsAtTerminal(t *testing.T) {
	live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, newFakeMediaBufferClock(time.Unix(0, 0), 1))
	live.projectAllocation(mediaBufferChunkSize, mediaBufferChunkSize, 0)
	unchangedClockReads := live.clock.reads.Load()
	live.setLifecycle(telemetry.MediaBufferLifecycleStarting)
	live.projectAllocation(mediaBufferChunkSize, mediaBufferChunkSize, 0)
	live.projectBlocker(telemetry.MediaBufferBlockerNone)
	live.setProducer(telemetry.MediaBufferProducerIdle)
	live.setConsumer(telemetry.MediaBufferConsumerIdle)
	if got := live.clock.reads.Load(); got != unchangedClockReads {
		t.Fatalf("unchanged projections read clock: before=%d after=%d", unchangedClockReads, got)
	}
	live.peakOwned.Store(0)
	live.projectAllocation(mediaBufferChunkSize, mediaBufferChunkSize, 0)
	if live.peakOwned.Load() != 0 {
		t.Fatal("unchanged allocation repeated peak work")
	}
	allocs := testing.AllocsPerRun(1000, func() {
		live.projectAllocation(mediaBufferChunkSize, mediaBufferChunkSize, 0)
		live.projectBlocker(telemetry.MediaBufferBlockerNone)
		started := live.beginProducerOperation(telemetry.MediaBufferProducerReadingBase)
		live.endProducerOperation(telemetry.MediaBufferProducerReadingBase, started)
		started = live.beginProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer)
		live.endProducerOperation(telemetry.MediaBufferProducerWaitingForBuffer, started)
		consumerStarted := live.beginConsumerOperation(telemetry.MediaBufferConsumerWaitingForData)
		live.endConsumerOperation(telemetry.MediaBufferConsumerWaitingForData, consumerStarted)
		consumerStarted = live.beginConsumerOperation(telemetry.MediaBufferConsumerWriting)
		live.endConsumerOperation(telemetry.MediaBufferConsumerWriting, consumerStarted)
		live.setQueued(1)
		live.setQueued(0)
		live.setWriting(1)
		live.setWriting(0)
		live.fallbackRead.Add(1)
		live.fallbackSent.Add(1)
	})
	if allocs != 0 {
		t.Fatalf("projection allocations=%f", allocs)
	}
	before := live.MediaBufferLiveSnapshot()
	bytesRead, bytesWritten := live.MediaBufferLiveBytes()
	if bytesRead <= 0 || bytesWritten != bytesRead {
		t.Fatalf("projected bytes=%d/%d", bytesRead, bytesWritten)
	}
	live.markTerminal()
	live.projectAllocation(0, 0, 0)
	live.setQueued(100)
	live.setWriting(100)
	live.setProducer(telemetry.MediaBufferProducerDone)
	after := live.MediaBufferLiveSnapshot()
	if !after.Terminal || after.TargetBytes != before.TargetBytes || after.QueuedBytes != before.QueuedBytes || after.WritingBytes != before.WritingBytes || after.Producer != before.Producer {
		t.Fatalf("post-terminal publication before=%+v after=%+v", before, after)
	}
}

func TestMediaBufferCompletionTimeUsesInjectedClock(t *testing.T) {
	origin := time.Date(2026, 7, 19, 12, 0, 0, 123000000, time.UTC)
	clock := newFakeMediaBufferClock(origin, 250)
	live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, clock)
	clock.advance(1750 * time.Millisecond)
	if got, want := live.completedAt(), origin.Add(2*time.Second); !got.Equal(want) {
		t.Fatalf("completed at=%s want=%s", got, want)
	}
}

func TestMediaBufferTerminalCaptureUsesOneClockRead(t *testing.T) {
	origin := time.Date(2026, 7, 19, 12, 0, 0, 123000000, time.UTC)
	clock := newFakeMediaBufferClock(origin, 2000)
	buffer := mustMediaBuffer(t, mediaBufferChunkSize)
	request := buffer.register()
	live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: request.id}, clock)
	request.attachLive(live)
	before := clock.reads.Load()
	snapshot, completedAt, err := request.closeAndCaptureTerminal(live)
	if err != nil {
		t.Fatal(err)
	}
	if got := clock.reads.Load() - before; got != 1 {
		t.Fatalf("terminal clock reads=%d want=1", got)
	}
	if snapshot.AgeMS != 2000 || !completedAt.Equal(origin.Add(2*time.Second)) {
		t.Fatalf("snapshot age=%d completed=%s", snapshot.AgeMS, completedAt)
	}
}

func TestMediaBufferCachedAggregateMatchesExhaustiveAccounting(t *testing.T) {
	buffer := mustMediaBuffer(t, 4*mediaBufferChunkSize)
	first := buffer.register()
	second := buffer.register()
	assertCachedMediaBufferAccounting(t, buffer)
	leases := []mediaBufferLease{
		requireAcceptedMediaBufferLease(t, first),
		requireAcceptedMediaBufferLease(t, first),
	}
	assertCachedMediaBufferAccounting(t, buffer)
	third := buffer.register()
	assertCachedMediaBufferAccounting(t, buffer)
	if err := first.releaseOptional(leases[0]); err != nil {
		t.Fatal(err)
	}
	assertCachedMediaBufferAccounting(t, buffer)
	closeMediaBufferRequests(t, second, third, first)
	assertCachedMediaBufferAccounting(t, buffer)
}

func TestMediaBufferRequestProjectionTransitionsRemainExact(t *testing.T) {
	assertProjection := func(t *testing.T, request *mediaBufferRequest, blocker telemetry.MediaBufferAllocationBlocker) {
		t.Helper()
		snapshot := request.snapshot()
		live := request.live.MediaBufferLiveSnapshot()
		if live.TargetBytes != snapshot.Target || live.OwnedBytes != snapshot.Owned || live.DebtBytes != snapshot.Debt || live.Blocker.Value != uint8(blocker) {
			t.Fatalf("request=%+v live=%+v blocker=%d", snapshot, live, blocker)
		}
	}
	attach := func(request *mediaBufferRequest) {
		request.attachLive(newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "test", StreamID: request.id}, newFakeMediaBufferClock(time.Unix(0, 0), 1)))
	}

	t.Run("grant accept release", func(t *testing.T) {
		buffer := mustMediaBuffer(t, mediaBufferChunkSize)
		request := buffer.register()
		attach(request)
		if _, err := request.requestOptional(); err != nil {
			t.Fatal(err)
		}
		assertProjection(t, request, telemetry.MediaBufferBlockerNone)
		lease, err := request.acceptOptional()
		if err != nil {
			t.Fatal(err)
		}
		assertProjection(t, request, telemetry.MediaBufferBlockerNone)
		if err := request.releaseOptional(lease); err != nil {
			t.Fatal(err)
		}
		assertProjection(t, request, telemetry.MediaBufferBlockerNone)
		closeMediaBufferCopyRequests(t, request)
	})

	t.Run("pool exhausted", func(t *testing.T) {
		buffer := mustMediaBuffer(t, 4*mediaBufferChunkSize)
		holder := buffer.register()
		leases := make([]mediaBufferLease, 0, 4)
		for range 4 {
			if _, err := holder.requestOptional(); err != nil {
				t.Fatal(err)
			}
			lease, err := holder.acceptOptional()
			if err != nil {
				t.Fatal(err)
			}
			leases = append(leases, lease)
		}
		waiter := buffer.register()
		attach(waiter)
		if _, err := waiter.requestOptional(); err != nil {
			t.Fatal(err)
		}
		assertProjection(t, waiter, telemetry.MediaBufferBlockerPoolExhausted)
		if err := holder.releaseOptional(leases[0]); err != nil {
			t.Fatal(err)
		}
		assertProjection(t, waiter, telemetry.MediaBufferBlockerNone)
		closeMediaBufferCopyRequests(t, waiter, holder)
	})

	t.Run("at target", func(t *testing.T) {
		buffer := mustMediaBuffer(t, mediaBufferChunkSize)
		request := buffer.register()
		attach(request)
		if _, err := request.requestOptional(); err != nil {
			t.Fatal(err)
		}
		lease, err := request.acceptOptional()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := request.requestOptional(); err != nil {
			t.Fatal(err)
		}
		assertProjection(t, request, telemetry.MediaBufferBlockerAtTarget)
		if err := request.cancelOptionalRequest(); err != nil {
			t.Fatal(err)
		}
		if err := request.releaseOptional(lease); err != nil {
			t.Fatal(err)
		}
		closeMediaBufferCopyRequests(t, request)
	})

	t.Run("debt", func(t *testing.T) {
		buffer := mustMediaBuffer(t, 2*mediaBufferChunkSize)
		request := buffer.register()
		for range 2 {
			if _, err := request.requestOptional(); err != nil {
				t.Fatal(err)
			}
			if _, err := request.acceptOptional(); err != nil {
				t.Fatal(err)
			}
		}
		attach(request)
		other := buffer.register()
		if _, err := request.requestOptional(); err != nil {
			t.Fatal(err)
		}
		assertProjection(t, request, telemetry.MediaBufferBlockerDebt)
		closeMediaBufferCopyRequests(t, other, request)
	})
}

func TestMediaBufferRequestProjectionAllocatesNothing(t *testing.T) {
	buffer := mustMediaBuffer(t, mediaBufferChunkSize)
	request := buffer.register()
	request.attachLive(newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "test", StreamID: request.id}, nil))
	allocs := testing.AllocsPerRun(1000, func() {
		buffer.mu.Lock()
		buffer.projectRequestLocked(request)
		buffer.mu.Unlock()
	})
	if allocs != 0 {
		t.Fatalf("request projection allocations=%f", allocs)
	}
	closeMediaBufferCopyRequests(t, request)
}

func TestMediaBufferCancelOptionalProjectionBranches(t *testing.T) {
	attach := func(request *mediaBufferRequest) *mediaBufferLiveState {
		live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "cancel", StreamID: request.id}, newFakeMediaBufferClock(time.Unix(0, 0), 1))
		request.attachLive(live)
		return live
	}
	assertLive := func(t *testing.T, request *mediaBufferRequest, live *mediaBufferLiveState, blocker telemetry.MediaBufferAllocationBlocker) {
		t.Helper()
		requestSnapshot := request.snapshot()
		snapshot := live.MediaBufferLiveSnapshot()
		if snapshot.TargetBytes != requestSnapshot.Target || snapshot.OwnedBytes != requestSnapshot.Owned || snapshot.DebtBytes != requestSnapshot.Debt || snapshot.Blocker.Value != uint8(blocker) {
			t.Fatalf("request=%+v live=%+v blocker=%d", requestSnapshot, snapshot, blocker)
		}
	}

	t.Run("pending reclaim schedules waiter", func(t *testing.T) {
		buffer := mustMediaBuffer(t, 4*mediaBufferChunkSize)
		holder := buffer.register()
		for range 3 {
			requireAcceptedMediaBufferLease(t, holder)
		}
		requireMediaBufferNotification(t, requireMediaBufferRequest(t, holder))
		holderLive := attach(holder)
		waiter := buffer.register()
		waiterLive := attach(waiter)
		requireNoMediaBufferNotification(t, requireMediaBufferRequest(t, waiter))
		assertLive(t, waiter, waiterLive, telemetry.MediaBufferBlockerPoolExhausted)
		if err := holder.cancelOptionalRequest(); err != nil {
			t.Fatal(err)
		}
		assertLive(t, holder, holderLive, telemetry.MediaBufferBlockerNone)
		assertLive(t, waiter, waiterLive, telemetry.MediaBufferBlockerNone)
		if snapshot := waiter.snapshot(); !snapshot.Pending || snapshot.Owned != mediaBufferChunkSize {
			t.Fatalf("waiter after pending handoff=%+v", snapshot)
		}
		controller := buffer.Snapshot()
		if controller.Allocated != 4*mediaBufferChunkSize || controller.Owned != 4*mediaBufferChunkSize || controller.Free != 0 || controller.ActiveRequests != 2 || controller.IndebtedRequests != 1 || controller.RequestDebtBytes != mediaBufferChunkSize {
			t.Fatalf("controller after pending cancel=%+v", controller)
		}
		closeMediaBufferCopyRequests(t, waiter, holder)
	})

	t.Run("waiting clears blocker only", func(t *testing.T) {
		buffer := mustMediaBuffer(t, 4*mediaBufferChunkSize)
		holder := buffer.register()
		for range 4 {
			requireAcceptedMediaBufferLease(t, holder)
		}
		waiter := buffer.register()
		waiterLive := attach(waiter)
		requireNoMediaBufferNotification(t, requireMediaBufferRequest(t, waiter))
		assertLive(t, waiter, waiterLive, telemetry.MediaBufferBlockerPoolExhausted)
		before := buffer.Snapshot()
		if err := waiter.cancelOptionalRequest(); err != nil {
			t.Fatal(err)
		}
		assertLive(t, waiter, waiterLive, telemetry.MediaBufferBlockerNone)
		after := buffer.Snapshot()
		if after != before {
			t.Fatalf("waiting cancellation changed controller before=%+v after=%+v", before, after)
		}
		closeMediaBufferCopyRequests(t, waiter, holder)
	})

	t.Run("no state is projection and controller no-op", func(t *testing.T) {
		buffer := mustMediaBuffer(t, mediaBufferChunkSize)
		request := buffer.register()
		live := attach(request)
		beforeLive := live.MediaBufferLiveSnapshot()
		beforeController := buffer.Snapshot()
		beforeReads := live.clock.reads.Load()
		if err := request.cancelOptionalRequest(); err != nil {
			t.Fatal(err)
		}
		afterLive := live.MediaBufferLiveSnapshot()
		if afterLive.Blocker != beforeLive.Blocker || afterLive.TargetBytes != beforeLive.TargetBytes || afterLive.OwnedBytes != beforeLive.OwnedBytes || afterLive.DebtBytes != beforeLive.DebtBytes {
			t.Fatalf("no-state live before=%+v after=%+v", beforeLive, afterLive)
		}
		if got := live.clock.reads.Load() - beforeReads; got != 1 {
			t.Fatalf("no-state snapshot clock reads=%d want=1", got)
		}
		if afterController := buffer.Snapshot(); afterController != beforeController {
			t.Fatalf("no-state controller before=%+v after=%+v", beforeController, afterController)
		}
		closeMediaBufferCopyRequests(t, request)
	})
}

func assertCachedMediaBufferAccounting(t *testing.T, buffer *mediaBuffer) {
	t.Helper()
	buffer.mu.Lock()
	var baseOnly, indebted int
	var debt int64
	for _, request := range buffer.requests {
		if request.owned == 0 {
			baseOnly++
		}
		if request.debt > 0 {
			indebted++
			debt += request.debt
		}
	}
	if baseOnly != buffer.baseOnlyRequests || indebted != buffer.indebtedRequests || debt != buffer.requestDebtBytes {
		buffer.mu.Unlock()
		t.Fatalf("cached=%d/%d/%d exhaustive=%d/%d/%d", buffer.baseOnlyRequests, buffer.indebtedRequests, buffer.requestDebtBytes, baseOnly, indebted, debt)
	}
	buffer.mu.Unlock()
	snapshot := buffer.Snapshot()
	if snapshot.BaseOnlyRequests != baseOnly || snapshot.IndebtedRequests != indebted || snapshot.RequestDebtBytes != debt {
		t.Fatalf("snapshot=%+v exhaustive=%d/%d/%d", snapshot, baseOnly, indebted, debt)
	}
}

func TestMediaCopyTypedSentinelsAndOutcomePrecedence(t *testing.T) {
	invalidRead := copyMediaBody(io.Discard, mediaBufferInvalidReader{n: -1}, -1)
	if !errors.Is(invalidRead.Err, ErrInvalidMediaRead) || invalidRead.Err.Error() != "invalid upstream media read" {
		t.Fatalf("invalid read=%+v", invalidRead)
	}
	invalidWrite := copyMediaBody(&mediaBufferInvalidWriter{n: -1}, bytes.NewBufferString("media"), 5)
	if !errors.Is(invalidWrite.Err, ErrInvalidMediaWrite) || invalidWrite.Err.Error() != "invalid downstream media write" {
		t.Fatalf("invalid write=%+v", invalidWrite)
	}

	cases := []struct {
		name   string
		result mediaCopyResult
		want   mediaCopyOutcome
	}{
		{"downstream invalid wins invariant", mediaCopyResult{PrimaryDirection: mediaDirectionDownstream, PrimaryErr: ErrInvalidMediaWrite, InvariantObserved: true}, mediaCopyOutcomeInvalidWrite},
		{"downstream short", mediaCopyResult{PrimaryDirection: mediaDirectionDownstream, PrimaryErr: io.ErrShortWrite}, mediaCopyOutcomeShortWrite},
		{"cancel wins invariant", mediaCopyResult{PrimaryErr: context.Canceled, InvariantObserved: true}, mediaCopyOutcomeCanceled},
		{"deadline", mediaCopyResult{PrimaryErr: context.DeadlineExceeded}, mediaCopyOutcomeCanceled},
		{"deadline wins invariant", mediaCopyResult{PrimaryErr: context.DeadlineExceeded, InvariantObserved: true}, mediaCopyOutcomeCanceled},
		{"deadline without primary", mediaCopyResult{Err: context.DeadlineExceeded, InvariantObserved: true}, mediaCopyOutcomeInvariantError},
		{"upstream length", mediaCopyResult{PrimaryDirection: mediaDirectionUpstream, PrimaryErr: errMediaLengthMismatch}, mediaCopyOutcomeLengthMismatch},
		{"upstream invalid", mediaCopyResult{PrimaryDirection: mediaDirectionUpstream, PrimaryErr: ErrInvalidMediaRead}, mediaCopyOutcomeInvalidRead},
		{"upstream no progress", mediaCopyResult{PrimaryDirection: mediaDirectionUpstream, PrimaryErr: io.ErrNoProgress}, mediaCopyOutcomeNoProgress},
		{"invariant only", mediaCopyResult{InvariantObserved: true}, mediaCopyOutcomeInvariantError},
		{"success", mediaCopyResult{}, mediaCopyOutcomeSuccess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyMediaCopyOutcome(tc.result); got != tc.want {
				t.Fatalf("outcome=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestMediaBufferCopyProjectsTerminalEngineState(t *testing.T) {
	buffer := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	request := buffer.register()
	live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: request.id}, newFakeMediaBufferClock(time.Unix(0, 0), 0))
	request.attachLive(live)
	payload := bytes.Repeat([]byte("x"), mediaCopyBufferSize+9)
	source := newMediaBufferTestSource(bytes.NewReader(payload), nil)
	cleanupChecked := false
	hooks := &mediaBufferCopyHooks{afterQueueCleanup: func() {
		cleanupChecked = true
		snapshot := live.MediaBufferLiveSnapshot()
		if snapshot.Consumer.Value != uint8(telemetry.MediaBufferConsumerIdle) || snapshot.Producer.Value != uint8(telemetry.MediaBufferProducerDone) || snapshot.QueuedBytes != 0 {
			t.Fatalf("pre-done cleanup state=%+v", snapshot)
		}
	}}
	result := copyBufferedMediaBodyWithHooks(context.Background(), io.Discard, source, source, make([]byte, mediaCopyBufferSize), request, int64(len(payload)), hooks)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	snapshot := live.MediaBufferLiveSnapshot()
	if !cleanupChecked || snapshot.Lifecycle.Value != uint8(telemetry.MediaBufferLifecycleClosing) || snapshot.Producer.Value != uint8(telemetry.MediaBufferProducerDone) || snapshot.Consumer.Value != uint8(telemetry.MediaBufferConsumerDone) {
		t.Fatalf("terminal engine states=%+v", snapshot)
	}
	if result.BytesRead != int64(len(payload)) || result.BytesWritten != int64(len(payload)) || snapshot.QueuedBytes != 0 || snapshot.WritingBytes != 0 {
		t.Fatalf("result=%+v projected gauges=%+v", result, snapshot)
	}
	closeMediaBufferCopyRequests(t, request)
}

func TestMediaBufferObservedRegistrationPreservesControllerOrder(t *testing.T) {
	controller := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	registry := telemetry.New(nil).MediaBufferLive()
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry}, NewMemoryStore())
	firstPaused := make(chan struct{})
	releaseFirst := make(chan struct{})
	server.mediaBufferHooks = &mediaBufferServerHooks{beforeLiveRegister: func(id uint64) {
		if id == 1 {
			close(firstPaused)
			<-releaseFirst
		}
	}}
	type registration struct {
		request *mediaBufferRequest
		live    *mediaBufferLiveState
	}
	results := make(chan registration, 2)
	go func() {
		request, live := server.registerMediaBufferRequest("/Videos/one/stream", &Session{}, "direct")
		results <- registration{request: request, live: live}
	}()
	awaitMediaBufferSignal(t, firstPaused)
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		request, live := server.registerMediaBufferRequest("/Videos/two/stream", &Session{}, "direct")
		results <- registration{request: request, live: live}
	}()
	awaitMediaBufferSignal(t, secondStarted)
	if snapshot := controller.Snapshot(); snapshot.ActiveRequests != 1 {
		t.Fatalf("sequencer allowed inverted controller registration: %+v", snapshot)
	}
	close(releaseFirst)
	first := <-results
	second := <-results
	if first.request.id > second.request.id {
		first, second = second, first
	}
	if first.request.id != 1 || second.request.id != 2 || first.live == nil || second.live == nil || registry.RegistrationDrops() != 0 {
		t.Fatalf("registrations first=%d/%v second=%d/%v drops=%d", first.request.id, first.live != nil, second.request.id, second.live != nil, registry.RegistrationDrops())
	}
	page := registry.Page(0, 2)
	if len(page.Items) != 2 || page.Items[0].MediaBufferRawStreamID() != 1 || page.Items[1].MediaBufferRawStreamID() != 2 {
		t.Fatalf("ordered page=%+v", page)
	}
	closeMediaBufferCopyRequests(t, second.request, first.request)
}

func TestMediaBufferBeginTransferRunsAfterRegistrationLocksReleased(t *testing.T) {
	controller := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	registry := telemetry.New(nil).MediaBufferLive()
	meter := newBlockingBeginTrafficMeter()
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry, Meter: meter}, NewMemoryStore())
	headerCommitted := make(chan struct{})
	server.mediaBufferHooks = &mediaBufferServerHooks{afterHeaderCommit: func() { close(headerCommitted) }}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/one/stream", nil)
	body := newMediaBufferServerBody(bytes.NewBufferString("media"))
	resp := mediaBufferServerResponse(req, http.StatusOK, 5, body)
	wrapResponseBodyOnce(resp)
	writer := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.writeProxyResponseWithSnapshot(writer, req, "/Videos/one/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
		close(done)
	}()
	meta := <-meter.entered
	if meta.MediaBuffer == nil || meta.MediaBuffer.StreamID != 1 {
		t.Fatalf("begin meta=%+v", meta)
	}
	select {
	case <-headerCommitted:
		t.Fatal("headers committed before transfer binding")
	default:
	}
	page := registry.Page(0, 2)
	if len(page.Items) != 1 {
		t.Fatalf("pre-bind page=%+v", page)
	}
	first := page.Items[0].(*mediaBufferLiveState)
	if snapshot := first.MediaBufferLiveSnapshot(); snapshot.TransferID != 0 || first.transfer.Load() != nil {
		t.Fatalf("pre-bind snapshot=%+v transfer=%p", snapshot, first.transfer.Load())
	}
	compacted := make(chan int, 1)
	go func() { compacted <- registry.CompactTerminal() }()
	select {
	case removed := <-compacted:
		if removed != 0 {
			t.Fatalf("pre-bind compact removed=%d", removed)
		}
	case <-time.After(time.Second):
		t.Fatal("BeginTransfer retained registry lock")
	}
	type registration struct {
		request *mediaBufferRequest
		live    *mediaBufferLiveState
	}
	registered := make(chan registration, 1)
	go func() {
		request, live := server.registerMediaBufferRequest("/Videos/two/stream", &Session{}, "direct")
		registered <- registration{request: request, live: live}
	}()
	var second registration
	select {
	case second = <-registered:
	case <-time.After(time.Second):
		t.Fatal("BeginTransfer retained registration sequencer")
	}
	if second.request.id != 2 || second.live == nil {
		t.Fatalf("second registration=%+v", second)
	}
	stopSnapshots := make(chan struct{})
	snapshotsDone := make(chan struct{})
	go func() {
		defer close(snapshotsDone)
		for {
			select {
			case <-stopSnapshots:
				return
			default:
				_ = first.MediaBufferLiveSnapshot()
				_, _ = first.MediaBufferLiveBytes()
			}
		}
	}()
	close(meter.release)
	awaitMediaBufferSignal(t, headerCommitted)
	awaitMediaBufferSignal(t, done)
	close(stopSnapshots)
	awaitMediaBufferSignal(t, snapshotsDone)
	handle := first.transfer.Load()
	if handle == nil || first.MediaBufferLiveSnapshot().TransferID != handle.ID() || writer.Body.String() != "media" {
		t.Fatalf("post-bind transfer=%p snapshot=%+v body=%q", handle, first.MediaBufferLiveSnapshot(), writer.Body.String())
	}
	page = registry.Page(0, 2)
	if len(page.Items) != 1 || page.Items[0].MediaBufferRawStreamID() != 2 {
		t.Fatalf("post-completion ordered page=%+v", page)
	}
	closeMediaBufferCopyRequests(t, second.request)
}

type blockingBeginTrafficMeter struct {
	delegate *telemetry.ByteMeter
	entered  chan telemetry.TransferMeta
	release  chan struct{}
}

func newBlockingBeginTrafficMeter() *blockingBeginTrafficMeter {
	return &blockingBeginTrafficMeter{delegate: telemetry.NewByteMeter(), entered: make(chan telemetry.TransferMeta, 1), release: make(chan struct{})}
}

func (m *blockingBeginTrafficMeter) AddEgress(n int64)  { m.delegate.AddEgress(n) }
func (m *blockingBeginTrafficMeter) AddIngress(n int64) { m.delegate.AddIngress(n) }
func (m *blockingBeginTrafficMeter) NoteError()         { m.delegate.NoteError() }
func (m *blockingBeginTrafficMeter) BeginTransfer(meta telemetry.TransferMeta) *telemetry.TransferHandle {
	m.entered <- meta
	<-m.release
	return m.delegate.BeginTransfer(meta)
}

func TestMediaBufferServerRegistrationOverlapsCompaction(t *testing.T) {
	registry := telemetry.New(nil).MediaBufferLive()
	blocker := &gatewayBlockingLiveState{id: 1, terminal: true, entered: make(chan struct{}), release: make(chan struct{})}
	if !registry.Register(blocker) {
		t.Fatal("register blocker")
	}
	compacted := make(chan int, 1)
	go func() { compacted <- registry.CompactTerminal() }()
	awaitMediaBufferSignal(t, blocker.entered)
	controller := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry}, NewMemoryStore())
	registering := make(chan struct{})
	server.mediaBufferHooks = &mediaBufferServerHooks{beforeLiveRegister: func(uint64) { close(registering) }}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	body := newMediaBufferServerBody(bytes.NewBufferString("media"))
	resp := mediaBufferServerResponse(req, http.StatusOK, 5, body)
	wrapResponseBodyOnce(resp)
	writer := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
		close(done)
	}()
	awaitMediaBufferSignal(t, registering)
	select {
	case <-done:
		t.Fatal("server registration did not wait for compaction WLock")
	case <-time.After(10 * time.Millisecond):
	}
	close(blocker.release)
	if removed := <-compacted; removed != 1 {
		t.Fatalf("removed=%d", removed)
	}
	awaitMediaBufferSignal(t, done)
	if writer.Body.String() != "media" || registry.RegistrationDrops() != 0 {
		t.Fatalf("body=%q drops=%d", writer.Body.String(), registry.RegistrationDrops())
	}
}

func TestMediaBufferCopyProjectsDirectBlockingStates(t *testing.T) {
	buffer := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	blocker := buffer.register()
	blockerLeases := []mediaBufferLease{acceptMediaBufferCopyLease(t, blocker), acceptMediaBufferCopyLease(t, blocker)}
	request := buffer.register()
	live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: request.id}, newFakeMediaBufferClock(time.Unix(0, 0), 0))
	request.attachLive(live)
	ctx, cancel := context.WithCancel(context.Background())
	writer := newMediaBufferBlockingWriter(nil)
	waiting := make(chan struct{})
	hooks := &mediaBufferCopyHooks{onOptionalWait: func() { close(waiting) }}
	source := newMediaBufferTestSource(bytes.NewReader(bytes.Repeat([]byte("a"), 2*mediaCopyBufferSize)), nil)
	resultCh := make(chan mediaCopyResult, 1)
	go func() {
		resultCh <- copyBufferedMediaBodyWithHooks(ctx, writer, source, source, make([]byte, mediaCopyBufferSize), request, -1, hooks)
	}()
	awaitMediaBufferSignal(t, writer.started)
	awaitMediaBufferSignal(t, waiting)
	snapshot := live.MediaBufferLiveSnapshot()
	if snapshot.Consumer.Value != uint8(telemetry.MediaBufferConsumerWriting) || snapshot.Producer.Value != uint8(telemetry.MediaBufferProducerWaitingForBuffer) {
		t.Fatalf("blocking states=%+v", snapshot)
	}
	cancel()
	close(writer.release)
	result := awaitMediaBufferResult(t, resultCh)
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("result=%+v", result)
	}
	terminal := live.MediaBufferLiveSnapshot()
	if terminal.Consumer.Value != uint8(telemetry.MediaBufferConsumerDone) || terminal.Producer.Value != uint8(telemetry.MediaBufferProducerDone) {
		t.Fatalf("final states=%+v", terminal)
	}
	for _, lease := range blockerLeases {
		if err := blocker.releaseOptional(lease); err != nil {
			t.Fatal(err)
		}
	}
	closeMediaBufferCopyRequests(t, blocker, request)
}

func TestMediaBufferQueueConsumerWaitBoundaries(t *testing.T) {
	t.Run("immediately queued data does not wait", func(t *testing.T) {
		live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, newFakeMediaBufferClock(time.Unix(0, 0), 0))
		queue := &mediaBufferCopyQueue{notify: make(chan struct{}, 1), live: live}
		if !queue.publish(mediaBufferCopyEvent{length: 1}) {
			t.Fatal("publish")
		}
		if _, ok := queue.next(context.Background(), nil); !ok {
			t.Fatal("next")
		}
		snapshot := live.MediaBufferLiveSnapshot()
		if snapshot.Consumer.Value != uint8(telemetry.MediaBufferConsumerIdle) || snapshot.Waits[mediaBufferWaitConsumer] != (telemetry.MediaBufferWaitStat{}) {
			t.Fatalf("immediate event snapshot=%+v", snapshot)
		}
	})

	for _, tc := range []struct {
		name     string
		duration time.Duration
		wake     func(*mediaBufferCopyQueue, context.CancelFunc)
		wantOK   bool
	}{
		{name: "data", duration: 2 * time.Second, wake: func(q *mediaBufferCopyQueue, _ context.CancelFunc) { q.publish(mediaBufferCopyEvent{length: 1}) }, wantOK: true},
		{name: "cancel", duration: 3 * time.Second, wake: func(_ *mediaBufferCopyQueue, cancel context.CancelFunc) { cancel() }},
		{name: "terminal", duration: 4 * time.Second, wake: func(q *mediaBufferCopyQueue, _ context.CancelFunc) { q.publish(mediaBufferCopyEvent{terminal: true}) }, wantOK: true},
		{name: "closing", duration: 5 * time.Second, wake: func(q *mediaBufferCopyQueue, _ context.CancelFunc) { q.close() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeMediaBufferClock(time.Unix(0, 0), 0)
			live := newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "boot", StreamID: 1}, clock)
			live.setConsumer(telemetry.MediaBufferConsumerWriting)
			queue := &mediaBufferCopyQueue{notify: make(chan struct{}, 1), live: live}
			waiting := make(chan struct{})
			hooks := &mediaBufferCopyHooks{onBeforeQueueWait: func() { close(waiting) }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan bool, 1)
			go func() {
				event, ok := queue.next(ctx, hooks)
				if ok && !event.terminal {
					live.setConsumer(telemetry.MediaBufferConsumerWriting)
				} else {
					live.setConsumer(telemetry.MediaBufferConsumerDone)
				}
				result <- ok
			}()
			awaitMediaBufferSignal(t, waiting)
			blocked := live.MediaBufferLiveSnapshot()
			if blocked.Consumer.Value != uint8(telemetry.MediaBufferConsumerWaitingForData) || blocked.Consumer.TransitionMS != 0 {
				t.Fatalf("blocked snapshot=%+v", blocked)
			}
			clock.advance(tc.duration)
			tc.wake(queue, cancel)
			if ok := <-result; ok != tc.wantOK {
				t.Fatalf("next ok=%v want=%v", ok, tc.wantOK)
			}
			snapshot := live.MediaBufferLiveSnapshot()
			wantState := telemetry.MediaBufferConsumerDone
			if tc.name == "data" {
				wantState = telemetry.MediaBufferConsumerWriting
			}
			wantWait := telemetry.MediaBufferWaitStat{TotalMS: tc.duration.Milliseconds(), MaxMS: tc.duration.Milliseconds()}
			if snapshot.Consumer.Value != uint8(wantState) || snapshot.Consumer.TransitionMS != tc.duration.Milliseconds() || snapshot.Waits[mediaBufferWaitConsumer] != wantWait {
				t.Fatalf("wake snapshot=%+v want state=%d wait=%+v", snapshot, wantState, wantWait)
			}
		})
	}
}

func TestMediaBufferServerIdentityAndCompletionBeforeAudit(t *testing.T) {
	controller := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	registry := telemetry.New(nil).MediaBufferLive()
	meter := telemetry.NewByteMeter()
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry, Meter: meter}, NewMemoryStore())
	headerChecked := false
	completionChecked := false
	server.mediaBufferHooks = &mediaBufferServerHooks{
		afterHeaderCommit: func() {
			headerChecked = true
			active := meter.ActiveTransfers()
			if len(active) != 1 || active[0].MediaBuffer == nil || active[0].MediaBuffer.BootID != registry.BootID() {
				t.Fatalf("active transfer=%+v", active)
			}
			state, ok := registry.Detail(active[0].MediaBuffer.StreamID)
			if !ok {
				t.Fatal("linked stream missing before header")
			}
			snapshot := state.MediaBufferLiveSnapshot()
			if snapshot.TransferID == 0 || snapshot.ItemID != "movie-1" {
				t.Fatalf("pre-header snapshot=%+v", snapshot)
			}
		},
		beforeFailureAudit: func() {
			completionChecked = true
			completion, ok := registry.TryCompletion()
			terminal := completion.Terminal
			controllerSnapshot := controller.Snapshot()
			if !ok || completion.Outcome != string(mediaCopyOutcomeDownstreamError) || completion.BytesRead != 5 || completion.BytesWritten != 0 || completion.CompletedAt.IsZero() || !terminal.Terminal || terminal.TransferID == 0 || terminal.Consumer.Value != uint8(telemetry.MediaBufferConsumerDone) || terminal.Producer.Value != uint8(telemetry.MediaBufferProducerDone) || terminal.OwnedBytes != 0 || terminal.Blocker.Value != uint8(telemetry.MediaBufferBlockerNone) || controllerSnapshot.ActiveRequests != 0 || meter.ActiveTransferCount() != 0 {
				t.Fatalf("completion before audit=%+v ok=%v", completion, ok)
			}
		},
	}
	ready := make(chan struct{})
	close(ready)
	writer := &mediaBufferServerFailureWriter{readBlocked: ready, err: timeoutMediaError{}}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/movie-1/stream", nil)
	body := newMediaBufferServerBody(bytes.NewBufferString("media"))
	resp := mediaBufferServerResponse(req, http.StatusOK, 5, body)
	wrapResponseBodyOnce(resp)
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("panic=%v", recovered)
			}
		}()
		server.writeProxyResponseWithSnapshot(writer, req, "/Videos/movie-1/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	}()
	if !headerChecked || !completionChecked || meter.ActiveTransferCount() != 0 || registry.CompactTerminal() != 1 {
		t.Fatalf("header=%v completion=%v transfers=%d", headerChecked, completionChecked, meter.ActiveTransferCount())
	}
}

func TestMediaBufferServerObservationDropHasNoTransferLink(t *testing.T) {
	registry := telemetry.New(nil).MediaBufferLive()
	for id := uint64(1); id <= telemetry.MediaBufferLiveCapacity; id++ {
		if !registry.Register(&gatewayTestLiveState{id: id}) {
			t.Fatalf("fill id=%d", id)
		}
	}
	controller := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	meter := telemetry.NewByteMeter()
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry, Meter: meter}, NewMemoryStore())
	server.mediaBufferHooks = &mediaBufferServerHooks{afterHeaderCommit: func() {
		active := meter.ActiveTransfers()
		if len(active) != 1 || active[0].MediaBuffer != nil {
			t.Fatalf("dropped observation transfer=%+v", active)
		}
	}}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	body := newMediaBufferServerBody(bytes.NewBufferString("media"))
	resp := mediaBufferServerResponse(req, http.StatusOK, 5, body)
	wrapResponseBodyOnce(resp)
	writer := httptest.NewRecorder()
	server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	if writer.Body.String() != "media" || registry.RegistrationDrops() != 1 {
		t.Fatalf("body=%q drops=%d", writer.Body.String(), registry.RegistrationDrops())
	}
	if _, ok := registry.TryCompletion(); ok {
		t.Fatal("dropped observation offered completion")
	}
}

func TestMediaBufferBytesUseTransferAndCompletionBoundaries(t *testing.T) {
	snapshotType := reflect.TypeOf(telemetry.MediaBufferLiveSnapshot{})
	if _, ok := snapshotType.FieldByName("BytesRead"); ok {
		t.Fatal("live sidecar retained duplicate BytesRead")
	}
	if _, ok := snapshotType.FieldByName("BytesWritten"); ok {
		t.Fatal("live sidecar retained duplicate BytesWritten")
	}

	registry := telemetry.New(nil).MediaBufferLive()
	meter := telemetry.NewByteMeter()
	controller := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry, Meter: meter}, NewMemoryStore())
	payload := bytes.Repeat([]byte("b"), 2*mediaCopyBufferSize)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	body := newMediaBufferServerBody(bytes.NewReader(payload))
	resp := mediaBufferServerResponse(req, http.StatusOK, int64(len(payload)), body)
	wrapResponseBodyOnce(resp)
	writer := newMediaBufferSecondWriteBlockingWriter()
	done := make(chan struct{})
	go func() {
		server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
		close(done)
	}()
	awaitMediaBufferSignal(t, writer.second)
	active := meter.ActiveTransfers()
	if len(active) != 1 || active[0].MediaBuffer == nil || active[0].BytesOut != mediaCopyBufferSize {
		t.Fatalf("active transfer boundary=%+v", active)
	}
	state, ok := registry.Detail(active[0].MediaBuffer.StreamID)
	if !ok {
		t.Fatal("live stream missing")
	}
	byteState, ok := state.(telemetry.MediaBufferLiveByteState)
	if !ok {
		t.Fatal("live stream does not implement byte projection")
	}
	bytesRead, bytesWritten := byteState.MediaBufferLiveBytes()
	if bytesRead != int64(len(payload)) || bytesWritten != mediaCopyBufferSize {
		t.Fatalf("live bytes=%d/%d", bytesRead, bytesWritten)
	}
	close(writer.release)
	awaitMediaBufferSignal(t, done)
	completion, ok := registry.TryCompletion()
	if !ok || completion.BytesRead != int64(len(payload)) || completion.BytesWritten != int64(len(payload)) || completion.CompletedAt.IsZero() || meter.ActiveTransferCount() != 0 {
		t.Fatalf("completion=%+v ok=%v active=%d", completion, ok, meter.ActiveTransferCount())
	}
	if finalRead, finalWritten := byteState.MediaBufferLiveBytes(); finalRead != completion.BytesRead || finalWritten != completion.BytesWritten {
		t.Fatalf("terminal live bytes=%d/%d completion=%d/%d", finalRead, finalWritten, completion.BytesRead, completion.BytesWritten)
	}
}

func TestMediaBufferBytesNilMeterFallback(t *testing.T) {
	registry := telemetry.New(nil).MediaBufferLive()
	controller := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry}, NewMemoryStore())
	payload := bytes.Repeat([]byte("f"), 2*mediaCopyBufferSize)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	body := newMediaBufferServerBody(bytes.NewReader(payload))
	resp := mediaBufferServerResponse(req, http.StatusOK, int64(len(payload)), body)
	wrapResponseBodyOnce(resp)
	writer := newMediaBufferSecondWriteBlockingWriter()
	done := make(chan struct{})
	go func() {
		server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
		close(done)
	}()
	awaitMediaBufferSignal(t, writer.second)
	page := registry.Page(0, 1)
	if len(page.Items) != 1 {
		t.Fatalf("live page=%+v", page)
	}
	byteState := page.Items[0].(telemetry.MediaBufferLiveByteState)
	if read, written := byteState.MediaBufferLiveBytes(); read != int64(len(payload)) || written != mediaCopyBufferSize {
		t.Fatalf("fallback live bytes=%d/%d", read, written)
	}
	close(writer.release)
	awaitMediaBufferSignal(t, done)
	completion, ok := registry.TryCompletion()
	if !ok || completion.BytesRead != int64(len(payload)) || completion.BytesWritten != int64(len(payload)) {
		t.Fatalf("fallback completion=%+v ok=%v", completion, ok)
	}
	if read, written := byteState.MediaBufferLiveBytes(); read != completion.BytesRead || written != completion.BytesWritten {
		t.Fatalf("retained fallback bytes=%d/%d completion=%d/%d", read, written, completion.BytesRead, completion.BytesWritten)
	}
}

func TestMediaBufferNilTransferHandleFallsBackAndDoesNotLeakObservation(t *testing.T) {
	registry := telemetry.New(nil).MediaBufferLive()
	controller := mustMediaBufferCopyController(t, 2*mediaBufferChunkSize)
	meter := &nilHandleTrafficMeter{}
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry, Meter: meter}, NewMemoryStore())
	payload := bytes.Repeat([]byte("n"), 2*mediaCopyBufferSize)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	body := newMediaBufferServerBody(bytes.NewReader(payload))
	resp := mediaBufferServerResponse(req, http.StatusOK, int64(len(payload)), body)
	wrapResponseBodyOnce(resp)
	writer := newMediaBufferSecondWriteBlockingWriter()
	done := make(chan struct{})
	go func() {
		server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
		close(done)
	}()
	awaitMediaBufferSignal(t, writer.second)
	page := registry.Page(0, 1)
	if len(page.Items) != 1 {
		t.Fatalf("nil-handle page=%+v", page)
	}
	live := page.Items[0].(*mediaBufferLiveState)
	if snapshot := live.MediaBufferLiveSnapshot(); snapshot.TransferID != 0 || live.transfer.Load() != nil {
		t.Fatalf("nil-handle linkage snapshot=%+v transfer=%p", snapshot, live.transfer.Load())
	}
	if read, written := live.MediaBufferLiveBytes(); read != int64(len(payload)) || written != mediaCopyBufferSize {
		t.Fatalf("nil-handle live bytes=%d/%d", read, written)
	}
	close(writer.release)
	awaitMediaBufferSignal(t, done)
	completion, ok := registry.TryCompletion()
	if !ok || completion.Terminal.TransferID != 0 || completion.BytesRead != int64(len(payload)) || completion.BytesWritten != int64(len(payload)) {
		t.Fatalf("nil-handle completion=%+v ok=%v", completion, ok)
	}
	if meter.ingress.Load() != uint64(len(payload)) || meter.egress.Load() != uint64(len(payload)) || meter.begins.Load() != 1 {
		t.Fatalf("nil-handle meter begins=%d bytes=%d/%d", meter.begins.Load(), meter.ingress.Load(), meter.egress.Load())
	}
	if removed := registry.CompactTerminal(); removed != 1 {
		t.Fatalf("nil-handle compact removed=%d", removed)
	}
}

type nilHandleTrafficMeter struct {
	ingress atomic.Uint64
	egress  atomic.Uint64
	errors  atomic.Uint64
	begins  atomic.Uint64
}

func (m *nilHandleTrafficMeter) AddEgress(n int64) {
	if n > 0 {
		m.egress.Add(uint64(n))
	}
}
func (m *nilHandleTrafficMeter) AddIngress(n int64) {
	if n > 0 {
		m.ingress.Add(uint64(n))
	}
}
func (m *nilHandleTrafficMeter) NoteError() { m.errors.Add(1) }
func (m *nilHandleTrafficMeter) BeginTransfer(telemetry.TransferMeta) *telemetry.TransferHandle {
	m.begins.Add(1)
	return nil
}

func TestMediaBufferCompletionDropDoesNotChangeCopyOrOwnership(t *testing.T) {
	registry := telemetry.New(nil).MediaBufferLive()
	for i := 0; i < 256; i++ {
		if !registry.OfferCompletion(telemetry.MediaBufferCompletion{}) {
			t.Fatalf("fill completion slot %d", i)
		}
	}
	controller := mustMediaBufferCopyController(t, mediaBufferChunkSize)
	meter := telemetry.NewByteMeter()
	server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry, Meter: meter}, NewMemoryStore())
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
	body := newMediaBufferServerBody(bytes.NewBufferString("media"))
	resp := mediaBufferServerResponse(req, http.StatusOK, 5, body)
	wrapResponseBodyOnce(resp)
	writer := httptest.NewRecorder()
	server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
	if writer.Body.String() != "media" || registry.CompletionDrops() != 1 || meter.ActiveTransferCount() != 0 {
		t.Fatalf("body=%q drops=%d transfers=%d", writer.Body.String(), registry.CompletionDrops(), meter.ActiveTransferCount())
	}
	if snapshot := controller.Snapshot(); snapshot.ActiveRequests != 0 || snapshot.Owned != 0 {
		t.Fatalf("controller after drop=%+v", snapshot)
	}
}

type mediaBufferSecondWriteBlockingWriter struct {
	*httptest.ResponseRecorder
	calls   int
	second  chan struct{}
	release chan struct{}
}

func newMediaBufferSecondWriteBlockingWriter() *mediaBufferSecondWriteBlockingWriter {
	return &mediaBufferSecondWriteBlockingWriter{
		ResponseRecorder: httptest.NewRecorder(),
		second:           make(chan struct{}),
		release:          make(chan struct{}),
	}
}

func (w *mediaBufferSecondWriteBlockingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 2 {
		close(w.second)
		<-w.release
	}
	return w.ResponseRecorder.Write(p)
}

type gatewayTestLiveState struct {
	id       uint64
	terminal bool
}

type gatewayBlockingLiveState struct {
	id       uint64
	terminal bool
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (s *gatewayBlockingLiveState) MediaBufferRawStreamID() uint64 { return s.id }
func (s *gatewayBlockingLiveState) MediaBufferTerminal() bool {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.terminal
}
func (s *gatewayBlockingLiveState) MediaBufferLiveSnapshot() telemetry.MediaBufferLiveSnapshot {
	panic("full snapshot used during compaction")
}

func (s *gatewayTestLiveState) MediaBufferRawStreamID() uint64 { return s.id }
func (s *gatewayTestLiveState) MediaBufferTerminal() bool      { return s.terminal }
func (s *gatewayTestLiveState) MediaBufferLiveSnapshot() telemetry.MediaBufferLiveSnapshot {
	return telemetry.MediaBufferLiveSnapshot{StreamID: s.id, Terminal: s.terminal}
}

func BenchmarkMediaBufferSnapshotO1(b *testing.B) {
	for _, n := range []int{1, 64, 512, 4096} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			buffer, err := newMediaBuffer(int64(n) * mediaBufferChunkSize)
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < n; i++ {
				buffer.register()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = buffer.Snapshot()
			}
		})
	}
}

func BenchmarkMediaBufferCopyDuringRegistryMaintenance(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 8*mediaCopyBufferSize)
	for _, n := range []int{512, telemetry.MediaBufferLiveCapacity} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				registry := telemetry.New(nil).MediaBufferLive()
				blocker := &gatewayBlockingLiveState{id: 1, terminal: true, entered: make(chan struct{}), release: make(chan struct{})}
				registry.Register(blocker)
				for id := uint64(2); id <= uint64(n); id++ {
					registry.Register(&gatewayTestLiveState{id: id, terminal: true})
				}
				controller, _ := newMediaBuffer(4 * mediaBufferChunkSize)
				server := NewServer(Config{MediaBuffer: &MediaBuffer{controller: controller}, MediaBufferLive: registry}, NewMemoryStore())
				registering := make(chan struct{})
				server.mediaBufferHooks = &mediaBufferServerHooks{beforeLiveRegister: func(uint64) { close(registering) }}
				req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
				body := newMediaBufferServerBody(bytes.NewReader(payload))
				resp := mediaBufferServerResponse(req, http.StatusOK, int64(len(payload)), body)
				wrapResponseBodyOnce(resp)
				writer := httptest.NewRecorder()
				compacted := make(chan int, 1)
				go func() { compacted <- registry.CompactTerminal() }()
				<-blocker.entered
				done := make(chan struct{})
				b.StartTimer()
				go func() {
					server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
					close(done)
				}()
				<-registering
				runtime.Gosched()
				select {
				case <-done:
					b.Fatal("copy completed while maintenance held WLock")
				default:
				}
				close(blocker.release)
				if removed := <-compacted; removed != n {
					b.Fatalf("removed=%d want=%d", removed, n)
				}
				<-done
				b.StopTimer()
				if writer.Body.Len() != len(payload) {
					b.Fatalf("body bytes=%d", writer.Body.Len())
				}
			}
		})
	}
}

func BenchmarkMediaBufferCopyObservation(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 8*mediaCopyBufferSize)
	for _, observed := range []bool{false, true} {
		name := "absent"
		if observed {
			name = "present"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				buffer, _ := newMediaBuffer(4 * mediaBufferChunkSize)
				request := buffer.register()
				if observed {
					request.attachLive(newMediaBufferLiveState(mediaBufferLiveIdentity{BootID: "bench", StreamID: request.id}, nil))
				}
				source := newMediaBufferTestSource(bytes.NewReader(payload), nil)
				base := make([]byte, mediaCopyBufferSize)
				b.StartTimer()
				result := copyBufferedMediaBody(context.Background(), io.Discard, source, source, base, request, int64(len(payload)))
				b.StopTimer()
				if result.Err != nil {
					b.Fatal(result.Err)
				}
				_ = request.close()
			}
		})
	}
}

func BenchmarkMediaBufferResponseObservation(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 8*mediaCopyBufferSize)
	for _, observed := range []bool{false, true} {
		name := "absent"
		if observed {
			name = "present"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				controller, _ := newMediaBuffer(4 * mediaBufferChunkSize)
				config := Config{MediaBuffer: &MediaBuffer{controller: controller}, Meter: telemetry.NewByteMeter()}
				var registry *telemetry.MediaBufferLiveRegistry
				if observed {
					registry = telemetry.New(nil).MediaBufferLive()
					config.MediaBufferLive = registry
				}
				server := NewServer(config, NewMemoryStore())
				req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Videos/item/stream", nil)
				body := newMediaBufferServerBody(bytes.NewReader(payload))
				resp := mediaBufferServerResponse(req, http.StatusOK, int64(len(payload)), body)
				wrapResponseBodyOnce(resp)
				writer := httptest.NewRecorder()
				b.StartTimer()
				server.writeProxyResponseWithSnapshot(writer, req, "/Videos/item/stream", resp, &Session{}, upstreamRequestSnapshot{}, "", "")
				b.StopTimer()
				if writer.Body.Len() != len(payload) || config.Meter.(*telemetry.ByteMeter).ActiveTransferCount() != 0 {
					b.Fatalf("body=%d active=%d", writer.Body.Len(), config.Meter.(*telemetry.ByteMeter).ActiveTransferCount())
				}
				if observed {
					completion, ok := registry.TryCompletion()
					if !ok || completion.BytesRead != int64(len(payload)) || completion.BytesWritten != int64(len(payload)) {
						b.Fatalf("completion=%+v ok=%v", completion, ok)
					}
				}
			}
		})
	}
}
