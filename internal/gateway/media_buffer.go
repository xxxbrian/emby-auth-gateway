package gateway

import (
	"errors"
	"fmt"
	"sync"
)

const (
	mediaBufferChunkSize     int64 = 32 << 10
	mediaBufferRequestCap    int64 = 512 << 20
	mediaBufferAutoBudgetCap int64 = 2 << 30
)

var (
	errMediaBufferBudget      = errors.New("invalid media buffer budget")
	errMediaBufferNoCandidate = errors.New("no finite media buffer memory candidate")
	errMediaBufferClosed      = errors.New("media buffer request closed")
	errMediaBufferNoGrant     = errors.New("media buffer grant not pending")
	errMediaBufferOwnership   = errors.New("invalid media buffer lease ownership")
)

type mediaBuffer struct {
	mu            sync.Mutex
	hardBudget    int64
	allocated     int64
	owned         int64
	free          []*mediaBufferChunk
	requests      []*mediaBufferRequest
	waiters       []*mediaBufferRequest
	nextRequestID uint64
	nextChunkID   uint64
	nextGrant     uint64
}

// MediaBuffer is the opaque adaptive media buffering controller.
type MediaBuffer struct {
	controller *mediaBuffer
}

type mediaBufferChunk struct {
	id         uint64
	generation uint64
	ownerID    uint64
	data       []byte
}

type mediaBufferLease struct {
	chunk      *mediaBufferChunk
	requestID  uint64
	generation uint64
}

type mediaBufferRequest struct {
	buffer  *mediaBuffer
	id      uint64
	target  int64
	owned   int64
	debt    int64
	waiting bool
	closed  bool
	notify  chan struct{}
	pending *mediaBufferLease
	chunks  map[*mediaBufferChunk]uint64
}

type mediaBufferSnapshot struct {
	Enabled          bool
	HardBudget       int64
	Allocated        int64
	Owned            int64
	Free             int64
	ActiveRequests   int
	BaseOnlyRequests int
	IndebtedRequests int
	RequestDebtBytes int64
}

type mediaBufferRequestSnapshot struct {
	ID         uint64
	Target     int64
	Owned      int64
	Debt       int64
	Requesting bool
	Pending    bool
	Closed     bool
}

func newMediaBuffer(hardBudget int64) (*mediaBuffer, error) {
	if hardBudget < mediaBufferChunkSize || hardBudget%mediaBufferChunkSize != 0 {
		return nil, fmt.Errorf("%w: must be a positive multiple of %d bytes", errMediaBufferBudget, mediaBufferChunkSize)
	}
	return &mediaBuffer{hardBudget: hardBudget}, nil
}

// NewMediaBuffer constructs an adaptive media buffering controller with an
// aligned hard budget in bytes.
func NewMediaBuffer(hardBudget int64) (*MediaBuffer, error) {
	controller, err := newMediaBuffer(hardBudget)
	if err != nil {
		return nil, err
	}
	return &MediaBuffer{controller: controller}, nil
}

func configuredMediaBuffer(controller *MediaBuffer) *mediaBuffer {
	if controller == nil {
		return nil
	}
	return controller.controller
}

func alignMediaBufferSize(size int64) int64 {
	if size <= 0 {
		return 0
	}
	return size / mediaBufferChunkSize * mediaBufferChunkSize
}

func minimumPositiveMediaBufferCandidate(candidates ...int64) (int64, bool) {
	var minimum int64
	for _, candidate := range candidates {
		if candidate <= 0 || minimum != 0 && candidate >= minimum {
			continue
		}
		minimum = candidate
	}
	return minimum, minimum > 0
}

func automaticMediaBufferBudget(candidates ...int64) (int64, error) {
	limit, ok := minimumPositiveMediaBufferCandidate(candidates...)
	if !ok {
		return 0, errMediaBufferNoCandidate
	}
	budget := limit / 8
	if budget > mediaBufferAutoBudgetCap {
		budget = mediaBufferAutoBudgetCap
	}
	budget = alignMediaBufferSize(budget)
	if budget < mediaBufferChunkSize {
		return 0, fmt.Errorf("%w: automatic budget below one chunk", errMediaBufferBudget)
	}
	return budget, nil
}

func (b *mediaBuffer) register() *mediaBufferRequest {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextRequestID++
	r := &mediaBufferRequest{
		buffer: b,
		id:     b.nextRequestID,
		notify: make(chan struct{}, 1),
		chunks: make(map[*mediaBufferChunk]uint64),
	}
	b.requests = append(b.requests, r)
	b.recomputeTargetsLocked()
	b.scheduleLocked()
	b.assertInvariantsLocked()
	return r
}

