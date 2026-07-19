package telemetry

import (
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// These strings are the complete wire vocabulary for media-buffer telemetry.
type MediaBufferHealth string

const (
	MediaBufferHealthDisabled MediaBufferHealth = "disabled"
	MediaBufferHealthIdle     MediaBufferHealth = "idle"
	MediaBufferHealthHealthy  MediaBufferHealth = "healthy"
	MediaBufferHealthWarning  MediaBufferHealth = "warning"
	MediaBufferHealthCritical MediaBufferHealth = "critical"
)

type MediaBufferObservation string

const (
	ObservationComplete    MediaBufferObservation = "complete"
	ObservationLimited     MediaBufferObservation = "limited"
	ObservationUnavailable MediaBufferObservation = "unavailable"
)

type MediaBufferWaitCondition string

const (
	WaitNone               MediaBufferWaitCondition = "none"
	WaitBufferAcquire      MediaBufferWaitCondition = "buffer_acquire"
	WaitPoolContention     MediaBufferWaitCondition = "pool_contention"
	WaitConsumerStarvation MediaBufferWaitCondition = "consumer_starvation"
	WaitUpstreamStall      MediaBufferWaitCondition = "upstream_stall"
	WaitDownstreamStall    MediaBufferWaitCondition = "downstream_stall"
	WaitCloseJoinStall     MediaBufferWaitCondition = "close_join_stall"
)

type MediaBufferMediaMode string

const (
	MediaBufferModeDirect  MediaBufferMediaMode = "direct"
	MediaBufferModeHLS     MediaBufferMediaMode = "hls"
	MediaBufferModeRange   MediaBufferMediaMode = "range"
	MediaBufferModeUnknown MediaBufferMediaMode = "unknown"
)

type MediaBufferLifecycleName string

const (
	LifecycleStarting MediaBufferLifecycleName = "starting"
	LifecycleActive   MediaBufferLifecycleName = "active"
	LifecycleClosing  MediaBufferLifecycleName = "closing"
)

type MediaBufferProducerName string

const (
	ProducerIdle             MediaBufferProducerName = "idle"
	ProducerReadingBase      MediaBufferProducerName = "reading_base"
	ProducerReadingOptional  MediaBufferProducerName = "reading_optional"
	ProducerWaitingForBuffer MediaBufferProducerName = "waiting_for_buffer"
	ProducerDone             MediaBufferProducerName = "done"
)

type MediaBufferConsumerName string

const (
	ConsumerIdle           MediaBufferConsumerName = "idle"
	ConsumerWaitingForData MediaBufferConsumerName = "waiting_for_data"
	ConsumerWriting        MediaBufferConsumerName = "writing"
	ConsumerDone           MediaBufferConsumerName = "done"
)

type MediaBufferBlockerName string

const (
	BlockerNone          MediaBufferBlockerName = "none"
	BlockerPoolExhausted MediaBufferBlockerName = "pool_exhausted"
	BlockerAtTarget      MediaBufferBlockerName = "at_target"
	BlockerDebt          MediaBufferBlockerName = "debt"
)

type MediaBufferOutcome string

const (
	OutcomeSuccess         MediaBufferOutcome = "success"
	OutcomeCanceled        MediaBufferOutcome = "canceled"
	OutcomeUpstreamError   MediaBufferOutcome = "upstream_error"
	OutcomeDownstreamError MediaBufferOutcome = "downstream_error"
	OutcomeShortWrite      MediaBufferOutcome = "short_write"
	OutcomeLengthMismatch  MediaBufferOutcome = "length_mismatch"
	OutcomeInvalidRead     MediaBufferOutcome = "invalid_read"
	OutcomeInvalidWrite    MediaBufferOutcome = "invalid_write"
	OutcomeNoProgress      MediaBufferOutcome = "no_progress"
	OutcomeInvariantError  MediaBufferOutcome = "invariant_error"
)

func (v MediaBufferHealth) Valid() bool {
	return v == MediaBufferHealthDisabled || v == MediaBufferHealthIdle || v == MediaBufferHealthHealthy || v == MediaBufferHealthWarning || v == MediaBufferHealthCritical
}
func (v MediaBufferObservation) Valid() bool {
	return v == ObservationComplete || v == ObservationLimited || v == ObservationUnavailable
}
func (v MediaBufferWaitCondition) Valid() bool {
	switch v {
	case WaitNone, WaitBufferAcquire, WaitPoolContention, WaitConsumerStarvation, WaitUpstreamStall, WaitDownstreamStall, WaitCloseJoinStall:
		return true
	}
	return false
}
func (v MediaBufferMediaMode) Valid() bool {
	return v == MediaBufferModeDirect || v == MediaBufferModeHLS || v == MediaBufferModeRange || v == MediaBufferModeUnknown
}
func (v MediaBufferLifecycleName) Valid() bool {
	return v == LifecycleStarting || v == LifecycleActive || v == LifecycleClosing
}
func (v MediaBufferProducerName) Valid() bool {
	return v == ProducerIdle || v == ProducerReadingBase || v == ProducerReadingOptional || v == ProducerWaitingForBuffer || v == ProducerDone
}
func (v MediaBufferConsumerName) Valid() bool {
	return v == ConsumerIdle || v == ConsumerWaitingForData || v == ConsumerWriting || v == ConsumerDone
}
func (v MediaBufferBlockerName) Valid() bool {
	return v == BlockerNone || v == BlockerPoolExhausted || v == BlockerAtTarget || v == BlockerDebt
}
func (v MediaBufferOutcome) Valid() bool { return validMediaBufferOutcome(v) }
func validMediaBufferOutcome(v MediaBufferOutcome) bool {
	switch v {
	case OutcomeSuccess, OutcomeCanceled, OutcomeUpstreamError, OutcomeDownstreamError, OutcomeShortWrite, OutcomeLengthMismatch, OutcomeInvalidRead, OutcomeInvalidWrite, OutcomeNoProgress, OutcomeInvariantError:
		return true
	}
	return false
}

type MediaBufferWaits struct {
	BufferAcquire      MediaBufferWaitStat `json:"buffer_acquire"`
	PoolContention     MediaBufferWaitStat `json:"pool_contention"`
	ConsumerStarvation MediaBufferWaitStat `json:"consumer_starvation"`
	UpstreamStall      MediaBufferWaitStat `json:"upstream_stall"`
	DownstreamStall    MediaBufferWaitStat `json:"downstream_stall"`
	CloseJoinStall     MediaBufferWaitStat `json:"close_join_stall"`
}

type MediaBufferStream struct {
	BootID            string                   `json:"boot_id"`
	StreamID          string                   `json:"stream_id"`
	TransferID        *string                  `json:"transfer_id"`
	UserID            *string                  `json:"user_id"`
	Username          *string                  `json:"username"`
	Device            *string                  `json:"device"`
	ItemID            *string                  `json:"item_id"`
	MediaMode         MediaBufferMediaMode     `json:"media_mode"`
	State             MediaBufferLifecycleName `json:"state"`
	ProducerState     MediaBufferProducerName  `json:"producer_state"`
	ConsumerState     MediaBufferConsumerName  `json:"consumer_state"`
	AllocationBlocker MediaBufferBlockerName   `json:"allocation_blocker"`
	TargetBytes       int64                    `json:"target_bytes"`
	OwnedBytes        int64                    `json:"owned_bytes"`
	DebtBytes         int64                    `json:"debt_bytes"`
	PrivateBaseBytes  int64                    `json:"private_base_bytes"`
	QueuedBytes       int64                    `json:"queued_bytes"`
	WritingBytes      int64                    `json:"writing_bytes"`
	BytesRead         int64                    `json:"bytes_read"`
	BytesWritten      int64                    `json:"bytes_written"`
	WaitCondition     MediaBufferWaitCondition `json:"wait_condition"`
	WaitStartedAt     *time.Time               `json:"wait_started_at"`
	WaitDurationMS    int64                    `json:"wait_duration_ms"`
	Health            MediaBufferHealth        `json:"health"`
	HealthReasons     []string                 `json:"health_reasons"`
	StartedAt         time.Time                `json:"started_at"`
	AgeMS             int64                    `json:"age_ms"`
}

type MediaBufferCompletionDTO struct {
	BootID                 string                   `json:"boot_id"`
	StreamID               string                   `json:"stream_id"`
	TransferID             *string                  `json:"transfer_id"`
	UserID                 *string                  `json:"user_id"`
	Username               *string                  `json:"username"`
	Device                 *string                  `json:"device"`
	ItemID                 *string                  `json:"item_id"`
	MediaMode              MediaBufferMediaMode     `json:"media_mode"`
	FinalState             MediaBufferLifecycleName `json:"final_state"`
	FinalProducerState     MediaBufferProducerName  `json:"final_producer_state"`
	FinalConsumerState     MediaBufferConsumerName  `json:"final_consumer_state"`
	FinalAllocationBlocker MediaBufferBlockerName   `json:"final_allocation_blocker"`
	Outcome                MediaBufferOutcome       `json:"outcome"`
	StartedAt              time.Time                `json:"started_at"`
	CompletedAt            time.Time                `json:"completed_at"`
	DurationMS             int64                    `json:"duration_ms"`
	BytesRead              int64                    `json:"bytes_read"`
	BytesWritten           int64                    `json:"bytes_written"`
	PeakOwnedBytes         int64                    `json:"peak_owned_bytes"`
	PeakDebtBytes          int64                    `json:"peak_debt_bytes"`
	PeakQueuedBytes        int64                    `json:"peak_queued_bytes"`
	PeakWritingBytes       int64                    `json:"peak_writing_bytes"`
	WaitsMS                MediaBufferWaits         `json:"waits_ms"`
	InvariantObserved      bool                     `json:"invariant_observed"`
}

type MediaBufferLivePageDTO struct {
	BootID                  string                 `json:"boot_id"`
	Items                   []MediaBufferStream    `json:"items"`
	NextCursor              string                 `json:"next_cursor"`
	HasMore                 bool                   `json:"has_more"`
	ObservationCompleteness MediaBufferObservation `json:"observation_completeness"`
}
type MediaBufferRecentPage struct {
	BootID string                     `json:"boot_id"`
	Items  []MediaBufferCompletionDTO `json:"items"`
}

// MediaBufferControllerSnapshot is the narrow, O(1) provider contract. The
// provider owns all controller locking and returns one coherent composition.
type MediaBufferControllerSnapshot struct {
	Enabled                                                                                            bool
	Available                                                                                          bool
	HardBudgetBytes, AllocatedBytes, OwnedBytes, FreeBytes, UnallocatedOptionalBytes, PrivateBaseBytes int64
	ActiveRequests, BaseOnlyRequests, IndebtedRequests                                                 int
	RequestDebtBytes                                                                                   int64
}
type MediaBufferControllerProvider func() MediaBufferControllerSnapshot

func sanitizeMediaBufferString(v string) *string {
	v = strings.TrimSpace(v)
	var b strings.Builder
	space := false
	for _, r := range v {
		if r == 0 || unicode.IsControl(r) {
			continue
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			if b.Len()+1 > 256 {
				break
			}
			b.WriteByte(' ')
			space = false
		}
		if b.Len()+utf8.RuneLen(r) > 256 {
			break
		}
		b.WriteRune(r)
	}
	v = b.String()
	if v == "" {
		return nil
	}
	return &v
}
func nonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
func idString(v uint64) string { return formatUint(v) }
func formatUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte(v%10) + '0'
		v /= 10
	}
	return string(b[i:])
}

