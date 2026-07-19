package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type sessionCloseCall struct {
	code   websocket.StatusCode
	reason string
}

type fakeSessionHubConnection struct {
	mu          sync.Mutex
	calls       []sessionCloseCall
	events      []string
	cancelCount int
	abortCount  int
	closeBlock  <-chan struct{}
	onCancel    func()
	onClose     func()
	wake        chan struct{}
}

func newFakeSessionHubConnection() *fakeSessionHubConnection {
	return &fakeSessionHubConnection{wake: make(chan struct{}, 16)}
}

func (c *fakeSessionHubConnection) Close(code websocket.StatusCode, reason string) error {
	if c.onClose != nil {
		c.onClose()
	}
	c.mu.Lock()
	c.calls = append(c.calls, sessionCloseCall{code: code, reason: reason})
	c.events = append(c.events, "close")
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	if c.closeBlock != nil {
		<-c.closeBlock
	}
	return nil
}

func (c *fakeSessionHubConnection) Cancel(context.Context) {
	if c.onCancel != nil {
		c.onCancel()
	}
	c.mu.Lock()
	c.cancelCount++
	c.events = append(c.events, "cancel")
	c.mu.Unlock()
}

func (c *fakeSessionHubConnection) Abort() {
	c.mu.Lock()
	c.abortCount++
	c.events = append(c.events, "abort")
	c.mu.Unlock()
}

func (c *fakeSessionHubConnection) WaitCancelled(context.Context) {}

