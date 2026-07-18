package gateway

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Schema-aligned typed bounds for CurrentPlayback (pbschema.CurrentPlaybacks).
const (
	currentPlaybackItemIDMaxBytes        = 80
	currentPlaybackPlaySessionIDMaxBytes = 255
	currentPlaybackMediaSourceIDMaxBytes = 255
	currentPlaybackItemSnapshotJSONMin   = 2
	currentPlaybackItemSnapshotJSONMax   = 65536
	currentPlaybackPlayStateJSONMin      = 2
	currentPlaybackPlayStateJSONMax      = 16384
	playbackCommunityRatingMax           = 10
	playbackVolumeLevelMax               = 100
)

// CurrentPlayback is the gateway-owned now-playing aggregate, keyed one-to-one
// by auth session token hash. It is not embedded in the session auth row.
type CurrentPlayback struct {
	GatewayTokenHash string
	ItemID           string
	PlaySessionID    string
	MediaSourceID    string
	ItemSnapshot     PlaybackItemSnapshot
	PlayState        PlaybackPlayState
	RunTimeTicks     int64
	StartedAt        time.Time
	LastReportedAt   time.Time
}

// ValidateCurrentPlayback checks typed in-memory current-playback prestate.
// expectedTokenHash is the authoritative map key / request / session token hash.
// Missing rows are handled by callers; a present invalid row is an integrity error
// and must never be silently omitted as idle.
func ValidateCurrentPlayback(cp *CurrentPlayback, expectedTokenHash string) error {
	if cp == nil {
		return fmt.Errorf("current playback integrity: row is nil")
	}
	if expectedTokenHash == "" {
		return fmt.Errorf("current playback integrity: expected gateway token hash is empty")
	}
	if cp.GatewayTokenHash != expectedTokenHash {
		return fmt.Errorf(
			"current playback integrity: gateway token hash %q does not match map/session key %q",
			cp.GatewayTokenHash,
			expectedTokenHash,
		)
	}
	if err := validateCurrentPlaybackItemID(cp.ItemID); err != nil {
		return err
	}
	if cp.ItemSnapshot.ID != cp.ItemID {
		return fmt.Errorf(
			"current playback integrity: item snapshot Id %q does not equal item_id %q",
			cp.ItemSnapshot.ID,
			cp.ItemID,
		)
	}
	if err := validatePlaybackItemSnapshotFields(cp.ItemSnapshot, cp.ItemID); err != nil {
		return err
	}
	if err := validatePlaybackPlayStateFields(cp.PlayState); err != nil {
		return err
	}
	if err := validateCurrentPlaybackOptionalIdentity("play_session_id", cp.PlaySessionID, currentPlaybackPlaySessionIDMaxBytes); err != nil {
		return err
	}
	if err := validateCurrentPlaybackOptionalIdentity("media_source_id", cp.MediaSourceID, currentPlaybackMediaSourceIDMaxBytes); err != nil {
		return err
	}
	if cp.RunTimeTicks < 0 {
		return fmt.Errorf("current playback integrity: run_time_ticks %d is negative", cp.RunTimeTicks)
	}
	if cp.StartedAt.IsZero() {
		return fmt.Errorf("current playback integrity: started_at is zero")
	}
	if cp.LastReportedAt.IsZero() {
		return fmt.Errorf("current playback integrity: last_reported_at is zero")
	}
	startedAt := cp.StartedAt.UTC()
	lastReportedAt := cp.LastReportedAt.UTC()
	if lastReportedAt.Before(startedAt) {
		return fmt.Errorf(
			"current playback integrity: last_reported_at %s is before started_at %s",
			lastReportedAt.Format(time.RFC3339Nano),
			startedAt.Format(time.RFC3339Nano),
		)
	}
	if err := validateCurrentPlaybackJSONSize("item_snapshot_json", cp.ItemSnapshot, currentPlaybackItemSnapshotJSONMin, currentPlaybackItemSnapshotJSONMax); err != nil {
		return err
	}
	if err := validateCurrentPlaybackJSONSize("play_state_json", cp.PlayState, currentPlaybackPlayStateJSONMin, currentPlaybackPlayStateJSONMax); err != nil {
		return err
	}
	return nil
}

