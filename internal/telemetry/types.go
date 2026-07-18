package telemetry

import "time"

// Snapshot is a JSON-friendly operational metrics view.
type Snapshot struct {
	TS        time.Time `json:"ts"`
	BootID    string    `json:"boot_id"`
	UptimeSec int64     `json:"uptime_sec"`

	Upstream    UpstreamStatus    `json:"upstream"`
	Capacity    CapacityStatus    `json:"capacity"`
	Traffic     TrafficStatus     `json:"traffic"`
	Reliability ReliabilityStatus `json:"reliability"`
	Runtime     RuntimeStatus     `json:"runtime"`
	Series      SeriesData        `json:"series"`
}

// UpstreamStatus summarizes recent upstream health.
type UpstreamStatus struct {
	LastOKAt        *time.Time `json:"last_ok_at,omitempty"`
	LastErrorAt     *time.Time `json:"last_error_at,omitempty"`
	LastStatusClass string     `json:"last_status_class,omitempty"`
	LastErrorKind   string     `json:"last_error_kind,omitempty"`
	LastLatencyMS   int64      `json:"last_latency_ms,omitempty"`
	AuthOK          bool       `json:"auth_ok"`
	LastAuthAt      *time.Time `json:"last_auth_at,omitempty"`
	LastAuthError   string     `json:"last_auth_error,omitempty"`
}

// CapacityStatus tracks shared-account load.
type CapacityStatus struct {
	ActivePlaybacks      int     `json:"active_playbacks"`
	ActiveMediaTransfers int     `json:"active_media_transfers"`
	ActiveSessions       int     `json:"active_sessions"`
	RejectRate5m         float64 `json:"reject_rate_5m"`
	Rejects5m            int64   `json:"rejects_5m"`
}

// TrafficStatus is short-window traffic rates.
type TrafficStatus struct {
	RPS          float64 `json:"rps"`
	MbpsIn       float64 `json:"mbps_in"`
	MbpsOut      float64 `json:"mbps_out"`
	ErrorRate15m float64 `json:"error_rate_15m"`
}

// ReliabilityStatus tracks local userdata/overlay health and telemetry drops.
type ReliabilityStatus struct {
	UserdataWriteFail5m int64  `json:"userdata_write_fail_5m"`
	OverlayFail5m       int64  `json:"overlay_fail_5m"`
	TelemetryDrops      uint64 `json:"telemetry_drops"`
}

// RuntimeStatus is process runtime stats sampled at snapshot time.
type RuntimeStatus struct {
	Goroutines int    `json:"goroutines"`
	HeapBytes  uint64 `json:"heap_bytes"`
}

// SeriesData holds recent chart points (low-cardinality values only).
type SeriesData struct {
	RPS       []SeriesPoint `json:"rps"`
	MbpsOut   []SeriesPoint `json:"mbps_out"`
	Errors    []SeriesPoint `json:"errors"`
	Playbacks []SeriesPoint `json:"playbacks"`
}

// SeriesPoint is a single chart sample.
type SeriesPoint struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Playback is an active playback session (current-state map entry).
type Playback struct {
	SessionID     string    `json:"session_id"`
	UserID        string    `json:"user_id"`
	Username      string    `json:"username"`
	Device        string    `json:"device"`
	ItemID        string    `json:"item_id"`
	ItemName      string    `json:"item_name,omitempty"`
	PositionTicks int64     `json:"position_ticks"`
	IsPaused      bool      `json:"is_paused"`
	LastSeen      time.Time `json:"last_seen"`
	StartedAt     time.Time `json:"started_at"`
}

// Transfer is an open media transfer (current-state map entry).
type Transfer struct {
	SessionID string    `json:"session_id"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	Device    string    `json:"device"`
	ItemID    string    `json:"item_id"`
	MediaMode string    `json:"media_mode"`
	BytesIn   int64     `json:"bytes_in"`
	BytesOut  int64     `json:"bytes_out"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
}

type sessionState struct {
	SessionID string
	UserID    string
	Username  string
	Device    string
	IP        string
	LastSeen  time.Time
}

type playbackState struct {
	Playback
}

type transferState struct {
	Transfer
}

type upstreamState struct {
	LastOKAt        time.Time
	HasLastOK       bool
	LastErrorAt     time.Time
	HasLastError    bool
	LastStatusClass string
	LastErrorKind   string
	LastLatencyMS   int64
	AuthOK          bool
	LastAuthAt      time.Time
	HasLastAuth     bool
	LastAuthError   string
}
