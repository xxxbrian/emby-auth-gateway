package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
)

func writeWebSocketEnvelope(t *testing.T, conn *websocket.Conn, messageType string, data any) {
	t.Helper()
	envelope := map[string]any{"MessageType": messageType}
	if data != nil {
		envelope["Data"] = data
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
}

func readSessionsEnvelope(t *testing.T, conn *websocket.Conn) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	messageType, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("message type = %v, want text", messageType)
	}
	envelope, err := decodeSessionEnvelope(payload)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.MessageType != "Sessions" {
		t.Fatalf("MessageType = %q, want Sessions", envelope.MessageType)
	}
	var sessions []map[string]any
	if err := json.Unmarshal(envelope.Data, &sessions); err != nil {
		t.Fatal(err)
	}
	return sessions
}

func sessionByID(t *testing.T, sessions []map[string]any, publicID string) map[string]any {
	t.Helper()
	for _, session := range sessions {
		if session["Id"] == publicID {
			return session
		}
	}
	t.Fatalf("session %q not found in %#v", publicID, sessions)
	return nil
}

func setWebSocketTestCapabilities(t *testing.T, store *MemoryStore, tokenHash string, commands []string, media []string, supportsMedia bool) {
	t.Helper()
	doc := map[string]any{
		"PlayableMediaTypes":   media,
		"SupportedCommands":    commands,
		"SupportsMediaControl": supportsMedia,
		"SupportsSync":         false,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := ParseSessionCapabilities(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateSessionCapabilities(context.Background(), tokenHash, caps, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketSessionsStartImmediateProjectionAndDisconnectPresence(t *testing.T) {
	server, httpServer, store, upstream, tickers := newWebSocketTransportServer(t, true)
	setWebSocketTestCapabilities(t, store, HashToken(websocketTestToken), []string{"MoveUp"}, nil, false)
	foreign := websocketTransportSession()
	foreign.GatewayTokenHash = HashToken("foreign-token")
	foreign.GatewayUserID = "user-2"
	foreign.SyntheticUserID = "gateway-user-2"
	foreign.PublicID = "session-11111111111111111111111111111111"
	store.Sessions[foreign.GatewayTokenHash] = foreign

	conn, _ := dialTestWebSocket(t, httpServer, nil)
	waitForWebSocketPresence(t, server.sessionHub)
	writeWebSocketEnvelope(t, conn, "SessionsStart", "0,1000")
	sessions := readSessionsEnvelope(t, conn)
	if len(sessions) != 1 {
		t.Fatalf("Sessions count = %d, want same-user session only: %#v", len(sessions), sessions)
	}
	current := sessionByID(t, sessions, websocketTestPublicID)
	if supports, _ := current["SupportsRemoteControl"].(bool); !supports {
		t.Fatalf("live approved session SupportsRemoteControl = %#v", current["SupportsRemoteControl"])
	}
	tickers.waitForTimer(t, time.Second)

	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	observing := server.sessionHub.(*observingSessionHub)
	select {
	case <-observing.unregistered:
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not unregister")
	}
	items, err := server.projectLocalSessions(context.Background(), websocketTransportSession(), nil)
	if err != nil {
		t.Fatal(err)
	}
	disconnected := sessionByID(t, mapsFromAny(t, items), websocketTestPublicID)
	if supports, _ := disconnected["SupportsRemoteControl"].(bool); supports {
		t.Fatal("disconnected session remained remotely controllable")
	}
	if upstream.hits != 0 {
		t.Fatalf("upstream hits = %d", upstream.hits)
	}
}

func mapsFromAny(t *testing.T, items []any) []map[string]any {
	t.Helper()
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		value, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("projection item = %T", item)
		}
		result = append(result, value)
	}
	return result
}

func waitForSubscriptionState(t *testing.T, server *Server, active bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server.websocketSessions.mu.Lock()
		var matched bool
		for _, services := range server.websocketSessions.byUser {
			for service := range services {
				service.mu.Lock()
				matched = service.subscriptionLive == active
				service.mu.Unlock()
				if matched {
					break
				}
			}
		}
		server.websocketSessions.mu.Unlock()
		if matched {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("subscription active=%v was not observed", active)
}

func TestWebSocketSessionsTimersStopClampAndReconnectCancellation(t *testing.T) {
	t.Run("delayed periodic and stop", func(t *testing.T) {
		server, httpServer, _, _, tickers := newWebSocketTransportServer(t, true)
		conn, _ := dialTestWebSocket(t, httpServer, nil)
		writeWebSocketEnvelope(t, conn, "SessionsStart", "500,1000")
		tickers.waitForTimer(t, 500*time.Millisecond)
		tickers.TickTimer(500 * time.Millisecond)
		readSessionsEnvelope(t, conn)
		tickers.waitForTimer(t, time.Second)
		tickers.TickTimer(time.Second)
		readSessionsEnvelope(t, conn)
		tickers.waitForTimer(t, time.Second)
		writeWebSocketEnvelope(t, conn, "SessionsStop", nil)
		waitForSubscriptionState(t, server, false)
		tickers.TickTimer(time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, _, err := conn.Read(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SessionsStop read error = %v, want deadline", err)
		}
	})

	t.Run("clamped", func(t *testing.T) {
		_, httpServer, _, _, tickers := newWebSocketTransportServer(t, true)
		conn, _ := dialTestWebSocket(t, httpServer, nil)
		writeWebSocketEnvelope(t, conn, "SessionsStart", "-50,999999")
		readSessionsEnvelope(t, conn)
		tickers.waitForTimer(t, 60*time.Second)
	})

	t.Run("reconnect cancels old timer", func(t *testing.T) {
		_, httpServer, _, _, tickers := newWebSocketTransportServer(t, true)
		oldConn, _ := dialTestWebSocket(t, httpServer, nil)
		writeWebSocketEnvelope(t, oldConn, "SessionsStart", "500,1000")
		tickers.waitForTimer(t, 500*time.Millisecond)
		newConn, _ := dialTestWebSocket(t, httpServer, nil)
		if got := readWebSocketClose(t, oldConn); got != websocket.StatusGoingAway {
			t.Fatalf("old close = %d", got)
		}
		tickers.TickTimer(500 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, _, err := newConn.Read(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("replacement received old subscription publication: %v", err)
		}
	})
}

func TestWebSocketSubscriptionSupervisorCoalescesStartsAndLatestWins(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	registration, _ := registerSession(t, hub, identity)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timers := newManualWebSocketTickerFactory()
	started := make(chan struct{})
	release := make(chan struct{})
	projected := make(chan struct{}, 4)
	var calls atomic.Int32
	service := &websocketSessionService{
		server:       &Server{sessionHub: hub, websocketNewTimer: timers.NewTimer},
		identity:     identity,
		registration: registration,
		ctx:          ctx,
		projectSessions: func(context.Context) ([]any, error) {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			} else {
				projected <- struct{}{}
			}
			return []any{}, nil
		},
	}
	service.startSubscriptionSupervisor()
	service.StartSessions(0, time.Second)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not begin projection")
	}
	baseline := runtime.NumGoroutine()
	for i := 0; i < 10_000; i++ {
		service.StartSessions(time.Duration(i%10+1)*time.Millisecond, time.Duration(i%20+1)*time.Second)
	}
	latestInitial := 777 * time.Millisecond
	latestInterval := 3 * time.Second
	service.StartSessions(latestInitial, latestInterval)
	if pending := len(service.supervisorWake); pending > 1 {
		t.Fatalf("supervisor wake depth = %d", pending)
	}
	if growth := runtime.NumGoroutine() - baseline; growth > 1 {
		t.Fatalf("SessionsStart goroutine growth = %d", growth)
	}
	close(release)
	timers.waitForTimer(t, latestInitial)
	timers.TickTimer(latestInitial)
	select {
	case <-projected:
	case <-time.After(2 * time.Second):
		t.Fatal("latest subscription config did not publish")
	}
	timers.waitForTimer(t, latestInterval)
	service.StopSessions()
	timers.TickTimer(latestInterval)
	select {
	case <-projected:
		t.Fatal("SessionsStop allowed another projection")
	case <-time.After(50 * time.Millisecond):
	}
	service.Close()
	select {
	case <-service.supervisorDone:
	default:
		t.Fatal("subscription supervisor did not terminate")
	}
	hub.CloseAll()
}

func TestWebSocketSubscriptionSupervisorStopCancelsBlockedProjection(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	registration, _ := registerSession(t, hub, identity)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	service := &websocketSessionService{
		server:       &Server{sessionHub: hub},
		identity:     identity,
		registration: registration,
		ctx:          ctx,
		projectSessions: func(context.Context) ([]any, error) {
			close(started)
			<-release
			return []any{}, nil
		},
	}
	service.startSubscriptionSupervisor()
	service.StartSessions(0, time.Second)
	<-started
	service.StopSessions()
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, active := service.activeSubscriptionGeneration(); !active {
			break
		}
		runtime.Gosched()
	}
	if _, ok := hub.Dequeue(identity, registration); ok {
		t.Fatal("canceled blocked projection was published")
	}
	service.Close()
	hub.CloseAll()
}

func TestServerCloseUsesOneDeadlineForBlockedSupervisors(t *testing.T) {
	const connections = 8
	before := runtime.NumGoroutine()
	server := NewServer(Config{GatewayBasePath: "/emby"}, NewMemoryStore())
	started := make(chan struct{}, connections)
	release := make(chan struct{})
	services := make([]*websocketSessionService, 0, connections)
	fakes := make([]*fakeSessionHubConnection, 0, connections)
	var orderMu sync.Mutex
	cancelCount := 0
	firstCloseCancelCount := -1

	for i := 0; i < connections; i++ {
		identity := sessionIdentity(fmt.Sprintf("token-%d", i), "user", fmt.Sprintf("public-%d", i))
		connection := newFakeSessionHubConnection()
		connection.onCancel = func() {
			orderMu.Lock()
			cancelCount++
			orderMu.Unlock()
		}
		connection.onClose = func() {
			orderMu.Lock()
			if firstCloseCancelCount < 0 {
				firstCloseCancelCount = cancelCount
			}
			orderMu.Unlock()
		}
		registration, err := server.sessionHub.Register(identity, connection)
		if err != nil {
			t.Fatal(err)
		}
		service := &websocketSessionService{
			server: server, identity: identity, registration: registration, ctx: context.Background(),
			projectSessions: func(context.Context) ([]any, error) {
				started <- struct{}{}
				<-release
				return []any{}, nil
			},
		}
		server.websocketSessions.Add(service)
		service.StartSessions(0, time.Second)
		services = append(services, service)
		fakes = append(fakes, connection)
	}
	for i := 0; i < connections; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("projector %d did not block", i)
		}
	}

	startedAt := time.Now()
	server.Close()
	elapsed := time.Since(startedAt)
	t.Logf("eight blocked supervisors closed in %s", elapsed)
	if elapsed < websocketShutdownTimeout-100*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("Server.Close duration = %s, want one %s shutdown budget and <=500ms total", elapsed, websocketShutdownTimeout)
	}
	orderMu.Lock()
	gotCancels, cancelsAtFirstClose := cancelCount, firstCloseCancelCount
	orderMu.Unlock()
	if gotCancels != connections || cancelsAtFirstClose != connections {
		t.Fatalf("cancel/close ordering = cancels %d, first close after %d; want all %d canceled first", gotCancels, cancelsAtFirstClose, connections)
	}
	for i, connection := range fakes {
		connection.mu.Lock()
		aborts := connection.abortCount
		connection.mu.Unlock()
		if aborts != 1 {
			t.Fatalf("connection %d aborts = %d, want forced abort at deadline", i, aborts)
		}
	}

	close(release)
	select {
	case <-server.websocketSessions.supervisorsDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked supervisors did not terminate after release")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("goroutines did not drain: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestWebSocketSessionsStartMalformedCloses1007(t *testing.T) {
	_, httpServer, _, _, _ := newWebSocketTransportServer(t, false)
	for _, data := range []any{"one", "1,2,3", "x,1000", map[string]any{"Initial": 0}} {
		conn, _ := dialTestWebSocket(t, httpServer, nil)
		writeWebSocketEnvelope(t, conn, "SessionsStart", data)
		if got := readWebSocketClose(t, conn); got != websocket.StatusInvalidFramePayloadData {
			t.Fatalf("Data %#v close = %d, want 1007", data, got)
		}
	}
}

func TestSessionSupportsRemoteControlPolicy(t *testing.T) {
	session := websocketTransportSession()
	live := func(identity SessionConnectionIdentity) bool {
		return identity.TokenHash == session.GatewayTokenHash && identity.PublicID == session.PublicID && identity.GatewayUserID == session.GatewayUserID
	}
	if sessionSupportsRemoteControl(session, live) {
		t.Fatal("empty capabilities enabled remote control")
	}
	session.Capabilities.SupportedCommands = []string{"UnapprovedFutureCommand"}
	if sessionSupportsRemoteControl(session, live) {
		t.Fatal("unapproved command enabled remote control")
	}
	session.Capabilities.SupportedCommands = []string{"Pause"}
	if sessionSupportsRemoteControl(session, live) {
		t.Fatal("playstate name without media-control capability enabled remote control")
	}
	session.Capabilities.SupportedCommands = []string{"mOvEuP"}
	if !sessionSupportsRemoteControl(session, live) {
		t.Fatal("approved general command did not enable remote control")
	}
	session.Capabilities.SupportedCommands = nil
	session.Capabilities.SupportsMediaControl = true
	if !sessionSupportsRemoteControl(session, live) {
		t.Fatal("media control did not enable approved playstate commands")
	}
	session.Capabilities.PlayableMediaTypes = []string{"Video"}
	if !sessionSupportsRemoteControl(session, live) {
		t.Fatal("media control with playable media did not enable play commands")
	}
	if sessionSupportsRemoteControl(session, func(SessionConnectionIdentity) bool { return false }) {
		t.Fatal("disconnected session enabled remote control")
	}
}

func TestSessionCommandCapabilityPredicatesShareServiceAllowlists(t *testing.T) {
	if !sessionCapabilitiesSupportGeneral(SessionCapabilities{SupportedCommands: []string{"MoveUp"}}) {
		t.Fatal("MoveUp was not recognized from the GeneralCommand allowlist")
	}
	if sessionCapabilitiesSupportGeneral(SessionCapabilities{SupportedCommands: []string{"Pause"}}) {
		t.Fatal("Pause was incorrectly treated as an approved GeneralCommand")
	}
	if !sessionCapabilitiesSupportPlaystate(SessionCapabilities{SupportsMediaControl: true}) {
		t.Fatal("media control did not enable playstate delivery")
	}
	if sessionCapabilitiesSupportPlay(SessionCapabilities{SupportsMediaControl: true}) {
		t.Fatal("play delivery was enabled without playable media")
	}
	if !sessionCapabilitiesSupportPlay(SessionCapabilities{SupportsMediaControl: true, PlayableMediaTypes: []string{"Video"}}) {
		t.Fatal("play delivery was not enabled with playable media")
	}
}

func TestWebSocketSessionPublicationSchedulerHighRateCoalescingAndFairness(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	published := make(chan string, 8)
	var blockFirst sync.Once
	registry := newWebSocketSubscriptionRegistryWithPublisher(func(_ context.Context, service *websocketSessionService) {
		published <- service.identity.GatewayUserID
		if service.identity.GatewayUserID == "user-1" {
			blockFirst.Do(func() {
				close(started)
				<-release
			})
		}
	})
	registry.Add(&websocketSessionService{identity: SessionConnectionIdentity{GatewayUserID: "user-1"}})
	registry.Add(&websocketSessionService{identity: SessionConnectionIdentity{GatewayUserID: "user-2"}})
	if !registry.PublishUser("user-1") {
		t.Fatal("initial publication was rejected")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("publication worker did not start")
	}
	baselineGoroutines := runtime.NumGoroutine()
	if !registry.PublishUser("user-2") {
		t.Fatal("second user publication was rejected")
	}
	for i := 0; i < 10_000; i++ {
		if !registry.PublishUser("user-1") {
			t.Fatal("coalesced publication was rejected")
		}
	}
	pending, dropped, closed := registry.stats()
	if pending != 2 || dropped != 0 || closed {
		t.Fatalf("scheduler stats = pending %d dropped %d closed %v", pending, dropped, closed)
	}
	if growth := runtime.NumGoroutine() - baselineGoroutines; growth > 1 {
		t.Fatalf("goroutine growth = %d, want at most one scheduler worker", growth)
	}
	close(release)

	want := []string{"user-1", "user-2", "user-1"}
	for i, expected := range want {
		select {
		case got := <-published:
			if got != expected {
				t.Fatalf("publication %d = %q, want %q", i, got, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for publication %d", i)
		}
	}
	select {
	case extra := <-published:
		t.Fatalf("high-rate events were not coalesced, extra publication %q", extra)
	case <-time.After(50 * time.Millisecond):
	}
	registry.Close()
}

func TestWebSocketSessionPublicationSchedulerBoundedOverflowAndShutdown(t *testing.T) {
	t.Run("drop newest unique user at capacity", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		published := make(chan string, websocketSessionPublicationMaxPendingUsers+2)
		var blockFirst sync.Once
		registry := newWebSocketSubscriptionRegistryWithPublisher(func(_ context.Context, service *websocketSessionService) {
			published <- service.identity.GatewayUserID
			if service.identity.GatewayUserID == "block" {
				blockFirst.Do(func() {
					close(started)
					<-release
				})
			}
		})
		registry.Add(&websocketSessionService{identity: SessionConnectionIdentity{GatewayUserID: "block"}})
		registry.PublishUser("block")
		<-started
		for i := 0; i <= websocketSessionPublicationMaxPendingUsers; i++ {
			userID := fmt.Sprintf("user-%03d", i)
			registry.Add(&websocketSessionService{identity: SessionConnectionIdentity{GatewayUserID: userID}})
			accepted := registry.PublishUser(userID)
			if i < websocketSessionPublicationMaxPendingUsers && !accepted {
				t.Fatalf("user %d rejected before capacity", i)
			}
			if i == websocketSessionPublicationMaxPendingUsers && accepted {
				t.Fatal("overflow user was accepted")
			}
		}
		pending, dropped, _ := registry.stats()
		if pending != websocketSessionPublicationMaxPendingUsers || dropped != 1 {
			t.Fatalf("overflow stats = pending %d dropped %d", pending, dropped)
		}
		close(release)
		seen := make(map[string]bool, websocketSessionPublicationMaxPendingUsers+1)
		for i := 0; i <= websocketSessionPublicationMaxPendingUsers; i++ {
			select {
			case userID := <-published:
				seen[userID] = true
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out after %d publications", i)
			}
		}
		if seen[fmt.Sprintf("user-%03d", websocketSessionPublicationMaxPendingUsers)] {
			t.Fatal("deterministically dropped newest user was published")
		}
		registry.Close()
	})

	t.Run("close cancels projector and rejects later work", func(t *testing.T) {
		started := make(chan struct{})
		stopped := make(chan struct{})
		registry := newWebSocketSubscriptionRegistryWithPublisher(func(ctx context.Context, _ *websocketSessionService) {
			close(started)
			<-ctx.Done()
			close(stopped)
		})
		registry.Add(&websocketSessionService{identity: SessionConnectionIdentity{GatewayUserID: "user"}})
		registry.PublishUser("user")
		<-started
		registry.Close()
		select {
		case <-stopped:
		default:
			t.Fatal("scheduler close did not cancel in-flight projector")
		}
		if registry.PublishUser("user") {
			t.Fatal("publication was accepted after close")
		}
		pending, _, closed := registry.stats()
		if pending != 0 || !closed {
			t.Fatalf("closed scheduler stats = pending %d closed %v", pending, closed)
		}
	})
}

func TestParseSessionsSubscriptionDataClamps(t *testing.T) {
	initial, interval, err := parseSessionsSubscriptionData(json.RawMessage(`"-1,999999"`))
	if err != nil {
		t.Fatal(err)
	}
	if initial != 0 || interval != 60*time.Second {
		t.Fatalf("clamped = %s,%s", initial, interval)
	}
	initial, interval, err = parseSessionsSubscriptionData(json.RawMessage(`"20000,1"`))
	if err != nil {
		t.Fatal(err)
	}
	if initial != 10*time.Second || interval != time.Second {
		t.Fatalf("clamped = %s,%s", initial, interval)
	}
}

func TestWebSocketProgressObjectStringProjectionGuardAndTelemetry(t *testing.T) {
	server, httpServer, store, upstream, _ := newWebSocketTransportServer(t, false)
	emitter := observe.NewEmitter(32)
	server.emitter = emitter
	conn, _ := dialTestWebSocket(t, httpServer, nil)
	writeWebSocketEnvelope(t, conn, "SessionsStart", "0,60000")
	readSessionsEnvelope(t, conn)

	writeWebSocketEnvelope(t, conn, "ReportPlaybackProgress", map[string]any{
		"ItemId": "item-1", "PositionTicks": 100, "EventName": "Pause",
		"Item": map[string]any{"Id": "item-1", "Name": "Movie", "Type": "Movie", "RunTimeTicks": 1000},
	})
	sessions := readSessionsEnvelope(t, conn)
	projected := sessionByID(t, sessions, websocketTestPublicID)
	nowPlaying, ok := projected["NowPlayingItem"].(map[string]any)
	if !ok || nowPlaying["Id"] != "item-1" || nowPlaying["Name"] != "Movie" {
		t.Fatalf("NowPlayingItem = %#v", projected["NowPlayingItem"])
	}
	playState := projected["PlayState"].(map[string]any)
	if playState["PositionTicks"] != float64(100) || playState["IsPaused"] != true {
		t.Fatalf("PlayState = %#v", playState)
	}
	if _, ok := nowPlaying["UserData"].(map[string]any); !ok {
		t.Fatalf("UserData missing: %#v", nowPlaying)
	}

	encoded := `{"ItemId":"item-1","PositionTicks":200,"EventName":"Unpause"}`
	writeWebSocketEnvelope(t, conn, "ReportPlaybackProgress", encoded)
	sessions = readSessionsEnvelope(t, conn)
	playState = sessionByID(t, sessions, websocketTestPublicID)["PlayState"].(map[string]any)
	if playState["PositionTicks"] != float64(200) || playState["IsPaused"] != false {
		t.Fatalf("string Progress PlayState = %#v", playState)
	}

	store.mu.RLock()
	eventCount := len(store.PlaybackEvents)
	store.mu.RUnlock()
	writeWebSocketEnvelope(t, conn, "ReportPlaybackProgress", map[string]any{"PositionTicks": 999})
	readSessionsEnvelope(t, conn)
	store.mu.RLock()
	if len(store.PlaybackEvents) != eventCount {
		store.mu.RUnlock()
		t.Fatal("missing-item Progress wrote an event")
	}
	store.mu.RUnlock()

	server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: HashToken(websocketTestToken), ItemID: "item-1"})
	writeWebSocketEnvelope(t, conn, "ReportPlaybackProgress", map[string]any{"ItemId": "item-1", "PositionTicks": 300})
	readSessionsEnvelope(t, conn)
	store.mu.RLock()
	current := store.CurrentPlaybacks[HashToken(websocketTestToken)]
	if current == nil || current.PlayState.PositionTicks == nil || *current.PlayState.PositionTicks != 200 {
		store.mu.RUnlock()
		t.Fatalf("guard-suppressed current = %#v", current)
	}
	store.mu.RUnlock()

	foundPlaybackTelemetry := false
	for len(emitter.Events()) > 0 {
		event := <-emitter.Events()
		if event.Kind == observe.KindPlayback && event.ItemID == "item-1" {
			foundPlaybackTelemetry = true
			if event.SessionID != websocketTestPublicID {
				t.Fatalf("telemetry SessionID = %q", event.SessionID)
			}
		}
	}
	if !foundPlaybackTelemetry {
		t.Fatal("playback telemetry missing after commit")
	}
	if upstream.hits != 0 {
		t.Fatalf("upstream hits = %d", upstream.hits)
	}
}

