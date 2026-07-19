package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

const (
	websocketPingInterval          = 30 * time.Second
	websocketPingTimeout           = 10 * time.Second
	websocketWriteTimeout          = 5 * time.Second
	websocketSweepInterval         = 15 * time.Second
	websocketSupervisorStopTimeout = 500 * time.Millisecond
	websocketShutdownTimeout       = 400 * time.Millisecond

	websocketSessionPublicationMaxPendingUsers = 256
)

type websocketTransportTiming struct {
	PingInterval  time.Duration
	PingTimeout   time.Duration
	WriteTimeout  time.Duration
	SweepInterval time.Duration
}

func defaultWebSocketTransportTiming() websocketTransportTiming {
	return websocketTransportTiming{
		PingInterval:  websocketPingInterval,
		PingTimeout:   websocketPingTimeout,
		WriteTimeout:  websocketWriteTimeout,
		SweepInterval: websocketSweepInterval,
	}
}

type websocketTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type realWebSocketTicker struct {
	*time.Ticker
}

type websocketTimer interface {
	Chan() <-chan time.Time
	Stop() bool
}

type realWebSocketTimer struct {
	*time.Timer
}

func (t realWebSocketTimer) Chan() <-chan time.Time { return t.C }

func newWebSocketTimer(delay time.Duration) websocketTimer {
	return realWebSocketTimer{Timer: time.NewTimer(delay)}
}

func (t realWebSocketTicker) Chan() <-chan time.Time { return t.C }

func newWebSocketTicker(interval time.Duration) websocketTicker {
	return realWebSocketTicker{Ticker: time.NewTicker(interval)}
}

type websocketMessageDisposition uint8

const websocketMessageIgnored websocketMessageDisposition = iota + 1

type websocketMessageHandler func(context.Context, *websocketSessionService, SessionOutboundEnvelope) (websocketMessageDisposition, error)

func ignoreWebSocketMessage(context.Context, *websocketSessionService, SessionOutboundEnvelope) (websocketMessageDisposition, error) {
	return websocketMessageIgnored, nil
}

type websocketMessageError struct {
	Code websocket.StatusCode
	Err  error
}

func (e *websocketMessageError) Error() string { return e.Err.Error() }
func (e *websocketMessageError) Unwrap() error { return e.Err }

type websocketSubscriptionRegistry struct {
	mu                  sync.Mutex
	byUser              map[string]map[*websocketSessionService]struct{}
	pending             map[string]struct{}
	order               []string
	signal              chan struct{}
	ctx                 context.Context
	cancel              context.CancelFunc
	done                chan struct{}
	closed              bool
	started             bool
	dropped             uint64
	publish             func(context.Context, *websocketSessionService)
	supervisorCount     int
	supervisorsDone     chan struct{}
	supervisorsDoneOnce sync.Once
}

func newWebSocketSubscriptionRegistry() *websocketSubscriptionRegistry {
	return newWebSocketSubscriptionRegistryWithPublisher(func(ctx context.Context, service *websocketSessionService) {
		service.PublishIfSubscribed(ctx)
	})
}

func newWebSocketSubscriptionRegistryWithPublisher(publish func(context.Context, *websocketSessionService)) *websocketSubscriptionRegistry {
	ctx, cancel := context.WithCancel(context.Background())
	return &websocketSubscriptionRegistry{
		byUser:          make(map[string]map[*websocketSessionService]struct{}),
		pending:         make(map[string]struct{}),
		order:           make([]string, 0, websocketSessionPublicationMaxPendingUsers),
		signal:          make(chan struct{}, 1),
		ctx:             ctx,
		cancel:          cancel,
		done:            make(chan struct{}),
		publish:         publish,
		supervisorsDone: make(chan struct{}),
	}
}

func (r *websocketSubscriptionRegistry) Add(service *websocketSessionService) {
	if r == nil || service == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	services := r.byUser[service.identity.GatewayUserID]
	if services == nil {
		services = make(map[*websocketSessionService]struct{})
		r.byUser[service.identity.GatewayUserID] = services
	}
	services[service] = struct{}{}
	startSupervisor := service.ctx != nil && service.supervisorDone == nil
	if startSupervisor {
		r.supervisorCount++
		service.supervisorExited = r.supervisorFinished
		service.startSubscriptionSupervisor()
	}
	if !r.started {
		r.started = true
		go r.run()
	}
	r.mu.Unlock()
}

