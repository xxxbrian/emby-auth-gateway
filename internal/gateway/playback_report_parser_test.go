package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPlaybackReportCommandFromDetailsAllFields(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	played := false
	pct := 0.0
	canSeek := false
	isPaused := true
	isMuted := false
	playMethod := "DirectPlay"
	audioIdx := 0
	subIdx := -1
	volume := 0
	rate := 0.0
	repeat := "RepeatNone"
	shuffle := false
	subOffset := 0.0

	details := playbackDetails{
		ItemID:               "item-1",
		ItemSnapshotID:       "item-1",
		PositionTicks:        0,
		HasPositionTicks:     true,
		Played:               &played,
		PlayedPercentage:     &pct,
		ItemName:             "Episode 1",
		ItemType:             "Episode",
		MediaType:            "Video",
		SeriesID:             "series-1",
		SeriesName:           "Show",
		SeasonID:             "season-1",
		ParentID:             "parent-1",
		IndexNumber:          1,
		ParentIndexNumber:    2,
		HasIndexNumber:       true,
		HasParentIndexNumber: true,
		RunTimeTicks:         9_000_000,
		HasRunTimeTicks:      true,
		ProductionYear:       2024,
		HasProductionYear:    true,
		PremiereDate:         "2024-01-01",
		CommunityRating:      8.5,
		HasCommunityRating:   true,
		OfficialRating:       "TV-14",
		ImageTags:            map[string]string{"Primary": "tag-1"},
		PlaySessionID:        "ps-1",
		MediaSourceID:        "ms-1",
		EventName:            "Pause",
		SessionID:            "client-session-must-not-bind",
		CanSeek:              &canSeek,
		IsPaused:             &isPaused,
		IsMuted:              &isMuted,
		PlayMethod:           &playMethod,
		AudioStreamIndex:     &audioIdx,
		SubtitleStreamIndex:  &subIdx,
		VolumeLevel:          &volume,
		PlaybackRate:         &rate,
		RepeatMode:           &repeat,
		Shuffle:              &shuffle,
		SubtitleOffset:       &subOffset,
	}

	cmd := playbackReportCommandFromDetails(
		PlaybackReportProgress,
		"authoritative-hash",
		now,
		"203.0.113.9",
		details,
	)

	if cmd.GatewayTokenHash != "authoritative-hash" {
		t.Fatalf("GatewayTokenHash = %q, want authoritative hash", cmd.GatewayTokenHash)
	}
	if cmd.GatewayTokenHash == details.SessionID {
		t.Fatalf("body SessionId must never become GatewayTokenHash")
	}
	if cmd.Kind != PlaybackReportProgress || cmd.ReceivedAt != now || cmd.RemoteIP != "203.0.113.9" {
		t.Fatalf("command context mismatch: %#v", cmd)
	}
	if cmd.ItemID != "item-1" || cmd.PlaySessionID != "ps-1" || cmd.MediaSourceID != "ms-1" {
		t.Fatalf("ids: %#v", cmd)
	}
	if cmd.EventName != "Pause" || cmd.RunTimeTicks != 9_000_000 {
		t.Fatalf("event/runtime: %#v", cmd)
	}
	if cmd.Played == nil || *cmd.Played || cmd.PlayedPercentage == nil || *cmd.PlayedPercentage != 0 {
		t.Fatalf("played pointers: Played=%v Pct=%v", cmd.Played, cmd.PlayedPercentage)
	}

	snap := cmd.ItemSnapshot
	if snap.ID != "item-1" || snap.Name != "Episode 1" || snap.Type != "Episode" || snap.MediaType != "Video" {
		t.Fatalf("snapshot identity: %#v", snap)
	}
	if snap.SeriesID != "series-1" || snap.SeriesName != "Show" || snap.SeasonID != "season-1" || snap.ParentID != "parent-1" {
		t.Fatalf("snapshot hierarchy: %#v", snap)
	}
	if snap.IndexNumber != 1 || snap.ParentIndexNumber != 2 || snap.RunTimeTicks != 9_000_000 {
		t.Fatalf("snapshot numbers: %#v", snap)
	}
	if snap.ProductionYear != 2024 || snap.PremiereDate != "2024-01-01" || snap.CommunityRating != 8.5 || snap.OfficialRating != "TV-14" {
		t.Fatalf("snapshot meta: %#v", snap)
	}
	if snap.ImageTags["Primary"] != "tag-1" {
		t.Fatalf("image tags: %#v", snap.ImageTags)
	}

	ps := cmd.PlayState
	if ps.PositionTicks == nil || *ps.PositionTicks != 0 {
		t.Fatalf("PositionTicks pointer missing/zero: %#v", ps.PositionTicks)
	}
	if ps.CanSeek == nil || *ps.CanSeek || ps.IsPaused == nil || !*ps.IsPaused || ps.IsMuted == nil || *ps.IsMuted {
		t.Fatalf("bool playstate: %#v", ps)
	}
	if ps.PlayMethod == nil || *ps.PlayMethod != "DirectPlay" {
		t.Fatalf("PlayMethod: %#v", ps.PlayMethod)
	}
	if ps.AudioStreamIndex == nil || *ps.AudioStreamIndex != 0 {
		t.Fatalf("AudioStreamIndex explicit zero: %#v", ps.AudioStreamIndex)
	}
	if ps.SubtitleStreamIndex == nil || *ps.SubtitleStreamIndex != -1 {
		t.Fatalf("SubtitleStreamIndex: %#v", ps.SubtitleStreamIndex)
	}
	if ps.VolumeLevel == nil || *ps.VolumeLevel != 0 {
		t.Fatalf("VolumeLevel explicit zero: %#v", ps.VolumeLevel)
	}
	if ps.PlaybackRate == nil || *ps.PlaybackRate != 0 {
		t.Fatalf("PlaybackRate explicit zero: %#v", ps.PlaybackRate)
	}
	if ps.RepeatMode == nil || *ps.RepeatMode != "RepeatNone" {
		t.Fatalf("RepeatMode: %#v", ps.RepeatMode)
	}
	if ps.Shuffle == nil || *ps.Shuffle {
		t.Fatalf("Shuffle explicit false: %#v", ps.Shuffle)
	}
	if ps.SubtitleOffset == nil || *ps.SubtitleOffset != 0 {
		t.Fatalf("SubtitleOffset explicit zero: %#v", ps.SubtitleOffset)
	}
	if ps.MediaSourceID == nil || *ps.MediaSourceID != "ms-1" {
		t.Fatalf("PlayState.MediaSourceID: %#v", ps.MediaSourceID)
	}
}

