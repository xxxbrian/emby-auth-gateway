package pbstore

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

func TestCurrentPlaybackCodecRoundTripAllOptionalsAndExplicitZeros(t *testing.T) {
	t.Parallel()

	parent := newCodecParentSession("sess_parent_01", "token-hash-a")
	record := newCodecCurrentPlaybackRecord(parent.Collection().Id)

	pos := int64(0)
	canSeek := false
	paused := false
	muted := true
	vol := 0
	audio := 0
	sub := -1
	ms := ""
	method := "DirectPlay"
	rate := 0.0
	repeat := "RepeatNone"
	shuffle := false
	subOff := 0.0

	in := gateway.CurrentPlayback{
		GatewayTokenHash: "token-hash-a",
		ItemID:           "item-1",
		PlaySessionID:    "play-sess-1",
		MediaSourceID:    "media-1",
		ItemSnapshot: gateway.PlaybackItemSnapshot{
			ID:                "item-1",
			Name:              "Episode",
			Type:              "Episode",
			MediaType:         "Video",
			SeriesID:          "series-1",
			SeriesName:        "Show",
			SeasonID:          "season-1",
			ParentID:          "parent-1",
			IndexNumber:       3,
			ParentIndexNumber: 1,
			RunTimeTicks:      6_000_000_000,
			ProductionYear:    2024,
			PremiereDate:      "2024-01-02",
			CommunityRating:   8.5,
			OfficialRating:    "TV-14",
			ImageTags:         map[string]string{"Primary": "tag-a", "Thumb": "tag-b"},
		},
		PlayState: gateway.PlaybackPlayState{
			PositionTicks:       &pos,
			CanSeek:             &canSeek,
			IsPaused:            &paused,
			IsMuted:             &muted,
			VolumeLevel:         &vol,
			AudioStreamIndex:    &audio,
			SubtitleStreamIndex: &sub,
			MediaSourceID:       &ms,
			PlayMethod:          &method,
			PlaybackRate:        &rate,
			RepeatMode:          &repeat,
			Shuffle:             &shuffle,
			SubtitleOffset:      &subOff,
		},
		RunTimeTicks:   6_000_000_000,
		StartedAt:      time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		LastReportedAt: time.Date(2030, 1, 2, 3, 5, 5, 0, time.UTC),
	}

	if err := setCurrentPlaybackRecord(record, parent, in); err != nil {
		t.Fatalf("set: %v", err)
	}
	out, err := currentPlaybackFromRecord(record, parent)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	if out.GatewayTokenHash != in.GatewayTokenHash || out.ItemID != in.ItemID ||
		out.PlaySessionID != in.PlaySessionID || out.MediaSourceID != in.MediaSourceID ||
		out.RunTimeTicks != in.RunTimeTicks {
		t.Fatalf("scalars = %#v", out)
	}
	if !out.StartedAt.Equal(in.StartedAt) || !out.LastReportedAt.Equal(in.LastReportedAt) {
		t.Fatalf("dates started=%v last=%v", out.StartedAt, out.LastReportedAt)
	}
	if !reflect.DeepEqual(out.ItemSnapshot, in.ItemSnapshot) {
		t.Fatalf("snapshot =\n%#v\nwant\n%#v", out.ItemSnapshot, in.ItemSnapshot)
	}
	if !playStateEqual(out.PlayState, in.PlayState) {
		t.Fatalf("play state =\n%#v\nwant\n%#v", out.PlayState, in.PlayState)
	}

	// Explicit zeros/false must survive compact JSON (not dropped as omitted).
	if out.PlayState.PositionTicks == nil || *out.PlayState.PositionTicks != 0 {
		t.Fatalf("PositionTicks = %v, want explicit 0", out.PlayState.PositionTicks)
	}
	if out.PlayState.CanSeek == nil || *out.PlayState.CanSeek {
		t.Fatalf("CanSeek = %v, want explicit false", out.PlayState.CanSeek)
	}
	if out.PlayState.IsPaused == nil || *out.PlayState.IsPaused {
		t.Fatalf("IsPaused = %v, want explicit false", out.PlayState.IsPaused)
	}
	if out.PlayState.PlaybackRate == nil || *out.PlayState.PlaybackRate != 0 {
		t.Fatalf("PlaybackRate = %v, want explicit 0", out.PlayState.PlaybackRate)
	}
}