func (c *fakeSessionHubConnection) waitForClose(t *testing.T) sessionCloseCall {
	t.Helper()
	select {
	case <-c.wake:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for close")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[len(c.calls)-1]
}

func sessionIdentity(token, user, publicID string) SessionConnectionIdentity {
	return SessionConnectionIdentity{TokenHash: token, GatewayUserID: user, PublicID: publicID}
}

func registerSession(t *testing.T, hub SessionHub, identity SessionConnectionIdentity) (SessionHubRegistration, *fakeSessionHubConnection) {
	t.Helper()
	connection := newFakeSessionHubConnection()
	registration, err := hub.Register(identity, connection)
	if err != nil {
		t.Fatal(err)
	}
	return registration, connection
}

func commandEnvelope(id string) SessionOutboundEnvelope {
	return SessionOutboundEnvelope{MessageType: "GeneralCommand", Data: json.RawMessage(fmt.Sprintf(`{"ID":%q}`, id)), MessageID: id}
}

func TestSessionHubReconnectGenerationAndUnregisterABA(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	oldRegistration, oldConnection := registerSession(t, hub, identity)
	newRegistration, newConnection := registerSession(t, hub, identity)
	if newRegistration.Generation <= oldRegistration.Generation {
		t.Fatalf("generations old=%d new=%d", oldRegistration.Generation, newRegistration.Generation)
	}
	if call := oldConnection.waitForClose(t); call.code != websocket.StatusGoingAway {
		t.Fatalf("replacement close = %d, want 1001", call.code)
	}

	hub.Unregister(identity, oldRegistration)
	presence, ok := hub.Lookup(identity.TokenHash)
	if !ok || presence.Generation != newRegistration.Generation {
		t.Fatalf("old unregister removed replacement: %#v, %v", presence, ok)
	}
	hub.Unregister(identity, newRegistration)
	hub.Unregister(identity, newRegistration)
	if _, ok := hub.Lookup(identity.TokenHash); ok {
		t.Fatal("current unregister did not remove connection")
	}
	select {
	case <-newConnection.wake:
		t.Fatal("unregister must not send a close frame")
	default:
	}
	newConnection.mu.Lock()
	if newConnection.cancelCount != 1 || len(newConnection.calls) != 0 {
		newConnection.mu.Unlock()
		t.Fatal("unregister did not cancel exactly its own generation")
	}
	newConnection.mu.Unlock()
}

func TestSessionHubFIFOAndGenerationSafeDequeue(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	registration, _ := registerSession(t, hub, identity)
	for _, id := range []string{"one", "two", "three"} {
		if result := hub.Enqueue(identity, commandEnvelope(id)); result.Status != SessionCommandEnqueued || result.Err != nil {
			t.Fatalf("enqueue %s = %#v", id, result)
		}
	}
	for _, want := range []string{"one", "two", "three"} {
		got, ok := hub.Dequeue(identity, registration)
		if !ok || got.MessageID != want {
			t.Fatalf("dequeue = %#v, %v; want %s", got, ok, want)
		}
		got.Data[0] = '['
	}
	if _, ok := hub.Dequeue(identity, SessionHubRegistration{Generation: registration.Generation + 1}); ok {
		t.Fatal("wrong generation dequeued a message")
	}
}

func TestSessionHubAtomicGenerationEnqueueRejectsReplacement(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	oldRegistration, _ := registerSession(t, hub, identity)
	newRegistration, _ := registerSession(t, hub, identity)

	result := hub.EnqueueGeneration(identity, oldRegistration.Generation, commandEnvelope("old"))
	if result.Status != SessionCommandDisconnected || result.Err != nil {
		t.Fatalf("old generation enqueue = %#v", result)
	}
	if _, ok := hub.Dequeue(identity, newRegistration); ok {
		t.Fatal("old generation command reached replacement queue")
	}

	result = hub.EnqueueGeneration(identity, newRegistration.Generation, commandEnvelope("new"))
	if result.Status != SessionCommandEnqueued || result.Err != nil {
		t.Fatalf("current generation enqueue = %#v", result)
	}
	queued, ok := hub.Dequeue(identity, newRegistration)
	if !ok || queued.MessageID != "new" {
		t.Fatalf("current generation dequeue = %#v, %v", queued, ok)
	}
}

func TestSessionHubSessionsCoalescingDoesNotReorderCommands(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	registration, _ := registerSession(t, hub, identity)
	hub.Enqueue(identity, commandEnvelope("before"))
	hub.PublishSessions(SessionPublication{GatewayUserID: "user", Sessions: []Session{{PublicID: "old", GatewayUserID: "user"}}})
	hub.Enqueue(identity, commandEnvelope("after"))
	hub.PublishSessions(SessionPublication{GatewayUserID: "user", Sessions: []Session{{PublicID: "new", GatewayUserID: "user"}}})

	first, _ := hub.Dequeue(identity, registration)
	second, _ := hub.Dequeue(identity, registration)
	third, _ := hub.Dequeue(identity, registration)
	if first.MessageID != "before" || second.MessageType != "Sessions" || third.MessageID != "after" {
		t.Fatalf("queue order = %#v, %#v, %#v", first, second, third)
	}
	if string(second.Data) == "" || !json.Valid(second.Data) || string(second.Data) == `[]` {
		t.Fatalf("coalesced Sessions Data = %s", second.Data)
	}
	if _, ok := hub.Dequeue(identity, registration); ok {
		t.Fatal("coalescing left more than one Sessions publication")
	}
}

func TestSessionHubCommandCountSaturationIsNonDroppable(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	_, connection := registerSession(t, hub, identity)
	for i := 0; i < sessionOutboundQueueCapacity; i++ {
		result := hub.Enqueue(identity, commandEnvelope(fmt.Sprint(i)))
		if result.Status != SessionCommandEnqueued {
			t.Fatalf("enqueue %d = %#v", i, result)
		}
	}
	result := hub.Enqueue(identity, commandEnvelope("overflow"))
	if result.Status != SessionCommandQueueFull || !errors.Is(result.Err, ErrStoreUnavailable) {
		t.Fatalf("overflow = %#v", result)
	}
	if call := connection.waitForClose(t); call.code != websocket.StatusTryAgainLater {
		t.Fatalf("overflow close = %d, want 1013", call.code)
	}
	if hub.Present(identity) {
		t.Fatal("saturated connection remained present")
	}
}

func TestSessionHubQueueByteSaturation(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	_, connection := registerSession(t, hub, identity)
	large := func(size int) SessionOutboundEnvelope {
		data, err := json.Marshal(strings.Repeat("x", size))
		if err != nil {
			t.Fatal(err)
		}
		return SessionOutboundEnvelope{MessageType: "GeneralCommand", Data: data}
	}
	if result := hub.Enqueue(identity, large(150<<10)); result.Status != SessionCommandEnqueued {
		t.Fatalf("first large enqueue = %#v", result)
	}
	result := hub.Enqueue(identity, large(110<<10))
	if result.Status != SessionCommandQueueFull || !errors.Is(result.Err, ErrStoreUnavailable) {
		t.Fatalf("byte overflow = %#v", result)
	}
	if call := connection.waitForClose(t); call.code != websocket.StatusTryAgainLater {
		t.Fatalf("byte overflow close = %d", call.code)
	}
}

func TestSessionHubPresenceIndexesAndTwoUserIsolation(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	one := sessionIdentity("one", "user-one", "shared-public")
	two := sessionIdentity("two", "user-two", "shared-public")
	regOne, _ := registerSession(t, hub, one)
	regTwo, _ := registerSession(t, hub, two)

	presence, ok := hub.Lookup("one")
	if !ok || presence.GatewayUserID != "user-one" || presence.PublicID != "shared-public" || presence.Generation != regOne.Generation {
		t.Fatalf("token lookup = %#v, %v", presence, ok)
	}
	if got := hub.LookupSession("user-one", "shared-public"); len(got) != 1 || got[0].Generation != regOne.Generation {
		t.Fatalf("user-one lookup = %#v", got)
	}
	if got := hub.LookupSession("user-two", "shared-public"); len(got) != 1 || got[0].Generation != regTwo.Generation {
		t.Fatalf("user-two lookup = %#v", got)
	}
	if got := hub.LookupSession("missing", "shared-public"); len(got) != 0 {
		t.Fatalf("missing lookup = %#v", got)
	}

	hub.PublishSessions(SessionPublication{GatewayUserID: "user-one", Sessions: []Session{{PublicID: "only-one"}}})
	if _, ok := hub.Dequeue(one, regOne); !ok {
		t.Fatal("target user did not receive Sessions")
	}
	if _, ok := hub.Dequeue(two, regTwo); ok {
		t.Fatal("other user received Sessions")
	}
}

func TestSessionHubCloseStatusesAndIdempotence(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	one := sessionIdentity("one", "user", "public")
	two := sessionIdentity("two", "user", "public")
	_, oneConnection := registerSession(t, hub, one)
	_, twoConnection := registerSession(t, hub, two)
	if got := hub.CloseByToken("one"); got != 1 {
		t.Fatalf("CloseByToken = %d", got)
	}
	if call := oneConnection.waitForClose(t); call.code != websocket.StatusPolicyViolation {
		t.Fatalf("revocation close = %d, want 1008", call.code)
	}
	if got := hub.CloseByToken("one"); got != 0 {
		t.Fatalf("second CloseByToken = %d", got)
	}
	if got := hub.CloseSession("user", "public"); got != 1 {
		t.Fatalf("CloseSession = %d", got)
	}
	if call := twoConnection.waitForClose(t); call.code != websocket.StatusPolicyViolation {
		t.Fatalf("session close = %d, want 1008", call.code)
	}
	if got := hub.CloseSession("user", "public"); got != 0 {
		t.Fatalf("second CloseSession = %d", got)
	}
}

func TestSessionHubCloseAll(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	connections := make([]*fakeSessionHubConnection, 0, 3)
	for i := 0; i < 3; i++ {
		_, connection := registerSession(t, hub, sessionIdentity(fmt.Sprintf("token-%d", i), "user", fmt.Sprintf("public-%d", i)))
		connections = append(connections, connection)
	}
	if got := hub.CloseAll(); got != 3 {
		t.Fatalf("CloseAll = %d", got)
	}
	for _, connection := range connections {
		if call := connection.waitForClose(t); call.code != websocket.StatusGoingAway {
			t.Fatalf("shutdown close = %d, want 1001", call.code)
		}
	}
	if got := hub.CloseAll(); got != 0 {
		t.Fatalf("second CloseAll = %d", got)
	}
}

func TestSessionHubTerminalCloseRejectsRegistrationAndOrdersCancellation(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	registration, connection := registerSession(t, hub, identity)
	if got := hub.CloseAll(); got != 1 {
		t.Fatalf("CloseAll = %d", got)
	}
	connection.waitForClose(t)
	connection.mu.Lock()
	events := append([]string(nil), connection.events...)
	connection.mu.Unlock()
	if len(events) < 2 || events[0] != "cancel" || events[1] != "close" {
		t.Fatalf("lifecycle order = %v, want cancel before close", events)
	}
	rejected := newFakeSessionHubConnection()
	if _, err := hub.Register(identity, rejected); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("post-close Register error = %v", err)
	}
	rejected.mu.Lock()
	if rejected.cancelCount != 1 || rejected.abortCount != 1 {
		rejected.mu.Unlock()
		t.Fatal("post-close Register did not cancel and abort rejected connection")
	}
	rejected.mu.Unlock()
	result := hub.EnqueueGeneration(identity, registration.Generation, commandEnvelope("late"))
	if result.Status != SessionCommandDisconnected || !errors.Is(result.Err, ErrStoreUnavailable) {
		t.Fatalf("post-close enqueue = %#v", result)
	}
	hub.PublishSessions(SessionPublication{GatewayUserID: "user", Sessions: []Session{{PublicID: "late"}}})
	if got := hub.CloseAll(); got != 0 {
		t.Fatalf("repeated CloseAll = %d", got)
	}
}