func (r *websocketSubscriptionRegistry) Remove(service *websocketSessionService) {
	if r == nil || service == nil {
		return
	}
	r.mu.Lock()
	services := r.byUser[service.identity.GatewayUserID]
	delete(services, service)
	if len(services) == 0 {
		delete(r.byUser, service.identity.GatewayUserID)
	}
	r.mu.Unlock()
}

func (r *websocketSubscriptionRegistry) PublishUser(gatewayUserID string) bool {
	if r == nil || gatewayUserID == "" {
		return false
	}
	r.mu.Lock()
	if r.closed || len(r.byUser[gatewayUserID]) == 0 {
		r.mu.Unlock()
		return false
	}
	if _, exists := r.pending[gatewayUserID]; exists {
		r.mu.Unlock()
		return true
	}
	if len(r.pending) >= websocketSessionPublicationMaxPendingUsers {
		r.dropped++
		r.mu.Unlock()
		return false
	}
	r.pending[gatewayUserID] = struct{}{}
	r.order = append(r.order, gatewayUserID)
	r.mu.Unlock()
	select {
	case r.signal <- struct{}{}:
	default:
	}
	return true
}

func (r *websocketSubscriptionRegistry) run() {
	defer close(r.done)
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.signal:
			for {
				services, ok := r.takeNext()
				if !ok {
					break
				}
				for _, service := range services {
					if r.ctx.Err() != nil {
						return
					}
					r.publish(r.ctx, service)
				}
			}
		}
	}
}

func (r *websocketSubscriptionRegistry) takeNext() ([]*websocketSessionService, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(r.order) == 0 {
		return nil, false
	}
	userID := r.order[0]
	copy(r.order, r.order[1:])
	r.order[len(r.order)-1] = ""
	r.order = r.order[:len(r.order)-1]
	delete(r.pending, userID)
	services := make([]*websocketSessionService, 0, len(r.byUser[userID]))
	for service := range r.byUser[userID] {
		services = append(services, service)
	}
	return services, true
}

func (r *websocketSubscriptionRegistry) BeginClose() (<-chan struct{}, <-chan struct{}) {
	if r == nil {
		done := make(chan struct{})
		close(done)
		return done, done
	}
	r.mu.Lock()
	if r.closed {
		supervisorsDone, schedulerDone := r.supervisorsDone, r.done
		r.mu.Unlock()
		return supervisorsDone, schedulerDone
	}
	r.closed = true
	r.pending = make(map[string]struct{})
	r.order = nil
	services := make([]*websocketSessionService, 0)
	for _, byService := range r.byUser {
		for service := range byService {
			services = append(services, service)
		}
	}
	r.cancel()
	started := r.started
	if !started {
		close(r.done)
	}
	if r.supervisorCount == 0 {
		r.supervisorsDoneOnce.Do(func() { close(r.supervisorsDone) })
	}
	supervisorsDone, schedulerDone := r.supervisorsDone, r.done
	r.mu.Unlock()
	for _, service := range services {
		service.CancelSupervisor()
	}
	return supervisorsDone, schedulerDone
}

func (r *websocketSubscriptionRegistry) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), websocketSupervisorStopTimeout)
	defer cancel()
	supervisorsDone, schedulerDone := r.BeginClose()
	for supervisorsDone != nil || schedulerDone != nil {
		select {
		case <-supervisorsDone:
			supervisorsDone = nil
		case <-schedulerDone:
			schedulerDone = nil
		case <-ctx.Done():
			return
		}
	}
}

func (r *websocketSubscriptionRegistry) supervisorFinished() {
	r.mu.Lock()
	if r.supervisorCount > 0 {
		r.supervisorCount--
	}
	if r.closed && r.supervisorCount == 0 {
		r.supervisorsDoneOnce.Do(func() { close(r.supervisorsDone) })
	}
	r.mu.Unlock()
}

func (r *websocketSubscriptionRegistry) stats() (pending int, dropped uint64, closed bool) {
	if r == nil {
		return 0, 0, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending), r.dropped, r.closed
}

func (s *Server) publishSessionsForUser(gatewayUserID string) {
	if s == nil || s.websocketSessions == nil {
		return
	}
	s.websocketSessions.PublishUser(gatewayUserID)
}

