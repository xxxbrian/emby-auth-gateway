package gateway

import (
	"errors"
	"fmt"
	"sync"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

const (
	mediaBufferChunkSize  int64 = 32 << 10
	mediaBufferRequestCap int64 = 512 << 20
)

var (
	errMediaBufferBudget    = errors.New("invalid media buffer budget")
	errMediaBufferClosed    = errors.New("media buffer request closed")
	errMediaBufferNoGrant   = errors.New("media buffer grant not pending")
	errMediaBufferOwnership = errors.New("invalid media buffer lease ownership")
)

type mediaBuffer struct {
	mu               sync.Mutex
	hardBudget       int64
	allocated        int64
	owned            int64
	free             []*mediaBufferChunk
	requests         []*mediaBufferRequest
	waiters          []*mediaBufferRequest
	nextRequestID    uint64
	nextChunkID      uint64
	nextGrant        uint64
	baseOnlyRequests int
	indebtedRequests int
	requestDebtBytes int64
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
	live    *mediaBufferLiveState
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
	b.addRequestCountersLocked(r)
	b.recomputeTargetsLocked()
	b.scheduleLocked()
	b.assertAccountingLocked()
	return r
}

func (b *mediaBuffer) Snapshot() mediaBufferSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	snapshot := mediaBufferSnapshot{
		Enabled:          true,
		HardBudget:       b.hardBudget,
		Allocated:        b.allocated,
		Owned:            b.owned,
		Free:             int64(len(b.free)) * mediaBufferChunkSize,
		ActiveRequests:   len(b.requests),
		BaseOnlyRequests: b.baseOnlyRequests,
		IndebtedRequests: b.indebtedRequests,
		RequestDebtBytes: b.requestDebtBytes,
	}
	return snapshot
}

func (r *mediaBufferRequest) attachLive(state *mediaBufferLiveState) {
	if r == nil || r.buffer == nil {
		return
	}
	b := r.buffer
	b.mu.Lock()
	if !r.closed {
		r.live = state
		b.projectRequestLocked(r)
	}
	b.mu.Unlock()
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
	b.projectRequestLocked(r)
	b.assertAccountingLocked()
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
	b.projectRequestLocked(r)
	b.assertAccountingLocked()
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
	b.projectRequestLocked(r)
	b.assertAccountingLocked()
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
	b.projectRequestLocked(r)
	b.assertAccountingLocked()
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
	b.removeRequestCountersLocked(r)
	index := b.requestIndexLocked(r)
	if index < 0 {
		panic("media buffer request missing during close")
	}
	b.requests = append(b.requests[:index], b.requests[index+1:]...)
	close(r.notify)
	b.recomputeTargetsLocked()
	b.scheduleLocked()
	b.assertAccountingLocked()
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
		b.removeRequestCountersLocked(r)
		r.target = target
		r.recomputeDebtLocked()
		b.addRequestCountersLocked(r)
		b.projectRequestLocked(r)
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
			b.projectRequestLocked(r)
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
		b.removeRequestCountersLocked(r)
		r.owned += mediaBufferChunkSize
		r.recomputeDebtLocked()
		b.addRequestCountersLocked(r)
		b.owned += mediaBufferChunkSize
		b.projectRequestLocked(r)
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
	b.removeRequestCountersLocked(r)
	r.owned -= mediaBufferChunkSize
	r.recomputeDebtLocked()
	b.addRequestCountersLocked(r)
	b.owned -= mediaBufferChunkSize
	b.free = append(b.free, chunk)
	b.projectRequestLocked(r)
}

func (b *mediaBuffer) addRequestCountersLocked(r *mediaBufferRequest) {
	if r.owned == 0 {
		b.baseOnlyRequests++
	}
	if r.debt > 0 {
		b.indebtedRequests++
		b.requestDebtBytes += r.debt
	}
}

func (b *mediaBuffer) removeRequestCountersLocked(r *mediaBufferRequest) {
	if r.owned == 0 {
		b.baseOnlyRequests--
	}
	if r.debt > 0 {
		b.indebtedRequests--
		b.requestDebtBytes -= r.debt
	}
}

func (b *mediaBuffer) projectRequestLocked(r *mediaBufferRequest) {
	if r == nil || r.live == nil {
		return
	}
	r.live.projectAllocation(r.target, r.owned, r.debt)
	r.live.projectBlocker(b.requestBlockerLocked(r))
}

func (b *mediaBuffer) requestBlockerLocked(r *mediaBufferRequest) telemetry.MediaBufferAllocationBlocker {
	if r == nil || !r.waiting || r.pending != nil || r.closed {
		return telemetry.MediaBufferBlockerNone
	}
	if r.debt > 0 {
		return telemetry.MediaBufferBlockerDebt
	}
	if r.owned+mediaBufferChunkSize > r.target {
		return telemetry.MediaBufferBlockerAtTarget
	}
	if len(b.free) == 0 && b.allocated+mediaBufferChunkSize > b.hardBudget {
		return telemetry.MediaBufferBlockerPoolExhausted
	}
	return telemetry.MediaBufferBlockerNone
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

func (b *mediaBuffer) assertAccountingLocked() {
	if int64(len(b.free)) > b.hardBudget/mediaBufferChunkSize {
		panic("invalid media buffer byte accounting")
	}
	freeBytes := int64(len(b.free)) * mediaBufferChunkSize
	if b.hardBudget < mediaBufferChunkSize || b.hardBudget%mediaBufferChunkSize != 0 ||
		b.allocated < 0 || b.allocated%mediaBufferChunkSize != 0 || b.allocated > b.hardBudget ||
		b.owned < 0 || b.owned%mediaBufferChunkSize != 0 || b.owned > b.allocated ||
		freeBytes < 0 || b.allocated != b.owned+freeBytes || len(b.waiters) > len(b.requests) {
		panic("invalid media buffer byte accounting")
	}
}