func mediaBufferStreamDTO(boot string, s MediaBufferLiveSnapshot, bytesRead, bytesWritten int64, now time.Time, started time.Time) MediaBufferStream {
	if s.StartedAt.IsZero() {
		s.StartedAt = started
	}
	age := nonNegative(now.Sub(s.StartedAt).Milliseconds())
	if s.AgeMS >= 0 {
		age = s.AgeMS
	}
	condition := selectedCondition(s, now)
	var waitStarted *time.Time
	var waitDuration int64
	if condition != WaitNone {
		t := conditionStart(s, condition).UTC()
		waitStarted = &t
		waitDuration = conditionDuration(s, now, condition).Milliseconds()
	}
	var transferID *string
	if s.TransferID != 0 {
		v := idString(s.TransferID)
		transferID = &v
	}
	return MediaBufferStream{BootID: boot, StreamID: idString(s.StreamID), TransferID: transferID, UserID: sanitizeMediaBufferString(s.UserID), Username: sanitizeMediaBufferString(s.Username), Device: sanitizeMediaBufferString(s.Device), ItemID: sanitizeMediaBufferString(s.ItemID), MediaMode: finiteMediaMode(s.MediaMode), State: lifecycleName(s.Lifecycle.Value), ProducerState: producerName(s.Producer.Value), ConsumerState: consumerName(s.Consumer.Value), AllocationBlocker: blockerName(s.Blocker.Value), TargetBytes: nonNegative(s.TargetBytes), OwnedBytes: nonNegative(s.OwnedBytes), DebtBytes: nonNegative(s.DebtBytes), PrivateBaseBytes: nonNegative(s.PrivateBaseBytes), QueuedBytes: nonNegative(s.QueuedBytes), WritingBytes: nonNegative(s.WritingBytes), BytesRead: nonNegative(bytesRead), BytesWritten: nonNegative(bytesWritten), WaitCondition: condition, WaitStartedAt: waitStarted, WaitDurationMS: waitDuration, Health: streamHealth(s, now), HealthReasons: streamReasons(s, now), StartedAt: s.StartedAt.UTC(), AgeMS: nonNegative(age)}
}
func finiteMediaMode(v string) MediaBufferMediaMode {
	switch strings.ToLower(v) {
	case "direct", "hls", "range":
		return MediaBufferMediaMode(strings.ToLower(v))
	default:
		return MediaBufferModeUnknown
	}
}
func lifecycleName(v uint8) MediaBufferLifecycleName {
	if v <= uint8(MediaBufferLifecycleClosing) {
		return []MediaBufferLifecycleName{LifecycleStarting, LifecycleActive, LifecycleClosing}[v]
	}
	return LifecycleStarting
}
func producerName(v uint8) MediaBufferProducerName {
	if v <= uint8(MediaBufferProducerDone) {
		return []MediaBufferProducerName{ProducerIdle, ProducerReadingBase, ProducerReadingOptional, ProducerWaitingForBuffer, ProducerDone}[v]
	}
	return ProducerIdle
}
func consumerName(v uint8) MediaBufferConsumerName {
	if v <= uint8(MediaBufferConsumerDone) {
		return []MediaBufferConsumerName{ConsumerIdle, ConsumerWaitingForData, ConsumerWriting, ConsumerDone}[v]
	}
	return ConsumerIdle
}
func blockerName(v uint8) MediaBufferBlockerName {
	if v <= uint8(MediaBufferBlockerDebt) {
		return []MediaBufferBlockerName{BlockerNone, BlockerPoolExhausted, BlockerAtTarget, BlockerDebt}[v]
	}
	return BlockerNone
}
func conditionTransitionMS(s MediaBufferLiveSnapshot, c MediaBufferWaitCondition) int64 {
	switch c {
	case WaitBufferAcquire:
		return s.Producer.TransitionMS
	case WaitPoolContention:
		if s.Producer.TransitionMS > s.Blocker.TransitionMS {
			return s.Producer.TransitionMS
		}
		return s.Blocker.TransitionMS
	case WaitConsumerStarvation:
		return s.Consumer.TransitionMS
	case WaitUpstreamStall:
		return s.Producer.TransitionMS
	case WaitDownstreamStall:
		return s.Consumer.TransitionMS
	case WaitCloseJoinStall:
		return s.Lifecycle.TransitionMS
	}
	return 0
}
func conditionStart(s MediaBufferLiveSnapshot, c MediaBufferWaitCondition) time.Time {
	return s.StartedAt.Add(time.Duration(conditionTransitionMS(s, c)) * time.Millisecond)
}
func conditionDuration(s MediaBufferLiveSnapshot, now time.Time, c MediaBufferWaitCondition) time.Duration {
	current := s.AgeMS
	if current <= 0 && !s.StartedAt.IsZero() {
		current = now.Sub(s.StartedAt).Milliseconds()
	}
	elapsed := current - conditionTransitionMS(s, c)
	if elapsed < 0 {
		elapsed = 0
	}
	return time.Duration(elapsed) * time.Millisecond
}
func selectedCondition(s MediaBufferLiveSnapshot, now time.Time) MediaBufferWaitCondition {
	candidates := []MediaBufferWaitCondition{}
	if s.Lifecycle.Value == uint8(MediaBufferLifecycleClosing) {
		candidates = append(candidates, WaitCloseJoinStall)
	}
	if s.Consumer.Value == uint8(MediaBufferConsumerWriting) {
		candidates = append(candidates, WaitDownstreamStall)
	}
	if s.Producer.Value == uint8(MediaBufferProducerReadingBase) || s.Producer.Value == uint8(MediaBufferProducerReadingOptional) {
		candidates = append(candidates, WaitUpstreamStall)
	}
	if s.Producer.Value == uint8(MediaBufferProducerWaitingForBuffer) && s.Blocker.Value == uint8(MediaBufferBlockerPoolExhausted) {
		candidates = append(candidates, WaitPoolContention)
	}
	if s.Producer.Value == uint8(MediaBufferProducerWaitingForBuffer) {
		candidates = append(candidates, WaitBufferAcquire)
	}
	if s.Consumer.Value == uint8(MediaBufferConsumerWaitingForData) {
		candidates = append(candidates, WaitConsumerStarvation)
	}
	best := WaitNone
	var longest time.Duration
	for _, c := range candidates {
		d := conditionDuration(s, now, c)
		if best == WaitNone || d > longest {
			best, longest = c, d
		}
	}
	return best
}
func streamHealth(s MediaBufferLiveSnapshot, now time.Time) MediaBufferHealth {
	critical := s.Lifecycle.Value == uint8(MediaBufferLifecycleClosing) && conditionDuration(s, now, WaitCloseJoinStall) >= 10*time.Second
	if critical {
		return MediaBufferHealthCritical
	}
	for _, c := range []MediaBufferWaitCondition{WaitPoolContention, WaitConsumerStarvation, WaitUpstreamStall, WaitDownstreamStall} {
		active := (c == WaitPoolContention && s.Producer.Value == uint8(MediaBufferProducerWaitingForBuffer) && s.Blocker.Value == uint8(MediaBufferBlockerPoolExhausted)) || (c == WaitConsumerStarvation && s.Consumer.Value == uint8(MediaBufferConsumerWaitingForData)) || (c == WaitUpstreamStall && (s.Producer.Value == uint8(MediaBufferProducerReadingBase) || s.Producer.Value == uint8(MediaBufferProducerReadingOptional))) || (c == WaitDownstreamStall && s.Consumer.Value == uint8(MediaBufferConsumerWriting))
		if active && conditionDuration(s, now, c) >= mediaBufferConditionThreshold(c) {
			return MediaBufferHealthWarning
		}
	}
	return MediaBufferHealthHealthy
}