type websocketSessionService struct {
	server       *Server
	connection   *websocketHubConnection
	identity     SessionConnectionIdentity
	registration SessionHubRegistration
	viewer       Session
	remoteIP     string
	ctx          context.Context

	mu                    sync.Mutex
	subscriptionGen       uint64
	subscriptionLive      bool
	subscriptionInitial   time.Duration
	subscriptionInterval  time.Duration
	subscriptionRunCancel context.CancelFunc
	supervisorWake        chan struct{}
	supervisorCancel      context.CancelFunc
	supervisorDone        chan struct{}
	supervisorCloseOnce   sync.Once
	supervisorExited      func()
	projectSessions       func(context.Context) ([]any, error)
	publishMu             sync.Mutex
}

func (service *websocketSessionService) StartSessions(initial, interval time.Duration) {
	service.mu.Lock()
	service.subscriptionGen++
	service.subscriptionLive = true
	service.subscriptionInitial = initial
	service.subscriptionInterval = interval
	cancel := service.subscriptionRunCancel
	service.subscriptionRunCancel = nil
	service.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	service.wakeSubscriptionSupervisor()
}

func (service *websocketSessionService) StopSessions() {
	service.mu.Lock()
	service.subscriptionGen++
	service.subscriptionLive = false
	cancel := service.subscriptionRunCancel
	service.subscriptionRunCancel = nil
	service.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	service.wakeSubscriptionSupervisor()
}

func (service *websocketSessionService) Close() {
	if service == nil {
		return
	}
	service.supervisorCloseOnce.Do(func() {
		service.CancelSupervisor()
		if service.supervisorDone != nil {
			select {
			case <-service.supervisorDone:
			case <-service.ctx.Done():
			}
		}
	})
}

func (service *websocketSessionService) CancelSupervisor() {
	if service == nil {
		return
	}
	service.StopSessions()
	if service.supervisorCancel != nil {
		service.supervisorCancel()
	}
	service.wakeSubscriptionSupervisor()
}

func (service *websocketSessionService) subscribed(generation uint64) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.subscriptionLive && (generation == 0 || generation == service.subscriptionGen)
}

func (service *websocketSessionService) activeSubscriptionGeneration() (uint64, bool) {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.subscriptionGen, service.subscriptionLive
}

type websocketSubscriptionConfig struct {
	generation uint64
	active     bool
	initial    time.Duration
	interval   time.Duration
}

func (service *websocketSessionService) startSubscriptionSupervisor() {
	ctx, cancel := context.WithCancel(service.ctx)
	service.mu.Lock()
	service.supervisorWake = make(chan struct{}, 1)
	service.supervisorCancel = cancel
	service.supervisorDone = make(chan struct{})
	done := service.supervisorDone
	service.mu.Unlock()
	go service.runSubscriptionSupervisor(ctx, done)
}

func (service *websocketSessionService) wakeSubscriptionSupervisor() {
	if service == nil || service.supervisorWake == nil {
		return
	}
	select {
	case service.supervisorWake <- struct{}{}:
	default:
	}
}

func (service *websocketSessionService) subscriptionConfig() websocketSubscriptionConfig {
	service.mu.Lock()
	defer service.mu.Unlock()
	return websocketSubscriptionConfig{
		generation: service.subscriptionGen,
		active:     service.subscriptionLive,
		initial:    service.subscriptionInitial,
		interval:   service.subscriptionInterval,
	}
}

func (service *websocketSessionService) installSubscriptionRun(config websocketSubscriptionConfig, cancel context.CancelFunc) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.subscriptionGen != config.generation || !service.subscriptionLive {
		return false
	}
	service.subscriptionRunCancel = cancel
	return true
}

func (service *websocketSessionService) clearSubscriptionRun(generation uint64) {
	service.mu.Lock()
	if service.subscriptionGen == generation {
		service.subscriptionRunCancel = nil
	}
	service.mu.Unlock()
}

func (service *websocketSessionService) disableSubscription(generation uint64) {
	service.mu.Lock()
	if service.subscriptionGen == generation {
		service.subscriptionLive = false
		service.subscriptionRunCancel = nil
	}
	service.mu.Unlock()
}

