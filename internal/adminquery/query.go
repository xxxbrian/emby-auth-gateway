// Package adminquery provides bounded admin read queries against core.App.
package adminquery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/controlplane"
	"github.com/xxxbrian/emby-auth-gateway/internal/pathpolicy"
)

const (
	DefaultConcurrency = 4
	MaxListLimit       = 500
	MaxAuditLimit      = 100
	MaxAuditWindow     = 24 * time.Hour
	AuditQueryTimeout  = 2 * time.Second
)

// Querier runs bounded admin reads.
type Querier struct {
	app core.App
	sem chan struct{}
	now func() time.Time
}

// New creates a Querier with max concurrent queries.
func New(app core.App, maxConcurrent int) *Querier {
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultConcurrency
	}
	return &Querier{
		app: app,
		sem: make(chan struct{}, maxConcurrent),
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (q *Querier) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case q.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Querier) release() {
	select {
	case <-q.sem:
	default:
	}
}

// UserDTO is a gateway user without secrets.
type UserDTO struct {
	ID              string    `json:"id"`
	Username        string    `json:"username"`
	Email           string    `json:"email,omitempty"`
	SyntheticUserID string    `json:"synthetic_user_id"`
	Enabled         bool      `json:"enabled"`
	Created         time.Time `json:"created,omitempty"`
	Updated         time.Time `json:"updated,omitempty"`
}