func TestPlaybackReportCommandMissingItemIsNoOpContract(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	cmd := playbackReportCommandFromDetails(
		PlaybackReportProgress,
		"hash-1",
		now,
		"127.0.0.1",
		playbackDetails{PlaySessionID: "ps-only", EventName: "Pause"},
	)
	if cmd.ItemID != "" {
		t.Fatalf("want empty ItemID, got %q", cmd.ItemID)
	}
	if cmd.GatewayTokenHash != "hash-1" || cmd.Kind != PlaybackReportProgress || cmd.ReceivedAt != now {
		t.Fatalf("command identity: %#v", cmd)
	}
	if cmd.PlaySessionID != "ps-only" || cmd.EventName != "Pause" {
		t.Fatalf("parsed non-item fields lost: %#v", cmd)
	}
	// Valid no-op contract for the pure reducer.
	plan, err := ReducePlaybackReport(PlaybackReduceInput{
		Command: cmd,
		Session: Session{GatewayTokenHash: "hash-1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "s1"},
	})
	if err != nil {
		t.Fatalf("reduce missing-item command: %v", err)
	}
	if plan.Result.Applied || plan.WriteDurable || plan.Event != nil {
		t.Fatalf("want no-op plan, got %#v", plan)
	}
}

func TestPlaybackDetailsJSONNestedItemAndNumericStrings(t *testing.T) {
	t.Parallel()
	details, ok := playbackDetailsFromJSON(map[string]any{
		"ItemId":                "top-item",
		"PlaybackPositionTicks": "12345",
		"RunTimeTicks":          "100000",
		"PlayedPercentage":      "12.5",
		"Played":                false,
		"PlaySessionId":         "ps-json",
		"MediaSourceId":         "ms-json",
		"EventName":             "Unpause",
		"SessionId":             "body-session",
		"CanSeek":               false,
		"IsPaused":              false,
		"IsMuted":               true,
		"PlayMethod":            "Transcode",
		"AudioStreamIndex":      "0",
		"SubtitleStreamIndex":   "-1",
		"VolumeLevel":           "0",
		"PlaybackRate":          "1.25",
		"RepeatMode":            "RepeatAll",
		"Shuffle":               true,
		"SubtitleOffset":        "0",
		"Item": map[string]any{
			"Id":                "nested-should-lose",
			"Name":              "Nested Name",
			"Type":              "Movie",
			"MediaType":         "Video",
			"SeriesId":          "ser-1",
			"SeriesName":        "Ser",
			"SeasonId":          "sea-1",
			"ParentId":          "par-1",
			"IndexNumber":       jsonNumber(3),
			"ParentIndexNumber": jsonNumber(1),
			"RunTimeTicks":      "999",
			"ProductionYear":    "2020",
			"PremiereDate":      "2020-05-01",
			"CommunityRating":   "7.25",
			"OfficialRating":    "PG",
			"ImageTags":         map[string]any{"Primary": "img-1", "Thumb": "img-2"},
		},
	})
	if !ok {
		t.Fatalf("expected ok with ItemId")
	}
	if details.ItemID != "top-item" || details.ItemSnapshotID != "nested-should-lose" {
		t.Fatalf("independent item identities: top=%q nested=%q", details.ItemID, details.ItemSnapshotID)
	}
	if !details.HasPositionTicks || details.PositionTicks != 12345 {
		t.Fatalf("position numeric string: %#v", details)
	}
	// Nested RunTimeTicks wins when present before top-level fill; top-level only fills when missing.
	if !details.HasRunTimeTicks || details.RunTimeTicks != 999 {
		t.Fatalf("nested runtime preferred: %#v", details)
	}
	if details.Played == nil || *details.Played || details.PlayedPercentage == nil || *details.PlayedPercentage != 12.5 {
		t.Fatalf("played fields: %#v", details)
	}
	if details.PlaySessionID != "ps-json" || details.MediaSourceID != "ms-json" || details.EventName != "Unpause" {
		t.Fatalf("session fields: %#v", details)
	}
	if details.SessionID != "body-session" {
		t.Fatalf("SessionId parse: %q", details.SessionID)
	}
	if details.CanSeek == nil || *details.CanSeek || details.IsPaused == nil || *details.IsPaused {
		t.Fatalf("explicit false bools: %#v", details)
	}
	if details.IsMuted == nil || !*details.IsMuted || details.Shuffle == nil || !*details.Shuffle {
		t.Fatalf("explicit true bools: %#v", details)
	}
	if details.PlayMethod == nil || *details.PlayMethod != "Transcode" {
		t.Fatalf("PlayMethod: %#v", details.PlayMethod)
	}
	if details.AudioStreamIndex == nil || *details.AudioStreamIndex != 0 {
		t.Fatalf("AudioStreamIndex string zero: %#v", details.AudioStreamIndex)
	}
	if details.SubtitleStreamIndex == nil || *details.SubtitleStreamIndex != -1 {
		t.Fatalf("SubtitleStreamIndex: %#v", details.SubtitleStreamIndex)
	}
	if details.VolumeLevel == nil || *details.VolumeLevel != 0 {
		t.Fatalf("VolumeLevel: %#v", details.VolumeLevel)
	}
	if details.PlaybackRate == nil || *details.PlaybackRate != 1.25 {
		t.Fatalf("PlaybackRate: %#v", details.PlaybackRate)
	}
	if details.SubtitleOffset == nil || *details.SubtitleOffset != 0 {
		t.Fatalf("SubtitleOffset: %#v", details.SubtitleOffset)
	}
	if details.ItemName != "Nested Name" || details.ItemType != "Movie" || details.MediaType != "Video" {
		t.Fatalf("nested snapshot strings: %#v", details)
	}
	if details.SeriesID != "ser-1" || details.SeasonID != "sea-1" || details.ParentID != "par-1" {
		t.Fatalf("nested ids: %#v", details)
	}
	if !details.HasIndexNumber || details.IndexNumber != 3 || !details.HasParentIndexNumber || details.ParentIndexNumber != 1 {
		t.Fatalf("nested indexes: %#v", details)
	}
	if !details.HasProductionYear || details.ProductionYear != 2020 || details.PremiereDate != "2020-05-01" {
		t.Fatalf("nested production: %#v", details)
	}
	if !details.HasCommunityRating || details.CommunityRating != 7.25 || details.OfficialRating != "PG" {
		t.Fatalf("nested ratings: %#v", details)
	}
	if details.ImageTags["Primary"] != "img-1" || details.ImageTags["Thumb"] != "img-2" {
		t.Fatalf("image tags: %#v", details.ImageTags)
	}

	// Nested Item.Id alone.
	nestedOnly, ok := playbackDetailsFromJSON(map[string]any{
		"Item": map[string]any{"Id": "from-nested"},
	})
	if !ok || nestedOnly.ItemID != "" || nestedOnly.ItemSnapshotID != "from-nested" {
		t.Fatalf("nested Item.Id: ok=%v details=%#v", ok, nestedOnly)
	}
	cmd := playbackReportCommandFromDetails(PlaybackReportPlaying, "hash", time.Now().UTC(), "", nestedOnly)
	if cmd.ItemID != "" || cmd.ItemSnapshot.ID != "from-nested" {
		t.Fatalf("command must preserve nested-only identity before prepare: %#v", cmd)
	}
	prepared, err := PreparePlaybackReportCommand(cmd)
	if err != nil || prepared.ItemID != "from-nested" || prepared.ItemSnapshot.ID != "from-nested" {
		t.Fatalf("prepare nested-only command: %#v err=%v", prepared, err)
	}
}

func TestPlaybackDetailsJSONFormQueryPrecedence(t *testing.T) {
	t.Parallel()

	t.Run("json body wins over query", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?ItemId=query-item&PositionTicks=1&PlaySessionId=q-ps&CanSeek=true", nil)
		req.Header.Set("Content-Type", "application/json")
		body := []byte(`{"ItemId":"body-item","PositionTicks":99,"PlaySessionId":"b-ps","CanSeek":false,"IsPaused":true}`)
		details, ok := playbackDetailsFromRequest(req, body)
		if !ok || details.ItemID != "body-item" {
			t.Fatalf("item precedence: ok=%v %#v", ok, details)
		}
		if !details.HasPositionTicks || details.PositionTicks != 99 {
			t.Fatalf("position body wins: %#v", details)
		}
		if details.PlaySessionID != "b-ps" {
			t.Fatalf("PlaySessionId body wins: %q", details.PlaySessionID)
		}
		if details.CanSeek == nil || *details.CanSeek {
			t.Fatalf("CanSeek body false wins: %#v", details.CanSeek)
		}
		if details.IsPaused == nil || !*details.IsPaused {
			t.Fatalf("IsPaused from body: %#v", details.IsPaused)
		}
	})

	t.Run("query fills gaps left by json body", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?ItemId=query-item&PlaybackPositionTicks=700&MediaSourceId=q-ms&IsMuted=true", nil)
		req.Header.Set("Content-Type", "application/json")
		body := []byte(`{"PlaySessionId":"body-ps","CanSeek":true}`)
		details, ok := playbackDetailsFromRequest(req, body)
		if !ok || details.ItemID != "query-item" {
			t.Fatalf("query item fill: ok=%v %#v", ok, details)
		}
		if !details.HasPositionTicks || details.PositionTicks != 700 {
			t.Fatalf("query position fill: %#v", details)
		}
		if details.PlaySessionID != "body-ps" || details.MediaSourceID != "q-ms" {
			t.Fatalf("gap fill: %#v", details)
		}
		if details.CanSeek == nil || !*details.CanSeek || details.IsMuted == nil || !*details.IsMuted {
			t.Fatalf("bool gap fill: %#v", details)
		}
	})

	t.Run("form body wins over query", func(t *testing.T) {
		t.Parallel()
		form := url.Values{}
		form.Set("ItemId", "form-item")
		form.Set("PositionTicks", "300")
		form.Set("CanSeek", "false")
		form.Set("VolumeLevel", "0")
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?ItemId=query-item&PositionTicks=1&CanSeek=true&VolumeLevel=50", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		details, ok := playbackDetailsFromRequest(req, []byte(form.Encode()))
		if !ok || details.ItemID != "form-item" {
			t.Fatalf("form item: ok=%v %#v", ok, details)
		}
		if !details.HasPositionTicks || details.PositionTicks != 300 {
			t.Fatalf("form position: %#v", details)
		}
		if details.CanSeek == nil || *details.CanSeek {
			t.Fatalf("form CanSeek false: %#v", details.CanSeek)
		}
		if details.VolumeLevel == nil || *details.VolumeLevel != 0 {
			t.Fatalf("form VolumeLevel zero: %#v", details.VolumeLevel)
		}
	})

	t.Run("query only", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?ItemId=query-only&PlaybackPositionTicks=42&Played=false&Shuffle=false", nil)
		details, ok := playbackDetailsFromRequest(req, nil)
		if !ok || details.ItemID != "query-only" || !details.HasPositionTicks || details.PositionTicks != 42 {
			t.Fatalf("query only: ok=%v %#v", ok, details)
		}
		if details.Played == nil || *details.Played || details.Shuffle == nil || *details.Shuffle {
			t.Fatalf("query explicit false: %#v", details)
		}
	})
}