func (service *websocketSessionService) runSubscriptionSupervisor(ctx context.Context, done chan struct{}) {
	defer func() {
		close(done)
		if service.supervisorExited != nil {
			service.supervisorExited()
		}
	}()
	var applied uint64
	for {
		config := service.subscriptionConfig()
		if config.generation == applied {
			select {
			case <-ctx.Done():
				return
			case <-service.supervisorWake:
				continue
			}
		}
		applied = config.generation
		if !config.active {
			continue
		}

		runCtx, cancel := context.WithCancel(ctx)
		if !service.installSubscriptionRun(config, cancel) {
			cancel()
			continue
		}
		completed := service.runSubscriptionConfig(runCtx, config)
		cancel()
		service.clearSubscriptionRun(config.generation)
		if !completed && runCtx.Err() == nil && ctx.Err() == nil {
			service.disableSubscription(config.generation)
		}
	}
}

func (service *websocketSessionService) runSubscriptionConfig(ctx context.Context, config websocketSubscriptionConfig) bool {
	if config.initial > 0 {
		timer := service.newTimer(config.initial)
		select {
		case <-ctx.Done():
			timer.Stop()
			return true
		case <-timer.Chan():
		}
	}
	if !service.publishSessionsWithContext(ctx, config.generation) {
		return ctx.Err() != nil
	}
	for {
		timer := service.newTimer(config.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return true
		case <-timer.Chan():
			if !service.publishSessionsWithContext(ctx, config.generation) {
				return ctx.Err() != nil
			}
		}
	}
}

func (service *websocketSessionService) newTimer(delay time.Duration) websocketTimer {
	newTimer := service.server.websocketNewTimer
	if newTimer == nil {
		newTimer = newWebSocketTimer
	}
	return newTimer(delay)
}

func (service *websocketSessionService) PublishIfSubscribed(ctx context.Context) {
	if generation, active := service.activeSubscriptionGeneration(); active {
		service.publishSessionsWithContext(ctx, generation)
	}
}

func (service *websocketSessionService) publishSessionsWithContext(ctx context.Context, generation uint64) bool {
	service.publishMu.Lock()
	defer service.publishMu.Unlock()
	if !service.subscribed(generation) || service.ctx.Err() != nil || ctx.Err() != nil {
		return false
	}
	var items []any
	var err error
	if service.projectSessions != nil {
		items, err = service.projectSessions(ctx)
	} else {
		items, err = service.server.projectLocalSessions(ctx, &service.viewer, nil)
	}
	if err != nil {
		_ = service.connection.Close(websocket.StatusInternalError, "session projection failed")
		return false
	}
	data, err := json.Marshal(items)
	if err != nil {
		_ = service.connection.Close(websocket.StatusInternalError, "session projection encode failed")
		return false
	}
	if !service.subscribed(generation) || service.ctx.Err() != nil || ctx.Err() != nil {
		return false
	}
	result := service.server.sessionHub.PublishSessionsTo(service.identity, service.registration, data)
	return result.Status == SessionCommandEnqueued
}

type websocketHubConnection struct {
	conn              *websocket.Conn
	cancel            context.CancelFunc
	transportCancel   context.CancelFunc
	closeOnce         sync.Once
	abortOnce         sync.Once
	closedOnce        sync.Once
	closed            chan struct{}
	closeRequested    chan struct{}
	closeStarted      chan struct{}
	requestOnce       sync.Once
	startedOnce       sync.Once
	shutdownMu        sync.Mutex
	shutdownCtx       context.Context
	applicationMu     sync.Mutex
	applicationActive int
	applicationIdle   chan struct{}
}

func (c *websocketHubConnection) Cancel(ctx context.Context) {
	if c == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.requestOnce.Do(func() { close(c.closeRequested) })
	c.shutdownMu.Lock()
	if c.shutdownCtx == nil {
		c.shutdownCtx = ctx
	}
	c.shutdownMu.Unlock()
	c.cancel()
}

func (c *websocketHubConnection) Abort() {
	if c == nil {
		return
	}
	c.abortOnce.Do(func() {
		c.startedOnce.Do(func() { close(c.closeStarted) })
		c.cancel()
		if c.transportCancel != nil {
			c.transportCancel()
		}
		_ = c.conn.CloseNow()
		c.closedOnce.Do(func() { close(c.closed) })
	})
}

func (c *websocketHubConnection) BeginApplication() {
	c.applicationMu.Lock()
	if c.applicationActive == 0 {
		c.applicationIdle = make(chan struct{})
	}
	c.applicationActive++
	c.applicationMu.Unlock()
}

func (c *websocketHubConnection) EndApplication() {
	c.applicationMu.Lock()
	if c.applicationActive > 0 {
		c.applicationActive--
		if c.applicationActive == 0 {
			close(c.applicationIdle)
		}
	}
	c.applicationMu.Unlock()
}