func TestSessionHubCloseAllWaitsForCallbackButIsBounded(t *testing.T) {
	t.Run("waits for callback", func(t *testing.T) {
		hub := NewProcessLocalSessionHub()
		release := make(chan struct{})
		connection := newFakeSessionHubConnection()
		connection.closeBlock = release
		if _, err := hub.Register(sessionIdentity("token", "user", "public"), connection); err != nil {
			t.Fatal(err)
		}
		returned := make(chan int, 1)
		go func() { returned <- hub.CloseAll() }()
		connection.waitForClose(t)
		select {
		case <-returned:
			t.Fatal("CloseAll returned before close callback completed")
		case <-time.After(30 * time.Millisecond):
		}
		close(release)
		select {
		case got := <-returned:
			if got != 1 {
				t.Fatalf("CloseAll = %d", got)
			}
		case <-time.After(time.Second):
			t.Fatal("CloseAll did not return after callback completed")
		}
	})

	t.Run("bounded stuck callback", func(t *testing.T) {
		hub := NewProcessLocalSessionHub()
		release := make(chan struct{})
		connection := newFakeSessionHubConnection()
		connection.closeBlock = release
		if _, err := hub.Register(sessionIdentity("token", "user", "public"), connection); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if got := hub.CloseAll(); got != 1 {
			t.Fatalf("CloseAll = %d", got)
		}
		elapsed := time.Since(started)
		if elapsed < sessionCloseAllWaitTimeout-100*time.Millisecond || elapsed > 2*sessionCloseAllWaitTimeout {
			t.Fatalf("bounded CloseAll duration = %s", elapsed)
		}
		close(release)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-hub.(*processLocalSessionHub).closeDone:
				return
			default:
				runtime.Gosched()
			}
		}
		t.Fatal("close worker did not finish after blocked callback released")
	})
}