func TestPlaybackDetailsFormQueryIdentityChannels(t *testing.T) {
	t.Parallel()

	t.Run("query same-source conflict preserved", func(t *testing.T) {
		values := url.Values{
			"ItemId":    {"item-a"},
			"Item.Id":   {"item-b"},
			"Item.Name": {"Item B"},
		}
		details, ok := playbackDetailsFromValues(values)
		if !ok || details.ItemID != "item-a" || details.ItemSnapshotID != "item-b" || details.ItemName != "Item B" {
			t.Fatalf("query identities/metadata = ok=%v %#v", ok, details)
		}
	})

	t.Run("form same-source conflict preserved", func(t *testing.T) {
		form := url.Values{
			"ItemId":    {"item-a"},
			"Item.Id":   {"item-b"},
			"Item.Type": {"Movie"},
		}
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		details, ok := playbackDetailsFromRequest(req, []byte(form.Encode()))
		if !ok || details.ItemID != "item-a" || details.ItemSnapshotID != "item-b" || details.ItemType != "Movie" {
			t.Fatalf("form identities/metadata = ok=%v %#v", ok, details)
		}
	})

	t.Run("json top and query nested conflict preserved", func(t *testing.T) {
		query := url.Values{"Item.Id": {"item-b"}, "Item.Name": {"Item B"}}
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing?"+query.Encode(), nil)
		req.Header.Set("Content-Type", "application/json")
		details, ok := playbackDetailsFromRequest(req, []byte(`{"ItemId":"item-a"}`))
		if !ok || details.ItemID != "item-a" || details.ItemSnapshotID != "item-b" || details.ItemName != "Item B" {
			t.Fatalf("mixed identities/metadata = ok=%v %#v", ok, details)
		}
	})

	t.Run("json nested and query top conflict preserved", func(t *testing.T) {
		query := url.Values{"ItemId": {"item-b"}}
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing?"+query.Encode(), nil)
		req.Header.Set("Content-Type", "application/json")
		details, ok := playbackDetailsFromRequest(req, []byte(`{"Item":{"Id":"item-a","Name":"Item A"}}`))
		if !ok || details.ItemID != "item-b" || details.ItemSnapshotID != "item-a" || details.ItemName != "Item A" {
			t.Fatalf("mixed identities/metadata = ok=%v %#v", ok, details)
		}
	})

	t.Run("identical cross-source identity accepts lower metadata", func(t *testing.T) {
		query := url.Values{"Item.Id": {"item-a"}, "Item.Name": {"Item A"}, "Item.Type": {"Movie"}}
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing?"+query.Encode(), nil)
		req.Header.Set("Content-Type", "application/json")
		details, ok := playbackDetailsFromRequest(req, []byte(`{"ItemId":"item-a"}`))
		if !ok || details.ItemID != "item-a" || details.ItemSnapshotID != "item-a" || details.ItemName != "Item A" || details.ItemType != "Movie" {
			t.Fatalf("identical mixed identity = ok=%v %#v", ok, details)
		}
		cmd := playbackReportCommandFromDetails(PlaybackReportPlaying, "hash", time.Now().UTC(), "", details)
		if _, err := PreparePlaybackReportCommand(cmd); err != nil {
			t.Fatalf("identical mixed identity prepare: %v", err)
		}
	})

	t.Run("different lower nested identity cannot donate metadata", func(t *testing.T) {
		query := url.Values{"Item.Id": {"item-b"}, "Item.Name": {"Wrong Item"}, "Item.Type": {"Episode"}}
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing?"+query.Encode(), nil)
		req.Header.Set("Content-Type", "application/json")
		details, ok := playbackDetailsFromRequest(req, []byte(`{"Item":{"Id":"item-a"}}`))
		if !ok || details.ItemSnapshotID != "item-a" {
			t.Fatalf("higher nested identity lost: ok=%v %#v", ok, details)
		}
		if details.ItemName != "" || details.ItemType != "" {
			t.Fatalf("lower item-b metadata attached to item-a: %#v", details)
		}
	})

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		query       url.Values
	}{
		{name: "nested-only form", contentType: "application/x-www-form-urlencoded", body: url.Values{"Item.Id": {"form-nested"}, "Item.Name": {"Form Name"}}.Encode()},
		{name: "nested-only query", query: url.Values{"Item.Id": {"query-nested"}, "Item.Name": {"Query Name"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := "/emby/Sessions/Playing"
			if len(tc.query) > 0 {
				target += "?" + tc.query.Encode()
			}
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			details, ok := playbackDetailsFromRequest(req, []byte(tc.body))
			if !ok || details.ItemID != "" || details.ItemSnapshotID == "" || details.ItemName == "" {
				t.Fatalf("nested-only parse = ok=%v %#v", ok, details)
			}
			cmd := playbackReportCommandFromDetails(PlaybackReportPlaying, "hash", time.Now().UTC(), "", details)
			prepared, err := PreparePlaybackReportCommand(cmd)
			if err != nil || prepared.ItemID != details.ItemSnapshotID || prepared.ItemSnapshot.Name != details.ItemName {
				t.Fatalf("nested-only prepare = %#v err=%v", prepared, err)
			}
		})
	}
}