func (c *websocketHubConnection) WaitCancelled(ctx context.Context) {
	c.applicationMu.Lock()
	idle := c.applicationIdle
	c.applicationMu.Unlock()
	if idle == nil {
		return
	}
	select {
	case <-idle:
	case <-ctx.Done():
	}
}

func (c *websocketHubConnection) WaitForCloseAttempt() {
	if c == nil {
		return
	}
	select {
	case <-c.closeRequested:
	default:
		return
	}
	c.shutdownMu.Lock()
	ctx := c.shutdownCtx
	c.shutdownMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-c.closeStarted:
	case <-ctx.Done():
		return
	}
	select {
	case <-c.closed:
	case <-ctx.Done():
	}
}

func (c *websocketHubConnection) Close(code websocket.StatusCode, reason string) error {
	var err error
	c.closeOnce.Do(func() {
		c.startedOnce.Do(func() { close(c.closeStarted) })
		err = c.conn.Close(code, reason)
		c.cancel()
		if c.transportCancel != nil {
			c.transportCancel()
		}
		c.closedOnce.Do(func() { close(c.closed) })
	})
	return err
}

func (s *Server) serveLocalWebSocket(w http.ResponseWriter, r *http.Request, rel string, session *Session, decision routeclass.Decision) {
	w.Header().Set("Cache-Control", "no-store")
	if !isUpgradeRequest(r) {
		w.Header().Set("Upgrade", "websocket")
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return
	}
	if !validWebSocketDeviceID(r, session.DeviceID) {
		writeWebSocketDeviceError(w, r, session.DeviceID)
		return
	}

	// coder/websocket accepts native clients without Origin and same-origin browser
	// clients, while rejecting cross-origin browser handshakes by default.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(sessionEnvelopeMaxBytes)

	identity := SessionConnectionIdentity{
		TokenHash:     session.GatewayTokenHash,
		PublicID:      session.PublicID,
		GatewayUserID: session.GatewayUserID,
	}
	transportCtx, transportCancel := context.WithCancel(context.Background())
	serviceCtx, serviceCancel := context.WithCancel(context.Background())
	hubConnection := &websocketHubConnection{
		conn: conn, cancel: serviceCancel, transportCancel: transportCancel, closed: make(chan struct{}),
		closeRequested: make(chan struct{}), closeStarted: make(chan struct{}),
	}
	registration, err := s.sessionHub.Register(identity, hubConnection)
	if err != nil {
		_ = hubConnection.Close(websocket.StatusInternalError, "session registration failed")
		return
	}
	s.noteSession(session, decision)
	service := &websocketSessionService{
		server:       s,
		connection:   hubConnection,
		identity:     identity,
		registration: registration,
		viewer:       *session,
		remoteIP:     remoteIP(r),
		ctx:          serviceCtx,
	}
	s.websocketSessions.Add(service)
	s.websocketSessions.PublishUser(identity.GatewayUserID)

	var cleanup sync.Once
	cleanupConnection := func() {
		cleanup.Do(func() {
			serviceCancel()
			transportCancel()
			service.Close()
			s.websocketSessions.Remove(service)
			s.sessionHub.Unregister(identity, registration)
			s.websocketSessions.PublishUser(identity.GatewayUserID)
		})
	}
	defer cleanupConnection()

	done := make(chan struct{}, 2)
	go func() {
		s.readWebSocketMessages(transportCtx, hubConnection, service)
		done <- struct{}{}
	}()
	go func() {
		s.writeWebSocketMessages(transportCtx, hubConnection, identity, registration)
		done <- struct{}{}
	}()

	<-done
	serviceCancel()
	transportCancel()
	<-done
	hubConnection.WaitForCloseAttempt()
}

func validWebSocketDeviceID(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	values := make([]string, 0, 1)
	for key, keyValues := range r.URL.Query() {
		if strings.EqualFold(key, "deviceId") {
			values = append(values, keyValues...)
		}
	}
	return len(values) == 1 && values[0] != "" && values[0] == expected
}

func writeWebSocketDeviceError(w http.ResponseWriter, r *http.Request, expected string) {
	count := 0
	value := ""
	for key, values := range r.URL.Query() {
		if !strings.EqualFold(key, "deviceId") {
			continue
		}
		count += len(values)
		if len(values) == 1 {
			value = values[0]
		}
	}
	if count == 1 && value != "" && expected != "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	http.Error(w, "invalid deviceId", http.StatusBadRequest)
}