func TestSessionHubConcurrentRegisterAndTerminalClose(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	start := make(chan struct{})
	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			identity := sessionIdentity(fmt.Sprintf("token-%d", i), "user", fmt.Sprintf("public-%d", i))
			_, err := hub.Register(identity, newFakeSessionHubConnection())
			if err != nil && !errors.Is(err, ErrStoreUnavailable) {
				t.Errorf("Register error = %v", err)
			}
		}(i)
	}
	close(start)
	hub.CloseAll()
	wg.Wait()
	for i := 0; i < workers; i++ {
		if _, ok := hub.Lookup(fmt.Sprintf("token-%d", i)); ok {
			t.Fatalf("token-%d remained live after terminal close", i)
		}
	}
	if _, err := hub.Register(sessionIdentity("late", "user", "public"), newFakeSessionHubConnection()); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("terminal Register error = %v", err)
	}
}

func TestSessionHubPendingReservationBlocksTerminalCompletion(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	concrete := hub.(*processLocalSessionHub)
	identity := sessionIdentity("token", "user", "public")
	_, connection := registerSession(t, hub, identity)
	reserved := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	concrete.closeReservationHook = func() {
		once.Do(func() {
			close(reserved)
			<-release
		})
	}
	closedByToken := make(chan int, 1)
	go func() { closedByToken <- hub.CloseByToken(identity.TokenHash) }()
	select {
	case <-reserved:
	case <-time.After(2 * time.Second):
		t.Fatal("remove+reserve barrier was not reached")
	}

	server := NewServer(Config{GatewayBasePath: "/emby"}, NewMemoryStore())
	server.sessionHub = hub
	returned := make(chan struct{})
	go func() {
		server.Close()
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("Server.Close returned while close reservation was pending")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-hub.CloseDone():
		t.Fatal("CloseDone closed while reservation was pending")
	default:
	}
	connection.mu.Lock()
	cancels := connection.cancelCount
	connection.mu.Unlock()
	if cancels == 0 {
		t.Fatal("terminal shutdown did not cancel reserved connection")
	}

	close(release)
	select {
	case got := <-closedByToken:
		if got != 1 {
			t.Fatalf("CloseByToken = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reserved close did not activate")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Server.Close did not finish after reservation completed")
	}
	select {
	case <-hub.CloseDone():
	case <-time.After(time.Second):
		t.Fatal("CloseDone did not close after reservation completed")
	}
}

func TestSessionHubReconnectStormUsesBoundedCloseWorker(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	release := make(chan struct{})
	first := newFakeSessionHubConnection()
	first.closeBlock = release
	if _, err := hub.Register(identity, first); err != nil {
		t.Fatal(err)
	}
	connections := make([]*fakeSessionHubConnection, 0, 1001)
	connections = append(connections, first)
	second := newFakeSessionHubConnection()
	connections = append(connections, second)
	if _, err := hub.Register(identity, second); err != nil {
		t.Fatal(err)
	}
	first.waitForClose(t)
	baseline := runtime.NumGoroutine()
	for i := 0; i < 999; i++ {
		connection := newFakeSessionHubConnection()
		connections = append(connections, connection)
		if _, err := hub.Register(identity, connection); err != nil {
			t.Fatal(err)
		}
	}
	concrete := hub.(*processLocalSessionHub)
	concrete.closeMu.Lock()
	queued := len(concrete.closeQueue)
	concrete.closeMu.Unlock()
	if queued > sessionCloseQueueCapacity {
		t.Fatalf("close queue = %d, max %d", queued, sessionCloseQueueCapacity)
	}
	if growth := runtime.NumGoroutine() - baseline; growth > 2 {
		t.Fatalf("reconnect storm goroutine growth = %d", growth)
	}
	aborted := 0
	for _, connection := range connections {
		connection.mu.Lock()
		aborted += connection.abortCount
		connection.mu.Unlock()
	}
	if aborted == 0 {
		t.Fatal("close queue overflow did not force bounded aborts")
	}
	if got := hub.CloseAll(); got != 1 {
		t.Fatalf("CloseAll = %d", got)
	}
	close(release)
	select {
	case <-concrete.closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("close worker did not drain after reconnect storm")
	}
}

func TestSessionHubReservationPathRaceStress(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	const count = 100
	identities := make([]SessionConnectionIdentity, count)
	registrations := make([]SessionHubRegistration, count)
	for i := 0; i < count; i++ {
		identities[i] = sessionIdentity(fmt.Sprintf("token-%d", i), fmt.Sprintf("user-%d", i), fmt.Sprintf("public-%d", i))
		registration, err := hub.Register(identities[i], newFakeSessionHubConnection())
		if err != nil {
			t.Fatal(err)
		}
		registrations[i] = registration
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			switch i % 5 {
			case 0:
				hub.CloseByToken(identities[i].TokenHash)
			case 1:
				hub.CloseSession(identities[i].GatewayUserID, identities[i].PublicID)
			case 2:
				for n := 0; n <= sessionOutboundQueueCapacity; n++ {
					hub.Enqueue(identities[i], commandEnvelope("fill"))
				}
			case 3:
				hub.Unregister(identities[i], registrations[i])
			case 4:
				_, err := hub.Replace(identities[i], newFakeSessionHubConnection())
				if err != nil && !errors.Is(err, ErrStoreUnavailable) {
					t.Errorf("Replace error = %v", err)
				}
			}
		}(i)
	}
	close(start)
	hub.BeginCloseAll(context.Background())
	wg.Wait()
	select {
	case <-hub.CloseDone():
	case <-time.After(2 * time.Second):
		t.Fatal("reservation paths did not drain")
	}
	concrete := hub.(*processLocalSessionHub)
	concrete.closeMu.Lock()
	pending := len(concrete.closePending)
	queued := len(concrete.closeQueue)
	registrationsLeft := concrete.closeRegistrations
	worker := concrete.closeWorker
	concrete.closeMu.Unlock()
	if pending != 0 || queued != 0 || registrationsLeft != 0 || worker {
		t.Fatalf("terminal accounting = pending %d queued %d registrations %d worker %v", pending, queued, registrationsLeft, worker)
	}
}

func TestSessionHubConcurrentGenerationStress(t *testing.T) {
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity("token", "user", "public")
	const workers = 8
	const iterations = 200
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				registration, err := hub.Register(identity, newFakeSessionHubConnection())
				if err != nil {
					t.Errorf("register: %v", err)
					return
				}
				hub.Lookup(identity.TokenHash)
				hub.LookupSession(identity.GatewayUserID, identity.PublicID)
				hub.Present(identity)
				hub.Enqueue(identity, commandEnvelope(fmt.Sprintf("%d-%d", worker, i)))
				hub.Dequeue(identity, registration)
				hub.Unregister(identity, registration)
			}
		}(worker)
	}
	wg.Wait()
	hub.CloseAll()
}