func TestWebSocketProgressRevokedAndOperationalCloseCodes(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		_, httpServer, store, _, _ := newWebSocketTransportServer(t, false)
		conn, _ := dialTestWebSocket(t, httpServer, nil)
		if err := store.RevokeSession(context.Background(), HashToken(websocketTestToken)); err != nil {
			t.Fatal(err)
		}
		writeWebSocketEnvelope(t, conn, "ReportPlaybackProgress", map[string]any{"ItemId": "item-1", "PositionTicks": 1})
		if got := readWebSocketClose(t, conn); got != websocket.StatusPolicyViolation {
			t.Fatalf("revoked close = %d, want 1008", got)
		}
	})

	t.Run("operational", func(t *testing.T) {
		store := testStore("http://127.0.0.1:1/emby")
		store.Sessions[HashToken(websocketTestToken)] = websocketTransportSession()
		failing := &websocketPlaybackFailureStore{MemoryStore: store, applyErr: ErrStoreUnavailable}
		server := NewServer(Config{GatewayBasePath: "/emby"}, failing)
		httpServer := httptestServer(t, server)
		conn, _ := dialTestWebSocket(t, httpServer, nil)
		writeWebSocketEnvelope(t, conn, "ReportPlaybackProgress", map[string]any{"ItemId": "item-1", "PositionTicks": 1})
		if got := readWebSocketClose(t, conn); got != websocket.StatusInternalError {
			t.Fatalf("operational close = %d, want 1011", got)
		}
	})
}

