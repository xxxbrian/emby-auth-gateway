package gateway

import (
	"context"
	"net/http"
	"time"
)

type Config struct {
	PublicBaseURL   string
	GatewayBasePath string
	GatewayServerID string
	HTTPClient      *http.Client
}

type Store interface {
	AuthenticateGatewayUser(ctx context.Context, username, password string) (*GatewayUser, error)
	ListPublicUsers(ctx context.Context) ([]GatewayUser, error)
	FindUserBySyntheticID(ctx context.Context, syntheticID string) (*GatewayUser, error)
	FindMappingByGatewayUserID(ctx context.Context, gatewayUserID string) (*UserMapping, error)
	DefaultBackend(ctx context.Context) (*BackendAccount, error)
	SaveSession(ctx context.Context, session *Session) error
	FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	RevokeSession(ctx context.Context, tokenHash string) error
}

type GatewayUser struct {
	ID              string
	Username        string
	SyntheticUserID string
	Enabled         bool
}

type BackendAccount struct {
	ID       string
	ServerID string
	BaseURL  string
	Username string
	Password string
	Enabled  bool
}

type UserMapping struct {
	ID               string
	GatewayUserID    string
	BackendAccountID string
	BackendAccount   BackendAccount
	Enabled          bool
}

type Session struct {
	GatewayTokenHash string
	GatewayUserID    string
	GatewayUsername  string
	SyntheticUserID  string
	BackendAccountID string
	BackendServerID  string
	BackendBaseURL   string
	BackendUserID    string
	BackendUsername  string
	BackendToken     string
	Client           string
	Device           string
	DeviceID         string
	Version          string
	RemoteIP         string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	RevokedAt        *time.Time
}

func (s *Session) Active(now time.Time) bool {
	if s == nil {
		return false
	}
	if s.RevokedAt != nil {
		return false
	}
	return s.ExpiresAt.IsZero() || now.Before(s.ExpiresAt)
}

type AuthHeader struct {
	Scheme   string
	UserID   string
	Client   string
	Device   string
	DeviceID string
	Version  string
	Token    string
	Fields   map[string]string
}
