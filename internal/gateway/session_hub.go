package gateway

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	sessionOutboundQueueCapacity = 32
	sessionOutboundQueueMaxBytes = 256 << 10
	sessionCloseQueueCapacity    = 256
	sessionCloseAllWaitTimeout   = 500 * time.Millisecond

	sessionCloseReasonReplaced = "connection replaced"
	sessionCloseReasonOverload = "outbound queue saturated"
	sessionCloseReasonRevoked  = "session revoked"
	sessionCloseReasonShutdown = "gateway shutdown"
)

type sessionIndexKey struct {
	gatewayUserID string
	publicID      string
}

type queuedSessionEnvelope struct {
	envelope    SessionOutboundEnvelope
	bytes       int
	coalescible bool
}

type liveSessionConnection struct {
	identity   SessionConnectionIdentity
	generation uint64
	connection SessionHubConnection
	notify     chan struct{}
	queue      []queuedSessionEnvelope
	queueBytes int
}

type sessionCloseRequest struct {
	connection SessionHubConnection
	ctx        context.Context
	code       websocket.StatusCode
	reason     string
	finish     context.CancelFunc
}

type sessionCloseReservation struct {
	id         uint64
	connection SessionHubConnection
	code       websocket.StatusCode
	reason     string
	handshake  bool
	finish     context.CancelFunc
	ctx        context.Context
}

type processLocalSessionHub struct {
	mu             sync.Mutex
	nextGeneration uint64
	byToken        map[string]*liveSessionConnection
	bySession      map[sessionIndexKey]map[string]uint64
	closed         bool

	closeMu              sync.Mutex
	closeQueue           []sessionCloseRequest
	closeWorker          bool
	closeTerminal        bool
	closeDone            chan struct{}
	closeDoneOnce        sync.Once
	closeRegistrations   int
	nextCloseReservation uint64
	closePending         map[uint64]*sessionCloseReservation
	closeConnections     map[SessionHubConnection]int
	terminalConnections  []SessionHubConnection
	abortOnce            sync.Once
	closeReservationHook func()
}

func NewProcessLocalSessionHub() SessionHub {
	return &processLocalSessionHub{
		byToken:          make(map[string]*liveSessionConnection),
		bySession:        make(map[sessionIndexKey]map[string]uint64),
		closeQueue:       make([]sessionCloseRequest, 0, sessionCloseQueueCapacity),
		closeDone:        make(chan struct{}),
		closePending:     make(map[uint64]*sessionCloseReservation),
		closeConnections: make(map[SessionHubConnection]int),
	}
}

func (h *processLocalSessionHub) Register(identity SessionConnectionIdentity, connection SessionHubConnection) (SessionHubRegistration, error) {
	if !validSessionConnectionIdentity(identity) || connection == nil {
		return SessionHubRegistration{}, ErrBadRequest
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		connection.Cancel(context.Background())
		connection.Abort()
		return SessionHubRegistration{}, ErrStoreUnavailable
	}
	h.nextGeneration++
	entry := &liveSessionConnection{
		identity:   identity,
		generation: h.nextGeneration,
		connection: connection,
		notify:     make(chan struct{}, 1),
		queue:      make([]queuedSessionEnvelope, 0, sessionOutboundQueueCapacity),
	}
	old := h.removeAndReserveLocked(identity.TokenHash, websocket.StatusGoingAway, sessionCloseReasonReplaced, true, nil)
	h.byToken[identity.TokenHash] = entry
	h.addSessionIndexLocked(entry)
	h.addCloseRegistrationLocked()
	registration := SessionHubRegistration{Generation: entry.generation, Notify: entry.notify}
	h.mu.Unlock()

	h.activateReservedClose(old, nil)
	return registration, nil
}

func (h *processLocalSessionHub) Replace(identity SessionConnectionIdentity, connection SessionHubConnection) (SessionHubRegistration, error) {
	return h.Register(identity, connection)
}

func (h *processLocalSessionHub) Unregister(identity SessionConnectionIdentity, registration SessionHubRegistration) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	entry := h.byToken[identity.TokenHash]
	var removed *sessionCloseReservation
	if entry != nil && entry.generation == registration.Generation && entry.identity == identity {
		removed = h.removeAndReserveLocked(identity.TokenHash, 0, "", false, nil)
	}
	h.mu.Unlock()
	h.activateReservedClose(removed, context.Background())
}

func (h *processLocalSessionHub) Lookup(tokenHash string) (SessionHubPresence, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.byToken[tokenHash]
	if entry == nil {
		return SessionHubPresence{}, false
	}
	return presenceFor(entry), true
}

