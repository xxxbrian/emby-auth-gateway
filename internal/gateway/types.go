package gateway

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/pathpolicy"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

type Config struct {
	PublicBaseURL            string
	GatewayBasePath          string
	GatewayServerID          string
	HTTPClient               *http.Client
	MinResumePct             float64
	MaxResumePct             float64
	MinResumeDurationSeconds float64
	Emitter                  *observe.Emitter                   // optional; nil = no-op
	Meter                    TrafficMeter                       // optional live bandwidth meter; nil = no-op
	MediaBuffer              *MediaBuffer                       // optional; nil preserves synchronous media copying
	MediaBufferLive          *telemetry.MediaBufferLiveRegistry // optional live observation; nil disables observation
}

type Store interface {
	LoadDefaultUpstreamRuntime(ctx context.Context) (*UpstreamRuntime, error)
	CompareAndSwapUpstreamAuth(ctx context.Context, update UpstreamAuthUpdate) error
	UpdateUpstreamServerInfo(ctx context.Context, update UpstreamServerInfoUpdate) error
	AuthenticateGatewayUser(ctx context.Context, username, password string) (*GatewayUser, error)
	FindGatewayUserByUsername(ctx context.Context, username string) (*GatewayUser, error)
	ListPublicUsers(ctx context.Context) ([]GatewayUser, error)
	FindUserBySyntheticID(ctx context.Context, syntheticID string) (*GatewayUser, error)
	RecordAudit(ctx context.Context, entry AuditLog) error
	CheckPathPolicy(ctx context.Context, method, relativePath string) (PathPolicyDecision, error)
	RecordPlaybackEvent(ctx context.Context, event PlaybackEvent) error
	FindPlaybackState(ctx context.Context, gatewayUserID, itemID string) (*PlaybackState, error)
	ListPlaybackStatesByItemIDs(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*PlaybackState, error)
	ListPlaybackAggregates(ctx context.Context, gatewayUserID string, seriesIDs, seasonIDs []string) (PlaybackAggregates, error)
	ListItemChildCounts(ctx context.Context, itemIDs []string) (map[string]ItemChildCount, error)
	SaveItemChildCount(ctx context.Context, count ItemChildCount) error
	// SaveItemChildCounts upserts many child-count rows. Invalid entries (empty id or count<=0) are skipped.
	// Implementations should minimize store round-trips (batch load existing, then save).
	SaveItemChildCounts(ctx context.Context, counts []ItemChildCount) error
	ListPlaybackStates(ctx context.Context, gatewayUserID string, filter PlaybackStateFilter) ([]PlaybackState, error)
	SavePlaybackState(ctx context.Context, state PlaybackState) error
	// SavePlaybackResolution persists metadata/orphan/last-seen fields for an item
	// without overwriting user-data fields (played, position, favorite, likes, etc.).
	// Creates a row if missing. Used by item resolution/repair paths.
	SavePlaybackResolution(ctx context.Context, state PlaybackState) error
	FindDisplayPreference(ctx context.Context, gatewayUserID, preferenceID, client string) (*DisplayPreference, error)
	SaveDisplayPreference(ctx context.Context, preference DisplayPreference) error
	SaveSession(ctx context.Context, session *Session) error
	FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	// SessionTokenExists reports whether any session row exists for tokenHash
	// (active, revoked, or expired) without hydrating account/session details.
	// false,nil means not found; non-nil error is an operational/store failure.
	SessionTokenExists(ctx context.Context, tokenHash string) (bool, error)
	RevokeSession(ctx context.Context, tokenHash string) error
}

// UpstreamSource is the singleton upstream authentication and identity configuration.
type UpstreamSource struct {
	ID               string
	Key              string
	ServerID         string
	ServerName       string
	ServerVersion    string
	VersionCheckedAt *time.Time
	BackendUsername  string
	BackendPassword  string
	BackendUserID    string
	BackendToken     string
	AuthGenerationID string
	TokenUpdatedAt   *time.Time
	LastLoginAt      *time.Time
	LastLoginError   string
	ClientIdentity   BackendClientIdentity
}

type UpstreamEndpoint struct {
	ID       string
	SourceID string
	Key      string
	BaseURL  string
	Active   bool
}

type UpstreamRuntime struct {
	Source   UpstreamSource
	Endpoint UpstreamEndpoint
}

type UpstreamAuthUpdate struct {
	SourceID             string
	ExpectedGenerationID string
	GenerationID         string
	DeviceID             string
	BackendUserID        string
	BackendToken         string
	AuthenticatedAt      time.Time
}

// UpstreamServerInfoUpdate updates metadata observed from the configured
// upstream without allowing its server namespace to change.
type UpstreamServerInfoUpdate struct {
	SourceID      string
	ServerID      string
	ServerName    string
	ServerVersion string
	CheckedAt     time.Time
}