func TestPlaybackDetailsRejectsCaseVariantIdentityAmbiguity(t *testing.T) {
	t.Parallel()

	jsonCases := []struct {
		name string
		body map[string]any
	}{
		{name: "top item id", body: map[string]any{"ItemId": "item-1", "itemid": "item-1"}},
		{name: "nested item id", body: map[string]any{"Item": map[string]any{"Id": "item-1", "id": "item-1"}}},
		{name: "item object", body: map[string]any{"Item": map[string]any{"Id": "item-1"}, "item": map[string]any{"Id": "item-1"}}},
	}
	for _, tc := range jsonCases {
		t.Run("json "+tc.name, func(t *testing.T) {
			_, _, err := playbackDetailsFromJSONChecked(tc.body)
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("err = %v, want ErrBadRequest", err)
			}
		})
	}

	valueCases := []struct {
		name   string
		values url.Values
	}{
		{name: "top item id", values: url.Values{"ItemId": {"item-1"}, "itemid": {"item-1"}}},
		{name: "nested item id", values: url.Values{"Item.Id": {"item-1"}, "item.id": {"item-1"}}},
	}
	for _, tc := range valueCases {
		t.Run("values "+tc.name, func(t *testing.T) {
			_, _, err := playbackDetailsFromValuesChecked(tc.values)
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("err = %v, want ErrBadRequest", err)
			}
		})
	}

	t.Run("same identity key repeated rejects", func(t *testing.T) {
		for _, values := range []url.Values{
			{"ItemId": {"first", "second"}},
			{"ItemId": {"same", "same"}},
			{"Item.Id": {"first", "second"}},
			{"Item.Id": {"same", "same"}},
		} {
			_, _, err := playbackDetailsFromValuesChecked(values)
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("values=%v err=%v, want ErrBadRequest", values, err)
			}
		}
	})

	t.Run("raw exact duplicate json keys reject", func(t *testing.T) {
		for _, body := range []string{
			`{"ItemId":"item-1","ItemId":"item-1"}`,
			`{"Item":{"Id":"item-1"},"Item":{"Id":"item-1"}}`,
			`{"Item":{"Id":"item-1","Id":"item-1"}}`,
		} {
			req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			_, _, err := playbackDetailsFromRequestChecked(req, []byte(body))
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("body=%s err=%v, want ErrBadRequest", body, err)
			}
		}
	})
}