func (h *processLocalSessionHub) LookupSession(gatewayUserID, publicID string) []SessionHubPresence {
	key := sessionIndexKey{gatewayUserID: gatewayUserID, publicID: publicID}
	h.mu.Lock()
	defer h.mu.Unlock()
	indexed := h.bySession[key]
	result := make([]SessionHubPresence, 0, len(indexed))
	for tokenHash, generation := range indexed {
		entry := h.byToken[tokenHash]
		if entry != nil && entry.generation == generation {
			result = append(result, presenceFor(entry))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Generation < result[j].Generation })
	return result
}

func (h *processLocalSessionHub) Present(identity SessionConnectionIdentity) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.byToken[identity.TokenHash]
	return entry != nil && entry.identity == identity
}

func (h *processLocalSessionHub) Enqueue(identity SessionConnectionIdentity, envelope SessionOutboundEnvelope) SessionCommandEnqueueResult {
	return h.enqueueGeneration(identity, 0, envelope)
}

func (h *processLocalSessionHub) EnqueueGeneration(identity SessionConnectionIdentity, generation uint64, envelope SessionOutboundEnvelope) SessionCommandEnqueueResult {
	if generation == 0 {
		return SessionCommandEnqueueResult{Status: SessionCommandDisconnected}
	}
	return h.enqueueGeneration(identity, generation, envelope)
}

func (h *processLocalSessionHub) enqueueGeneration(identity SessionConnectionIdentity, expectedGeneration uint64, envelope SessionOutboundEnvelope) SessionCommandEnqueueResult {
	payload, err := encodeSessionEnvelope(envelope)
	if err != nil {
		return SessionCommandEnqueueResult{Status: SessionCommandQueueFull, Err: ErrBadRequest}
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return SessionCommandEnqueueResult{Status: SessionCommandDisconnected, Err: ErrStoreUnavailable}
	}
	entry := h.byToken[identity.TokenHash]
	if entry == nil || entry.identity != identity || expectedGeneration != 0 && entry.generation != expectedGeneration {
		h.mu.Unlock()
		return SessionCommandEnqueueResult{Status: SessionCommandDisconnected}
	}
	if len(entry.queue) >= sessionOutboundQueueCapacity || entry.queueBytes+len(payload) > sessionOutboundQueueMaxBytes {
		removed := h.removeAndReserveLocked(identity.TokenHash, websocket.StatusTryAgainLater, sessionCloseReasonOverload, true, nil)
		h.mu.Unlock()
		h.activateReservedClose(removed, nil)
		return SessionCommandEnqueueResult{Status: SessionCommandQueueFull, Err: ErrStoreUnavailable}
	}
	entry.queue = append(entry.queue, queuedSessionEnvelope{envelope: cloneSessionEnvelope(envelope), bytes: len(payload)})
	entry.queueBytes += len(payload)
	notifySessionWriter(entry)
	h.mu.Unlock()
	return SessionCommandEnqueueResult{Status: SessionCommandEnqueued}
}

func (h *processLocalSessionHub) Dequeue(identity SessionConnectionIdentity, registration SessionHubRegistration) (SessionOutboundEnvelope, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry := h.byToken[identity.TokenHash]
	if entry == nil || entry.identity != identity || entry.generation != registration.Generation || len(entry.queue) == 0 {
		return SessionOutboundEnvelope{}, false
	}
	queued := entry.queue[0]
	copy(entry.queue, entry.queue[1:])
	entry.queue[len(entry.queue)-1] = queuedSessionEnvelope{}
	entry.queue = entry.queue[:len(entry.queue)-1]
	entry.queueBytes -= queued.bytes
	if len(entry.queue) > 0 {
		notifySessionWriter(entry)
	}
	return cloneSessionEnvelope(queued.envelope), true
}

