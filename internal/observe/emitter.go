package observe

import (
	"sync"
	"sync/atomic"
	"time"
)

const defaultBuffer = 1024

// Emitter is a concrete non-blocking event bus.
// TryEmit never blocks; overflow increments Drops and returns false.
type Emitter struct {
	mu     sync.RWMutex
	ch     chan Event
	drops  atomic.Uint64
	closed bool
}

// NewEmitter creates an emitter with the given buffer size.
// A non-positive buffer defaults to 1024.
func NewEmitter(buffer int) *Emitter {
	if buffer <= 0 {
		buffer = defaultBuffer
	}
	return &Emitter{ch: make(chan Event, buffer)}
}

// TryEmit enqueues ev without blocking.
// Returns false if the emitter is nil, closed, or the buffer is full (drop).
func (e *Emitter) TryEmit(ev Event) bool {
	if e == nil {
		return false
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.closed {
		return false
	}
	select {
	case e.ch <- ev:
		return true
	default:
		e.drops.Add(1)
		return false
	}
}

// Events returns the receive-only event channel.
func (e *Emitter) Events() <-chan Event {
	if e == nil {
		return nil
	}
	return e.ch
}

// Close closes the event channel. It is idempotent and race-safe with TryEmit.
// After Close, TryEmit always returns false.
func (e *Emitter) Close() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	close(e.ch)
}

// Drops returns the number of events dropped due to a full buffer.
func (e *Emitter) Drops() uint64 {
	if e == nil {
		return 0
	}
	return e.drops.Load()
}