func (s *Server) readWebSocketMessages(ctx context.Context, connection *websocketHubConnection, service *websocketSessionService) {
	for {
		_, payload, err := connection.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil && websocket.CloseStatus(err) == -1 {
				_ = connection.Close(websocket.StatusInternalError, "websocket read failed")
			}
			return
		}
		envelope, err := decodeSessionEnvelope(payload)
		if err != nil {
			var closeErr *sessionEnvelopeCloseError
			if errors.As(err, &closeErr) {
				_ = connection.Close(closeErr.Code, "invalid websocket message")
			} else {
				_ = connection.Close(websocket.StatusInternalError, "websocket decode failed")
			}
			return
		}
		handler := s.websocketOnMessage
		if handler == nil {
			handler = s.handleWebSocketMessage
		}
		connection.BeginApplication()
		_, handlerErr := handler(service.ctx, service, envelope)
		connection.EndApplication()
		if handlerErr != nil {
			if service.ctx.Err() != nil {
				return
			}
			var messageErr *websocketMessageError
			if errors.As(handlerErr, &messageErr) {
				_ = connection.Close(messageErr.Code, "websocket message rejected")
			} else {
				_ = connection.Close(websocket.StatusInternalError, "websocket message failed")
			}
			return
		}
		if service.ctx.Err() != nil {
			return
		}
	}
}

func (s *Server) handleWebSocketMessage(ctx context.Context, service *websocketSessionService, envelope SessionOutboundEnvelope) (websocketMessageDisposition, error) {
	switch envelope.MessageType {
	case "SessionsStart":
		initial, interval, err := parseSessionsSubscriptionData(envelope.Data)
		if err != nil {
			return websocketMessageIgnored, &websocketMessageError{Code: websocket.StatusInvalidFramePayloadData, Err: err}
		}
		service.StartSessions(initial, interval)
		return websocketMessageIgnored, nil
	case "SessionsStop":
		service.StopSessions()
		return websocketMessageIgnored, nil
	case "ReportPlaybackProgress":
		details, err := playbackDetailsFromWebSocketData(envelope.Data)
		if err != nil {
			return websocketMessageIgnored, &websocketMessageError{Code: websocket.StatusInvalidFramePayloadData, Err: err}
		}
		_, err = s.applyPlaybackReportCore(ctx, playbackReportApplication{
			Kind:       PlaybackReportProgress,
			Session:    &service.viewer,
			Details:    details,
			ReceivedAt: time.Now().UTC(),
			RemoteIP:   service.remoteIP,
			Method:     "WEBSOCKET",
			Path:       "/embywebsocket",
		})
		if err != nil {
			code := websocket.StatusInternalError
			if errors.Is(err, ErrBadRequest) {
				code = websocket.StatusInvalidFramePayloadData
			} else if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrNotFound) {
				code = websocket.StatusPolicyViolation
			}
			return websocketMessageIgnored, &websocketMessageError{Code: code, Err: err}
		}
		return websocketMessageIgnored, nil
	default:
		return websocketMessageIgnored, nil
	}
}

func parseSessionsSubscriptionData(data json.RawMessage) (time.Duration, time.Duration, error) {
	var value string
	if len(bytes.TrimSpace(data)) == 0 || json.Unmarshal(data, &value) != nil {
		return 0, 0, fmt.Errorf("%w: SessionsStart Data must be a string", ErrBadRequest)
	}
	if strings.HasPrefix(strings.TrimSpace(value), "\"") {
		var nested string
		if err := json.Unmarshal([]byte(value), &nested); err != nil {
			return 0, 0, fmt.Errorf("%w: invalid SessionsStart Data", ErrBadRequest)
		}
		value = nested
	}
	parts := strings.Split(value, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%w: SessionsStart Data requires two integers", ErrBadRequest)
	}
	initialMS, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: invalid initial delay", ErrBadRequest)
	}
	intervalMS, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: invalid interval", ErrBadRequest)
	}
	initialMS = min(max(initialMS, 0), 10000)
	intervalMS = min(max(intervalMS, 1000), 60000)
	return time.Duration(initialMS) * time.Millisecond, time.Duration(intervalMS) * time.Millisecond, nil
}

