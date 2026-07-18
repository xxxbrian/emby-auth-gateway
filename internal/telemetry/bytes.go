package telemetry

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// TransferMeta describes an in-flight proxy body transfer for the admin UI.
type TransferMeta struct {
	SessionID string
	UserID    string
	Username  string
	Device    string
	ItemID    string
	MediaMode string
	Method    string
}

// liveTransfer is one active request body copy.
type liveTransfer struct {
	id        uint64
	meta      TransferMeta
	startedAt time.Time
	ingress   atomic.Int64
	egress    atomic.Int64
	lastSeen  atomic.Int64 // unix nano
}

// TransferHandle is returned by BeginTransfer. Hot-path Add* methods use only
// atomics on the handle and meter — no map lock.
type TransferHandle struct {
	meter *ByteMeter
	tr    *liveTransfer
	ended atomic.Bool
}

// ByteMeter holds monotonic ingress/egress totals and active transfer handles.
type ByteMeter struct {
	ingress          atomic.Uint64
	egress           atomic.Uint64
	completedEgress  atomic.Uint64
	completedIngress atomic.Uint64
	errors           atomic.Uint64 // media/proxy body errors for sampler

	nextID atomic.Uint64

	mu        sync.Mutex
	transfers map[uint64]*liveTransfer
}

// NewByteMeter creates an empty live byte meter.
func NewByteMeter() *ByteMeter {
	return &ByteMeter{transfers: make(map[uint64]*liveTransfer)}
}

// AddEgress records n bytes successfully written to a client (n > 0 only).
func (m *ByteMeter) AddEgress(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.egress.Add(uint64(n))
}

// AddIngress records n bytes successfully read from upstream (n > 0 only).
func (m *ByteMeter) AddIngress(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.ingress.Add(uint64(n))
}

// NoteError increments the live error counter (sampled into traffic error rings).
func (m *ByteMeter) NoteError() {
	if m == nil {
		return
	}
	m.errors.Add(1)
}

// Totals returns monotonic cumulative ingress and egress byte counts.
func (m *ByteMeter) Totals() (ingress, egress uint64) {
	if m == nil {
		return 0, 0
	}
	return m.ingress.Load(), m.egress.Load()
}

// ErrorTotal returns monotonic proxy/media body error count.
func (m *ByteMeter) ErrorTotal() uint64 {
	if m == nil {
		return 0
	}
	return m.errors.Load()
}

// CompletedEgress returns bytes from transfers that have ended.
func (m *ByteMeter) CompletedEgress() uint64 {
	if m == nil {
		return 0
	}
	return m.completedEgress.Load()
}

// BeginTransfer registers an active transfer and returns a handle for atomics-only I/O.
func (m *ByteMeter) BeginTransfer(meta TransferMeta) *TransferHandle {
	if m == nil {
		return nil
	}
	id := m.nextID.Add(1)
	if id == 0 {
		id = m.nextID.Add(1)
	}
	now := time.Now().UTC()
	tr := &liveTransfer{
		id:        id,
		meta:      meta,
		startedAt: now,
	}
	tr.lastSeen.Store(now.UnixNano())
	m.mu.Lock()
	m.transfers[id] = tr
	m.mu.Unlock()
	return &TransferHandle{meter: m, tr: tr}
}

// AddEgress records successful client write bytes (atomics only).
func (h *TransferHandle) AddEgress(n int64) {
	if h == nil || h.meter == nil || h.tr == nil || n <= 0 || h.ended.Load() {
		return
	}
	h.meter.egress.Add(uint64(n))
	h.tr.egress.Add(n)
	h.tr.lastSeen.Store(time.Now().UnixNano())
}

// AddIngress records successful upstream read bytes (atomics only).
func (h *TransferHandle) AddIngress(n int64) {
	if h == nil || h.meter == nil || h.tr == nil || n <= 0 || h.ended.Load() {
		return
	}
	h.meter.ingress.Add(uint64(n))
	h.tr.ingress.Add(n)
	h.tr.lastSeen.Store(time.Now().UnixNano())
}

// ID returns the transfer id (0 if nil).
func (h *TransferHandle) ID() uint64 {
	if h == nil || h.tr == nil {
		return 0
	}
	return h.tr.id
}

// End removes the transfer from the active set. Idempotent.
func (h *TransferHandle) End(err error) {
	if h == nil || h.meter == nil || h.tr == nil {
		return
	}
	if !h.ended.CompareAndSwap(false, true) {
		return
	}
	if err != nil {
		h.meter.errors.Add(1)
	}
	id := h.tr.id
	h.meter.mu.Lock()
	tr := h.meter.transfers[id]
	delete(h.meter.transfers, id)
	h.meter.mu.Unlock()
	if tr == nil {
		return
	}
	if e := tr.egress.Load(); e > 0 {
		h.meter.completedEgress.Add(uint64(e))
	}
	if i := tr.ingress.Load(); i > 0 {
		h.meter.completedIngress.Add(uint64(i))
	}
}

// ActiveTransferCount returns the number of open transfer handles.
func (m *ByteMeter) ActiveTransferCount() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	n := len(m.transfers)
	m.mu.Unlock()
	return n
}

// ActiveTransfers returns a snapshot of in-flight transfers for the admin API.
func (m *ByteMeter) ActiveTransfers() []Transfer {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	out := make([]Transfer, 0, len(m.transfers))
	for _, tr := range m.transfers {
		last := tr.startedAt
		if ns := tr.lastSeen.Load(); ns > 0 {
			last = time.Unix(0, ns).UTC()
		}
		out = append(out, Transfer{
			SessionID: tr.meta.SessionID,
			UserID:    tr.meta.UserID,
			Username:  tr.meta.Username,
			Device:    tr.meta.Device,
			ItemID:    tr.meta.ItemID,
			MediaMode: tr.meta.MediaMode,
			BytesIn:   tr.ingress.Load(),
			BytesOut:  tr.egress.Load(),
			StartedAt: tr.startedAt,
			LastSeen:  last,
		})
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}