func TestCurrentPlaybackCodecCompactJSON(t *testing.T) {
	t.Parallel()

	snapJSON, err := marshalPlaybackItemSnapshot(gateway.PlaybackItemSnapshot{
		ID:   "item-1",
		Name: "A",
		ImageTags: map[string]string{
			"Z": "1",
			"A": "2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapJSON, " ") || strings.Contains(snapJSON, "\n") || strings.Contains(snapJSON, "\t") {
		t.Fatalf("snapshot JSON not compact: %q", snapJSON)
	}
	// Deterministic map key order (encoding/json sorts map keys).
	if !strings.Contains(snapJSON, `"ImageTags":{"A":"2","Z":"1"}`) {
		t.Fatalf("snapshot JSON map order not deterministic: %s", snapJSON)
	}

	pos := int64(1)
	paused := false
	psJSON, err := marshalPlaybackPlayState(gateway.PlaybackPlayState{
		PositionTicks: &pos,
		IsPaused:      &paused,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(psJSON, " ") || strings.Contains(psJSON, "\n") {
		t.Fatalf("play state JSON not compact: %q", psJSON)
	}
	if psJSON != `{"PositionTicks":1,"IsPaused":false}` {
		// Field order follows struct declaration; accept either stable compact form.
		var probe map[string]any
		if err := json.Unmarshal([]byte(psJSON), &probe); err != nil {
			t.Fatal(err)
		}
		if probe["PositionTicks"] != float64(1) || probe["IsPaused"] != false {
			t.Fatalf("play state JSON = %s", psJSON)
		}
	}
}

func TestCurrentPlaybackStrictCodecRejectsNonCanonicalDocuments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		parse func(string) error
		raw   string
		want  string
	}{
		{
			name: "snapshot_unknown_UserData",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id":"item-1","UserData":{"Played":true}}`,
			want: "unknown field",
		},
		{
			name: "snapshot_unknown_AccessToken",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id":"item-1","AccessToken":"secret"}`,
			want: "unknown field",
		},
		{
			name: "snapshot_arbitrary_key",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id":"item-1","Arbitrary":"value"}`,
			want: "unknown field",
		},
		{
			name: "play_state_unknown_key",
			parse: func(raw string) error {
				_, err := parsePlaybackPlayState(raw)
				return err
			},
			raw:  `{"PositionTicks":1,"Arbitrary":true}`,
			want: "unknown field",
		},
		{
			name: "play_state_MediaSource",
			parse: func(raw string) error {
				_, err := parsePlaybackPlayState(raw)
				return err
			},
			raw:  `{"MediaSource":{"Id":"media-1"}}`,
			want: "unknown field",
		},
		{
			name: "snapshot_duplicate_top_level",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id":"item-1","Id":"item-2"}`,
			want: "duplicate key",
		},
		{
			name: "play_state_duplicate_top_level",
			parse: func(raw string) error {
				_, err := parsePlaybackPlayState(raw)
				return err
			},
			raw:  `{"IsPaused":false,"IsPaused":true}`,
			want: "duplicate key",
		},
		{
			name: "snapshot_duplicate_nested_ImageTags_key",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id":"item-1","ImageTags":{"Primary":"a","Primary":"b"}}`,
			want: "duplicate key",
		},
		{
			name: "snapshot_leading_whitespace",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  ` {"Id":"item-1"}`,
			want: "whitespace",
		},
		{
			name: "snapshot_trailing_whitespace",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  "{\"Id\":\"item-1\"}\n",
			want: "whitespace",
		},
		{
			name: "snapshot_internal_whitespace",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id": "item-1"}`,
			want: "canonical",
		},
		{
			name: "snapshot_noncanonical_key_order",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Name":"A","Id":"item-1"}`,
			want: "canonical",
		},
		{
			name: "snapshot_noncanonical_escaping",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id":"item-1","Name":"\u0041"}`,
			want: "canonical",
		},
		{
			name: "snapshot_noncanonical_number",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id":"item-1","CommunityRating":1.0}`,
			want: "canonical",
		},
		{
			name: "snapshot_trailing_JSON_data",
			parse: func(raw string) error {
				_, err := parsePlaybackItemSnapshot(raw)
				return err
			},
			raw:  `{"Id":"item-1"}{}`,
			want: "trailing data",
		},
		{
			name: "play_state_trailing_JSON_data",
			parse: func(raw string) error {
				_, err := parsePlaybackPlayState(raw)
				return err
			},
			raw:  `{}false`,
			want: "trailing data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.parse(tt.raw)
			if err == nil {
				t.Fatal("expected integrity error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestCurrentPlaybackStrictCodecAcceptsCanonicalRoundTripAndExplicitZeros(t *testing.T) {
	t.Parallel()

	snapshot := gateway.PlaybackItemSnapshot{
		ID:              "item-1",
		Name:            "A",
		CommunityRating: 1,
		ImageTags:       map[string]string{"Thumb": "b", "Primary": "a"},
	}
	snapshotJSON, err := marshalPlaybackItemSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	gotSnapshot, err := parsePlaybackItemSnapshot(snapshotJSON)
	if err != nil {
		t.Fatalf("parse canonical snapshot: %v", err)
	}
	if !reflect.DeepEqual(gotSnapshot, snapshot) {
		t.Fatalf("snapshot = %#v, want %#v", gotSnapshot, snapshot)
	}

	zeroTicks := int64(0)
	zeroVolume := 0
	falseValue := false
	zeroRate := float64(0)
	playState := gateway.PlaybackPlayState{
		PositionTicks: &zeroTicks,
		CanSeek:       &falseValue,
		IsPaused:      &falseValue,
		VolumeLevel:   &zeroVolume,
		PlaybackRate:  &zeroRate,
		Shuffle:       &falseValue,
	}
	playStateJSON, err := marshalPlaybackPlayState(playState)
	if err != nil {
		t.Fatal(err)
	}
	gotPlayState, err := parsePlaybackPlayState(playStateJSON)
	if err != nil {
		t.Fatalf("parse canonical play state: %v", err)
	}
	if !playStateEqual(gotPlayState, playState) {
		t.Fatalf("play state = %#v, want %#v", gotPlayState, playState)
	}
	if gotPlayState.PositionTicks == nil || *gotPlayState.PositionTicks != 0 ||
		gotPlayState.CanSeek == nil || *gotPlayState.CanSeek ||
		gotPlayState.IsPaused == nil || *gotPlayState.IsPaused ||
		gotPlayState.VolumeLevel == nil || *gotPlayState.VolumeLevel != 0 ||
		gotPlayState.PlaybackRate == nil || *gotPlayState.PlaybackRate != 0 ||
		gotPlayState.Shuffle == nil || *gotPlayState.Shuffle {
		t.Fatalf("explicit zero/false pointers were not preserved: %#v", gotPlayState)
	}
}

func TestCurrentPlaybackCodecCloneIsolation(t *testing.T) {
	t.Parallel()

	parent := newCodecParentSession("sess_clone_01", "hash-clone")
	record := newCodecCurrentPlaybackRecord(parent.Collection().Id)

	tags := map[string]string{"Primary": "t1"}
	pos := int64(10)
	in := gateway.CurrentPlayback{
		GatewayTokenHash: "hash-clone",
		ItemID:           "item-clone",
		ItemSnapshot: gateway.PlaybackItemSnapshot{
			ID:        "item-clone",
			ImageTags: tags,
		},
		PlayState: gateway.PlaybackPlayState{
			PositionTicks: &pos,
		},
		StartedAt:      time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		LastReportedAt: time.Date(2030, 1, 1, 0, 1, 0, 0, time.UTC),
	}
	if err := setCurrentPlaybackRecord(record, parent, in); err != nil {
		t.Fatal(err)
	}

	// Mutating inputs after set must not change persisted JSON.
	tags["Primary"] = "mutated"
	pos = 999
	if record.GetString("item_snapshot_json") == "" || strings.Contains(record.GetString("item_snapshot_json"), "mutated") {
		t.Fatalf("set did not isolate ImageTags: %s", record.GetString("item_snapshot_json"))
	}
	if strings.Contains(record.GetString("play_state_json"), "999") {
		t.Fatalf("set did not isolate PositionTicks: %s", record.GetString("play_state_json"))
	}

	out, err := currentPlaybackFromRecord(record, parent)
	if err != nil {
		t.Fatal(err)
	}
	out.ItemSnapshot.ImageTags["Primary"] = "hydrated-mut"
	*out.PlayState.PositionTicks = 42

	again, err := currentPlaybackFromRecord(record, parent)
	if err != nil {
		t.Fatal(err)
	}
	if again.ItemSnapshot.ImageTags["Primary"] != "t1" {
		t.Fatalf("hydrate ImageTags not isolated: %v", again.ItemSnapshot.ImageTags)
	}
	if again.PlayState.PositionTicks == nil || *again.PlayState.PositionTicks != 10 {
		t.Fatalf("hydrate pointer not isolated: %v", again.PlayState.PositionTicks)
	}
}

func TestCurrentPlaybackCodecRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	parent := newCodecParentSession("sess_bad_json", "hash-json")
	base := validCodecPlayback(parent.GetString("gateway_token_hash"), "item-json")

	cases := []struct {
		name   string
		mutate func(*core.Record)
		want   string
	}{
		{
			name: "snapshot_null",
			mutate: func(r *core.Record) {
				r.Set("item_snapshot_json", "null")
			},
			want: "item_snapshot_json",
		},
		{
			name: "snapshot_array",
			mutate: func(r *core.Record) {
				r.Set("item_snapshot_json", `[]`)
			},
			want: "item_snapshot_json",
		},
		{
			name: "snapshot_malformed",
			mutate: func(r *core.Record) {
				r.Set("item_snapshot_json", `{"Id":`)
			},
			want: "item_snapshot_json",
		},
		{
			name: "snapshot_oversize",
			mutate: func(r *core.Record) {
				// Valid object but over schema max.
				r.Set("item_snapshot_json", `{"Id":"item-json","Pad":"`+strings.Repeat("x", currentPlaybackItemSnapshotJSONMax)+`"}`)
			},
			want: "item_snapshot_json",
		},
		{
			name: "play_state_null",
			mutate: func(r *core.Record) {
				r.Set("play_state_json", "null")
			},
			want: "play_state_json",
		},
		{
			name: "play_state_array",
			mutate: func(r *core.Record) {
				r.Set("play_state_json", `["x"]`)
			},
			want: "play_state_json",
		},
		{
			name: "play_state_oversize",
			mutate: func(r *core.Record) {
				r.Set("play_state_json", `{"Pad":"`+strings.Repeat("y", currentPlaybackPlayStateJSONMax)+`"}`)
			},
			want: "play_state_json",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := newCodecCurrentPlaybackRecord(parent.Collection().Id)
			if err := setCurrentPlaybackRecord(record, parent, base); err != nil {
				t.Fatal(err)
			}
			tc.mutate(record)
			_, err := currentPlaybackFromRecord(record, parent)
			if err == nil {
				t.Fatal("expected integrity error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	// Marshal path also rejects oversize after encoding.
	huge := gateway.PlaybackItemSnapshot{
		ID:   "item-json",
		Name: strings.Repeat("n", currentPlaybackItemSnapshotJSONMax),
	}
	if _, err := marshalPlaybackItemSnapshot(huge); err == nil {
		t.Fatal("expected oversize snapshot marshal failure")
	}
}

func TestCurrentPlaybackCodecRejectsItemSnapshotMismatch(t *testing.T) {
	t.Parallel()

	parent := newCodecParentSession("sess_mismatch", "hash-mismatch")
	record := newCodecCurrentPlaybackRecord(parent.Collection().Id)

	cp := validCodecPlayback("hash-mismatch", "item-a")
	cp.ItemSnapshot.ID = "item-b"
	if err := setCurrentPlaybackRecord(record, parent, cp); err == nil || !strings.Contains(err.Error(), "does not equal item_id") {
		t.Fatalf("set error = %v", err)
	}

	cp = validCodecPlayback("hash-mismatch", "item-a")
	if err := setCurrentPlaybackRecord(record, parent, cp); err != nil {
		t.Fatal(err)
	}
	// Corrupt persisted snapshot Id without changing item_id.
	record.Set("item_snapshot_json", `{"Id":"item-other","Name":"x"}`)
	_, err := currentPlaybackFromRecord(record, parent)
	if err == nil || !strings.Contains(err.Error(), "does not equal item_id") {
		t.Fatalf("hydrate error = %v", err)
	}
}

func TestCurrentPlaybackCodecRejectsBadRelationAndToken(t *testing.T) {
	t.Parallel()

	parent := newCodecParentSession("sess_rel_ok", "hash-rel")
	other := newCodecParentSession("sess_rel_other", "hash-other")
	record := newCodecCurrentPlaybackRecord(parent.Collection().Id)
	cp := validCodecPlayback("hash-rel", "item-rel")
	if err := setCurrentPlaybackRecord(record, parent, cp); err != nil {
		t.Fatal(err)
	}

	_, err := currentPlaybackFromRecord(record, other)
	if err == nil || !strings.Contains(err.Error(), "does not match parent") {
		t.Fatalf("hydrate wrong parent: %v", err)
	}

	// Domain token hash must not disagree with authoritative parent.
	cp.GatewayTokenHash = "wrong-hash"
	if err := setCurrentPlaybackRecord(record, parent, cp); err == nil || !strings.Contains(err.Error(), "token hash") {
		t.Fatalf("set wrong token: %v", err)
	}

	// Empty relation on record.
	record.Set("gateway_session", "")
	_, err = currentPlaybackFromRecord(record, parent)
	if err == nil || !strings.Contains(err.Error(), "empty gateway_session") {
		t.Fatalf("empty relation: %v", err)
	}
}

func TestCurrentPlaybackCodecRejectsBadRuntimeAndDates(t *testing.T) {
	t.Parallel()

	parent := newCodecParentSession("sess_rt", "hash-rt")
	record := newCodecCurrentPlaybackRecord(parent.Collection().Id)
	cp := validCodecPlayback("hash-rt", "item-rt")

	cp.RunTimeTicks = -1
	if err := setCurrentPlaybackRecord(record, parent, cp); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("set negative runtime: %v", err)
	}

	cp = validCodecPlayback("hash-rt", "item-rt")
	cp.StartedAt = time.Time{}
	if err := setCurrentPlaybackRecord(record, parent, cp); err == nil || !strings.Contains(err.Error(), "started_at") {
		t.Fatalf("set zero started: %v", err)
	}

	cp = validCodecPlayback("hash-rt", "item-rt")
	cp.LastReportedAt = time.Time{}
	if err := setCurrentPlaybackRecord(record, parent, cp); err == nil || !strings.Contains(err.Error(), "last_reported_at") {
		t.Fatalf("set zero last: %v", err)
	}

	cp = validCodecPlayback("hash-rt", "item-rt")
	cp.StartedAt = time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	cp.LastReportedAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := setCurrentPlaybackRecord(record, parent, cp); err == nil || !strings.Contains(err.Error(), "before started_at") {
		t.Fatalf("set reversed dates: %v", err)
	}

	// Hydrate path: fractional and negative runtime are integrity failures.
	cp = validCodecPlayback("hash-rt", "item-rt")
	if err := setCurrentPlaybackRecord(record, parent, cp); err != nil {
		t.Fatal(err)
	}
	record.Set("run_time_ticks", 1.5)
	_, err := currentPlaybackFromRecord(record, parent)
	if err == nil || !strings.Contains(err.Error(), "not an integer") {
		t.Fatalf("fractional runtime: %v", err)
	}

	record.Set("run_time_ticks", -3)
	_, err = currentPlaybackFromRecord(record, parent)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative runtime hydrate: %v", err)
	}

	record.Set("run_time_ticks", int64(0))
	record.Set("started_at", time.Time{})
	_, err = currentPlaybackFromRecord(record, parent)
	if err == nil || !strings.Contains(err.Error(), "started_at") {
		t.Fatalf("zero started hydrate: %v", err)
	}
}

func TestCurrentPlaybackCodecExistingRecordUpdate(t *testing.T) {
	t.Parallel()

	parent := newCodecParentSession("sess_upd", "hash-upd")
	record := newCodecCurrentPlaybackRecord(parent.Collection().Id)
	first := validCodecPlayback("hash-upd", "item-1")
	if err := setCurrentPlaybackRecord(record, parent, first); err != nil {
		t.Fatal(err)
	}
	second := validCodecPlayback("hash-upd", "item-2")
	second.PlaySessionID = "next"
	second.RunTimeTicks = 100
	if err := setCurrentPlaybackRecord(record, parent, second); err != nil {
		t.Fatal(err)
	}
	out, err := currentPlaybackFromRecord(record, parent)
	if err != nil {
		t.Fatal(err)
	}
	if out.ItemID != "item-2" || out.PlaySessionID != "next" || out.RunTimeTicks != 100 {
		t.Fatalf("updated = %#v", out)
	}
}

func newCodecParentSession(id, tokenHash string) *core.Record {
	c := core.NewBaseCollection("gateway_sessions", "pbc_sessions_codec01")
	c.Fields.Add(&core.TextField{Name: "gateway_token_hash", Max: 128})
	rec := core.NewRecord(c)
	rec.Id = id
	rec.Set("gateway_token_hash", tokenHash)
	return rec
}

func newCodecCurrentPlaybackRecord(sessionCollectionID string) *core.Record {
	return core.NewRecord(pbschema.CurrentPlaybacks(sessionCollectionID))
}

func validCodecPlayback(tokenHash, itemID string) gateway.CurrentPlayback {
	return gateway.CurrentPlayback{
		GatewayTokenHash: tokenHash,
		ItemID:           itemID,
		ItemSnapshot: gateway.PlaybackItemSnapshot{
			ID: itemID,
		},
		PlayState:      gateway.PlaybackPlayState{},
		StartedAt:      time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		LastReportedAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func playStateEqual(a, b gateway.PlaybackPlayState) bool {
	return reflect.DeepEqual(clonePlaybackPlayState(a), clonePlaybackPlayState(b))
}

// --- ListCurrentPlaybacks (repository list path) ---

func TestListCurrentPlaybacksEmptyUnknownAndNoCurrent(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "list-empty", "syn-list-empty")

	// Empty / whitespace-only input -> empty non-nil map.
	got, err := store.ListCurrentPlaybacks(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("nil input = %#v", got)
	}
	got, err = store.ListCurrentPlaybacks(context.Background(), []string{"", "  ", "\t"})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("whitespace input = %#v err=%v", got, err)
	}

	// Unknown token omitted.
	got, err = store.ListCurrentPlaybacks(context.Background(), []string{"missing-hash"})
	if err != nil || len(got) != 0 {
		t.Fatalf("unknown = %#v err=%v", got, err)
	}

	// Session without current row omitted.
	createListSession(t, store, userID, "hash-no-current", "device-nc")
	got, err = store.ListCurrentPlaybacks(context.Background(), []string{"hash-no-current"})
	if err != nil || len(got) != 0 {
		t.Fatalf("no current = %#v err=%v", got, err)
	}
}

func TestListCurrentPlaybacksOneAndMultipleSessions(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "list-multi", "syn-list-multi")

	createListSession(t, store, userID, "hash-a", "dev-a")
	createListSession(t, store, userID, "hash-b", "dev-b")
	createListSession(t, store, userID, "hash-c", "dev-c")
	saveListCurrent(t, app, "hash-a", "item-a")
	saveListCurrent(t, app, "hash-b", "item-b")
	// hash-c has no current row.

	got, err := store.ListCurrentPlaybacks(context.Background(), []string{"hash-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["hash-a"].ItemID != "item-a" {
		t.Fatalf("single = %#v", got)
	}

	got, err = store.ListCurrentPlaybacks(context.Background(), []string{"hash-a", "hash-b", "hash-c", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("multi len = %d, want 2: %#v", len(got), got)
	}
	if got["hash-a"].ItemID != "item-a" || got["hash-b"].ItemID != "item-b" {
		t.Fatalf("multi = %#v", got)
	}
	if _, ok := got["hash-c"]; ok {
		t.Fatal("session without current must be omitted")
	}
}

func TestListCurrentPlaybacksTwoUsersDeviceIsolation(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	u1 := createGatewayUser(t, app, "list-u1", "syn-list-u1")
	u2 := createGatewayUser(t, app, "list-u2", "syn-list-u2")

	createListSession(t, store, u1, "u1-dev1", "d1")
	createListSession(t, store, u1, "u1-dev2", "d2")
	createListSession(t, store, u2, "u2-dev1", "d3")
	saveListCurrent(t, app, "u1-dev1", "item-u1-1")
	saveListCurrent(t, app, "u1-dev2", "item-u1-2")
	saveListCurrent(t, app, "u2-dev1", "item-u2-1")

	got, err := store.ListCurrentPlaybacks(context.Background(), []string{"u1-dev1", "u1-dev2", "u2-dev1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	if got["u1-dev1"].ItemID != "item-u1-1" || got["u1-dev2"].ItemID != "item-u1-2" || got["u2-dev1"].ItemID != "item-u2-1" {
		t.Fatalf("isolation = %#v", got)
	}

	// Request only user2 device: must not include user1 rows.
	got, err = store.ListCurrentPlaybacks(context.Background(), []string{"u2-dev1"})
	if err != nil || len(got) != 1 || got["u2-dev1"].ItemID != "item-u2-1" {
		t.Fatalf("user2 only = %#v err=%v", got, err)
	}
}

func TestListCurrentPlaybacksDuplicateInputDedupe(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "list-dedupe", "syn-list-dedupe")
	createListSession(t, store, userID, "hash-dedupe", "dev")
	saveListCurrent(t, app, "hash-dedupe", "item-dedupe")

	got, err := store.ListCurrentPlaybacks(context.Background(), []string{
		"  hash-dedupe  ",
		"hash-dedupe",
		"",
		"hash-dedupe",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["hash-dedupe"].ItemID != "item-dedupe" {
		t.Fatalf("dedupe = %#v", got)
	}
}

func TestListCurrentPlaybacksBatchAboveSafeThreshold(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "list-batch", "syn-list-batch")

	const n = playbackStateItemIDBatchLimit + 5 // 55 with limit 50
	hashes := make([]string, 0, n)
	for i := 0; i < n; i++ {
		hash := fmt.Sprintf("batch-hash-%03d", i)
		createListSession(t, store, userID, hash, fmt.Sprintf("dev-%d", i))
		saveListCurrent(t, app, hash, fmt.Sprintf("item-%03d", i))
		hashes = append(hashes, hash)
	}

	got, err := store.ListCurrentPlaybacks(context.Background(), hashes)
	if err != nil {
		t.Fatalf("batch list: %v", err)
	}
	if len(got) != n {
		t.Fatalf("batch len = %d, want %d", len(got), n)
	}
	for i := 0; i < n; i++ {
		hash := fmt.Sprintf("batch-hash-%03d", i)
		if got[hash].ItemID != fmt.Sprintf("item-%03d", i) {
			t.Fatalf("hash %s = %#v", hash, got[hash])
		}
	}
}

func TestListCurrentPlaybacksCorruptFailsWholeCall(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "list-corrupt", "syn-list-corrupt")

	createListSession(t, store, userID, "hash-good", "dev-good")
	createListSession(t, store, userID, "hash-bad-json", "dev-bad-json")
	createListSession(t, store, userID, "hash-bad-date", "dev-bad-date")
	createListSession(t, store, userID, "hash-bad-item", "dev-bad-item")
	saveListCurrent(t, app, "hash-good", "item-good")
	saveListCurrent(t, app, "hash-bad-json", "item-bad-json")
	saveListCurrent(t, app, "hash-bad-date", "item-bad-date")
	saveListCurrent(t, app, "hash-bad-item", "item-bad-item")

	badJSONID := listSessionAuthID(t, app, "hash-bad-json")
	badDateID := listSessionAuthID(t, app, "hash-bad-date")
	badItemID := listSessionAuthID(t, app, "hash-bad-item")

	// Corrupt JSON on one row.
	if _, err := app.DB().NewQuery(`
		UPDATE gateway_current_playbacks SET item_snapshot_json = 'null'
		WHERE gateway_session = {:sid}
	`).Bind(map[string]any{"sid": badJSONID}).Execute(); err != nil {
		t.Fatal(err)
	}
	_, err := store.ListCurrentPlaybacks(context.Background(), []string{"hash-good", "hash-bad-json"})
	if err == nil || !strings.Contains(err.Error(), "item_snapshot_json") {
		t.Fatalf("corrupt JSON err = %v", err)
	}

	// List only good still works (corrupt not in request).
	got, err := store.ListCurrentPlaybacks(context.Background(), []string{"hash-good"})
	if err != nil || len(got) != 1 {
		t.Fatalf("good alone after other corrupt: %#v err=%v", got, err)
	}

	// Reversed dates via raw SQL.
	if _, err := app.DB().NewQuery(`
		UPDATE gateway_current_playbacks
		SET started_at = '2030-01-02 00:00:00.000Z', last_reported_at = '2030-01-01 00:00:00.000Z'
		WHERE gateway_session = {:sid}
	`).Bind(map[string]any{"sid": badDateID}).Execute(); err != nil {
		t.Fatal(err)
	}
	_, err = store.ListCurrentPlaybacks(context.Background(), []string{"hash-good", "hash-bad-date"})
	if err == nil || !strings.Contains(err.Error(), "last_reported_at") {
		t.Fatalf("bad date err = %v", err)
	}

	// Item snapshot Id mismatch.
	if _, err := app.DB().NewQuery(`
		UPDATE gateway_current_playbacks
		SET item_snapshot_json = '{"Id":"other-item"}'
		WHERE gateway_session = {:sid}
	`).Bind(map[string]any{"sid": badItemID}).Execute(); err != nil {
		t.Fatal(err)
	}
	_, err = store.ListCurrentPlaybacks(context.Background(), []string{"hash-good", "hash-bad-item"})
	if err == nil || !strings.Contains(err.Error(), "does not equal item_id") {
		t.Fatalf("item mismatch err = %v", err)
	}
}

func TestCurrentPlaybackNestedCorruptionFailsListAndApplyWithoutMutation(t *testing.T) {
	tests := []struct {
		name  string
		field string
		raw   string
		want  string
	}{
		{
			name:  "negative_snapshot_runtime",
			field: "item_snapshot_json",
			raw:   `{"Id":"item-bad","RunTimeTicks":-1}`,
			want:  "negative",
		},
		{
			name:  "negative_position",
			field: "play_state_json",
			raw:   `{"PositionTicks":-1}`,
			want:  "position_ticks",
		},
		{
			name:  "overlong_snapshot_name",
			field: "item_snapshot_json",
			raw:   `{"Id":"item-bad","Name":"` + strings.Repeat("n", 513) + `"}`,
			want:  "item_snapshot.name",
		},
		{
			name:  "rating_out_of_range",
			field: "item_snapshot_json",
			raw:   `{"Id":"item-bad","CommunityRating":11}`,
			want:  "community_rating",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newCurrentPlaybackListApp(t)
			store := New(app)
			userID := createGatewayUser(t, app, "nested-"+tt.name, "syn-"+tt.name)
			createListSession(t, store, userID, "hash-good", "dev-good")
			createListSession(t, store, userID, "hash-bad", "dev-bad")
			saveListCurrent(t, app, "hash-good", "item-good")
			saveListCurrent(t, app, "hash-bad", "item-bad")
			badSessionID := listSessionAuthID(t, app, "hash-bad")

			query := fmt.Sprintf("UPDATE gateway_current_playbacks SET %s = {:raw} WHERE gateway_session = {:sid}", tt.field)
			if _, err := app.DB().NewQuery(query).Bind(map[string]any{"raw": tt.raw, "sid": badSessionID}).Execute(); err != nil {
				t.Fatal(err)
			}

			listed, err := store.ListCurrentPlaybacks(context.Background(), []string{"hash-good", "hash-bad"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("List error = %v, want %q", err, tt.want)
			}
			if listed != nil {
				t.Fatalf("List returned partial results: %#v", listed)
			}

			eventsBefore := countPlaybackEventsForUser(t, app, userID)
			durableBefore := countUserItemData(t, app, userID)
			currentsBefore := countAllCurrentPlaybacks(t, app)
			activityBefore := sessionActivity(t, store, "hash-bad")
			_, err = store.ApplyPlaybackReport(context.Background(), gateway.PlaybackReportCommand{
				GatewayTokenHash: "hash-bad",
				Kind:             gateway.PlaybackReportProgress,
				ReceivedAt:       time.Date(2030, 1, 2, 3, 6, 0, 0, time.UTC),
				ItemID:           "item-bad",
				PlayState:        gateway.PlaybackPlayState{PositionTicks: int64Ptr(10)},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply error = %v, want %q", err, tt.want)
			}
			if countPlaybackEventsForUser(t, app, userID) != eventsBefore ||
				countUserItemData(t, app, userID) != durableBefore ||
				countAllCurrentPlaybacks(t, app) != currentsBefore {
				t.Fatal("Apply mutated playback state after nested integrity failure")
			}
			if activityAfter := sessionActivity(t, store, "hash-bad"); !activityAfter.Equal(activityBefore) {
				t.Fatalf("activity changed: %v -> %v", activityBefore, activityAfter)
			}
		})
	}
}

func TestListCurrentPlaybacksDuplicateRowsFailOrUniqueConstraint(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "list-dup", "syn-list-dup")
	createListSession(t, store, userID, "hash-dup", "dev-dup")
	saveListCurrent(t, app, "hash-dup", "item-dup")
	sessID := listSessionAuthID(t, app, "hash-dup")

	// Bypass unique index and insert a second current row for the same session.
	if _, err := app.DB().NewQuery(`DROP INDEX IF EXISTS idx_gateway_current_playbacks_session`).Execute(); err != nil {
		t.Fatalf("drop unique index: %v", err)
	}
	if _, err := app.DB().NewQuery(`
		INSERT INTO gateway_current_playbacks
			(id, gateway_session, item_id, play_session_id, media_source_id, item_snapshot_json, play_state_json, run_time_ticks, started_at, last_reported_at, created, updated)
		VALUES
			('dupcurrent00002', {:sid}, 'item-dup-2', '', '', '{"Id":"item-dup-2"}', '{}', 0,
			 '2030-01-02 03:04:05.000Z', '2030-01-02 03:04:05.000Z',
			 '2030-01-02 03:04:05.000Z', '2030-01-02 03:04:05.000Z')
	`).Bind(map[string]any{"sid": sessID}).Execute(); err != nil {
		// If insert is still blocked by another constraint, the unique path holds.
		t.Logf("second row insert blocked (unique still enforced elsewhere): %v", err)
		// Assert first list still works under unique constraint.
		got, listErr := store.ListCurrentPlaybacks(context.Background(), []string{"hash-dup"})
		if listErr != nil || len(got) != 1 {
			t.Fatalf("unique-constrained list = %#v err=%v", got, listErr)
		}
		return
	}

	_, err := store.ListCurrentPlaybacks(context.Background(), []string{"hash-dup"})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate rows err = %v", err)
	}
}

func TestListCurrentPlaybacksContextCancellation(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "list-ctx", "syn-list-ctx")
	createListSession(t, store, userID, "hash-ctx", "dev-ctx")
	saveListCurrent(t, app, "hash-ctx", "item-ctx")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.ListCurrentPlaybacks(ctx, []string{"hash-ctx"})
	if err == nil {
		t.Fatal("expected context error")
	}
	if !strings.Contains(err.Error(), "context") && err != context.Canceled {
		// Accept any context-canceled wrapping.
		if ctx.Err() == nil {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestListCurrentPlaybacksCloneIsolation(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "list-clone", "syn-list-clone")
	createListSession(t, store, userID, "hash-clone-list", "dev-clone")

	col, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "hash-clone-list")
	if err != nil {
		t.Fatal(err)
	}
	rec := core.NewRecord(col)
	cp := gateway.CurrentPlayback{
		GatewayTokenHash: "hash-clone-list",
		ItemID:           "item-clone-list",
		ItemSnapshot: gateway.PlaybackItemSnapshot{
			ID:        "item-clone-list",
			ImageTags: map[string]string{"Primary": "orig"},
		},
		PlayState: gateway.PlaybackPlayState{
			PositionTicks: int64Ptr(100),
		},
		StartedAt:      time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		LastReportedAt: time.Date(2030, 1, 2, 3, 5, 5, 0, time.UTC),
	}
	if err := setCurrentPlaybackRecord(rec, parent, cp); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListCurrentPlaybacks(context.Background(), []string{"hash-clone-list"})
	if err != nil {
		t.Fatal(err)
	}
	row := got["hash-clone-list"]
	row.ItemSnapshot.ImageTags["Primary"] = "mutated"
	*row.PlayState.PositionTicks = 999

	again, err := store.ListCurrentPlaybacks(context.Background(), []string{"hash-clone-list"})
	if err != nil {
		t.Fatal(err)
	}
	row2 := again["hash-clone-list"]
	if row2.ItemSnapshot.ImageTags["Primary"] != "orig" {
		t.Fatalf("ImageTags leaked: %v", row2.ItemSnapshot.ImageTags)
	}
	if row2.PlayState.PositionTicks == nil || *row2.PlayState.PositionTicks != 100 {
		t.Fatalf("PositionTicks leaked: %v", row2.PlayState.PositionTicks)
	}
}

func newCurrentPlaybackListApp(t *testing.T) core.App {
	t.Helper()
	app := newSessionTestApp(t)
	ensureCurrentPlaybacksCollection(t, app)
	return app
}

func ensureCurrentPlaybacksCollection(t *testing.T, app core.App) {
	t.Helper()
	if _, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection); err == nil {
		return
	}
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find gateway_sessions: %v", err)
	}
	if err := app.Save(pbschema.CurrentPlaybacks(sessions.Id)); err != nil {
		t.Fatalf("create gateway_current_playbacks: %v", err)
	}
}

func createListSession(t *testing.T, store *Store, userID, tokenHash, deviceID string) {
	t.Helper()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: tokenHash,
		GatewayUserID:    userID,
		GatewayUsername:  "list-user",
		SyntheticUserID:  "syn-list",
		Client:           "TestClient",
		Device:           "TestDevice",
		DeviceID:         deviceID,
		Version:          "1.0",
		RemoteIP:         "127.0.0.1",
		CreatedAt:        now,
		ExpiresAt:        time.Now().UTC().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession %s: %v", tokenHash, err)
	}
}

func listSessionAuthID(t *testing.T, app core.App, tokenHash string) string {
	t.Helper()
	auth, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", tokenHash)
	if err != nil {
		t.Fatalf("find auth %s: %v", tokenHash, err)
	}
	return auth.Id
}

func saveListCurrent(t *testing.T, app core.App, tokenHash, itemID string) {
	t.Helper()
	auth, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", tokenHash)
	if err != nil {
		t.Fatalf("find auth %s: %v", tokenHash, err)
	}
	col, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	rec := core.NewRecord(col)
	cp := validCodecPlayback(tokenHash, itemID)
	if err := setCurrentPlaybackRecord(rec, auth, cp); err != nil {
		t.Fatalf("set current: %v", err)
	}
	if err := app.Save(rec); err != nil {
		t.Fatalf("save current: %v", err)
	}
}

func int64Ptr(v int64) *int64 { return &v }