func TestPlaybackDetailsBodySessionIdNeverBindsGatewayTokenHash(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{
		"ItemId":"item-1",
		"SessionId":"forged-session",
		"PlaySessionId":"ps-1",
		"PositionTicks":10
	}`))
	req.Header.Set("Content-Type", "application/json")
	data, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	details, ok := playbackDetailsFromRequest(req, data)
	if !ok {
		t.Fatalf("details not ok: %#v", details)
	}
	if details.SessionID != "forged-session" {
		t.Fatalf("SessionId should be parsed for awareness: %q", details.SessionID)
	}
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	cmd := playbackReportCommandFromDetails(PlaybackReportProgress, "real-token-hash", now, "10.0.0.1", details)
	if cmd.GatewayTokenHash != "real-token-hash" {
		t.Fatalf("GatewayTokenHash = %q", cmd.GatewayTokenHash)
	}
	if cmd.GatewayTokenHash == details.SessionID || cmd.GatewayTokenHash == "forged-session" {
		t.Fatalf("body SessionId must never bind GatewayTokenHash")
	}
	if cmd.PlaySessionID != "ps-1" {
		t.Fatalf("PlaySessionId lost: %q", cmd.PlaySessionID)
	}
}

func TestPlaybackReportBodyLimitBoundary(t *testing.T) {
	t.Parallel()

	t.Run("exactly 1 MiB accepted", func(t *testing.T) {
		t.Parallel()
		payload := bytes.Repeat([]byte("a"), playbackReportBodyLimit)
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress", bytes.NewReader(payload))
		data, err := readPlaybackReportBody(req)
		if err != nil {
			t.Fatalf("exact limit should succeed: %v", err)
		}
		if len(data) != playbackReportBodyLimit {
			t.Fatalf("len=%d want %d", len(data), playbackReportBodyLimit)
		}
	})

	t.Run("oversize rejected", func(t *testing.T) {
		t.Parallel()
		payload := bytes.Repeat([]byte("b"), playbackReportBodyLimit+1)
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress", bytes.NewReader(payload))
		_, err := readPlaybackReportBody(req)
		if err == nil {
			t.Fatal("expected oversize error")
		}
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("err = %v, want ErrBadRequest wrapper", err)
		}
		var maxErr *http.MaxBytesError
		if !errors.As(err, &maxErr) {
			t.Fatalf("want MaxBytesError cause, got %v", err)
		}
	})

	t.Run("named limit is 1 MiB", func(t *testing.T) {
		t.Parallel()
		if playbackReportBodyLimit != 1<<20 {
			t.Fatalf("playbackReportBodyLimit = %d, want 1 MiB", playbackReportBodyLimit)
		}
	})
}

func TestPlaybackDetailsMalformedBodyFallsBackCompatibly(t *testing.T) {
	t.Parallel()

	t.Run("malformed json falls back to query", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?ItemId=query-item&PositionTicks=55", nil)
		req.Header.Set("Content-Type", "application/json")
		details, ok := playbackDetailsFromRequest(req, []byte(`{not-json`))
		if !ok || details.ItemID != "query-item" || !details.HasPositionTicks || details.PositionTicks != 55 {
			t.Fatalf("malformed json fallback: ok=%v %#v", ok, details)
		}
	})

	t.Run("non-object json falls back to query", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?ItemId=from-query", nil)
		req.Header.Set("Content-Type", "application/json")
		details, ok := playbackDetailsFromRequest(req, []byte(`["array"]`))
		if !ok || details.ItemID != "from-query" {
			t.Fatalf("non-object fallback: ok=%v %#v", ok, details)
		}
	})

	t.Run("empty body uses query", func(t *testing.T) {
		t.Parallel()
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?ItemId=empty-body&PlaybackPositionTicks=7", nil)
		details, ok := playbackDetailsFromRequest(req, nil)
		if !ok || details.ItemID != "empty-body" || details.PositionTicks != 7 {
			t.Fatalf("empty body: ok=%v %#v", ok, details)
		}
	})

	t.Run("malformed form falls back to query", func(t *testing.T) {
		t.Parallel()
		// url.ParseQuery is lenient; force form content-type with body that is not used when ParseQuery fails.
		// ParseQuery rarely fails; empty form values still merge. Use invalid percent-encoding that ParseQuery rejects.
		req := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing/Progress?ItemId=query-after-bad-form&PositionTicks=3", nil)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		details, ok := playbackDetailsFromRequest(req, []byte("%zzzz"))
		if !ok || details.ItemID != "query-after-bad-form" || details.PositionTicks != 3 {
			t.Fatalf("malformed form fallback: ok=%v %#v", ok, details)
		}
	})
}

func TestPlaybackReportKindFromRel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rel  string
		kind PlaybackReportKind
		ok   bool
	}{
		{"/Sessions/Playing", PlaybackReportPlaying, true},
		{"/Sessions/Playing/Progress", PlaybackReportProgress, true},
		{"/Sessions/Playing/Stopped", PlaybackReportStopped, true},
		{"/Sessions/Playing/Ping", PlaybackReportPing, true},
		{"/Sessions/Playing/Progress/", PlaybackReportProgress, true},
		{"/Users/x/Items", "", false},
	}
	for _, tc := range cases {
		kind, ok := playbackReportKindFromRel(tc.rel)
		if ok != tc.ok || kind != tc.kind {
			t.Fatalf("rel=%q => kind=%q ok=%v, want %q %v", tc.rel, kind, ok, tc.kind, tc.ok)
		}
	}
}

func TestPlaybackDetailsOmittedPointersStayNil(t *testing.T) {
	t.Parallel()
	details, ok := playbackDetailsFromJSON(map[string]any{
		"ItemId":        "item-1",
		"PositionTicks": float64(100),
	})
	if !ok {
		t.Fatal("expected ok")
	}
	if details.Played != nil || details.PlayedPercentage != nil {
		t.Fatalf("omitted played fields should be nil: %#v", details)
	}
	if details.CanSeek != nil || details.IsPaused != nil || details.IsMuted != nil {
		t.Fatalf("omitted bools should be nil: %#v", details)
	}
	if details.PlayMethod != nil || details.AudioStreamIndex != nil || details.VolumeLevel != nil {
		t.Fatalf("omitted optionals should be nil: %#v", details)
	}
	if details.PlaybackRate != nil || details.RepeatMode != nil || details.Shuffle != nil || details.SubtitleOffset != nil {
		t.Fatalf("omitted optionals should be nil: %#v", details)
	}
	cmd := playbackReportCommandFromDetails(PlaybackReportPlaying, "h", time.Now().UTC(), "", details)
	if cmd.PlayState.CanSeek != nil || cmd.PlayState.IsPaused != nil || cmd.Played != nil {
		t.Fatalf("command omitted pointers: %#v", cmd.PlayState)
	}
	if cmd.PlayState.PositionTicks == nil || *cmd.PlayState.PositionTicks != 100 {
		t.Fatalf("present position must be pointer: %#v", cmd.PlayState.PositionTicks)
	}
}

// jsonNumber helps build json.Number values when constructing maps without a decoder.
func jsonNumber(v int64) interface{} {
	// Use float64 as map literal path (UseNumber only applies to decoder).
	// int64Field accepts float64 as well.
	return float64(v)
}
