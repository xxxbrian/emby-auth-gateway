// Package observe provides a non-blocking event emitter for gateway telemetry.
package observe

import "time"

// Kind is a low-cardinality event category.
type Kind string

const (
	KindAuthLogin           Kind = "auth_login"
	KindAuthLogout          Kind = "auth_logout"
	KindUpstreamRequest     Kind = "upstream_request"
	KindUpstreamAuthRefresh Kind = "upstream_auth_refresh"
	KindCapacityReject      Kind = "capacity_reject"
	KindPlayback            Kind = "playback"
	KindMediaTransfer       Kind = "media_transfer"
	KindUserdataError       Kind = "userdata_error"
	KindRequest             Kind = "request"
	KindReliability         Kind = "reliability"
)

// Route class values (low cardinality).
const (
	RouteAuth     = "auth"
	RouteMetadata = "metadata"
	RouteUserdata = "userdata"
	RouteMedia    = "media"
	RoutePlayback = "playback"
	RouteOther    = "other"
)

// Outcome values (low cardinality).
const (
	OutcomeOK      = "ok"
	OutcomeError   = "error"
	OutcomeDenied  = "denied"
	OutcomeTimeout = "timeout"
)

// Status class values (low cardinality).
const (
	Status2xx = "2xx"
	Status3xx = "3xx"
	Status4xx = "4xx"
	Status5xx = "5xx"
	Status0   = "0"
)

// Media mode values (low cardinality).
const (
	MediaDirect  = "direct"
	MediaHLS     = "hls"
	MediaRange   = "range"
	MediaUnknown = "unknown"
)

// Direction values (low cardinality).
const (
	DirectionUpstream   = "upstream"
	DirectionDownstream = "downstream"
)

// Playback event values.
const (
	PlaybackPlaying  = "playing"
	PlaybackProgress = "progress"
	PlaybackStopped  = "stopped"
)

// Event is a fixed-schema observation. Series labels must stay low-cardinality;
// identity fields (UserID, ItemName, etc.) are for current-state maps only.
type Event struct {
	Kind Kind
	At   time.Time

	// Fixed low-cardinality fields only.
	RouteClass  string // auth|metadata|userdata|media|playback|other
	Outcome     string // ok|error|denied|timeout
	StatusClass string // 2xx|3xx|4xx|5xx|0
	ErrorKind   string
	MediaMode   string // direct|hls|range|unknown
	Direction   string // upstream|downstream|""
	Method      string

	// Identity for current-state maps only (not time-series labels).
	UserID    string
	Username  string
	SessionID string
	Device    string
	ItemID    string
	ItemName  string // optional; telemetry must not put this in series labels

	BytesIn       int64
	BytesOut      int64
	DurationMS    int64
	PositionTicks int64
	IsPaused      bool
	PlaybackEvent string // playing|progress|stopped
}
