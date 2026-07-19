package gateway

import (
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

const (
	mediaBufferPackedChunkBits = 20
	mediaBufferPackedChunkMask = (1 << mediaBufferPackedChunkBits) - 1
	mediaBufferPackedTimeBits  = 8
)

const (
	mediaBufferWaitAcquire = iota
	mediaBufferWaitPool
	mediaBufferWaitConsumer
	mediaBufferWaitUpstream
	mediaBufferWaitDownstream
	mediaBufferWaitClose
	mediaBufferWaitCount
)

type mediaBufferClock struct {
	origin time.Time
	fakeMS *atomic.Int64
	reads  atomic.Uint64
}

func newMediaBufferClock() *mediaBufferClock {
	return &mediaBufferClock{origin: time.Now().UTC()}
}

func newFakeMediaBufferClock(origin time.Time, initialMS int64) *mediaBufferClock {
	value := &atomic.Int64{}
	value.Store(initialMS)
	return &mediaBufferClock{origin: origin.UTC(), fakeMS: value}
}

func (c *mediaBufferClock) nowMS() int64 {
	if c == nil {
		return 0
	}
	if c.fakeMS != nil {
		c.reads.Add(1)
		return c.fakeMS.Load()
	}
	ms := time.Since(c.origin).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

func (c *mediaBufferClock) advance(delta time.Duration) {
	if c != nil && c.fakeMS != nil {
		c.fakeMS.Add(delta.Milliseconds())
	}
}

type mediaBufferLiveIdentity struct {
	BootID    string
	StreamID  uint64
	UserID    string
	Username  string
	Device    string
	ItemID    string
	MediaMode string
}

type mediaBufferLiveState struct {
	identity  mediaBufferLiveIdentity
	clock     *mediaBufferClock
	startedAt time.Time

	transferID atomic.Uint64
	allocation atomic.Uint64
	blocker    atomic.Uint64
	lifecycle  atomic.Uint64
	producer   atomic.Uint64
	consumer   atomic.Uint64

	queued  atomic.Int64
	writing atomic.Int64

	peakOwned   atomic.Int64
	peakDebt    atomic.Int64
	peakQueued  atomic.Int64
	peakWriting atomic.Int64
	waitTotal   [mediaBufferWaitCount]atomic.Int64
	waitMax     [mediaBufferWaitCount]atomic.Int64
	poolWait    atomic.Uint64

	terminal atomic.Bool
}

func newMediaBufferLiveState(identity mediaBufferLiveIdentity, clock *mediaBufferClock) *mediaBufferLiveState {
	if clock == nil {
		clock = newMediaBufferClock()
	}
	identity.BootID = sanitizeMediaBufferIdentity(identity.BootID)
	identity.UserID = sanitizeMediaBufferIdentity(identity.UserID)
	identity.Username = sanitizeMediaBufferIdentity(identity.Username)
	identity.Device = sanitizeMediaBufferIdentity(identity.Device)
	identity.ItemID = sanitizeMediaBufferIdentity(identity.ItemID)
	identity.MediaMode = sanitizeMediaBufferIdentity(identity.MediaMode)
	now := clock.nowMS()
	state := &mediaBufferLiveState{identity: identity, clock: clock, startedAt: clock.origin}
	state.lifecycle.Store(packMediaBufferTimed(uint8(telemetry.MediaBufferLifecycleStarting), now))
	state.producer.Store(packMediaBufferTimed(uint8(telemetry.MediaBufferProducerIdle), now))
	state.consumer.Store(packMediaBufferTimed(uint8(telemetry.MediaBufferConsumerIdle), now))
	state.blocker.Store(packMediaBufferTimed(uint8(telemetry.MediaBufferBlockerNone), now))
	return state
}

func sanitizeMediaBufferIdentity(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var builder strings.Builder
	if len(value) < 256 {
		builder.Grow(len(value))
	} else {
		builder.Grow(256)
	}
	space := false
	for _, r := range value {
		if unicode.IsSpace(r) {
			space = builder.Len() > 0
			continue
		}
		if r == 0 || unicode.IsControl(r) {
			continue
		}
		if space {
			if builder.Len()+1 > 256 {
				break
			}
			builder.WriteByte(' ')
			space = false
		}
		size := utf8.RuneLen(r)
		if size < 0 || builder.Len()+size > 256 {
			break
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func packMediaBufferAllocation(targetBytes, ownedBytes, debtBytes int64) uint64 {
	target := uint64(targetBytes / mediaBufferChunkSize)
	owned := uint64(ownedBytes / mediaBufferChunkSize)
	debt := uint64(debtBytes / mediaBufferChunkSize)
	if target > mediaBufferPackedChunkMask {
		target = mediaBufferPackedChunkMask
	}
	if owned > mediaBufferPackedChunkMask {
		owned = mediaBufferPackedChunkMask
	}
	if debt > mediaBufferPackedChunkMask {
		debt = mediaBufferPackedChunkMask
	}
	return target | owned<<mediaBufferPackedChunkBits | debt<<(2*mediaBufferPackedChunkBits)
}

func unpackMediaBufferAllocation(word uint64) (targetBytes, ownedBytes, debtBytes int64) {
	target := word & mediaBufferPackedChunkMask
	owned := (word >> mediaBufferPackedChunkBits) & mediaBufferPackedChunkMask
	debt := (word >> (2 * mediaBufferPackedChunkBits)) & mediaBufferPackedChunkMask
	return int64(target) * mediaBufferChunkSize, int64(owned) * mediaBufferChunkSize, int64(debt) * mediaBufferChunkSize
}

func packMediaBufferTimed(value uint8, ms int64) uint64 {
	if ms < 0 {
		ms = 0
	}
	return uint64(ms)<<mediaBufferPackedTimeBits | uint64(value)
}

func unpackMediaBufferTimed(word uint64) telemetry.MediaBufferTimedValue {
	return telemetry.MediaBufferTimedValue{Value: uint8(word), TransitionMS: int64(word >> mediaBufferPackedTimeBits)}
}

func (s *mediaBufferLiveState) MediaBufferRawStreamID() uint64 {
	if s == nil {
		return 0
	}
	return s.identity.StreamID
}

func (s *mediaBufferLiveState) MediaBufferTerminal() bool {
	return s == nil || s.terminal.Load()
}

func (s *mediaBufferLiveState) MediaBufferLiveSnapshot() telemetry.MediaBufferLiveSnapshot {
	if s == nil {
		return telemetry.MediaBufferLiveSnapshot{}
	}
	target, owned, debt := unpackMediaBufferAllocation(s.allocation.Load())
	age := s.clock.nowMS()
	snapshot := telemetry.MediaBufferLiveSnapshot{
		BootID:           s.identity.BootID,
		StreamID:         s.identity.StreamID,
		TransferID:       s.transferID.Load(),
		UserID:           s.identity.UserID,
		Username:         s.identity.Username,
		Device:           s.identity.Device,
		ItemID:           s.identity.ItemID,
		MediaMode:        s.identity.MediaMode,
		StartedAt:        s.startedAt,
		AgeMS:            age,
		Lifecycle:        unpackMediaBufferTimed(s.lifecycle.Load()),
		Producer:         unpackMediaBufferTimed(s.producer.Load()),
		Consumer:         unpackMediaBufferTimed(s.consumer.Load()),
		Blocker:          unpackMediaBufferTimed(s.blocker.Load()),
		TargetBytes:      target,
		OwnedBytes:       owned,
		DebtBytes:        debt,
		PrivateBaseBytes: mediaBufferChunkSize,
		QueuedBytes:      s.queued.Load(),
		WritingBytes:     s.writing.Load(),
		PeakOwnedBytes:   s.peakOwned.Load(),
		PeakDebtBytes:    s.peakDebt.Load(),
		PeakQueuedBytes:  s.peakQueued.Load(),
		PeakWritingBytes: s.peakWriting.Load(),
		Terminal:         s.terminal.Load(),
	}
	for i := range snapshot.Waits {
		snapshot.Waits[i] = telemetry.MediaBufferWaitStat{TotalMS: s.waitTotal[i].Load(), MaxMS: s.waitMax[i].Load()}
	}
	return snapshot
}

func (s *mediaBufferLiveState) bindTransferID(id uint64) {
	if s != nil && id != 0 {
		s.transferID.CompareAndSwap(0, id)
	}
}

func (s *mediaBufferLiveState) projectAllocation(target, owned, debt int64) {
	if s == nil || s.terminal.Load() {
		return
	}
	word := packMediaBufferAllocation(target, owned, debt)
	if s.allocation.Load() == word {
		return
	}
	s.allocation.Store(word)
	updateMediaBufferPeak(&s.peakOwned, owned)
	updateMediaBufferPeak(&s.peakDebt, debt)
}

func (s *mediaBufferLiveState) projectBlocker(blocker telemetry.MediaBufferAllocationBlocker) {
	if s == nil || s.terminal.Load() {
		return
	}
	old := unpackMediaBufferTimed(s.blocker.Load())
	if old.Value == uint8(blocker) {
		return
	}
	now := s.clock.nowMS()
	s.blocker.Store(packMediaBufferTimed(uint8(blocker), now))
	s.refreshPoolContention(now)
}

func (s *mediaBufferLiveState) setLifecycle(state telemetry.MediaBufferLifecycle) {
	if s == nil || s.terminal.Load() {
		return
	}
	old := unpackMediaBufferTimed(s.lifecycle.Load())
	if old.Value == uint8(state) {
		return
	}
	now := s.clock.nowMS()
	if old.Value == uint8(telemetry.MediaBufferLifecycleClosing) {
		s.finishWait(mediaBufferWaitClose, now-old.TransitionMS)
	}
	s.lifecycle.Store(packMediaBufferTimed(uint8(state), now))
}

func (s *mediaBufferLiveState) setProducer(state telemetry.MediaBufferProducerState) {
	if s == nil || s.terminal.Load() {
		return
	}
	s.transitionProducer(state)
}

func (s *mediaBufferLiveState) beginProducerOperation(state telemetry.MediaBufferProducerState) int64 {
	if s == nil {
		return 0
	}
	now := s.clock.nowMS()
	s.producer.Store(packMediaBufferTimed(uint8(state), now))
	if state == telemetry.MediaBufferProducerWaitingForBuffer {
		s.refreshPoolContention(now)
	}
	return now
}

func (s *mediaBufferLiveState) endProducerOperation(state telemetry.MediaBufferProducerState, started int64) {
	if s == nil {
		return
	}
	now := s.clock.nowMS()
	s.finishProducerWait(state, now-started)
	s.producer.Store(packMediaBufferTimed(uint8(telemetry.MediaBufferProducerIdle), now))
	if state == telemetry.MediaBufferProducerWaitingForBuffer {
		s.refreshPoolContention(now)
	}
}

func (s *mediaBufferLiveState) transitionProducer(state telemetry.MediaBufferProducerState) {
	old := unpackMediaBufferTimed(s.producer.Load())
	if old.Value == uint8(state) {
		return
	}
	now := s.clock.nowMS()
	s.finishProducerWait(telemetry.MediaBufferProducerState(old.Value), now-old.TransitionMS)
	s.producer.Store(packMediaBufferTimed(uint8(state), now))
	if old.Value == uint8(telemetry.MediaBufferProducerWaitingForBuffer) || state == telemetry.MediaBufferProducerWaitingForBuffer {
		s.refreshPoolContention(now)
	}
}

func (s *mediaBufferLiveState) finishProducerWait(state telemetry.MediaBufferProducerState, duration int64) {
	switch state {
	case telemetry.MediaBufferProducerWaitingForBuffer:
		s.finishWait(mediaBufferWaitAcquire, duration)
	case telemetry.MediaBufferProducerReadingBase, telemetry.MediaBufferProducerReadingOptional:
		s.finishWait(mediaBufferWaitUpstream, duration)
	}
}

func (s *mediaBufferLiveState) setConsumer(state telemetry.MediaBufferConsumerState) {
	if s == nil || s.terminal.Load() {
		return
	}
	s.transitionConsumer(state)
}

func (s *mediaBufferLiveState) beginConsumerOperation(state telemetry.MediaBufferConsumerState) int64 {
	if s == nil {
		return 0
	}
	now := s.clock.nowMS()
	s.consumer.Store(packMediaBufferTimed(uint8(state), now))
	return now
}

func (s *mediaBufferLiveState) endConsumerOperation(state telemetry.MediaBufferConsumerState, started int64) {
	if s == nil {
		return
	}
	now := s.clock.nowMS()
	s.finishConsumerWait(state, now-started)
	s.consumer.Store(packMediaBufferTimed(uint8(telemetry.MediaBufferConsumerIdle), now))
}

func (s *mediaBufferLiveState) transitionConsumer(state telemetry.MediaBufferConsumerState) {
	old := unpackMediaBufferTimed(s.consumer.Load())
	if old.Value == uint8(state) {
		return
	}
	now := s.clock.nowMS()
	s.finishConsumerWait(telemetry.MediaBufferConsumerState(old.Value), now-old.TransitionMS)
	s.consumer.Store(packMediaBufferTimed(uint8(state), now))
}

func (s *mediaBufferLiveState) finishConsumerWait(state telemetry.MediaBufferConsumerState, duration int64) {
	switch state {
	case telemetry.MediaBufferConsumerWaitingForData:
		s.finishWait(mediaBufferWaitConsumer, duration)
	case telemetry.MediaBufferConsumerWriting:
		s.finishWait(mediaBufferWaitDownstream, duration)
	}
}

func (s *mediaBufferLiveState) setQueued(bytes int64) {
	if s == nil || s.terminal.Load() {
		return
	}
	s.queued.Store(bytes)
	if bytes > 0 {
		updateMediaBufferPeak(&s.peakQueued, bytes)
	}
}

func (s *mediaBufferLiveState) setWriting(bytes int64) {
	if s == nil || s.terminal.Load() {
		return
	}
	s.writing.Store(bytes)
	if bytes > 0 {
		updateMediaBufferPeak(&s.peakWriting, bytes)
	}
}

func (s *mediaBufferLiveState) markTerminal() {
	if s == nil || !s.terminal.CompareAndSwap(false, true) {
		return
	}
	now := s.clock.nowMS()
	lifecycle := unpackMediaBufferTimed(s.lifecycle.Load())
	if lifecycle.Value == uint8(telemetry.MediaBufferLifecycleClosing) {
		s.finishWait(mediaBufferWaitClose, now-lifecycle.TransitionMS)
	}
	s.refreshPoolContention(now)
}

func (s *mediaBufferLiveState) refreshPoolContention(now int64) {
	producer := unpackMediaBufferTimed(s.producer.Load())
	blocker := unpackMediaBufferTimed(s.blocker.Load())
	active := !s.terminal.Load() && producer.Value == uint8(telemetry.MediaBufferProducerWaitingForBuffer) && blocker.Value == uint8(telemetry.MediaBufferBlockerPoolExhausted)
	if active {
		start := producer.TransitionMS
		if blocker.TransitionMS > start {
			start = blocker.TransitionMS
		}
		s.poolWait.CompareAndSwap(0, uint64(start)+1)
		return
	}
	for {
		started := s.poolWait.Load()
		if started == 0 {
			return
		}
		if s.poolWait.CompareAndSwap(started, 0) {
			s.finishWait(mediaBufferWaitPool, now-int64(started-1))
			return
		}
	}
}

func (s *mediaBufferLiveState) finishWait(index int, duration int64) {
	if s == nil || index < 0 || index >= len(s.waitTotal) || duration <= 0 {
		return
	}
	s.waitTotal[index].Add(duration)
	updateMediaBufferPeak(&s.waitMax[index], duration)
}

func updateMediaBufferPeak(peak *atomic.Int64, value int64) {
	for current := peak.Load(); value > current; current = peak.Load() {
		if peak.CompareAndSwap(current, value) {
			return
		}
	}
}
