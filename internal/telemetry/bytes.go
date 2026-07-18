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
}

// ByteMeter holds monotonic ingress/egress totals and active transfer handles.
// Hot path Add* methods use only atomics (no map lock on the common path after
// the handle pointer is obtained). Begin/End take a mutex.
type ByteMeter struct {
	ingress          atomic.Uint64
	egress           atomic.Uint64
	completedEgress  atomic.Uint64
	completedIngress atomic.Uint64

	nextID atomic.Uint64

	mu        sync.RWMutex
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

// Totals returns monotonic cumulative ingress and egress byte counts.
func (m *ByteMeter) Totals() (ingress, egress uint64) {
	if m == nil {
		return 0, 0
	}
	return m.ingress.Load(), m.egress.Load()
}

// CompletedEgress returns bytes from transfers that have ended (for diagnostics).
func (m *ByteMeter) CompletedEgress() uint64 {
	if m == nil {
		return 0
	}
	return m.completedEgress.Load()
}

// BeginTransfer registers an active transfer and returns a unique id.
// id 0 is reserved as "no transfer".
func (m *ByteMeter) BeginTransfer(meta TransferMeta) uint64 {
	if m == nil {
		return 0
	}
	id := m.nextID.Add(1)
	if id == 0 {
		id = m.nextID.Add(1)
	}
	tr := &liveTransfer{
		id:        id,
		meta:      meta,
		startedAt: time.Now().UTC(),
	}
	m.mu.Lock()
	m.transfers[id] = tr
	m.mu.Unlock()
	return id
}

// AddTransferEgress adds to both the transfer handle and global egress.
func (m *ByteMeter) AddTransferEgress(id uint64, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.egress.Add(uint64(n))
	if id == 0 {
		return
	}
	m.mu.RLock()
	tr := m.transfers[id]
	m.mu.RUnlock()
	if tr != nil {
		tr.egress.Add(n)
	}
}

// AddTransferIngress adds to both the transfer handle and global ingress.
func (m *ByteMeter) AddTransferIngress(id uint64, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.ingress.Add(uint64(n))
	if id == 0 {
		return
	}
	m.mu.RLock()
	tr := m.transfers[id]
	m.mu.RUnlock()
	if tr != nil {
		tr.ingress.Add(n)
	}
}

// EndTransfer removes an active transfer. err is reserved for future outcome tagging.
func (m *ByteMeter) EndTransfer(id uint64, err error) {
	if m == nil || id == 0 {
		return
	}
	_ = err
	m.mu.Lock()
	tr := m.transfers[id]
	delete(m.transfers, id)
	m.mu.Unlock()
	if tr == nil {
		return
	}
	if e := tr.egress.Load(); e > 0 {
		m.completedEgress.Add(uint64(e))
	}
	if i := tr.ingress.Load(); i > 0 {
		m.completedIngress.Add(uint64(i))
	}
}

// ActiveTransferCount returns the number of open transfer handles.
func (m *ByteMeter) ActiveTransferCount() int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	n := len(m.transfers)
	m.mu.RUnlock()
	return n
}

// ActiveTransfers returns a snapshot of in-flight transfers for the admin API.
func (m *ByteMeter) ActiveTransfers() []Transfer {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	out := make([]Transfer, 0, len(m.transfers))
	for _, tr := range m.transfers {
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
			LastSeen:  time.Now().UTC(),
		})
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}