// SessionDTO is a gateway session row.
type SessionDTO struct {
	ID              string     `json:"id"`
	GatewayUserID   string     `json:"gateway_user_id"`
	GatewayUsername string     `json:"gateway_username,omitempty"`
	SyntheticUserID string     `json:"synthetic_user_id,omitempty"`
	Client          string     `json:"client,omitempty"`
	Device          string     `json:"device,omitempty"`
	DeviceID        string     `json:"device_id,omitempty"`
	Version         string     `json:"version,omitempty"`
	RemoteIP        string     `json:"remote_ip,omitempty"`
	ExpiresAt       time.Time  `json:"expires_at"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	Created         time.Time  `json:"created,omitempty"`
	Active          bool       `json:"active"`
}

// AuditDTO is a redacted audit log row.
type AuditDTO struct {
	ID                string    `json:"id"`
	GatewayUserID     string    `json:"gateway_user_id,omitempty"`
	SyntheticUserID   string    `json:"synthetic_user_id,omitempty"`
	Event             string    `json:"event"`
	Message           string    `json:"message,omitempty"`
	Method            string    `json:"method,omitempty"`
	Path              string    `json:"path,omitempty"`
	Status            int       `json:"status,omitempty"`
	RemoteIP          string    `json:"remote_ip,omitempty"`
	Created           time.Time `json:"created"`
	ErrorKind         string    `json:"error_kind,omitempty"`
	Direction         string    `json:"direction,omitempty"`
	BytesTransferred  int64     `json:"bytes_transferred,omitempty"`
	DurationMS        int64     `json:"duration_ms,omitempty"`
	UpstreamStatus    int       `json:"upstream_status,omitempty"`
	ResponseCommitted bool      `json:"response_committed"`
	IsError           bool      `json:"is_error"`
	Severity          string    `json:"severity"`
}

// UpstreamDTO is a redacted upstream configuration view.
type UpstreamDTO struct {
	Configured                  bool       `json:"configured"`
	Key                         string     `json:"key,omitempty"`
	ServerID                    string     `json:"server_id,omitempty"`
	ServerName                  string     `json:"server_name,omitempty"`
	ServerVersion               string     `json:"server_version,omitempty"`
	VersionCheckedAt            *time.Time `json:"version_checked_at,omitempty"`
	BackendUsername             string     `json:"backend_username,omitempty"`
	PasswordSet                 bool       `json:"password_set"`
	TokenSet                    bool       `json:"token_set"`
	BackendUserID               string     `json:"backend_user_id,omitempty"`
	TokenUpdatedAt              *time.Time `json:"token_updated_at,omitempty"`
	LastLoginAt                 *time.Time `json:"last_login_at,omitempty"`
	LastLoginError              string     `json:"last_login_error,omitempty"`
	BackendUserAgent            string     `json:"backend_user_agent,omitempty"`
	BackendAuthorizationClient  string     `json:"backend_authorization_client,omitempty"`
	BackendAuthorizationDevice  string     `json:"backend_authorization_device,omitempty"`
	BackendAuthorizationVersion string     `json:"backend_authorization_version,omitempty"`
	// Device ID is operational identity, not a secret token.
	BackendAuthorizationDeviceID string `json:"backend_authorization_device_id,omitempty"`
	AuthGenerationSet            bool   `json:"auth_generation_set"`
	BaseURL                      string `json:"base_url,omitempty"`
	EndpointKey                  string `json:"endpoint_key,omitempty"`
	EndpointActive               bool   `json:"endpoint_active,omitempty"`
}

// ListUsers returns gateway users (hard-capped at MaxListLimit).
func (q *Querier) ListUsers(ctx context.Context) ([]UserDTO, error) {
	if err := q.acquire(ctx); err != nil {
		return nil, err
	}
	defer q.release()

	records, err := q.app.FindRecordsByFilter("users", "", "username", MaxListLimit, 0)
	if err != nil {
		return nil, err
	}
	out := make([]UserDTO, 0, len(records))
	for _, r := range records {
		out = append(out, userFromRecord(r))
	}
	return out, nil
}

// GetUser returns one gateway user by id.
func (q *Querier) GetUser(ctx context.Context, id string) (UserDTO, error) {
	if err := q.acquire(ctx); err != nil {
		return UserDTO{}, err
	}
	defer q.release()

	id = strings.TrimSpace(id)
	if id == "" {
		return UserDTO{}, fmt.Errorf("user id is required")
	}
	r, err := q.app.FindRecordById("users", id)
	if err != nil {
		return UserDTO{}, err
	}
	return userFromRecord(r), nil
}

// ListSessions returns gateway sessions, optionally filtered by user id.
// Always hard-capped at MaxListLimit (including when filtered by user).
// Active = revoked_at empty and expires_at > now.
func (q *Querier) ListSessions(ctx context.Context, userID string) ([]SessionDTO, error) {
	if err := q.acquire(ctx); err != nil {
		return nil, err
	}
	defer q.release()

	userID = strings.TrimSpace(userID)
	var records []*core.Record
	var err error
	if userID != "" {
		records, err = q.app.FindRecordsByFilter(
			"gateway_sessions",
			"gateway_user = {:user}",
			"-created",
			MaxListLimit,
			0,
			dbx.Params{"user": userID},
		)
	} else {
		records, err = q.app.FindRecordsByFilter("gateway_sessions", "", "-created", MaxListLimit, 0)
	}
	if err != nil {
		return nil, err
	}
	now := q.now()
	out := make([]SessionDTO, 0, len(records))
	for _, r := range records {
		out = append(out, sessionFromRecord(r, now))
	}
	return out, nil
}

// ListPolicies returns path policies.
func (q *Querier) ListPolicies(ctx context.Context) ([]pathpolicy.Policy, error) {
	if err := q.acquire(ctx); err != nil {
		return nil, err
	}
	defer q.release()
	return controlplane.ListPolicies(ctx, q.app)
}

// GetUpstream returns a redacted upstream DTO (never secrets).
func (q *Querier) GetUpstream(ctx context.Context) (UpstreamDTO, error) {
	if err := q.acquire(ctx); err != nil {
		return UpstreamDTO{}, err
	}
	defer q.release()

	state, err := controlplane.LoadUpstreamState(q.app)
	if err != nil {
		return UpstreamDTO{}, err
	}
	if state.Source == nil {
		return UpstreamDTO{Configured: false}, nil
	}
	src := state.Source
	dto := UpstreamDTO{
		Configured:                   true,
		Key:                          src.GetString("key"),
		ServerID:                     src.GetString("server_id"),
		ServerName:                   src.GetString("server_name"),
		ServerVersion:                src.GetString("server_version"),
		BackendUsername:              src.GetString("backend_username"),
		PasswordSet:                  src.GetString("backend_password") != "",
		TokenSet:                     src.GetString("backend_token") != "",
		BackendUserID:                src.GetString("backend_user_id"),
		LastLoginError:               src.GetString("last_login_error"),
		BackendUserAgent:             src.GetString("backend_user_agent"),
		BackendAuthorizationClient:   src.GetString("backend_authorization_client"),
		BackendAuthorizationDevice:   src.GetString("backend_authorization_device"),
		BackendAuthorizationVersion:  src.GetString("backend_authorization_version"),
		BackendAuthorizationDeviceID: src.GetString("backend_authorization_device_id"),
		AuthGenerationSet:            src.GetString("auth_generation_id") != "",
	}
	if t := src.GetDateTime("version_checked_at"); !t.IsZero() {
		tt := t.Time().UTC()
		dto.VersionCheckedAt = &tt
	}
	if t := src.GetDateTime("token_updated_at"); !t.IsZero() {
		tt := t.Time().UTC()
		dto.TokenUpdatedAt = &tt
	}
	if t := src.GetDateTime("last_login_at"); !t.IsZero() {
		tt := t.Time().UTC()
		dto.LastLoginAt = &tt
	}
	if defaultEP, err := defaultEndpoint(state.Endpoints); err == nil && defaultEP != nil {
		dto.BaseURL = defaultEP.GetString("base_url")
		dto.EndpointKey = defaultEP.GetString("key")
		dto.EndpointActive = defaultEP.GetBool("enabled")
	}
	return dto, nil
}

// ListAudit preserves the legacy bounded list interface. New callers use ListAuditPage.
func (q *Querier) ListAudit(ctx context.Context, from, to time.Time, limit int, cursor string) ([]AuditDTO, error) {
	page, err := q.ListAuditPage(ctx, AuditFilter{From: from, To: to, Limit: limit, Cursor: cursor})
	return page.Items, err
}

func userFromRecord(r *core.Record) UserDTO {
	dto := UserDTO{
		ID:              r.Id,
		Username:        r.GetString("username"),
		Email:           r.Email(),
		SyntheticUserID: r.GetString("synthetic_user_id"),
		Enabled:         r.GetBool("enabled"),
	}
	if t := r.GetDateTime("created"); !t.IsZero() {
		dto.Created = t.Time().UTC()
	}
	if t := r.GetDateTime("updated"); !t.IsZero() {
		dto.Updated = t.Time().UTC()
	}
	return dto
}

func sessionFromRecord(r *core.Record, now time.Time) SessionDTO {
	dto := SessionDTO{
		ID:              r.Id,
		GatewayUserID:   r.GetString("gateway_user"),
		GatewayUsername: r.GetString("gateway_username"),
		SyntheticUserID: r.GetString("synthetic_user_id"),
		Client:          r.GetString("client"),
		Device:          r.GetString("device"),
		DeviceID:        r.GetString("device_id"),
		Version:         r.GetString("version"),
		RemoteIP:        r.GetString("remote_ip"),
	}
	if t := r.GetDateTime("expires_at"); !t.IsZero() {
		dto.ExpiresAt = t.Time().UTC()
	}
	if t := r.GetDateTime("revoked_at"); !t.IsZero() {
		tt := t.Time().UTC()
		dto.RevokedAt = &tt
	}
	if t := r.GetDateTime("created"); !t.IsZero() {
		dto.Created = t.Time().UTC()
	}
	dto.Active = dto.RevokedAt == nil && dto.ExpiresAt.After(now)
	return dto
}

func auditFromRecord(r *core.Record) AuditDTO {
	dto := AuditDTO{
		ID:                r.Id,
		GatewayUserID:     r.GetString("gateway_user"),
		SyntheticUserID:   r.GetString("synthetic_user_id"),
		Event:             r.GetString("event"),
		Message:           r.GetString("message"),
		Method:            r.GetString("method"),
		Path:              r.GetString("path"),
		Status:            r.GetInt("status"),
		RemoteIP:          r.GetString("remote_ip"),
		ErrorKind:         r.GetString("error_kind"),
		Direction:         r.GetString("direction"),
		BytesTransferred:  int64(r.GetInt("bytes_transferred")),
		DurationMS:        int64(r.GetInt("duration_ms")),
		UpstreamStatus:    r.GetInt("upstream_status"),
		ResponseCommitted: r.GetBool("response_committed"),
	}
	if t := r.GetDateTime("created"); !t.IsZero() {
		dto.Created = t.Time().UTC()
	}
	return sanitizeAudit(dto)
}

func defaultEndpoint(endpoints []*core.Record) (*core.Record, error) {
	var defaultEP *core.Record
	for _, ep := range endpoints {
		if ep.GetBool("is_default") {
			if defaultEP != nil {
				return nil, fmt.Errorf("multiple default endpoints")
			}
			defaultEP = ep
		}
	}
	return defaultEP, nil
}
