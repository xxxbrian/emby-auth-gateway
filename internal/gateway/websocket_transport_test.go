package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	websocketTestToken    = "gateway-token"
	websocketTestDeviceID = "device-1"
	websocketTestPublicID = "session-0123456789abcdef0123456789abcdef"
)

type manualWebSocketTicker struct {
	ch chan time.Time
}

func (t *manualWebSocketTicker) Chan() <-chan time.Time { return t.ch }
func (t *manualWebSocketTicker) Stop()                  {}

type manualWebSocketTimer struct {
	mu      sync.Mutex
	ch      chan time.Time
	stopped bool
}

func (t *manualWebSocketTimer) Chan() <-chan time.Time { return t.ch }
func (t *manualWebSocketTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasActive := !t.stopped
	t.stopped = true
	return wasActive
}

func (t *manualWebSocketTimer) Tick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	t.stopped = true
	select {
	case t.ch <- time.Now():
	default:
	}
}

type observingSessionHub struct {
	SessionHub
	registered   chan SessionHubPresence
	unregistered chan SessionConnectionIdentity
}

func newObservingSessionHub() *observingSessionHub {
	return &observingSessionHub{
		SessionHub:   NewProcessLocalSessionHub(),
		registered:   make(chan SessionHubPresence, 32),
		unregistered: make(chan SessionConnectionIdentity, 32),
	}
}

func (h *observingSessionHub) Unregister(identity SessionConnectionIdentity, registration SessionHubRegistration) {
	h.SessionHub.Unregister(identity, registration)
	h.unregistered <- identity
}

func (h *observingSessionHub) Register(identity SessionConnectionIdentity, connection SessionHubConnection) (SessionHubRegistration, error) {
	registration, err := h.SessionHub.Register(identity, connection)
	if err != nil {
		return SessionHubRegistration{}, err
	}
	presence, _ := h.SessionHub.Lookup(identity.TokenHash)
	h.registered <- presence
	return registration, nil
}

type manualWebSocketTickerFactory struct {
	mu           sync.Mutex
	tickers      map[time.Duration][]*manualWebSocketTicker
	timers       map[time.Duration][]*manualWebSocketTimer
	created      chan time.Duration
	timerCreated chan time.Duration
}

func newManualWebSocketTickerFactory() *manualWebSocketTickerFactory {
	return &manualWebSocketTickerFactory{
		tickers:      make(map[time.Duration][]*manualWebSocketTicker),
		timers:       make(map[time.Duration][]*manualWebSocketTimer),
		created:      make(chan time.Duration, 32),
		timerCreated: make(chan time.Duration, 64),
	}
}

func (f *manualWebSocketTickerFactory) NewTimer(delay time.Duration) websocketTimer {
	timer := &manualWebSocketTimer{ch: make(chan time.Time, 1)}
	f.mu.Lock()
	f.timers[delay] = append(f.timers[delay], timer)
	f.mu.Unlock()
	f.timerCreated <- delay
	return timer
}

func (f *manualWebSocketTickerFactory) New(interval time.Duration) websocketTicker {
	ticker := &manualWebSocketTicker{ch: make(chan time.Time, 1)}
	f.mu.Lock()
	f.tickers[interval] = append(f.tickers[interval], ticker)
	f.mu.Unlock()
	f.created <- interval
	return ticker
}

func (f *manualWebSocketTickerFactory) waitForTimer(t *testing.T, delay time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		select {
		case got := <-f.timerCreated:
			if got == delay {
				return
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for websocket timer %s", delay)
		}
	}
}

func (f *manualWebSocketTickerFactory) TickTimer(delay time.Duration) {
	f.mu.Lock()
	timers := append([]*manualWebSocketTimer(nil), f.timers[delay]...)
	f.mu.Unlock()
	for _, timer := range timers {
		timer.Tick()
	}
}