const (
	upstreamSourceIDMaxLength       = 15
	upstreamServerIDMaxLength       = 255
	upstreamServerNameMaxLength     = 255
	upstreamServerVersionMaxLength  = 80
	upstreamAuthGenerationMaxLength = 128
	upstreamDeviceIDMaxLength       = 255
	upstreamBackendUserIDMaxLength  = 80
)

func ValidateUpstreamRuntime(runtime UpstreamRuntime) error {
	source := runtime.Source
	if strings.TrimSpace(source.ID) == "" || source.Key != "default" || strings.TrimSpace(source.ServerID) == "" || strings.TrimSpace(source.BackendUsername) == "" || strings.TrimSpace(source.BackendPassword) == "" {
		return invalidUpstreamTopology("missing required source fields")
	}
	identity := source.ClientIdentity
	if strings.TrimSpace(identity.UserAgent) == "" || strings.TrimSpace(identity.Client) == "" || strings.TrimSpace(identity.Device) == "" || strings.TrimSpace(identity.DeviceID) == "" || strings.TrimSpace(identity.Version) == "" {
		return invalidUpstreamTopology("missing client identity fields")
	}
	if source.AuthGenerationID != "" {
		if err := validatePersistedUpstreamAuth(source); err != nil {
			return err
		}
	}
	if !runtime.Endpoint.Active {
		return invalidUpstreamTopology("invalid active endpoint")
	}
	if err := ValidateUpstreamEndpoint(source.ID, runtime.Endpoint); err != nil {
		return err
	}
	return nil
}

func ValidateUpstreamEndpoint(sourceID string, endpoint UpstreamEndpoint) error {
	if strings.TrimSpace(endpoint.ID) == "" || endpoint.SourceID != sourceID || strings.TrimSpace(endpoint.Key) == "" {
		return invalidUpstreamTopology("invalid endpoint")
	}
	parsed, err := url.Parse(endpoint.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" {
		return invalidUpstreamTopology("invalid endpoint URL")
	}
	return nil
}

func ValidateUpstreamAuthUpdate(update UpstreamAuthUpdate) error {
	if strings.TrimSpace(update.SourceID) == "" || !validUpstreamGeneration(update.GenerationID) || (update.ExpectedGenerationID != "" && !validUpstreamGeneration(update.ExpectedGenerationID)) || update.GenerationID == update.ExpectedGenerationID || !validUpstreamDeviceID(update.DeviceID) || !validUpstreamBackendUserID(update.BackendUserID) || !validUpstreamToken(update.BackendToken) || update.AuthenticatedAt.IsZero() {
		return fmt.Errorf("%w: invalid upstream authentication update", ErrBadRequest)
	}
	return nil
}

func ValidateUpstreamServerInfoUpdate(update UpstreamServerInfoUpdate) error {
	if !validUpstreamSourceID(update.SourceID) || !validUpstreamServerID(update.ServerID) ||
		!validOptionalUpstreamServerName(update.ServerName) || !validOptionalUpstreamServerVersion(update.ServerVersion) ||
		update.CheckedAt.IsZero() {
		return fmt.Errorf("%w: invalid upstream server info update", ErrBadRequest)
	}
	return nil
}

func validatePersistedUpstreamAuth(source UpstreamSource) error {
	if !validUpstreamGeneration(source.AuthGenerationID) || !validUpstreamDeviceID(source.ClientIdentity.DeviceID) || !validUpstreamBackendUserID(source.BackendUserID) || !validUpstreamToken(source.BackendToken) || source.TokenUpdatedAt == nil || source.TokenUpdatedAt.IsZero() || source.LastLoginAt == nil || source.LastLoginAt.IsZero() {
		return invalidUpstreamTopology("incomplete managed authentication")
	}
	return nil
}

func validUpstreamGeneration(value string) bool {
	return isTrimmed(value) && len(value) <= upstreamAuthGenerationMaxLength
}

func validUpstreamSourceID(value string) bool {
	return isTrimmed(value) && utf8.RuneCountInString(value) <= upstreamSourceIDMaxLength
}

func validUpstreamServerID(value string) bool {
	return isTrimmed(value) && utf8.RuneCountInString(value) <= upstreamServerIDMaxLength
}

func validOptionalUpstreamServerName(value string) bool {
	return value == "" || (isTrimmed(value) && utf8.RuneCountInString(value) <= upstreamServerNameMaxLength)
}

func validOptionalUpstreamServerVersion(value string) bool {
	return value == "" || (isTrimmed(value) && utf8.RuneCountInString(value) <= upstreamServerVersionMaxLength)
}

func validUpstreamDeviceID(value string) bool {
	return isTrimmed(value) && len(value) <= upstreamDeviceIDMaxLength
}

func validUpstreamBackendUserID(value string) bool {
	return isTrimmed(value) && len(value) <= upstreamBackendUserIDMaxLength
}