func validateCurrentPlaybackItemID(itemID string) error {
	if itemID == "" {
		return fmt.Errorf("current playback integrity: item_id is required")
	}
	if len(itemID) > currentPlaybackItemIDMaxBytes {
		return fmt.Errorf("current playback integrity: item_id length %d exceeds max %d", len(itemID), currentPlaybackItemIDMaxBytes)
	}
	if !utf8.ValidString(itemID) {
		return fmt.Errorf("current playback integrity: item_id is not valid UTF-8")
	}
	if strings.TrimSpace(itemID) != itemID {
		return fmt.Errorf("current playback integrity: item_id is not canonical")
	}
	if itemID == "." || itemID == ".." || strings.ContainsAny(itemID, `/\`) {
		return fmt.Errorf("current playback integrity: item_id must be one safe path segment")
	}
	for _, r := range itemID {
		if unicode.IsControl(r) {
			return fmt.Errorf("current playback integrity: item_id contains a control character")
		}
	}
	return nil
}

func validateCurrentPlaybackOptionalText(field, value string, maxBytes int) error {
	if len(value) > maxBytes {
		return fmt.Errorf("current playback integrity: %s length %d exceeds max %d", field, len(value), maxBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("current playback integrity: %s is not valid UTF-8", field)
	}
	return nil
}

func validateCurrentPlaybackOptionalIdentity(field, value string, maxBytes int) error {
	if err := validateCurrentPlaybackOptionalText(field, value, maxBytes); err != nil {
		return err
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("current playback integrity: %s is not canonical", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("current playback integrity: %s contains a control character", field)
		}
	}
	return nil
}

func validatePlaybackItemSnapshotFields(snap PlaybackItemSnapshot, itemID string) error {
	if itemID != "" && snap.ID != itemID {
		return fmt.Errorf("current playback integrity: item snapshot Id %q does not equal item_id %q", snap.ID, itemID)
	}
	checks := []struct {
		field string
		value string
		max   int
	}{
		{"item_snapshot.name", snap.Name, playbackSnapshotNameMaxBytes},
		{"item_snapshot.type", snap.Type, playbackSnapshotTypeMaxBytes},
		{"item_snapshot.media_type", snap.MediaType, playbackSnapshotTypeMaxBytes},
		{"item_snapshot.series_id", snap.SeriesID, playbackSnapshotSeriesMaxBytes},
		{"item_snapshot.series_name", snap.SeriesName, playbackSnapshotNameMaxBytes},
		{"item_snapshot.season_id", snap.SeasonID, playbackSnapshotSeriesMaxBytes},
		{"item_snapshot.parent_id", snap.ParentID, playbackSnapshotSeriesMaxBytes},
		{"item_snapshot.premiere_date", snap.PremiereDate, playbackSnapshotDateMaxBytes},
		{"item_snapshot.official_rating", snap.OfficialRating, playbackSnapshotRatingMaxBytes},
	}
	for _, check := range checks {
		if err := validateCurrentPlaybackOptionalText(check.field, check.value, check.max); err != nil {
			return err
		}
	}
	if snap.IndexNumber < 0 || snap.ParentIndexNumber < 0 || snap.RunTimeTicks < 0 || snap.ProductionYear < 0 {
		return fmt.Errorf("current playback integrity: item snapshot numeric field is negative")
	}
	if math.IsNaN(snap.CommunityRating) || math.IsInf(snap.CommunityRating, 0) || snap.CommunityRating < 0 || snap.CommunityRating > playbackCommunityRatingMax {
		return fmt.Errorf("current playback integrity: item_snapshot.community_rating must be finite and within [0,%d]", playbackCommunityRatingMax)
	}
	if len(snap.ImageTags) > playbackMaxImageTags {
		return fmt.Errorf("current playback integrity: item_snapshot.image_tags count %d exceeds max %d", len(snap.ImageTags), playbackMaxImageTags)
	}
	for key, value := range snap.ImageTags {
		if key == "" {
			return fmt.Errorf("current playback integrity: item_snapshot.image_tags key is empty")
		}
		if err := validateCurrentPlaybackOptionalText("item_snapshot.image_tags.key", key, playbackImageTagKeyMaxBytes); err != nil {
			return err
		}
		for _, r := range key {
			if unicode.IsControl(r) {
				return fmt.Errorf("current playback integrity: item_snapshot.image_tags key contains a control character")
			}
		}
		if err := validateCurrentPlaybackOptionalText("item_snapshot.image_tags.value", value, playbackImageTagValueMaxBytes); err != nil {
			return err
		}
	}
	return nil
}

func validatePlaybackPlayStateFields(ps PlaybackPlayState) error {
	if ps.PositionTicks != nil && *ps.PositionTicks < 0 {
		return fmt.Errorf("current playback integrity: position_ticks must be nonnegative")
	}
	if ps.VolumeLevel != nil && (*ps.VolumeLevel < 0 || *ps.VolumeLevel > playbackVolumeLevelMax) {
		return fmt.Errorf("current playback integrity: volume_level must be within [0,%d]", playbackVolumeLevelMax)
	}
	if ps.AudioStreamIndex != nil && *ps.AudioStreamIndex < -1 {
		return fmt.Errorf("current playback integrity: audio_stream_index must be at least -1")
	}
	if ps.SubtitleStreamIndex != nil && *ps.SubtitleStreamIndex < -1 {
		return fmt.Errorf("current playback integrity: subtitle_stream_index must be at least -1")
	}
	if ps.PlaybackRate != nil && (math.IsNaN(*ps.PlaybackRate) || math.IsInf(*ps.PlaybackRate, 0) || *ps.PlaybackRate < 0) {
		return fmt.Errorf("current playback integrity: playback_rate must be finite and nonnegative")
	}
	if ps.SubtitleOffset != nil && (math.IsNaN(*ps.SubtitleOffset) || math.IsInf(*ps.SubtitleOffset, 0)) {
		return fmt.Errorf("current playback integrity: subtitle_offset must be finite")
	}
	if ps.MediaSourceID != nil {
		if err := validateCurrentPlaybackOptionalIdentity("play_state.media_source_id", *ps.MediaSourceID, currentPlaybackMediaSourceIDMaxBytes); err != nil {
			return err
		}
	}
	if ps.PlayMethod != nil {
		if err := validateCurrentPlaybackOptionalText("play_method", *ps.PlayMethod, playbackPlayMethodMaxBytes); err != nil {
			return err
		}
	}
	if ps.RepeatMode != nil {
		if err := validateCurrentPlaybackOptionalText("repeat_mode", *ps.RepeatMode, playbackRepeatModeMaxBytes); err != nil {
			return err
		}
	}
	return nil
}

func validateCurrentPlaybackJSONSize(field string, value any, minBytes, maxBytes int) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("current playback integrity: marshal %s: %w", field, err)
	}
	if len(raw) < minBytes || len(raw) > maxBytes {
		return fmt.Errorf("current playback integrity: %s length %d out of bounds [%d,%d]", field, len(raw), minBytes, maxBytes)
	}
	return nil
}

// PlaybackItemSnapshot is the bounded canonical item metadata allowlist for
// current playback storage and NowPlayingItem projection. It never carries
// raw client/upstream JSON, UserData, media sources, URLs, or tokens.
type PlaybackItemSnapshot struct {
	ID                string            `json:"Id,omitempty"`
	Name              string            `json:"Name,omitempty"`
	Type              string            `json:"Type,omitempty"`
	MediaType         string            `json:"MediaType,omitempty"`
	SeriesID          string            `json:"SeriesId,omitempty"`
	SeriesName        string            `json:"SeriesName,omitempty"`
	SeasonID          string            `json:"SeasonId,omitempty"`
	ParentID          string            `json:"ParentId,omitempty"`
	IndexNumber       int               `json:"IndexNumber,omitempty"`
	ParentIndexNumber int               `json:"ParentIndexNumber,omitempty"`
	RunTimeTicks      int64             `json:"RunTimeTicks,omitempty"`
	ProductionYear    int               `json:"ProductionYear,omitempty"`
	PremiereDate      string            `json:"PremiereDate,omitempty"`
	CommunityRating   float64           `json:"CommunityRating,omitempty"`
	OfficialRating    string            `json:"OfficialRating,omitempty"`
	ImageTags         map[string]string `json:"ImageTags,omitempty"`
}

// PlaybackPlayState is the typed PlayState projection/patch. Pointer fields
// distinguish omission from an explicit zero value when merging reports.
type PlaybackPlayState struct {
	PositionTicks       *int64   `json:"PositionTicks,omitempty"`
	CanSeek             *bool    `json:"CanSeek,omitempty"`
	IsPaused            *bool    `json:"IsPaused,omitempty"`
	IsMuted             *bool    `json:"IsMuted,omitempty"`
	VolumeLevel         *int     `json:"VolumeLevel,omitempty"`
	AudioStreamIndex    *int     `json:"AudioStreamIndex,omitempty"`
	SubtitleStreamIndex *int     `json:"SubtitleStreamIndex,omitempty"`
	MediaSourceID       *string  `json:"MediaSourceId,omitempty"`
	PlayMethod          *string  `json:"PlayMethod,omitempty"`
	PlaybackRate        *float64 `json:"PlaybackRate,omitempty"`
	RepeatMode          *string  `json:"RepeatMode,omitempty"`
	Shuffle             *bool    `json:"Shuffle,omitempty"`
	SubtitleOffset      *float64 `json:"SubtitleOffset,omitempty"`
}

// PlaybackReportKind identifies the authenticated local playback report route.
type PlaybackReportKind string

const (
	PlaybackReportPlaying  PlaybackReportKind = "Playing"
	PlaybackReportProgress PlaybackReportKind = "Progress"
	PlaybackReportStopped  PlaybackReportKind = "Stopped"
	PlaybackReportPing     PlaybackReportKind = "Ping"
)