func (h *processLocalSessionHub) PublishSessions(publication SessionPublication) {
	data, err := json.Marshal(sessionPublicationDTOs(publication.Sessions))
	if err != nil {
		return
	}
	envelope := SessionOutboundEnvelope{MessageType: "Sessions", Data: data}
	payload, err := encodeSessionEnvelope(envelope)
	if err != nil {
		return
	}

	var overloaded []*sessionCloseReservation
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	for tokenHash, entry := range h.byToken {
		if entry.identity.GatewayUserID != publication.GatewayUserID {
			continue
		}
		replaced := false
		for i := range entry.queue {
			if !entry.queue[i].coalescible {
				continue
			}
			nextBytes := entry.queueBytes - entry.queue[i].bytes + len(payload)
			if nextBytes > sessionOutboundQueueMaxBytes {
				overloaded = append(overloaded, h.removeAndReserveLocked(tokenHash, websocket.StatusTryAgainLater, sessionCloseReasonOverload, true, nil))
			} else {
				entry.queue[i] = queuedSessionEnvelope{envelope: cloneSessionEnvelope(envelope), bytes: len(payload), coalescible: true}
				entry.queueBytes = nextBytes
				notifySessionWriter(entry)
			}
			replaced = true
			break
		}
		if replaced {
			continue
		}
		if len(entry.queue) >= sessionOutboundQueueCapacity || entry.queueBytes+len(payload) > sessionOutboundQueueMaxBytes {
			overloaded = append(overloaded, h.removeAndReserveLocked(tokenHash, websocket.StatusTryAgainLater, sessionCloseReasonOverload, true, nil))
			continue
		}
		entry.queue = append(entry.queue, queuedSessionEnvelope{envelope: cloneSessionEnvelope(envelope), bytes: len(payload), coalescible: true})
		entry.queueBytes += len(payload)
		notifySessionWriter(entry)
	}
	h.mu.Unlock()
	for _, reservation := range overloaded {
		h.activateReservedClose(reservation, nil)
	}
}

func (h *processLocalSessionHub) PublishSessionsTo(identity SessionConnectionIdentity, registration SessionHubRegistration, data json.RawMessage) SessionCommandEnqueueResult {
	envelope := SessionOutboundEnvelope{MessageType: "Sessions", Data: cloneRawMessage(data)}
	payload, err := encodeSessionEnvelope(envelope)
	if err != nil {
		return SessionCommandEnqueueResult{Status: SessionCommandQueueFull, Err: ErrBadRequest}
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return SessionCommandEnqueueResult{Status: SessionCommandDisconnected, Err: ErrStoreUnavailable}
	}
	entry := h.byToken[identity.TokenHash]
	if entry == nil || entry.identity != identity || entry.generation != registration.Generation {
		h.mu.Unlock()
		return SessionCommandEnqueueResult{Status: SessionCommandDisconnected}
	}
	for i := range entry.queue {
		if !entry.queue[i].coalescible {
			continue
		}
		nextBytes := entry.queueBytes - entry.queue[i].bytes + len(payload)
		if nextBytes > sessionOutboundQueueMaxBytes {
			removed := h.removeAndReserveLocked(identity.TokenHash, websocket.StatusTryAgainLater, sessionCloseReasonOverload, true, nil)
			h.mu.Unlock()
			h.activateReservedClose(removed, nil)
			return SessionCommandEnqueueResult{Status: SessionCommandQueueFull, Err: ErrStoreUnavailable}
		}
		entry.queue[i] = queuedSessionEnvelope{envelope: cloneSessionEnvelope(envelope), bytes: len(payload), coalescible: true}
		entry.queueBytes = nextBytes
		notifySessionWriter(entry)
		h.mu.Unlock()
		return SessionCommandEnqueueResult{Status: SessionCommandEnqueued}
	}
	if len(entry.queue) >= sessionOutboundQueueCapacity || entry.queueBytes+len(payload) > sessionOutboundQueueMaxBytes {
		removed := h.removeAndReserveLocked(identity.TokenHash, websocket.StatusTryAgainLater, sessionCloseReasonOverload, true, nil)
		h.mu.Unlock()
		h.activateReservedClose(removed, nil)
		return SessionCommandEnqueueResult{Status: SessionCommandQueueFull, Err: ErrStoreUnavailable}
	}
	entry.queue = append(entry.queue, queuedSessionEnvelope{envelope: cloneSessionEnvelope(envelope), bytes: len(payload), coalescible: true})
	entry.queueBytes += len(payload)
	notifySessionWriter(entry)
	h.mu.Unlock()
	return SessionCommandEnqueueResult{Status: SessionCommandEnqueued}
}

func (h *processLocalSessionHub) CloseByToken(tokenHash string) int {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0
	}
	reservation := h.removeAndReserveLocked(tokenHash, websocket.StatusPolicyViolation, sessionCloseReasonRevoked, true, nil)
	h.mu.Unlock()
	if reservation == nil {
		return 0
	}
	h.activateReservedClose(reservation, nil)
	return 1
}