func playbackDetailsFromWebSocketData(data json.RawMessage) (playbackDetails, error) {
	payload := bytes.TrimSpace(data)
	if len(payload) == 0 {
		return playbackDetails{}, fmt.Errorf("%w: progress Data required", ErrBadRequest)
	}
	if payload[0] == '"' {
		var encoded string
		if err := json.Unmarshal(payload, &encoded); err != nil {
			return playbackDetails{}, fmt.Errorf("%w: invalid progress Data", ErrBadRequest)
		}
		payload = []byte(encoded)
	}
	return playbackDetailsFromJSONBytes(payload)
}

func (s *Server) writeWebSocketMessages(ctx context.Context, connection *websocketHubConnection, identity SessionConnectionIdentity, registration SessionHubRegistration) {
	newTicker := s.websocketNewTicker
	if newTicker == nil {
		newTicker = newWebSocketTicker
	}
	timing := s.websocketTiming
	if timing.PingInterval <= 0 || timing.PingTimeout <= 0 || timing.WriteTimeout <= 0 || timing.SweepInterval <= 0 {
		timing = defaultWebSocketTransportTiming()
	}
	pingTicker := newTicker(timing.PingInterval)
	sweepTicker := newTicker(timing.SweepInterval)
	defer pingTicker.Stop()
	defer sweepTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-registration.Notify:
			if !ok {
				select {
				case <-connection.closed:
				case <-ctx.Done():
				}
				return
			}
			for {
				envelope, ok := s.sessionHub.Dequeue(identity, registration)
				if !ok {
					break
				}
				payload, err := encodeSessionEnvelope(envelope)
				if err != nil {
					_ = connection.Close(websocket.StatusInternalError, "websocket encode failed")
					return
				}
				writeCtx, cancel := context.WithTimeout(ctx, timing.WriteTimeout)
				err = connection.conn.Write(writeCtx, websocket.MessageText, payload)
				cancel()
				if err != nil {
					if ctx.Err() == nil && websocket.CloseStatus(err) == -1 {
						_ = connection.Close(websocket.StatusInternalError, "websocket write failed")
					}
					return
				}
			}
		case <-pingTicker.Chan():
			pingCtx, cancel := context.WithTimeout(ctx, timing.PingTimeout)
			err := connection.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() == nil && websocket.CloseStatus(err) == -1 {
					_ = connection.Close(websocket.StatusInternalError, "websocket ping failed")
				}
				return
			}
		case <-sweepTicker.Chan():
			active, err := s.websocketSessionActive(ctx, identity)
			if err != nil {
				_ = connection.Close(websocket.StatusInternalError, "session lookup failed")
				return
			}
			if !active {
				s.removeTokenLeases(identity.TokenHash)
				_ = connection.Close(websocket.StatusPolicyViolation, sessionCloseReasonRevoked)
				return
			}
		}
	}
}

func (s *Server) websocketSessionActive(ctx context.Context, identity SessionConnectionIdentity) (bool, error) {
	session, err := s.sessions.FindSessionByTokenHash(ctx, identity.TokenHash)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if session == nil || !session.Active(time.Now().UTC()) {
		return false, nil
	}
	return session.GatewayTokenHash == identity.TokenHash &&
		session.GatewayUserID == identity.GatewayUserID &&
		session.PublicID == identity.PublicID, nil
}

// CloseWebSockets closes all process-local live connections during shutdown.
func (s *Server) CloseWebSockets() int {
	if s == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), websocketShutdownTimeout)
	defer cancel()
	var supervisorsDone, schedulerDone <-chan struct{}
	if s.websocketSessions != nil {
		supervisorsDone, schedulerDone = s.websocketSessions.BeginClose()
	}
	var hubDone <-chan struct{}
	count := 0
	if s.sessionHub == nil {
		hubDone = closedSignal()
	} else {
		count = s.sessionHub.BeginCloseAll(ctx)
		hubDone = s.sessionHub.CloseDone()
	}
	for supervisorsDone != nil || schedulerDone != nil || hubDone != nil {
		select {
		case <-supervisorsDone:
			supervisorsDone = nil
		case <-schedulerDone:
			schedulerDone = nil
		case <-hubDone:
			hubDone = nil
		case <-ctx.Done():
			if s.sessionHub != nil {
				s.sessionHub.AbortCloseAll()
			}
			return count
		}
	}
	return count
}

func closedSignal() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