func mediaBufferConditionThreshold(c MediaBufferWaitCondition) time.Duration {
	if c == WaitBufferAcquire || c == WaitPoolContention || c == WaitConsumerStarvation {
		return 2 * time.Second
	}
	return 10 * time.Second
}
func streamReasons(s MediaBufferLiveSnapshot, now time.Time) []string {
	out := make([]string, 0, 4)
	for _, c := range []MediaBufferWaitCondition{WaitPoolContention, WaitConsumerStarvation, WaitUpstreamStall, WaitDownstreamStall, WaitCloseJoinStall} {
		if streamHealthFor(s, now, c) {
			out = append(out, string(c))
		}
	}
	sort.Strings(out)
	return out
}
func streamHealthFor(s MediaBufferLiveSnapshot, now time.Time, c MediaBufferWaitCondition) bool {
	threshold := mediaBufferConditionThreshold(c)
	return conditionDuration(s, now, c) >= threshold && ((c == WaitPoolContention && s.Producer.Value == 3 && s.Blocker.Value == 1) || (c == WaitConsumerStarvation && s.Consumer.Value == 1) || (c == WaitUpstreamStall && (s.Producer.Value == 1 || s.Producer.Value == 2)) || (c == WaitDownstreamStall && s.Consumer.Value == 2) || (c == WaitCloseJoinStall && s.Lifecycle.Value == 2))
}