func (h *processLocalSessionHub) CloseSession(gatewayUserID, publicID string) int {
	key := sessionIndexKey{gatewayUserID: gatewayUserID, publicID: publicID}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0
	}
	indexed := h.bySession[key]
	reservations := make([]*sessionCloseReservation, 0, len(indexed))
	for tokenHash := range indexed {
		if reservation := h.removeAndReserveLocked(tokenHash, websocket.StatusPolicyViolation, sessionCloseReasonRevoked, true, nil); reservation != nil {
			reservations = append(reservations, reservation)
		}
	}
	h.mu.Unlock()
	for _, reservation := range reservations {
		h.activateReservedClose(reservation, nil)
	}
	return len(reservations)
}

func (h *processLocalSessionHub) BeginCloseAll(ctx context.Context) int {
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0
	}
	h.closed = true
	reservations := make([]*sessionCloseReservation, 0, len(h.byToken))
	for tokenHash := range h.byToken {
		reservations = append(reservations, h.removeAndReserveLocked(tokenHash, websocket.StatusGoingAway, sessionCloseReasonShutdown, true, nil))
	}
	h.closeMu.Lock()
	h.terminalConnections = make([]SessionHubConnection, 0, len(h.closeConnections))
	for connection := range h.closeConnections {
		h.terminalConnections = append(h.terminalConnections, connection)
	}
	h.closeMu.Unlock()
	h.mu.Unlock()
	for _, connection := range h.terminalConnections {
		connection.Cancel(ctx)
	}
	for _, reservation := range reservations {
		h.activateReservedCloseState(reservation, ctx, false)
	}
	h.finishCloseScheduling()
	return len(reservations)
}

func (h *processLocalSessionHub) CloseDone() <-chan struct{} {
	return h.closeDone
}

func (h *processLocalSessionHub) AbortCloseAll() {
	h.abortOnce.Do(func() {
		h.closeMu.Lock()
		connections := append([]SessionHubConnection(nil), h.terminalConnections...)
		h.closeMu.Unlock()
		for _, connection := range connections {
			connection.Abort()
		}
	})
}

func (h *processLocalSessionHub) CloseAll() int {
	ctx, cancel := context.WithTimeout(context.Background(), sessionCloseAllWaitTimeout)
	defer cancel()
	count := h.BeginCloseAll(ctx)
	select {
	case <-h.CloseDone():
	case <-ctx.Done():
		h.AbortCloseAll()
	}
	return count
}

func (h *processLocalSessionHub) addSessionIndexLocked(entry *liveSessionConnection) {
	key := sessionIndexKey{gatewayUserID: entry.identity.GatewayUserID, publicID: entry.identity.PublicID}
	indexed := h.bySession[key]
	if indexed == nil {
		indexed = make(map[string]uint64)
		h.bySession[key] = indexed
	}
	indexed[entry.identity.TokenHash] = entry.generation
}

func (h *processLocalSessionHub) removeTokenLocked(tokenHash string) *liveSessionConnection {
	entry := h.byToken[tokenHash]
	if entry == nil {
		return nil
	}
	delete(h.byToken, tokenHash)
	key := sessionIndexKey{gatewayUserID: entry.identity.GatewayUserID, publicID: entry.identity.PublicID}
	indexed := h.bySession[key]
	if indexed[entry.identity.TokenHash] == entry.generation {
		delete(indexed, entry.identity.TokenHash)
	}
	if len(indexed) == 0 {
		delete(h.bySession, key)
	}
	close(entry.notify)
	h.closeMu.Lock()
	if h.closeRegistrations > 0 {
		h.closeRegistrations--
	}
	h.closeTerminalDoneLocked()
	h.closeMu.Unlock()
	return entry
}

func (h *processLocalSessionHub) addCloseRegistrationLocked() {
	h.closeMu.Lock()
	h.closeRegistrations++
	h.closeMu.Unlock()
}

func (h *processLocalSessionHub) removeAndReserveLocked(tokenHash string, code websocket.StatusCode, reason string, handshake bool, finish context.CancelFunc) *sessionCloseReservation {
	entry := h.removeTokenLocked(tokenHash)
	if entry == nil {
		return nil
	}
	h.closeMu.Lock()
	h.nextCloseReservation++
	reservation := &sessionCloseReservation{
		id: h.nextCloseReservation, connection: entry.connection,
		code: code, reason: reason, handshake: handshake, finish: finish,
	}
	h.closePending[reservation.id] = reservation
	h.closeConnections[reservation.connection]++
	h.closeMu.Unlock()
	return reservation
}

func validSessionConnectionIdentity(identity SessionConnectionIdentity) bool {
	return identity.TokenHash != "" && strings.TrimSpace(identity.TokenHash) == identity.TokenHash &&
		identity.PublicID != "" && strings.TrimSpace(identity.PublicID) == identity.PublicID &&
		identity.GatewayUserID != "" && strings.TrimSpace(identity.GatewayUserID) == identity.GatewayUserID
}

