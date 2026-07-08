package gateway

import (
	"context"
	"crypto/sha1"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Config struct {
	PublicBaseURL            string
	GatewayBasePath          string
	GatewayServerID          string
	HTTPClient               *http.Client
	MinResumePct             float64
	MaxResumePct             float64
	MinResumeDurationSeconds float64
}

type Store interface {
	AuthenticateGatewayUser(ctx context.Context, username, password string) (*GatewayUser, error)
	FindGatewayUserByUsername(ctx context.Context, username string) (*GatewayUser, error)
	ListPublicUsers(ctx context.Context) ([]GatewayUser, error)
	FindUserBySyntheticID(ctx context.Context, syntheticID string) (*GatewayUser, error)
	FindMappingByGatewayUserID(ctx context.Context, gatewayUserID string) (*UserMapping, error)
	DefaultBackend(ctx context.Context) (*BackendAccount, error)
	RecordAudit(ctx context.Context, entry AuditLog) error
	CheckPathPolicy(ctx context.Context, method, relativePath string) (PathPolicyDecision, error)
	RecordPlaybackEvent(ctx context.Context, event PlaybackEvent) error
	FindPlaybackState(ctx context.Context, gatewayUserID, itemID string) (*PlaybackState, error)
	ListPlaybackStatesByItemIDs(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*PlaybackState, error)
	ListPlaybackAggregates(ctx context.Context, gatewayUserID string, seriesIDs, seasonIDs []string) (PlaybackAggregates, error)
	ListItemChildCounts(ctx context.Context, backendAccountID string, itemIDs []string) (map[string]ItemChildCount, error)
	SaveItemChildCount(ctx context.Context, count ItemChildCount) error
	ListPlaybackStates(ctx context.Context, gatewayUserID string, filter PlaybackStateFilter) ([]PlaybackState, error)
	SavePlaybackState(ctx context.Context, state PlaybackState) error
	FindDisplayPreference(ctx context.Context, gatewayUserID, preferenceID, client string) (*DisplayPreference, error)
	SaveDisplayPreference(ctx context.Context, preference DisplayPreference) error
	SaveSession(ctx context.Context, session *Session) error
	FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	RevokeSession(ctx context.Context, tokenHash string) error
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
	CreatedAt       time.Time
}

type PathPolicy struct {
	ID       string
	Method   string
	Path     string
	Action   string
	Reason   string
	Priority int
	Enabled  bool
}

func (p PathPolicy) Deny() bool {
	return strings.EqualFold(p.Action, "deny")
}

type PathPolicyDecision struct {
	Allowed  bool
	Action   string
	PolicyID string
}

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
	BackendAccountID string
	ItemID           string
	ChildCount       int
	UpdatedAt        time.Time
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
	policy, ok := FirstMatchingPathPolicy(policies, method, path)
	if !ok {
		return PathPolicyDecision{Allowed: true, Action: "allow"}
	}
	if policy.Deny() {
		return PathPolicyDecision{Allowed: false, Action: "deny", PolicyID: policy.ID}
	}
	return PathPolicyDecision{Allowed: true, Action: "allow", PolicyID: policy.ID}
}

func FirstMatchingPathPolicy(policies []PathPolicy, method, path string) (PathPolicy, bool) {
	matched := make([]PathPolicy, 0, len(policies))
	for _, policy := range policies {
		if policy.Enabled && methodMatches(policy.Method, method) && pathMatches(policy.Path, path) {
			matched = append(matched, policy)
		}
	}
	if len(matched) == 0 {
		return PathPolicy{}, false
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Action != matched[j].Action {
			return strings.EqualFold(matched[i].Action, "deny")
		}
		if matched[i].Priority != matched[j].Priority {
			return matched[i].Priority > matched[j].Priority
		}
		if matched[i].Method != matched[j].Method {
			return matched[i].Method < matched[j].Method
		}
		if matched[i].Path != matched[j].Path {
			return matched[i].Path < matched[j].Path
		}
		return matched[i].ID < matched[j].ID
	})
	return matched[0], true
}

type GatewayUser struct {
	ID              string
	Username        string
	SyntheticUserID string
	Enabled         bool
}

type BackendAccount struct {
	ID             string
	ServerID       string
	BaseURL        string
	Username       string
	Password       string
	Enabled        bool
	ClientIdentity BackendClientIdentity
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
	BackendIdentity  BackendClientIdentity
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
