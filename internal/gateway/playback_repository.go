package gateway

import (
	"context"
	"time"
)

// PlaybackReportCommand is the repository input for one local playback report.
// GatewayTokenHash is the sole ownership key; client payload must not supply
// gateway user or session identity.
//
// Callers (and repositories) must run PreparePlaybackReportCommand before any
// store lookup so token/item keys are canonical.
type PlaybackReportCommand struct {
	GatewayTokenHash string
	Kind             PlaybackReportKind
	ReceivedAt       time.Time
	RemoteIP         string

	ItemID        string
	PlaySessionID string
	MediaSourceID string

	// ItemSnapshot and PlayState are allowlisted patches; empty fields omit.
	ItemSnapshot PlaybackItemSnapshot
	PlayState    PlaybackPlayState
	// MetadataConfirmed is trusted internal provenance. HTTP parsing never sets it;
	// the boundary sets it only after upstream metadata exactly matches ItemID.
	MetadataConfirmed bool

	// RunTimeTicks is 0 when unknown. Played/PlayedPercentage are explicit only
	// when present on the report (Stopped completion semantics).
	RunTimeTicks     int64
	Played           *bool
	PlayedPercentage *float64

	// EventName carries client EventName such as Pause/Unpause for IsPaused overrides.
	EventName string

	// Policy controls durable stop completion. Zero fields are filled with
	// defaults by PreparePlaybackReportCommand (including MinDurationSeconds=0).
	// Explicit nonzero thresholds are preserved when valid.
	Policy PlaybackResumePolicy
}

// PlaybackReportResult is the minimal post-commit outcome for HTTP success and
// telemetry. Applied is false for true no-op successes (missing item, mismatch
// suppressions, empty reports). Identity fields are repository-derived.
type PlaybackReportResult struct {
	Applied         bool
	PublicSessionID string
	GatewayUserID   string
	SyntheticUserID string
	ItemID          string
	// Current is the resulting now-playing row; nil when cleared or absent.
	Current *CurrentPlayback
	// Durable is the resulting durable item state when written; nil for Ping/no-op.
	Durable *PlaybackState
}

// PlaybackRepository is the persistence boundary for gateway-owned current
// playback and the atomic apply path that also updates durable item state.
type PlaybackRepository interface {
	// ApplyPlaybackReport loads session/current/durable state, reduces the report,
	// and commits current playback, durable state, event (except Ping), and
	// profile activity as one unit.
	ApplyPlaybackReport(ctx context.Context, cmd PlaybackReportCommand) (PlaybackReportResult, error)
	// ListCurrentPlaybacks returns current playback rows keyed by gateway token hash
	// for the requested hashes. Missing hashes are omitted from the map.
	ListCurrentPlaybacks(ctx context.Context, tokenHashes []string) (map[string]CurrentPlayback, error)
}
