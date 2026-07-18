package gateway

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"
)

func TestMediaBufferConstructorAndAutomaticBudgetBoundaries(t *testing.T) {
	for _, budget := range []int64{-1, 0, mediaBufferChunkSize - 1, mediaBufferChunkSize + 1} {
		if _, err := newMediaBuffer(budget); !errors.Is(err, errMediaBufferBudget) {
			t.Fatalf("newMediaBuffer(%d) error=%v, want budget error", budget, err)
		}
	}
	buffer := mustMediaBuffer(t, mediaBufferChunkSize)
	if got := buffer.Snapshot(); got.HardBudget != mediaBufferChunkSize || got.Allocated != 0 || got.Owned != 0 || got.Free != 0 || !got.Enabled {
		t.Fatalf("initial snapshot=%+v", got)
	}

	if candidate, ok := minimumPositiveMediaBufferCandidate(0, -1, 16<<30, 8<<30); !ok || candidate != 8<<30 {
		t.Fatalf("minimum candidate=%d ok=%v", candidate, ok)
	}
	if _, ok := minimumPositiveMediaBufferCandidate(0, -1); ok {
		t.Fatal("non-positive candidates were accepted")
	}

	tests := []struct {
		name       string
		candidates []int64
		want       int64
		wantErr    error
	}{
		{name: "no candidate", wantErr: errMediaBufferNoCandidate},
		{name: "non-positive only", candidates: []int64{0, -1, math.MinInt64}, wantErr: errMediaBufferNoCandidate},
		{name: "minimum and one eighth", candidates: []int64{16 << 30, 8 << 30}, want: 1 << 30},
		{name: "two GiB cap", candidates: []int64{math.MaxInt64}, want: mediaBufferAutoBudgetCap},
		{name: "alignment", candidates: []int64{8 * (3*mediaBufferChunkSize + 123)}, want: 3 * mediaBufferChunkSize},
		{name: "exact one chunk", candidates: []int64{8 * mediaBufferChunkSize}, want: mediaBufferChunkSize},
		{name: "below one chunk", candidates: []int64{8*mediaBufferChunkSize - 1}, wantErr: errMediaBufferBudget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := automaticMediaBufferBudget(tt.candidates...)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("budget=%d error=%v, want budget=%d error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestMediaBufferTargetsAndAlignedRemainder(t *testing.T) {
	const budget = int64(2 << 30)
	buffer := mustMediaBuffer(t, budget)
	requests := make([]*mediaBufferRequest, 0, 5)
	for count := 1; count <= 5; count++ {
		requests = append(requests, buffer.register())
		want := mediaBufferRequestCap
		if count == 5 {
			want = alignMediaBufferSize(budget / 5)
		}
		for index, request := range requests {
			if got := request.snapshot(); got.Target != want || got.Debt != 0 || got.Owned != 0 {
				t.Fatalf("N=%d request=%d snapshot=%+v want target=%d", count, index, got, want)
			}
		}
	}
	if remainder := budget - int64(len(requests))*requests[0].snapshot().Target; remainder != mediaBufferChunkSize {
		t.Fatalf("N=5 remainder=%d want %d", remainder, mediaBufferChunkSize)
	}
	closeMediaBufferRequests(t, requests...)
}

func TestMediaBufferLazyStickyAllocationAndPromotion(t *testing.T) {
	buffer := mustMediaBuffer(t, 2*mediaBufferChunkSize)
	request := buffer.register()
	first := requireAcceptedMediaBufferLease(t, request)
	second := requireAcceptedMediaBufferLease(t, request)
	if len(first.bytes()) != int(mediaBufferChunkSize) || len(second.bytes()) != int(mediaBufferChunkSize) {
		t.Fatal("optional lease byte size mismatch")
	}
	assertMediaBufferSnapshot(t, buffer, 2*mediaBufferChunkSize, 2*mediaBufferChunkSize, 0)

	blocked := requireMediaBufferRequest(t, request)
	requireNoMediaBufferNotification(t, blocked)
	if err := request.releaseOptional(first); err != nil {
		t.Fatal(err)
	}
	requireMediaBufferNotification(t, blocked)
	reused := requireMediaBufferAcceptance(t, request)
	if reused.chunk != first.chunk || reused.generation == first.generation {
		t.Fatal("free chunk was not reused with a new generation")
	}
	if err := request.releaseOptional(reused); err != nil {
		t.Fatal(err)
	}
	assertMediaBufferSnapshot(t, buffer, 2*mediaBufferChunkSize, mediaBufferChunkSize, mediaBufferChunkSize)
	closeMediaBufferRequests(t, request)
	assertMediaBufferSnapshot(t, buffer, 2*mediaBufferChunkSize, 0, 2*mediaBufferChunkSize)
}

func TestMediaBufferLeaseRejectsStaleAndInvalidOwnership(t *testing.T) {
	buffer := mustMediaBuffer(t, 2*mediaBufferChunkSize)
	firstRequest := buffer.register()
	firstLease := requireAcceptedMediaBufferLease(t, firstRequest)
	if err := firstRequest.releaseOptional(firstLease); err != nil {
		t.Fatal(err)
	}
	secondLease := requireAcceptedMediaBufferLease(t, firstRequest)
	if secondLease.chunk != firstLease.chunk || secondLease.generation == firstLease.generation {
		t.Fatal("same-request ABA setup failed")
	}
	if err := firstRequest.releaseOptional(firstLease); !errors.Is(err, errMediaBufferOwnership) {
		t.Fatalf("same-request stale release error=%v", err)
	}
	if err := firstRequest.releaseOptional(secondLease); err != nil {
		t.Fatal(err)
	}
	if err := firstRequest.releaseOptional(secondLease); !errors.Is(err, errMediaBufferOwnership) {
		t.Fatalf("duplicate current release error=%v", err)
	}

	secondRequest := buffer.register()
	thirdLease := requireAcceptedMediaBufferLease(t, secondRequest)
	if thirdLease.chunk != firstLease.chunk {
		t.Fatal("cross-request stale setup did not reuse chunk")
	}
	if err := firstRequest.releaseOptional(firstLease); !errors.Is(err, errMediaBufferOwnership) {
		t.Fatalf("cross-request stale release error=%v", err)
	}
	if err := secondRequest.close(); err != nil {
		t.Fatal(err)
	}
	if err := secondRequest.releaseOptional(thirdLease); !errors.Is(err, errMediaBufferClosed) {
		t.Fatalf("post-close stale release error=%v", err)
	}
	closeMediaBufferRequests(t, firstRequest)
}

func TestMediaBufferPendingGrantRequiresAcceptance(t *testing.T) {
	buffer := mustMediaBuffer(t, mediaBufferChunkSize)
	request := buffer.register()
	notify := requireMediaBufferRequest(t, request)
	requireMediaBufferNotification(t, notify)
	if got := request.snapshot(); !got.Pending || !got.Requesting || got.Owned != mediaBufferChunkSize {
		t.Fatalf("pending snapshot=%+v", got)
	}
	lease := requireMediaBufferAcceptance(t, request)
	if got := request.snapshot(); got.Pending || got.Requesting || got.Owned != mediaBufferChunkSize {
		t.Fatalf("accepted snapshot=%+v", got)
	}
	if lease.requestID != request.id || lease.generation == 0 || lease.chunk == nil {
		t.Fatalf("lease=%+v", lease)
	}
	closeMediaBufferRequests(t, request)
}

func TestMediaBufferCancellationBeforeGrantRemovesWaiter(t *testing.T) {
	buffer := mustMediaBuffer(t, 2*mediaBufferChunkSize)
	holder := buffer.register()
	holderLeases := []mediaBufferLease{
		requireAcceptedMediaBufferLease(t, holder),
		requireAcceptedMediaBufferLease(t, holder),
	}
	waiter := buffer.register()
	notify := requireMediaBufferRequest(t, waiter)
	requireNoMediaBufferNotification(t, notify)
	if err := waiter.cancelOptionalRequest(); err != nil {
		t.Fatal(err)
	}
	if err := holder.releaseOptional(holderLeases[0]); err != nil {
		t.Fatal(err)
	}
	requireNoMediaBufferNotification(t, notify)
	if got := waiter.snapshot(); got.Requesting || got.Pending || got.Owned != 0 {
		t.Fatalf("canceled waiter snapshot=%+v", got)
	}
	closeMediaBufferRequests(t, holder, waiter)
}

func TestMediaBufferCancellationReclaimsQueuedPendingGrant(t *testing.T) {
	buffer := mustMediaBuffer(t, mediaBufferChunkSize)
	request := buffer.register()
	notify := requireMediaBufferRequest(t, request)
	if err := request.cancelOptionalRequest(); err != nil {
		t.Fatal(err)
	}
	requireNoMediaBufferNotification(t, notify)
	if _, err := request.acceptOptional(); !errors.Is(err, errMediaBufferNoGrant) {
		t.Fatalf("accept after cancellation error=%v", err)
	}
	assertMediaBufferSnapshot(t, buffer, mediaBufferChunkSize, 0, mediaBufferChunkSize)
	closeMediaBufferRequests(t, request)
}

func TestMediaBufferCancellationRacingGrantIsReclaimed(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		buffer := mustMediaBuffer(t, 2*mediaBufferChunkSize)
		holder := buffer.register()
		holderLeases := []mediaBufferLease{
			requireAcceptedMediaBufferLease(t, holder),
			requireAcceptedMediaBufferLease(t, holder),
		}
		waiter := buffer.register()
		requireNoMediaBufferNotification(t, requireMediaBufferRequest(t, waiter))

		start := make(chan struct{})
		ready := make(chan struct{}, 2)
		errorsCh := make(chan error, 2)
		go func() {
			ready <- struct{}{}
			<-start
			errorsCh <- holder.releaseOptional(holderLeases[0])
		}()
		go func() {
			ready <- struct{}{}
			<-start
			errorsCh <- waiter.cancelOptionalRequest()
		}()
		<-ready
		<-ready
		close(start)
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
		if got := waiter.snapshot(); got.Requesting || got.Pending || got.Owned != 0 {
			t.Fatalf("iteration=%d waiter=%+v", iteration, got)
		}
		assertMediaBufferSnapshot(t, buffer, 2*mediaBufferChunkSize, mediaBufferChunkSize, mediaBufferChunkSize)
		closeMediaBufferRequests(t, holder, waiter)
	}
}

func TestMediaBufferCloseReclaimsPendingAndRejectsNewRequest(t *testing.T) {
	buffer := mustMediaBuffer(t, mediaBufferChunkSize)
	request := buffer.register()
	notify := requireMediaBufferRequest(t, request)
	if err := request.close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-notify; ok {
		t.Fatal("closed pending notification remained open")
	}
	assertMediaBufferSnapshot(t, buffer, mediaBufferChunkSize, 0, mediaBufferChunkSize)
	if _, err := request.requestOptional(); !errors.Is(err, errMediaBufferClosed) {
		t.Fatalf("request after close error=%v", err)
	}
	if _, err := request.acceptOptional(); !errors.Is(err, errMediaBufferClosed) {
		t.Fatalf("accept after close error=%v", err)
	}
	if err := request.cancelOptionalRequest(); !errors.Is(err, errMediaBufferClosed) {
		t.Fatalf("cancel after close error=%v", err)
	}
}

func TestMediaBufferRequesterFIFOUsesRequestOrder(t *testing.T) {
	buffer := mustMediaBuffer(t, 3*mediaBufferChunkSize)
	blocker := buffer.register()
	blockerLeases := []mediaBufferLease{
		requireAcceptedMediaBufferLease(t, blocker),
		requireAcceptedMediaBufferLease(t, blocker),
		requireAcceptedMediaBufferLease(t, blocker),
	}
	firstRegistered := buffer.register()
	secondRegistered := buffer.register()
	thirdRegistered := buffer.register()

	thirdNotify := requireMediaBufferRequest(t, thirdRegistered)
	firstNotify := requireMediaBufferRequest(t, firstRegistered)
	secondNotify := requireMediaBufferRequest(t, secondRegistered)
	requireNoMediaBufferNotification(t, thirdNotify)
	requireNoMediaBufferNotification(t, firstNotify)
	requireNoMediaBufferNotification(t, secondNotify)

	if err := blocker.close(); err != nil {
		t.Fatal(err)
	}
	thirdLease := requireNotifiedAcceptance(t, thirdRegistered, thirdNotify)
	firstLease := requireNotifiedAcceptance(t, firstRegistered, firstNotify)
	secondLease := requireNotifiedAcceptance(t, secondRegistered, secondNotify)
	if !(thirdLease.generation < firstLease.generation && firstLease.generation < secondLease.generation) {
		t.Fatalf("request-order generations third=%d first=%d second=%d", thirdLease.generation, firstLease.generation, secondLease.generation)
	}
	for _, lease := range blockerLeases {
		if err := blocker.releaseOptional(lease); !errors.Is(err, errMediaBufferClosed) {
			t.Fatalf("stale blocker lease error=%v", err)
		}
	}
	closeMediaBufferRequests(t, firstRegistered, secondRegistered, thirdRegistered)
}

func TestMediaBufferLocalDebtBlocksOnlyIndebtedWaiter(t *testing.T) {
	buffer := mustMediaBuffer(t, 4*mediaBufferChunkSize)
	incumbent := buffer.register()
	incumbentLeases := []mediaBufferLease{
		requireAcceptedMediaBufferLease(t, incumbent),
		requireAcceptedMediaBufferLease(t, incumbent),
		requireAcceptedMediaBufferLease(t, incumbent),
	}
	newcomer := buffer.register()
	if got := incumbent.snapshot(); got.Target != 2*mediaBufferChunkSize || got.Debt != mediaBufferChunkSize {
		t.Fatalf("incumbent snapshot=%+v", got)
	}
	incumbentNotify := requireMediaBufferRequest(t, incumbent)
	newcomerLease := requireAcceptedMediaBufferLease(t, newcomer)
	requireNoMediaBufferNotification(t, incumbentNotify)
	if err := incumbent.releaseOptional(incumbentLeases[0]); err != nil {
		t.Fatal(err)
	}
	if got := incumbent.snapshot(); got.Debt != 0 || !got.Requesting {
		t.Fatalf("incumbent after debt drain=%+v", got)
	}
	if err := newcomer.releaseOptional(newcomerLease); err != nil {
		t.Fatal(err)
	}
	requireNoMediaBufferNotification(t, incumbentNotify)
	closeMediaBufferRequests(t, incumbent, newcomer)
}

func TestMediaBufferConcurrentMembershipAndHandshakeAccounting(t *testing.T) {
	const workers = 16
	const iterations = 100
	buffer := mustMediaBuffer(t, workers*mediaBufferChunkSize)
	start := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			defer wg.Done()
			<-start
			request := buffer.register()
			defer func() {
				if err := request.close(); err != nil {
					t.Errorf("close: %v", err)
				}
			}()
			for iteration := 0; iteration < iterations; iteration++ {
				notify, err := request.requestOptional()
				if err != nil {
					t.Errorf("request: %v", err)
					return
				}
				if (worker+iteration)%4 == 0 {
					if err := request.cancelOptionalRequest(); err != nil {
						t.Errorf("cancel: %v", err)
						return
					}
					continue
				}
				if _, ok := <-notify; !ok {
					t.Error("notification closed")
					return
				}
				lease, err := request.acceptOptional()
				if err != nil {
					t.Errorf("accept: %v", err)
					return
				}
				if err := request.releaseOptional(lease); err != nil {
					t.Errorf("release: %v", err)
					return
				}
			}
		}(worker)
	}
	close(start)
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent membership and handshake deadlocked")
	}
	final := buffer.Snapshot()
	if final.ActiveRequests != 0 || final.Owned != 0 || final.Free != final.Allocated || final.Allocated > final.HardBudget {
		t.Fatalf("final snapshot=%+v", final)
	}
	buffer.assertInvariants()
}