func validUpstreamToken(value string) bool {
	return isTrimmed(value)
}

func isTrimmed(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func invalidUpstreamTopology(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidUpstreamTopology, message)
}

type AuditLog struct {
	ID              string
	GatewayUserID   string
	SyntheticUserID string
	Event           string
	Message         string
	RemoteIP        string
	Method          string
	Path            string
	Status          int
	ErrorKind       string
	Direction       string
	// BytesTransferred is the number of response body bytes successfully written downstream.
	BytesTransferred  int64
	DurationMS        int
	UpstreamStatus    int
	ResponseCommitted bool
	CreatedAt         time.Time
}

type PathPolicy = pathpolicy.Policy
type PathPolicyDecision = pathpolicy.Decision

type PlaybackEvent struct {
	ID               string
	GatewayUserID    string
	SyntheticUserID  string
	ItemID           string
	ItemName         string
	Event            string
	PositionTicks    int64
	Played           *bool
	PlayedPercentage *float64
	RemoteIP         string
	CreatedAt        time.Time
}

type PlaybackState struct {
	ID                    string
	GatewayUserID         string
	SyntheticUserID       string
	ItemID                string
	ItemName              string
	ItemType              string
	SeriesID              string
	SeriesName            string
	SeasonID              string
	IndexNumber           int
	ParentIndexNumber     int
	RunTimeTicks          int64
	PlaybackPositionTicks int64
	Played                bool
	PlayedPercentage      *float64
	LastPlayedDate        *time.Time
	PlayCount             int
	IsFavorite            bool
	Likes                 *bool
	Fingerprint           string
	OrphanedAt            *time.Time
	LastSeenAt            *time.Time
	UpdatedAt             time.Time
}

type PlaybackStateFilter struct {
	Played          *bool
	Favorite        *bool
	Resumable       *bool
	SeriesID        string
	SeasonID        string
	IncludeOrphaned bool
}

type PlaybackAggregate struct {
	PlayedCount      int
	KnownItemCount   int
	TotalItemCount   int
	LastPlayedDate   *time.Time
	LastActivityDate *time.Time
}

type PlaybackAggregates struct {
	Series  map[string]PlaybackAggregate
	Seasons map[string]PlaybackAggregate
}

type ItemChildCount struct {
	ItemID     string
	ChildCount int
	UpdatedAt  time.Time
}

type DisplayPreference struct {
	ID              string
	GatewayUserID   string
	SyntheticUserID string
	PreferenceID    string
	Client          string
	PayloadJSON     string
	UpdatedAt       time.Time
}

func DecidePathPolicy(policies []PathPolicy, method, path string) PathPolicyDecision {
	return pathpolicy.Decide(policies, method, path)
}

func FirstMatchingPathPolicy(policies []PathPolicy, method, path string) (PathPolicy, bool) {
	return pathpolicy.FirstMatch(policies, method, path)
}

type GatewayUser struct {
	ID              string
	Username        string
	SyntheticUserID string
	Enabled         bool
}

type BackendClientIdentity struct {
	UserAgent string
	Client    string
	Device    string
	DeviceID  string
	Version   string
}

func DefaultBackendClientIdentity() BackendClientIdentity {
	return BackendClientIdentity{
		UserAgent: defaultBackendUserAgent,
		Client:    defaultBackendAuthorizationClient,
		Device:    defaultBackendAuthorizationDevice,
		Version:   defaultBackendAuthorizationVersion,
	}
}

func (i BackendClientIdentity) WithDefaults() BackendClientIdentity {
	defaults := DefaultBackendClientIdentity()
	if strings.TrimSpace(i.UserAgent) == "" {
		i.UserAgent = defaults.UserAgent
	}
	if strings.TrimSpace(i.Client) == "" {
		i.Client = defaults.Client
	}
	if strings.TrimSpace(i.Device) == "" {
		i.Device = defaults.Device
	}
	if strings.TrimSpace(i.Version) == "" {
		i.Version = defaults.Version
	}
	return i
}

func StableBackendDeviceID(seed string) string {
	sum := sha1.Sum([]byte(seed))
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08X-%04X-%04X-%04X-%012X",
		uint32(sum[0])<<24|uint32(sum[1])<<16|uint32(sum[2])<<8|uint32(sum[3]),
		uint16(sum[4])<<8|uint16(sum[5]),
		uint16(sum[6])<<8|uint16(sum[7]),
		uint16(sum[8])<<8|uint16(sum[9]),
		uint64(sum[10])<<40|uint64(sum[11])<<32|uint64(sum[12])<<24|uint64(sum[13])<<16|uint64(sum[14])<<8|uint64(sum[15]),
	)
}

type Session struct {
	GatewayTokenHash string
	GatewayUserID    string
	GatewayUsername  string
	SyntheticUserID  string
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
