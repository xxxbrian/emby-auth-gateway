package gateway

import "time"

const (
	mediaLeaseRegistryMaxGlobal   = 4096
	mediaLeaseRegistryMaxPerToken = 64
	mediaLeaseTTL                 = 12 * time.Hour
)

type PlaySessionID string
type LiveStreamID string

// MediaLease binds negotiation identifiers to one gateway token hash until expiry.
type MediaLease struct {
	GatewayTokenHash string
	PlaySessionID    PlaySessionID
	LiveStreamID     LiveStreamID
	ExpiresAt        time.Time
}

// MediaLeaseRegistry is bounded, process-local state; restart intentionally forgets leases.
type MediaLeaseRegistry interface {
	Register(MediaLease) error
	RegisterAll(gatewayTokenHash string, playSessionIDs []PlaySessionID, liveStreamIDs []LiveStreamID) error
	Validate(gatewayTokenHash string, playSessionID PlaySessionID, liveStreamID LiveStreamID, now time.Time) (MediaLease, error)
	ValidateAll(gatewayTokenHash string, playSessionIDs []PlaySessionID, liveStreamIDs []LiveStreamID, now time.Time) error
	Release(gatewayTokenHash string, playSessionIDs []PlaySessionID, liveStreamIDs []LiveStreamID) error
	RemoveSession(gatewayTokenHash string)
	Owners() []string
	Sweep(now time.Time) int
}