func (b *mediaBuffer) Snapshot() mediaBufferSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	snapshot := mediaBufferSnapshot{
		Enabled:        true,
		HardBudget:     b.hardBudget,
		Allocated:      b.allocated,
		Owned:          b.owned,
		Free:           int64(len(b.free)) * mediaBufferChunkSize,
		ActiveRequests: len(b.requests),
	}
	for _, r := range b.requests {
		if r.owned == 0 {
			snapshot.BaseOnlyRequests++
		}
		if r.debt > 0 {
			snapshot.IndebtedRequests++
			snapshot.RequestDebtBytes += r.debt
		}
	}
	return snapshot
}

func (r *mediaBufferRequest) requestOptional() (<-chan struct{}, error) {
	b := r.buffer
	b.mu.Lock()
	defer b.mu.Unlock()

	if r.closed {
		return nil, errMediaBufferClosed
	}
	if r.pending != nil || r.waiting {
		return r.notify, nil
	}
	r.waiting = true
	b.waiters = append(b.waiters, r)
	b.scheduleLocked()
	b.assertInvariantsLocked()
	return r.notify, nil
}

func (r *mediaBufferRequest) acceptOptional() (mediaBufferLease, error) {
	b := r.buffer
	b.mu.Lock()
	defer b.mu.Unlock()

	if r.closed {
		return mediaBufferLease{}, errMediaBufferClosed
	}
	if r.pending == nil {
		return mediaBufferLease{}, errMediaBufferNoGrant
	}
	lease := *r.pending
	r.pending = nil
	drainMediaBufferNotification(r.notify)
	r.chunks[lease.chunk] = lease.generation
	b.assertInvariantsLocked()
	return lease, nil
}

func (r *mediaBufferRequest) cancelOptionalRequest() error {
	b := r.buffer
	b.mu.Lock()
	defer b.mu.Unlock()

	if r.closed {
		return errMediaBufferClosed
	}
	if r.waiting {
		b.removeWaiterLocked(r)
	}
	if r.pending != nil {
		b.reclaimPendingLocked(r)
	} else {
		drainMediaBufferNotification(r.notify)
	}
	b.scheduleLocked()
	b.assertInvariantsLocked()
	return nil
}

func (r *mediaBufferRequest) releaseOptional(lease mediaBufferLease) error {
	b := r.buffer
	b.mu.Lock()
	defer b.mu.Unlock()

	if r.closed {
		return errMediaBufferClosed
	}
	if !b.validAcceptedLeaseLocked(r, lease) {
		return errMediaBufferOwnership
	}
	b.releaseAcceptedLocked(r, lease.chunk)
	b.scheduleLocked()
	b.assertInvariantsLocked()
	return nil
}

func (r *mediaBufferRequest) close() error {
	b := r.buffer
	b.mu.Lock()
	defer b.mu.Unlock()

	if r.closed {
		return nil
	}
	r.closed = true
	if r.waiting {
		b.removeWaiterLocked(r)
	}
	if r.pending != nil {
		b.reclaimPendingLocked(r)
	} else {
		drainMediaBufferNotification(r.notify)
	}
	for chunk := range r.chunks {
		b.releaseAcceptedLocked(r, chunk)
	}
	index := b.requestIndexLocked(r)
	if index < 0 {
		panic("media buffer request missing during close")
	}
	b.requests = append(b.requests[:index], b.requests[index+1:]...)
	close(r.notify)
	b.recomputeTargetsLocked()
	b.scheduleLocked()
	b.assertInvariantsLocked()
	return nil
}

func (r *mediaBufferRequest) snapshot() mediaBufferRequestSnapshot {
	b := r.buffer
	b.mu.Lock()
	defer b.mu.Unlock()
	return mediaBufferRequestSnapshot{
		ID:         r.id,
		Target:     r.target,
		Owned:      r.owned,
		Debt:       r.debt,
		Requesting: r.waiting || r.pending != nil,
		Pending:    r.pending != nil,
		Closed:     r.closed,
	}
}

func (l mediaBufferLease) bytes() []byte {
	if l.chunk == nil {
		return nil
	}
	return l.chunk.data
}

func (b *mediaBuffer) recomputeTargetsLocked() {
	if len(b.requests) == 0 {
		return
	}
	target := alignMediaBufferSize(b.hardBudget / int64(len(b.requests)))
	if target > mediaBufferRequestCap {
		target = mediaBufferRequestCap
	}
	for _, r := range b.requests {
		r.target = target
		r.recomputeDebtLocked()
	}
}

