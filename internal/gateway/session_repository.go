package gateway

import (
	"context"
	"time"
)

// SessionRepository is the persistence boundary for gateway authentication sessions
// and their sidecar profile projection (public ID, capabilities, activity).
type SessionRepository interface {
	// CreateSession persists a new auth session and profile aggregate atomically.
	// Empty PublicID is generated; empty capabilities RawJSON defaults to "{}".
	CreateSession(ctx context.Context, session Session) (*Session, error)
	FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	// FindActiveSessionByPublicID resolves an active public session within one gateway user.
	// Missing, foreign, revoked, expired, and profile-hole sessions are indistinguishable.
	FindActiveSessionByPublicID(ctx context.Context, gatewayUserID, publicID string, now time.Time) (*Session, error)
	// SessionTokenExists reports whether any session row exists for tokenHash,
	// including revoked and expired sessions, without hydrating account details.
	SessionTokenExists(ctx context.Context, tokenHash string) (bool, error)
	RevokeSession(ctx context.Context, tokenHash string) error
	// UpdateSessionCapabilities stores canonical capabilities and always updates activity.
	UpdateSessionCapabilities(ctx context.Context, tokenHash string, capabilities SessionCapabilities, at time.Time) (*Session, error)
	// TouchSessionActivity updates last activity when older than minInterval; returns whether a write occurred.
	TouchSessionActivity(ctx context.Context, tokenHash string, at time.Time, minInterval time.Duration) (bool, error)
	// ListActiveSessions returns active sessions for gatewayUserID, hydrating/repairing profiles.
	// Any repair failure fails the entire list. Order: LastActivityAt desc, PublicID asc.
	ListActiveSessions(ctx context.Context, gatewayUserID string, now time.Time) ([]Session, error)
}
