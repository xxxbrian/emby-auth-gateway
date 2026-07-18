package gateway

import "context"

// SessionRepository is the persistence boundary for gateway authentication sessions.
type SessionRepository interface {
	SaveSession(ctx context.Context, session *Session) error
	FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	// SessionTokenExists reports whether any session row exists for tokenHash,
	// including revoked and expired sessions, without hydrating account details.
	SessionTokenExists(ctx context.Context, tokenHash string) (bool, error)
	RevokeSession(ctx context.Context, tokenHash string) error
}
