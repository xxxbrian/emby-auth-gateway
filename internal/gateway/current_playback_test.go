package gateway

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestValidateCurrentPlayback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	valid := func() CurrentPlayback {
		return CurrentPlayback{
			GatewayTokenHash: "hash-1",
			ItemID:           "item-1",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-1", Name: "Movie"},
			RunTimeTicks:     0,
			StartedAt:        now,
			LastReportedAt:   now.Add(time.Minute),
		}
	}

	t.Run("valid", func(t *testing.T) {
		cp := valid()
		zero := 0
		falseValue := false
		streamNone := -1
		rating := 8.5
		cp.ItemSnapshot.CommunityRating = rating
		cp.PlayState = PlaybackPlayState{
			PositionTicks:       int64Ptr(0),
			CanSeek:             &falseValue,
			VolumeLevel:         &zero,
			AudioStreamIndex:    &streamNone,
			SubtitleStreamIndex: &streamNone,
			PlaybackRate:        float64Ptr(0),
			SubtitleOffset:      float64Ptr(0),
		}
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err != nil {
			t.Fatalf("valid: %v", err)
		}
	})
	t.Run("nil row", func(t *testing.T) {
		if err := ValidateCurrentPlayback(nil, "hash-1"); err == nil || !strings.Contains(err.Error(), "nil") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("token map mismatch", func(t *testing.T) {
		cp := valid()
		cp.GatewayTokenHash = "other"
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty item id", func(t *testing.T) {
		cp := valid()
		cp.ItemID = ""
		cp.ItemSnapshot.ID = ""
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "item_id") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("item id too long", func(t *testing.T) {
		cp := valid()
		cp.ItemID = strings.Repeat("x", currentPlaybackItemIDMaxBytes+1)
		cp.ItemSnapshot.ID = cp.ItemID
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "exceeds max") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("snapshot id mismatch", func(t *testing.T) {
		cp := valid()
		cp.ItemSnapshot.ID = "other"
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "snapshot Id") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unsafe item ids", func(t *testing.T) {
		for _, itemID := range []string{".", "..", "a/b", `a\b`, "a\x00b"} {
			cp := valid()
			cp.ItemID = itemID
			cp.ItemSnapshot.ID = itemID
			if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil {
				t.Fatalf("item id %q accepted", itemID)
			}
		}
		cp := valid()
		cp.ItemID = "percent%2Ftext"
		cp.ItemSnapshot.ID = cp.ItemID
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err != nil {
			t.Fatalf("escaped percent text should remain a safe segment: %v", err)
		}
	})
	t.Run("nested constraints", func(t *testing.T) {
		tests := []struct {
			name   string
			mutate func(*CurrentPlayback)
			want   string
		}{
			{"negative snapshot runtime", func(cp *CurrentPlayback) { cp.ItemSnapshot.RunTimeTicks = -1 }, "negative"},
			{"negative snapshot index", func(cp *CurrentPlayback) { cp.ItemSnapshot.IndexNumber = -1 }, "negative"},
			{"overlong name", func(cp *CurrentPlayback) { cp.ItemSnapshot.Name = strings.Repeat("n", playbackSnapshotNameMaxBytes+1) }, "name"},
			{"overlong series", func(cp *CurrentPlayback) {
				cp.ItemSnapshot.SeriesName = strings.Repeat("s", playbackSnapshotNameMaxBytes+1)
			}, "series_name"},
			{"too many image tags", func(cp *CurrentPlayback) {
				cp.ItemSnapshot.ImageTags = map[string]string{}
				for i := 0; i <= playbackMaxImageTags; i++ {
					cp.ItemSnapshot.ImageTags[fmt.Sprintf("tag-%d", i)] = "v"
				}
			}, "image_tags"},
			{"overlong image tag value", func(cp *CurrentPlayback) {
				cp.ItemSnapshot.ImageTags = map[string]string{"Primary": strings.Repeat("v", playbackImageTagValueMaxBytes+1)}
			}, "image_tags.value"},
			{"rating nonfinite", func(cp *CurrentPlayback) { cp.ItemSnapshot.CommunityRating = math.NaN() }, "community_rating"},
			{"rating out of range", func(cp *CurrentPlayback) { cp.ItemSnapshot.CommunityRating = 11 }, "community_rating"},
			{"negative position", func(cp *CurrentPlayback) { cp.PlayState.PositionTicks = int64Ptr(-1) }, "position_ticks"},
			{"volume out of range", func(cp *CurrentPlayback) { cp.PlayState.VolumeLevel = intPtr(101) }, "volume_level"},
			{"stream below sentinel", func(cp *CurrentPlayback) { cp.PlayState.SubtitleStreamIndex = intPtr(-2) }, "stream_index"},
			{"rate nonfinite", func(cp *CurrentPlayback) { cp.PlayState.PlaybackRate = float64Ptr(math.Inf(1)) }, "playback_rate"},
			{"subtitle offset nonfinite", func(cp *CurrentPlayback) { cp.PlayState.SubtitleOffset = float64Ptr(math.NaN()) }, "subtitle_offset"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				cp := valid()
				tt.mutate(&cp)
				if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("err = %v, want %q", err, tt.want)
				}
			})
		}
	})
	t.Run("serialized size bounds", func(t *testing.T) {
		oversized := map[string]string{"Value": strings.Repeat("x", currentPlaybackItemSnapshotJSONMax)}
		if err := validateCurrentPlaybackJSONSize("item_snapshot_json", oversized, currentPlaybackItemSnapshotJSONMin, currentPlaybackItemSnapshotJSONMax); err == nil || !strings.Contains(err.Error(), "out of bounds") {
			t.Fatalf("snapshot size error = %v", err)
		}
		oversized["Value"] = strings.Repeat("x", currentPlaybackPlayStateJSONMax)
		if err := validateCurrentPlaybackJSONSize("play_state_json", oversized, currentPlaybackPlayStateJSONMin, currentPlaybackPlayStateJSONMax); err == nil || !strings.Contains(err.Error(), "out of bounds") {
			t.Fatalf("play state size error = %v", err)
		}
	})
	t.Run("negative runtime", func(t *testing.T) {
		cp := valid()
		cp.RunTimeTicks = -1
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "negative") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("zero started_at", func(t *testing.T) {
		cp := valid()
		cp.StartedAt = time.Time{}
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "started_at") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("zero last_reported_at", func(t *testing.T) {
		cp := valid()
		cp.LastReportedAt = time.Time{}
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "last_reported_at") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("reversed dates", func(t *testing.T) {
		cp := valid()
		cp.StartedAt = now.Add(time.Hour)
		cp.LastReportedAt = now
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "before started_at") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("play session id too long", func(t *testing.T) {
		cp := valid()
		cp.PlaySessionID = strings.Repeat("p", currentPlaybackPlaySessionIDMaxBytes+1)
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "play_session_id") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("media source id too long", func(t *testing.T) {
		cp := valid()
		cp.MediaSourceID = strings.Repeat("m", currentPlaybackMediaSourceIDMaxBytes+1)
		if err := ValidateCurrentPlayback(&cp, "hash-1"); err == nil || !strings.Contains(err.Error(), "media_source_id") {
			t.Fatalf("err = %v", err)
		}
	})
}

func intPtr(v int) *int             { return &v }
func float64Ptr(v float64) *float64 { return &v }