func (f *manualWebSocketTickerFactory) waitFor(t *testing.T, intervals ...time.Duration) {
	t.Helper()
	want := make(map[time.Duration]int, len(intervals))
	for _, interval := range intervals {
		want[interval]++
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for len(want) > 0 {
		select {
		case interval := <-f.created:
			if remaining := want[interval]; remaining > 1 {
				want[interval] = remaining - 1
			} else {
				delete(want, interval)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for websocket tickers: %#v", want)
		}
	}
}

func (f *manualWebSocketTickerFactory) Tick(interval time.Duration) {
	f.mu.Lock()
	tickers := append([]*manualWebSocketTicker(nil), f.tickers[interval]...)
	f.mu.Unlock()
	for _, ticker := range tickers {
		select {
		case ticker.ch <- time.Now():
		default:
		}
	}
}

func websocketTransportSession() *Session {
	now := time.Now().UTC()
	return &Session{
		GatewayTokenHash: HashToken(websocketTestToken),
		GatewayUserID:    "user-1",
		GatewayUsername:  "alice",
		SyntheticUserID:  "gateway-user",
		Client:           "TestClient",
		Device:           "TestDevice",
		DeviceID:         websocketTestDeviceID,
		Version:          "1.0",
		CreatedAt:        now,
		ExpiresAt:        now.Add(time.Hour),
		PublicID:         websocketTestPublicID,
		Capabilities:     defaultSessionCapabilities(),
		LastActivityAt:   now,
	}
}

func newWebSocketTransportServer(t *testing.T, manualTiming bool) (*Server, *httptest.Server, *MemoryStore, *countingRoundTripper, *manualWebSocketTickerFactory) {
	t.Helper()
	store := testStore("http://127.0.0.1:1/emby")
	store.Sessions[HashToken(websocketTestToken)] = websocketTransportSession()
	upstream := &countingRoundTripper{}
	hub := newObservingSessionHub()
	server := newServerWithSessionHub(Config{
		GatewayBasePath: "/emby",
		HTTPClient:      &http.Client{Transport: upstream},
	}, &noUpstreamLoadStore{MemoryStore: store}, hub)
	var tickers *manualWebSocketTickerFactory
	if manualTiming {
		tickers = newManualWebSocketTickerFactory()
		server.websocketNewTicker = tickers.New
		server.websocketNewTimer = tickers.NewTimer
	}
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		server.CloseWebSockets()
		httpServer.Close()
	})
	return server, httpServer, store, upstream, tickers
}

func websocketURL(httpServer *httptest.Server, rawQuery string) string {
	return "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/emby/embywebsocket?" + rawQuery
}

func validWebSocketQuery() string {
	return "api_key=" + websocketTestToken + "&deviceId=" + websocketTestDeviceID
}

func dialTestWebSocket(t *testing.T, httpServer *httptest.Server, opts *websocket.DialOptions) (*websocket.Conn, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, websocketURL(httpServer, validWebSocketQuery()), opts)
	if err != nil {
		t.Fatalf("dial websocket: %v (response=%v)", err, response)
	}
	return conn, response
}

func waitForWebSocketPresence(t *testing.T, hub SessionHub) SessionHubPresence {
	t.Helper()
	observing, ok := hub.(*observingSessionHub)
	if !ok {
		t.Fatal("test server does not use observing SessionHub")
	}
	select {
	case presence := <-observing.registered:
		return presence
	case <-time.After(2 * time.Second):
		t.Fatal("websocket registration did not become present")
	}
	return SessionHubPresence{}
}

func readWebSocketClose(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("expected websocket close")
	}
	return websocket.CloseStatus(err)
}

func TestLocalWebSocketAcceptsTextBinaryPingAndCleanClose(t *testing.T) {
	server, httpServer, _, upstream, tickers := newWebSocketTransportServer(t, true)
	received := make(chan SessionOutboundEnvelope, 2)
	server.websocketOnMessage = func(_ context.Context, _ *websocketSessionService, envelope SessionOutboundEnvelope) (websocketMessageDisposition, error) {
		received <- envelope
		return websocketMessageIgnored, nil
	}
	pingReceived := make(chan struct{}, 1)
	conn, response := dialTestWebSocket(t, httpServer, &websocket.DialOptions{
		OnPingReceived: func(context.Context, []byte) bool {
			pingReceived <- struct{}{}
			return true
		},
	})
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d", response.StatusCode)
	}
	if extension := response.Header.Get("Sec-WebSocket-Extensions"); extension != "" {
		t.Fatalf("compression extension negotiated: %q", extension)
	}
	waitForWebSocketPresence(t, server.sessionHub)
	tickers.waitFor(t, websocketPingInterval, websocketSweepInterval)

	for _, messageType := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := conn.Write(ctx, messageType, []byte(`{"MessageType":"FutureMessage","Data":[1,true]}`))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case envelope := <-received:
			if envelope.MessageType != "FutureMessage" {
				t.Fatalf("received envelope = %#v", envelope)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for ignored message callback")
		}
	}

	readDone := make(chan error, 1)
	go func() {
		_, _, err := conn.Read(context.Background())
		readDone <- err
	}()
	tickers.Tick(websocketPingInterval)
	select {
	case <-pingReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive server ping")
	}
	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not stop after clean close")
	}
	if upstream.hits != 0 {
		t.Fatalf("upstream hits = %d", upstream.hits)
	}
}

