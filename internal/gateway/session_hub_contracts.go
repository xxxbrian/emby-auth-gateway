package gateway

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"
)

// SessionConnectionIdentity is the immutable ownership key for a live connection.
type SessionConnectionIdentity struct {
	TokenHash     string
	PublicID      string
	GatewayUserID string
}

// SessionOutboundEnvelope is the only process-local message shape sent to a client.
type SessionOutboundEnvelope struct {
	MessageType string
	Data        json.RawMessage
	MessageID   string
}

type SessionHubConnection interface {
	// Cancel synchronously stops connection-owned work before any close handshake.
	Cancel(context.Context)
	// Abort force-closes transport work when a bounded close request cannot be queued.
	Abort()
	// WaitCancelled waits for connection-owned application work to observe Cancel.
	WaitCancelled(context.Context)
	Close(code websocket.StatusCode, reason string) error
}

type SessionHubRegistration struct {
	Generation uint64
	Notify     <-chan struct{}
}

type SessionHubPresence struct {
	PublicID      string
	GatewayUserID string
	Generation    uint64
}

// SessionPublication is a coalescible Sessions update for one gateway user.
type SessionPublication struct {
	GatewayUserID string
	Sessions      []Session
}

type SessionCommandEnqueueStatus uint8

const (
	SessionCommandEnqueued SessionCommandEnqueueStatus = iota + 1
	SessionCommandDisconnected
	SessionCommandQueueFull
)

type SessionCommandEnqueueResult struct {
	Status SessionCommandEnqueueStatus
	Err    error
}

// SessionHub is process-local and deliberately has no replay or generic pub/sub API.
type SessionHub interface {
	Register(SessionConnectionIdentity, SessionHubConnection) (SessionHubRegistration, error)
	Replace(SessionConnectionIdentity, SessionHubConnection) (SessionHubRegistration, error)
	Unregister(SessionConnectionIdentity, SessionHubRegistration)
	Lookup(tokenHash string) (SessionHubPresence, bool)
	LookupSession(gatewayUserID, publicID string) []SessionHubPresence
	Present(SessionConnectionIdentity) bool
	Enqueue(SessionConnectionIdentity, SessionOutboundEnvelope) SessionCommandEnqueueResult
	// EnqueueGeneration atomically validates identity and generation while holding
	// the same hub lock used to append to the exact live connection queue.
	EnqueueGeneration(SessionConnectionIdentity, uint64, SessionOutboundEnvelope) SessionCommandEnqueueResult
	Dequeue(SessionConnectionIdentity, SessionHubRegistration) (SessionOutboundEnvelope, bool)
	PublishSessions(SessionPublication)
	PublishSessionsTo(SessionConnectionIdentity, SessionHubRegistration, json.RawMessage) SessionCommandEnqueueResult
	CloseByToken(tokenHash string) int
	CloseSession(gatewayUserID, publicID string) int
	BeginCloseAll(context.Context) int
	CloseDone() <-chan struct{}
	AbortCloseAll()
	CloseAll() int
}