func (r *mediaBufferRequest) recomputeDebtLocked() {
	r.debt = r.owned - r.target
	if r.debt < 0 {
		r.debt = 0
	}
}

func (b *mediaBuffer) scheduleLocked() {
	population := b.waiters
	b.waiters = population[:0]
	for _, r := range population {
		if !b.canGrantLocked(r) {
			b.waiters = append(b.waiters, r)
			continue
		}
		chunk := b.takeChunkLocked()
		if chunk == nil {
			b.waiters = append(b.waiters, r)
			continue
		}
		r.waiting = false
		b.nextGrant++
		chunk.generation = b.nextGrant
		chunk.ownerID = r.id
		lease := mediaBufferLease{chunk: chunk, requestID: r.id, generation: chunk.generation}
		r.pending = &lease
		r.owned += mediaBufferChunkSize
		r.recomputeDebtLocked()
		b.owned += mediaBufferChunkSize
		select {
		case r.notify <- struct{}{}:
		default:
			panic("media buffer pending notification slot occupied")
		}
	}
	for index := len(b.waiters); index < len(population); index++ {
		population[index] = nil
	}
}

func (b *mediaBuffer) canGrantLocked(r *mediaBufferRequest) bool {
	if r.closed || !r.waiting || r.pending != nil || r.debt != 0 {
		return false
	}
	if r.owned+mediaBufferChunkSize > r.target {
		return false
	}
	return len(b.free) > 0 || b.allocated+mediaBufferChunkSize <= b.hardBudget
}

func (b *mediaBuffer) takeChunkLocked() *mediaBufferChunk {
	if count := len(b.free); count > 0 {
		chunk := b.free[count-1]
		b.free[count-1] = nil
		b.free = b.free[:count-1]
		return chunk
	}
	if b.allocated+mediaBufferChunkSize > b.hardBudget {
		return nil
	}
	b.nextChunkID++
	chunk := &mediaBufferChunk{id: b.nextChunkID, data: make([]byte, mediaBufferChunkSize)}
	b.allocated += mediaBufferChunkSize
	return chunk
}

func (b *mediaBuffer) validAcceptedLeaseLocked(r *mediaBufferRequest, lease mediaBufferLease) bool {
	if lease.chunk == nil || lease.requestID != r.id || lease.chunk.ownerID != r.id || lease.chunk.generation != lease.generation {
		return false
	}
	generation, ok := r.chunks[lease.chunk]
	return ok && generation == lease.generation
}

func (b *mediaBuffer) reclaimPendingLocked(r *mediaBufferRequest) {
	lease := *r.pending
	if lease.requestID != r.id || lease.chunk == nil || lease.chunk.ownerID != r.id || lease.chunk.generation != lease.generation {
		panic("invalid media buffer pending lease")
	}
	r.pending = nil
	drainMediaBufferNotification(r.notify)
	b.releaseOwnedChunkLocked(r, lease.chunk)
}

func (b *mediaBuffer) releaseAcceptedLocked(r *mediaBufferRequest, chunk *mediaBufferChunk) {
	delete(r.chunks, chunk)
	b.releaseOwnedChunkLocked(r, chunk)
}

func (b *mediaBuffer) releaseOwnedChunkLocked(r *mediaBufferRequest, chunk *mediaBufferChunk) {
	chunk.ownerID = 0
	r.owned -= mediaBufferChunkSize
	r.recomputeDebtLocked()
	b.owned -= mediaBufferChunkSize
	b.free = append(b.free, chunk)
}

func (b *mediaBuffer) removeWaiterLocked(request *mediaBufferRequest) {
	for index, waiter := range b.waiters {
		if waiter != request {
			continue
		}
		copy(b.waiters[index:], b.waiters[index+1:])
		b.waiters[len(b.waiters)-1] = nil
		b.waiters = b.waiters[:len(b.waiters)-1]
		request.waiting = false
		return
	}
	panic("media buffer waiter missing")
}

func (b *mediaBuffer) requestIndexLocked(request *mediaBufferRequest) int {
	for index, candidate := range b.requests {
		if candidate == request {
			return index
		}
	}
	return -1
}

func drainMediaBufferNotification(notify chan struct{}) {
	select {
	case <-notify:
	default:
	}
}

func (b *mediaBuffer) assertInvariants() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.assertInvariantsLocked()
}

func (b *mediaBuffer) assertAccountingLocked() {
	freeBytes := int64(len(b.free)) * mediaBufferChunkSize
	if b.allocated < 0 || b.owned < 0 || b.owned > b.allocated || b.allocated > b.hardBudget || b.allocated != b.owned+freeBytes {
		panic("invalid media buffer byte accounting")
	}
}