func TestLocalWebSocketHandshakeDenialsAndOriginPolicy(t *testing.T) {
	_, httpServer, store, upstream, _ := newWebSocketTransportServer(t, false)
	conflictingToken := strings.Repeat("A", 43)
	conflictingSession := websocketTransportSession()
	conflictingSession.GatewayTokenHash = HashToken(conflictingToken)
	conflictingSession.PublicID = "session-fedcba9876543210fedcba9876543210"
	store.Sessions[conflictingSession.GatewayTokenHash] = conflictingSession
	tests := []struct {
		name    string
		query   string
		header  http.Header
		status  int
		prepare func()
	}{
		{"missing device", "api_key=" + websocketTestToken, nil, http.StatusBadRequest, nil},
		{"empty device", "api_key=" + websocketTestToken + "&deviceId=", nil, http.StatusBadRequest, nil},
		{"duplicate device", "api_key=" + websocketTestToken + "&deviceId=" + websocketTestDeviceID + "&DeviceID=" + websocketTestDeviceID, nil, http.StatusBadRequest, nil},
		{"mismatched device", "api_key=" + websocketTestToken + "&DEVICEid=other", nil, http.StatusUnauthorized, nil},
		{"invalid token", "api_key=invalid&deviceId=" + websocketTestDeviceID, nil, http.StatusUnauthorized, nil},
		{"conflicting token", "api_key=" + websocketTestToken + "&token=" + conflictingToken + "&deviceId=" + websocketTestDeviceID, nil, http.StatusBadRequest, nil},
		{"cross origin browser", validWebSocketQuery(), http.Header{"Origin": []string{"https://evil.example"}}, http.StatusForbidden, nil},
		{"revoked session", validWebSocketQuery(), nil, http.StatusUnauthorized, func() { _ = store.RevokeSession(context.Background(), HashToken(websocketTestToken)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.prepare != nil {
				tt.prepare()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, response, err := websocket.Dial(ctx, websocketURL(httpServer, tt.query), &websocket.DialOptions{HTTPHeader: tt.header})
			if conn != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "unexpected")
			}
			if err == nil || response == nil || response.StatusCode != tt.status {
				t.Fatalf("dial err=%v response=%v, want status %d", err, response, tt.status)
			}
		})
		if tt.name == "cross origin browser" {
			// Restore an active session before the revocation case.
			store.Sessions[HashToken(websocketTestToken)] = websocketTransportSession()
		}
	}
	if upstream.hits != 0 {
		t.Fatalf("upstream hits = %d", upstream.hits)
	}
}

func TestLocalWebSocketExpiredAndMalformedCredentialDenials(t *testing.T) {
	_, httpServer, store, upstream, _ := newWebSocketTransportServer(t, false)
	expired := websocketTransportSession()
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	store.Sessions[HashToken(websocketTestToken)] = expired
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	conn, response, err := websocket.Dial(ctx, websocketURL(httpServer, validWebSocketQuery()), nil)
	cancel()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "unexpected")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired dial err=%v response=%v", err, response)
	}

	req := httptest.NewRequest(http.MethodGet, httpServer.URL+"/emby/embywebsocket?api_key=%ZZ&deviceId="+websocketTestDeviceID, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	recorder := httptest.NewRecorder()
	server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: &http.Client{Transport: upstream}}, &noUpstreamLoadStore{MemoryStore: store})
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("malformed query status = %d", recorder.Code)
	}
	if upstream.hits != 0 {
		t.Fatalf("upstream hits = %d", upstream.hits)
	}
}