func presenceFor(entry *liveSessionConnection) SessionHubPresence {
	return SessionHubPresence{
		PublicID:      entry.identity.PublicID,
		GatewayUserID: entry.identity.GatewayUserID,
		Generation:    entry.generation,
	}
}

func notifySessionWriter(entry *liveSessionConnection) {
	select {
	case entry.notify <- struct{}{}:
	default:
	}
}

func (h *processLocalSessionHub) activateReservedClose(reservation *sessionCloseReservation, ctx context.Context) {
	h.activateReservedCloseState(reservation, ctx, true)
}

func (h *processLocalSessionHub) activateReservedCloseState(reservation *sessionCloseReservation, ctx context.Context, cancelConnection bool) {
	if reservation == nil {
		return
	}
	if h.closeReservationHook != nil {
		h.closeReservationHook()
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), sessionCloseAllWaitTimeout)
		reservation.finish = cancel
	}
	if cancelConnection {
		reservation.connection.Cancel(ctx)
	}
	reservation.ctx = ctx
	if !reservation.handshake {
		h.completePendingReservation(reservation)
		return
	}

	h.closeMu.Lock()
	if _, pending := h.closePending[reservation.id]; !pending {
		h.closeMu.Unlock()
		return
	}
	if len(h.closeQueue) >= sessionCloseQueueCapacity {
		h.closeMu.Unlock()
		reservation.connection.Abort()
		h.completePendingReservation(reservation)
		return
	}
	delete(h.closePending, reservation.id)
	h.closeQueue = append(h.closeQueue, sessionCloseRequest{
		connection: reservation.connection, ctx: reservation.ctx,
		code: reservation.code, reason: reservation.reason, finish: reservation.finish,
	})
	if !h.closeWorker {
		h.closeWorker = true
		go h.runCloseWorker()
	}
	h.closeMu.Unlock()
}

func (h *processLocalSessionHub) completePendingReservation(reservation *sessionCloseReservation) {
	h.closeMu.Lock()
	if _, pending := h.closePending[reservation.id]; pending {
		delete(h.closePending, reservation.id)
		h.releaseCloseConnectionLocked(reservation.connection)
	}
	h.closeTerminalDoneLocked()
	h.closeMu.Unlock()
	if reservation.finish != nil {
		reservation.finish()
	}
}

func (h *processLocalSessionHub) releaseCloseConnectionLocked(connection SessionHubConnection) {
	if count := h.closeConnections[connection]; count > 1 {
		h.closeConnections[connection] = count - 1
	} else {
		delete(h.closeConnections, connection)
	}
}

func (h *processLocalSessionHub) runCloseWorker() {
	for {
		h.closeMu.Lock()
		if len(h.closeQueue) == 0 {
			h.closeWorker = false
			h.closeTerminalDoneLocked()
			h.closeMu.Unlock()
			return
		}
		request := h.closeQueue[0]
		copy(h.closeQueue, h.closeQueue[1:])
		h.closeQueue[len(h.closeQueue)-1] = sessionCloseRequest{}
		h.closeQueue = h.closeQueue[:len(h.closeQueue)-1]
		h.closeMu.Unlock()
		request.connection.WaitCancelled(request.ctx)
		_ = request.connection.Close(request.code, request.reason)
		h.closeMu.Lock()
		h.releaseCloseConnectionLocked(request.connection)
		h.closeTerminalDoneLocked()
		h.closeMu.Unlock()
		if request.finish != nil {
			request.finish()
		}
	}
}

func (h *processLocalSessionHub) finishCloseScheduling() {
	h.closeMu.Lock()
	h.closeTerminal = true
	h.closeTerminalDoneLocked()
	h.closeMu.Unlock()
}

func (h *processLocalSessionHub) closeTerminalDoneLocked() {
	if h.closeTerminal && h.closeRegistrations == 0 && len(h.closePending) == 0 && !h.closeWorker && len(h.closeQueue) == 0 {
		h.closeDoneOnce.Do(func() { close(h.closeDone) })
	}
}

func cloneSessionEnvelope(envelope SessionOutboundEnvelope) SessionOutboundEnvelope {
	envelope.Data = cloneRawMessage(envelope.Data)
	return envelope
}

func sessionPublicationDTOs(sessions []Session) []map[string]any {
	result := make([]map[string]any, 0, len(sessions))
	for i := range sessions {
		result = append(result, sessionInfoDTO(&sessions[i], "", nil, nil))
	}
	return result
}