type websocketPlaybackFailureStore struct {
	*MemoryStore
	applyErr error
}

func (s *websocketPlaybackFailureStore) ApplyPlaybackReport(context.Context, PlaybackReportCommand) (PlaybackReportResult, error) {
	return PlaybackReportResult{}, s.applyErr
}

func (s *websocketPlaybackFailureStore) LoadDefaultUpstreamRuntime(context.Context) (*UpstreamRuntime, error) {
	panic("upstream runtime must not be loaded")
}

func httptestServer(t *testing.T, server *Server) *httptest.Server {
	t.Helper()
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		server.CloseWebSockets()
		httpServer.Close()
	})
	return httpServer
}

func TestWebSocketSessionPublicationsForCapabilitiesConnectionAndLogout(t *testing.T) {
	server, httpServer, store, upstream, _ := newWebSocketTransportServer(t, false)
	viewer, _ := dialTestWebSocket(t, httpServer, nil)
	writeWebSocketEnvelope(t, viewer, "SessionsStart", "0,60000")
	readSessionsEnvelope(t, viewer)

	capsReq, err := http.NewRequest(http.MethodPost, httpServer.URL+"/emby/Sessions/Capabilities?api_key="+websocketTestToken+"&SupportedCommands=Pause", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(capsReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities status = %d", resp.StatusCode)
	}
	readSessionsEnvelope(t, viewer)

	secondToken := strings.Repeat("B", 43)
	second := websocketTransportSession()
	second.GatewayTokenHash = HashToken(secondToken)
	second.PublicID = "session-22222222222222222222222222222222"
	second.DeviceID = "device-2"
	store.Sessions[second.GatewayTokenHash] = second
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	secondConn, response, err := websocket.Dial(ctx, websocketURL(httpServer, "api_key="+secondToken+"&deviceId=device-2"), nil)
	cancel()
	if err != nil {
		t.Fatalf("second dial: %v response=%v", err, response)
	}
	if got := len(readSessionsEnvelope(t, viewer)); got != 2 {
		t.Fatalf("connection publication count = %d, want 2", got)
	}

	logoutReq, err := http.NewRequest(http.MethodPost, httpServer.URL+"/emby/Sessions/Logout?api_key="+secondToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	logoutResp, err := http.DefaultClient.Do(logoutReq)
	if err != nil {
		t.Fatal(err)
	}
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", logoutResp.StatusCode)
	}
	if got := len(readSessionsEnvelope(t, viewer)); got != 1 {
		t.Fatalf("logout publication count = %d, want 1", got)
	}
	if got := readWebSocketClose(t, secondConn); got != websocket.StatusPolicyViolation {
		t.Fatalf("logout close = %d, want 1008", got)
	}
	if upstream.hits != 0 {
		t.Fatalf("upstream hits = %d", upstream.hits)
	}
	_ = server
}