func TestLocalWebSocketMalformedNonUTF8AndOversizeCloseCodes(t *testing.T) {
	tests := []struct {
		name    string
		typeID  websocket.MessageType
		payload []byte
		code    websocket.StatusCode
	}{
		{"malformed", websocket.MessageText, []byte(`{"MessageType":`), websocket.StatusInvalidFramePayloadData},
		{"non UTF-8 binary", websocket.MessageBinary, []byte{0xff, 0xfe}, websocket.StatusInvalidFramePayloadData},
		{"oversize", websocket.MessageBinary, []byte(strings.Repeat("x", sessionEnvelopeMaxBytes+1)), websocket.StatusMessageTooBig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
			conn, _ := dialTestWebSocket(t, httpServer, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := conn.Write(ctx, tt.typeID, tt.payload)
			cancel()
			if err != nil && websocket.CloseStatus(err) != tt.code {
				t.Fatalf("write error = %v", err)
			}
			if got := readWebSocketClose(t, conn); got != tt.code {
				t.Fatalf("close code = %d, want %d", got, tt.code)
			}
		})
	}
}

func TestLocalWebSocketWriterDeliveryOrder(t *testing.T) {
	server, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
	conn, _ := dialTestWebSocket(t, httpServer, nil)
	waitForWebSocketPresence(t, server.sessionHub)
	identity := SessionConnectionIdentity{TokenHash: HashToken(websocketTestToken), GatewayUserID: "user-1", PublicID: websocketTestPublicID}
	for _, id := range []string{"one", "two", "three"} {
		result := server.sessionHub.Enqueue(identity, SessionOutboundEnvelope{MessageType: "GeneralCommand", MessageID: id, Data: json.RawMessage(`{"Name":"GoHome"}`)})
		if result.Status != SessionCommandEnqueued {
			t.Fatalf("enqueue %s = %#v", id, result)
		}
	}
	for _, want := range []string{"one", "two", "three"} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		messageType, payload, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.MessageText {
			t.Fatalf("message type = %v", messageType)
		}
		envelope, err := decodeSessionEnvelope(payload)
		if err != nil || envelope.MessageID != want {
			t.Fatalf("envelope = %#v, err=%v; want %s", envelope, err, want)
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestWebSocketHubConnectionCancelIsImmediateAndNonBlocking(t *testing.T) {
	called := make(chan struct{})
	connection := &websocketHubConnection{
		cancel:         func() { close(called) },
		closeRequested: make(chan struct{}),
		closeStarted:   make(chan struct{}),
		closed:         make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	connection.Cancel(ctx)
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("Cancel blocked for %s", elapsed)
	}
	select {
	case <-called:
	default:
		t.Fatal("Cancel did not invoke c.cancel synchronously")
	}
}

func TestLocalWebSocketBlockedHandlerObservesServiceCancellationBeforeClose(t *testing.T) {
	server, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
	started := make(chan struct{})
	events := make(chan string, 2)
	var mutation atomic.Int32
	server.websocketOnMessage = func(ctx context.Context, _ *websocketSessionService, _ SessionOutboundEnvelope) (websocketMessageDisposition, error) {
		close(started)
		<-ctx.Done()
		if ctx.Err() == nil {
			mutation.Add(1)
		}
		events <- "handler-canceled"
		return websocketMessageIgnored, ctx.Err()
	}
	conn, _ := dialTestWebSocket(t, httpServer, nil)
	waitForWebSocketPresence(t, server.sessionHub)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"FutureMessage"}`)); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	closeStatus := make(chan websocket.StatusCode, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, err := conn.Read(ctx)
		closeStatus <- websocket.CloseStatus(err)
	}()
	server.Close()
	status := <-closeStatus
	events <- fmt.Sprintf("close-%d", status)
	first := <-events
	second := <-events
	if first != "handler-canceled" || second != fmt.Sprintf("close-%d", websocket.StatusGoingAway) {
		t.Fatalf("handler/close order = %q then %q", first, second)
	}
	if mutation.Load() != 0 {
		t.Fatal("handler mutated state after service cancellation")
	}
}

func TestLocalWebSocketReconnectReplacementAndGenerationSafety(t *testing.T) {
	server, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
	oldConn, _ := dialTestWebSocket(t, httpServer, nil)
	oldPresence := waitForWebSocketPresence(t, server.sessionHub)
	newConn, _ := dialTestWebSocket(t, httpServer, nil)
	newPresence := waitForWebSocketPresence(t, server.sessionHub)
	if newPresence.Generation <= oldPresence.Generation {
		t.Fatalf("generations old=%d new=%d", oldPresence.Generation, newPresence.Generation)
	}
	if got := readWebSocketClose(t, oldConn); got != websocket.StatusGoingAway {
		t.Fatalf("replacement close = %d, want 1001", got)
	}

	identity := SessionConnectionIdentity{TokenHash: HashToken(websocketTestToken), GatewayUserID: "user-1", PublicID: websocketTestPublicID}
	result := server.sessionHub.Enqueue(identity, SessionOutboundEnvelope{MessageType: "GeneralCommand", MessageID: "new"})
	if result.Status != SessionCommandEnqueued {
		t.Fatalf("enqueue after replacement = %#v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, payload, err := newConn.Read(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := decodeSessionEnvelope(payload)
	if err != nil || envelope.MessageID != "new" {
		t.Fatalf("replacement delivery = %#v, %v", envelope, err)
	}
	_ = newConn.Close(websocket.StatusNormalClosure, "done")
}

func TestLocalWebSocketShutdownAndRevocationCloseCodes(t *testing.T) {
	t.Run("server close", func(t *testing.T) {
		server, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
		conn, _ := dialTestWebSocket(t, httpServer, nil)
		waitForWebSocketPresence(t, server.sessionHub)
		server.Close()
		if got := readWebSocketClose(t, conn); got != websocket.StatusGoingAway {
			t.Fatalf("Server.Close status = %d, want 1001", got)
		}
	})

	t.Run("shutdown", func(t *testing.T) {
		server, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
		conn, _ := dialTestWebSocket(t, httpServer, nil)
		waitForWebSocketPresence(t, server.sessionHub)
		if got := server.CloseWebSockets(); got != 1 {
			t.Fatalf("CloseWebSockets = %d", got)
		}
		if got := readWebSocketClose(t, conn); got != websocket.StatusGoingAway {
			t.Fatalf("shutdown close = %d", got)
		}
	})

	t.Run("revocation sweep", func(t *testing.T) {
		server, httpServer, store, _, tickers := newWebSocketTransportServer(t, true)
		conn, _ := dialTestWebSocket(t, httpServer, nil)
		waitForWebSocketPresence(t, server.sessionHub)
		tickers.waitFor(t, websocketPingInterval, websocketSweepInterval)
		if err := store.RevokeSession(context.Background(), HashToken(websocketTestToken)); err != nil {
			t.Fatal(err)
		}
		tickers.Tick(websocketSweepInterval)
		if got := readWebSocketClose(t, conn); got != websocket.StatusPolicyViolation {
			t.Fatalf("revocation close = %d", got)
		}
	})
}

func TestLocalWebSocketMessageHandlerFailureClosesInternalError(t *testing.T) {
	server, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
	server.websocketOnMessage = func(context.Context, *websocketSessionService, SessionOutboundEnvelope) (websocketMessageDisposition, error) {
		return websocketMessageIgnored, errors.New("failed")
	}
	conn, _ := dialTestWebSocket(t, httpServer, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err := conn.Write(ctx, websocket.MessageText, []byte(`{"MessageType":"FutureMessage"}`))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if got := readWebSocketClose(t, conn); got != websocket.StatusInternalError {
		t.Fatalf("internal close = %d", got)
	}
}

func TestLocalWebSocketDeviceKeyIsCaseInsensitiveButExact(t *testing.T) {
	_, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
	query := url.Values{"api_key": {websocketTestToken}, "DeViCeId": {websocketTestDeviceID}}.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	conn, response, err := websocket.Dial(ctx, websocketURL(httpServer, query), nil)
	cancel()
	if err != nil {
		t.Fatalf("case-insensitive deviceId dial: %v response=%v", err, response)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestLocalWebSocketAllowsSameOriginBrowser(t *testing.T) {
	_, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	conn, response, err := websocket.Dial(ctx, websocketURL(httpServer, validWebSocketQuery()), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{httpServer.URL}},
	})
	cancel()
	if err != nil {
		t.Fatalf("same-origin dial: %v response=%v", err, response)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "done")
}

func TestServerWebSocketHubInjection(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	store := testStore("http://127.0.0.1:1/emby")
	server := newServerWithSessionHub(Config{GatewayBasePath: "/emby"}, store, hub)
	if server.sessionHub != hub {
		t.Fatal("server did not retain injected SessionHub")
	}
}
