package telemetry

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MediaBufferLiveCapacity is the accepted per-boot observation bound.
	MediaBufferLiveCapacity       = 4096
	mediaBufferCompletionCapacity = 256
)

type MediaBufferLifecycle uint8

const (
	MediaBufferLifecycleStarting MediaBufferLifecycle = iota
	MediaBufferLifecycleActive
	MediaBufferLifecycleClosing
)

type MediaBufferProducerState uint8

const (
	MediaBufferProducerIdle MediaBufferProducerState = iota
	MediaBufferProducerReadingBase
	MediaBufferProducerReadingOptional
	MediaBufferProducerWaitingForBuffer
	MediaBufferProducerDone
)

type MediaBufferConsumerState uint8

const (
	MediaBufferConsumerIdle MediaBufferConsumerState = iota
	MediaBufferConsumerWaitingForData
	MediaBufferConsumerWriting
	MediaBufferConsumerDone
)

type MediaBufferAllocationBlocker uint8

const (
	MediaBufferBlockerNone MediaBufferAllocationBlocker = iota
	MediaBufferBlockerPoolExhausted
	MediaBufferBlockerAtTarget
	MediaBufferBlockerDebt
)

type MediaBufferTimedValue struct {
	Value        uint8
	TransitionMS int64
}

type MediaBufferWaitStat struct {
	TotalMS int64
	MaxMS   int64
}

type MediaBufferLiveSnapshot struct {
	BootID     string
	StreamID   uint64
	TransferID uint64
	UserID     string
	Username   string
	Device     string
	ItemID     string
	MediaMode  string
	StartedAt  time.Time
	AgeMS      int64

	Lifecycle MediaBufferTimedValue
	Producer  MediaBufferTimedValue
	Consumer  MediaBufferTimedValue
	Blocker   MediaBufferTimedValue

	TargetBytes      int64
	OwnedBytes       int64
	DebtBytes        int64
	PrivateBaseBytes int64
	QueuedBytes      int64
	WritingBytes     int64

	PeakOwnedBytes   int64
	PeakDebtBytes    int64
	PeakQueuedBytes  int64
	PeakWritingBytes int64
	Waits            [6]MediaBufferWaitStat
	Terminal         bool
}

// MediaBufferLiveState is read only and is never called from a media hot path.
type MediaBufferLiveState interface {
	MediaBufferRawStreamID() uint64
	MediaBufferTerminal() bool
	MediaBufferLiveSnapshot() MediaBufferLiveSnapshot
}

type MediaBufferLivePage struct {
	Items      []MediaBufferLiveState
	NextCursor uint64
	HasMore    bool
}

type MediaBufferObservationCompleteness uint8

const (
	MediaBufferObservationComplete MediaBufferObservationCompleteness = iota
	MediaBufferObservationLimited
	MediaBufferObservationUnavailable
)

type MediaBufferLiveAggregate struct {
	ObservedActiveRequests   int
	UnobservedActiveRequests int
	QueuedBytes              int64
	WritingBytes             int64
	LiveRegistrationDrops    uint64
	CompletionDrops          uint64
	Completeness             MediaBufferObservationCompleteness
}

type MediaBufferCompletion struct {
	Terminal          MediaBufferLiveSnapshot
	Outcome           string
	InvariantObserved bool
	BytesRead         int64
	BytesWritten      int64
}

// MediaBufferLiveRegistry owns bounded live membership only. History and recent
// retention are intentionally outside Phase 1.
type MediaBufferLiveRegistry struct {
	bootID string

	mu    sync.RWMutex
	slots []MediaBufferLiveState

	registrationDrops atomic.Uint64
	completionDrops   atomic.Uint64
	completions       chan MediaBufferCompletion
}

func newMediaBufferLiveRegistry(bootID string) *MediaBufferLiveRegistry {
	return &MediaBufferLiveRegistry{
		bootID:      bootID,
		slots:       make([]MediaBufferLiveState, 0, MediaBufferLiveCapacity),
		completions: make(chan MediaBufferCompletion, mediaBufferCompletionCapacity),
	}
}

func (r *MediaBufferLiveRegistry) BootID() string {
	if r == nil {
		return ""
	}
	return r.bootID
}

// Register appends one monotonic slot after the strict fixed-cap guard.
func (r *MediaBufferLiveRegistry) Register(state MediaBufferLiveState) bool {
	if r == nil || state == nil || state.MediaBufferRawStreamID() == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.slots) >= MediaBufferLiveCapacity {
		r.registrationDrops.Add(1)
		return false
	}
	id := state.MediaBufferRawStreamID()
	if n := len(r.slots); n > 0 && r.slots[n-1].MediaBufferRawStreamID() >= id {
		r.registrationDrops.Add(1)
		return false
	}
	r.slots = append(r.slots, state)
	return true
}