func (b *mediaBuffer) assertInvariantsLocked() {
	b.assertAccountingLocked()
	if b.hardBudget < mediaBufferChunkSize || b.hardBudget%mediaBufferChunkSize != 0 || b.allocated%mediaBufferChunkSize != 0 || b.owned%mediaBufferChunkSize != 0 {
		panic("invalid media buffer byte accounting")
	}

	seenChunks := make(map[*mediaBufferChunk]string, b.allocated/mediaBufferChunkSize)
	seenGenerations := make(map[uint64]struct{}, b.owned/mediaBufferChunkSize)
	for _, chunk := range b.free {
		if chunk == nil || len(chunk.data) != int(mediaBufferChunkSize) || chunk.ownerID != 0 {
			panic("invalid free media buffer chunk")
		}
		if _, exists := seenChunks[chunk]; exists {
			panic("duplicate free media buffer chunk")
		}
		seenChunks[chunk] = "free"
	}

	requestSet := make(map[*mediaBufferRequest]struct{}, len(b.requests))
	requestIDs := make(map[uint64]struct{}, len(b.requests))
	expectedTarget := int64(0)
	if len(b.requests) > 0 {
		expectedTarget = alignMediaBufferSize(b.hardBudget / int64(len(b.requests)))
		if expectedTarget > mediaBufferRequestCap {
			expectedTarget = mediaBufferRequestCap
		}
	}
	var requestOwned int64
	for _, r := range b.requests {
		if r == nil || r.buffer != b || r.closed {
			panic("invalid registered media buffer request")
		}
		if _, exists := requestSet[r]; exists {
			panic("duplicate registered media buffer request")
		}
		if _, exists := requestIDs[r.id]; exists || r.id == 0 {
			panic("duplicate media buffer request id")
		}
		requestSet[r] = struct{}{}
		requestIDs[r.id] = struct{}{}
		expectedDebt := r.owned - r.target
		if expectedDebt < 0 {
			expectedDebt = 0
		}
		if r.target != expectedTarget || r.debt != expectedDebt || r.owned < 0 || r.owned%mediaBufferChunkSize != 0 {
			panic("invalid media buffer request accounting")
		}
		expectedOwned := int64(len(r.chunks)) * mediaBufferChunkSize
		if r.pending != nil {
			expectedOwned += mediaBufferChunkSize
		}
		if r.owned != expectedOwned {
			panic("media buffer request ownership map mismatch")
		}
		for chunk, generation := range r.chunks {
			if chunk == nil || generation == 0 || chunk.ownerID != r.id || chunk.generation != generation || len(chunk.data) != int(mediaBufferChunkSize) {
				panic("invalid accepted media buffer lease")
			}
			if _, exists := seenChunks[chunk]; exists {
				panic("media buffer chunk has multiple owners")
			}
			if _, exists := seenGenerations[generation]; exists {
				panic("duplicate active media buffer generation")
			}
			seenChunks[chunk] = "accepted"
			seenGenerations[generation] = struct{}{}
		}
		if r.pending != nil {
			lease := r.pending
			if lease.chunk == nil || lease.requestID != r.id || lease.generation == 0 || lease.chunk.ownerID != r.id || lease.chunk.generation != lease.generation || r.waiting {
				panic("invalid pending media buffer lease")
			}
			if _, exists := seenChunks[lease.chunk]; exists {
				panic("pending media buffer chunk has multiple owners")
			}
			if _, exists := seenGenerations[lease.generation]; exists {
				panic("duplicate active media buffer generation")
			}
			seenChunks[lease.chunk] = "pending"
			seenGenerations[lease.generation] = struct{}{}
		} else if len(r.notify) != 0 {
			panic("media buffer notification without pending lease")
		}
		requestOwned += r.owned
	}
	if requestOwned != b.owned || int64(len(seenChunks))*mediaBufferChunkSize != b.allocated {
		panic("media buffer aggregate ownership mismatch")
	}

	waiterSet := make(map[*mediaBufferRequest]struct{}, len(b.waiters))
	for _, r := range b.waiters {
		if _, registered := requestSet[r]; !registered || r.closed || !r.waiting || r.pending != nil {
			panic("invalid media buffer waiter")
		}
		if _, exists := waiterSet[r]; exists {
			panic("duplicate media buffer waiter")
		}
		waiterSet[r] = struct{}{}
	}
	for _, r := range b.requests {
		_, queued := waiterSet[r]
		if queued != r.waiting {
			panic("media buffer waiter state mismatch")
		}
	}
}