func mustMediaBuffer(t *testing.T, budget int64) *mediaBuffer {
	t.Helper()
	buffer, err := newMediaBuffer(budget)
	if err != nil {
		t.Fatal(err)
	}
	return buffer
}

func requireMediaBufferRequest(t *testing.T, request *mediaBufferRequest) <-chan struct{} {
	t.Helper()
	notify, err := request.requestOptional()
	if err != nil {
		t.Fatal(err)
	}
	return notify
}

func requireAcceptedMediaBufferLease(t *testing.T, request *mediaBufferRequest) mediaBufferLease {
	t.Helper()
	notify := requireMediaBufferRequest(t, request)
	return requireNotifiedAcceptance(t, request, notify)
}

func requireNotifiedAcceptance(t *testing.T, request *mediaBufferRequest, notify <-chan struct{}) mediaBufferLease {
	t.Helper()
	requireMediaBufferNotification(t, notify)
	return requireMediaBufferAcceptance(t, request)
}

func requireMediaBufferAcceptance(t *testing.T, request *mediaBufferRequest) mediaBufferLease {
	t.Helper()
	lease, err := request.acceptOptional()
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func requireMediaBufferNotification(t *testing.T, notify <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-notify:
		if !ok {
			t.Fatal("notification channel closed")
		}
	default:
		t.Fatal("expected immediate media buffer notification")
	}
}

func requireNoMediaBufferNotification(t *testing.T, notify <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-notify:
		t.Fatalf("unexpected notification ok=%v", ok)
	default:
	}
}

func assertMediaBufferSnapshot(t *testing.T, buffer *mediaBuffer, allocated, owned, free int64) {
	t.Helper()
	buffer.assertInvariants()
	snapshot := buffer.Snapshot()
	if snapshot.Allocated != allocated || snapshot.Owned != owned || snapshot.Free != free || snapshot.Allocated != snapshot.Owned+snapshot.Free || snapshot.Allocated > snapshot.HardBudget {
		t.Fatalf("snapshot=%+v want allocated=%d owned=%d free=%d", snapshot, allocated, owned, free)
	}
}

func closeMediaBufferRequests(t *testing.T, requests ...*mediaBufferRequest) {
	t.Helper()
	for _, request := range requests {
		if err := request.close(); err != nil {
			t.Fatal(err)
		}
		request.buffer.assertInvariants()
	}
}