func (r *MediaBufferLiveRegistry) RegistrationDrops() uint64 {
	if r == nil {
		return 0
	}
	return r.registrationDrops.Load()
}

func (r *MediaBufferLiveRegistry) CompletionDrops() uint64 {
	if r == nil {
		return 0
	}
	return r.completionDrops.Load()
}

func (r *MediaBufferLiveRegistry) OfferCompletion(summary MediaBufferCompletion) bool {
	if r == nil {
		return false
	}
	select {
	case r.completions <- summary:
		return true
	default:
		r.completionDrops.Add(1)
		return false
	}
}

func (r *MediaBufferLiveRegistry) TryCompletion() (MediaBufferCompletion, bool) {
	if r == nil {
		return MediaBufferCompletion{}, false
	}
	select {
	case summary := <-r.completions:
		return summary, true
	default:
		return MediaBufferCompletion{}, false
	}
}

// Page examines at most limit raw slots plus one non-consuming lookahead.
func (r *MediaBufferLiveRegistry) Page(cursor uint64, limit int) MediaBufferLivePage {
	if r == nil || limit <= 0 {
		return MediaBufferLivePage{}
	}
	if limit > 200 {
		limit = 200
	}
	pointers := make([]MediaBufferLiveState, limit)
	r.mu.RLock()
	start := sort.Search(len(r.slots), func(i int) bool {
		return r.slots[i].MediaBufferRawStreamID() > cursor
	})
	end := start + limit
	if end > len(r.slots) {
		end = len(r.slots)
	}
	pointers = pointers[:copy(pointers, r.slots[start:end])]
	hasMore := end < len(r.slots)
	r.mu.RUnlock()

	items := pointers[:0]
	for _, state := range pointers {
		if !state.MediaBufferTerminal() {
			items = append(items, state)
		}
	}
	next := cursor
	if len(pointers) > 0 {
		next = pointers[len(pointers)-1].MediaBufferRawStreamID()
	}
	return MediaBufferLivePage{Items: items, NextCursor: next, HasMore: hasMore}
}

func (r *MediaBufferLiveRegistry) Detail(streamID uint64) (MediaBufferLiveState, bool) {
	if r == nil || streamID == 0 {
		return nil, false
	}
	r.mu.RLock()
	index := sort.Search(len(r.slots), func(i int) bool {
		return r.slots[i].MediaBufferRawStreamID() >= streamID
	})
	var state MediaBufferLiveState
	if index < len(r.slots) && r.slots[index].MediaBufferRawStreamID() == streamID {
		state = r.slots[index]
	}
	r.mu.RUnlock()
	if state == nil || state.MediaBufferTerminal() {
		return nil, false
	}
	return state, true
}

// CompactTerminal performs one stable bounded pass and reuses slot backing.
func (r *MediaBufferLiveRegistry) CompactTerminal() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	write := 0
	removed := 0
	for read, state := range r.slots {
		if read >= MediaBufferLiveCapacity {
			break
		}
		if state.MediaBufferTerminal() {
			removed++
			continue
		}
		r.slots[write] = state
		write++
	}
	for i := write; i < len(r.slots); i++ {
		r.slots[i] = nil
	}
	r.slots = r.slots[:write]
	return removed
}

// Aggregate selects the first at most activeN nonterminal rows in raw-ID order.
func (r *MediaBufferLiveRegistry) Aggregate(activeN uint32) MediaBufferLiveAggregate {
	if r == nil {
		return MediaBufferLiveAggregate{Completeness: MediaBufferObservationUnavailable}
	}
	result := MediaBufferLiveAggregate{
		LiveRegistrationDrops: r.registrationDrops.Load(),
		CompletionDrops:       r.completionDrops.Load(),
		Completeness:          MediaBufferObservationComplete,
	}
	r.mu.RLock()
	for _, state := range r.slots {
		if result.ObservedActiveRequests >= int(activeN) {
			break
		}
		snapshot := state.MediaBufferLiveSnapshot()
		if snapshot.Terminal {
			continue
		}
		result.ObservedActiveRequests++
		result.QueuedBytes += snapshot.QueuedBytes
		result.WritingBytes += snapshot.WritingBytes
	}
	r.mu.RUnlock()
	result.UnobservedActiveRequests = int(activeN) - result.ObservedActiveRequests
	if result.UnobservedActiveRequests > 0 {
		result.Completeness = MediaBufferObservationLimited
	}
	return result
}
