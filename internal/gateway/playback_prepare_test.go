package gateway

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPreparePlaybackReportCommandCanonicalKeys(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	t.Run("trim and derive snapshot id from item", func(t *testing.T) {
		cmd, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "  hash-1  ",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			ItemID:           "  item-1  ",
			PlaySessionID:    "  ps  ",
			MediaSourceID:    "  ms  ",
			RemoteIP:         "  1.2.3.4  ",
			EventName:        "  Pause  ",
		})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if cmd.GatewayTokenHash != "hash-1" || cmd.ItemID != "item-1" || cmd.ItemSnapshot.ID != "item-1" {
			t.Fatalf("canonical ids: %#v", cmd)
		}
		if cmd.PlaySessionID != "ps" || cmd.MediaSourceID != "ms" || cmd.RemoteIP != "1.2.3.4" || cmd.EventName != "Pause" {
			t.Fatalf("trimmed optionals: %#v", cmd)
		}
		if cmd.PlayState.IsPaused == nil || !*cmd.PlayState.IsPaused {
			t.Fatalf("Pause EventName not applied: %#v", cmd.PlayState)
		}
		if cmd.Policy.MinPct != defaultMinResumePct || cmd.Policy.MaxPct != defaultMaxResumePct || cmd.Policy.MinDurationSeconds != defaultMinResumeDurationSeconds {
			t.Fatalf("default policy: %#v", cmd.Policy)
		}
	})

	t.Run("derive item id from snapshot", func(t *testing.T) {
		cmd, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportPlaying,
			ReceivedAt:       now,
			ItemSnapshot:     PlaybackItemSnapshot{ID: "  from-snap  "},
		})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if cmd.ItemID != "from-snap" || cmd.ItemSnapshot.ID != "from-snap" {
			t.Fatalf("derived: %#v", cmd)
		}
	})

	t.Run("snapshot id conflict rejected", func(t *testing.T) {
		_, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			ItemID:           "item-a",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-b"},
		})
		if err == nil || !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("missing item remains no-op contract", func(t *testing.T) {
		cmd, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			PlaySessionID:    "ps-only",
		})
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if cmd.ItemID != "" || cmd.ItemSnapshot.ID != "" {
			t.Fatalf("want empty item: %#v", cmd)
		}
	})

	t.Run("overlong item id rejected", func(t *testing.T) {
		_, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			ItemID:           strings.Repeat("x", currentPlaybackItemIDMaxBytes+1),
		})
		if err == nil || !errors.Is(err, ErrBadRequest) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unsafe item identities rejected", func(t *testing.T) {
		for _, itemID := range []string{".", "..", "a/b", `a\b`, "a\x00b", "a\nb"} {
			_, err := PreparePlaybackReportCommand(PlaybackReportCommand{
				GatewayTokenHash: "h1",
				Kind:             PlaybackReportProgress,
				ReceivedAt:       now,
				ItemID:           itemID,
			})
			if err == nil || !errors.Is(err, ErrBadRequest) {
				t.Fatalf("item id %q error = %v", itemID, err)
			}
		}
		cmd, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			ItemID:           "percent%2Ftext",
		})
		if err != nil || cmd.ItemID != "percent%2Ftext" {
			t.Fatalf("percent text: cmd=%#v err=%v", cmd, err)
		}
	})

	t.Run("overlong image tags rejected", func(t *testing.T) {
		tags := make(map[string]string, playbackMaxImageTags+1)
		for i := 0; i < playbackMaxImageTags+1; i++ {
			tags["tag-"+strings.Repeat("x", 4)+string(rune('A'+i%26))+string(rune('0'+i/26%10))+string(rune('a'+i/260%26))] = "v"
		}
		if len(tags) <= playbackMaxImageTags {
			t.Fatalf("test setup: only %d unique tags", len(tags))
		}
		_, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			ItemID:           "item-1",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-1", ImageTags: tags},
		})
		if err == nil || !strings.Contains(err.Error(), "image_tags") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("nonfinite numerics rejected", func(t *testing.T) {
		nan := math.NaN()
		_, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			ItemID:           "item-1",
			PlayedPercentage: &nan,
		})
		if err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("err = %v", err)
		}
		inf := math.Inf(1)
		_, err = PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			ItemID:           "item-1",
			PlayState:        PlaybackPlayState{PlaybackRate: &inf},
		})
		if err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("negative runtime rejected", func(t *testing.T) {
		_, err := PreparePlaybackReportCommand(PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now,
			ItemID:           "item-1",
			RunTimeTicks:     -1,
		})
		if err == nil || !strings.Contains(err.Error(), "nonnegative") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPreparePlaybackReportCommandDoesNotMutateCaller(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.FixedZone("input-zone", 2*60*60))

	t.Run("success clones before normalization", func(t *testing.T) {
		input := mutablePrepareCommand(now)
		before := clonePlaybackReportCommand(input)

		prepared, err := PreparePlaybackReportCommand(input)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if !reflect.DeepEqual(input, before) {
			t.Fatalf("input mutated during successful prepare:\n got  %#v\n want %#v", input, before)
		}
		assertPreparedCommandIndependent(t, input, prepared)
		if prepared.PlayState.MediaSourceID == nil || *prepared.PlayState.MediaSourceID != "media-1" ||
			prepared.PlayState.PlayMethod == nil || *prepared.PlayState.PlayMethod != "DirectPlay" ||
			prepared.PlayState.RepeatMode == nil || *prepared.PlayState.RepeatMode != "RepeatNone" {
			t.Fatalf("normalization changed: %#v", prepared.PlayState)
		}

		prepared.ItemSnapshot.ImageTags["Primary"] = "changed"
		*prepared.PlayState.PositionTicks = 99
		*prepared.PlayState.CanSeek = true
		*prepared.PlayState.IsPaused = true
		*prepared.PlayState.IsMuted = true
		*prepared.PlayState.VolumeLevel = 99
		*prepared.PlayState.AudioStreamIndex = 2
		*prepared.PlayState.SubtitleStreamIndex = 3
		*prepared.PlayState.MediaSourceID = "changed"
		*prepared.PlayState.PlayMethod = "changed"
		*prepared.PlayState.PlaybackRate = 2
		*prepared.PlayState.RepeatMode = "changed"
		*prepared.PlayState.Shuffle = true
		*prepared.PlayState.SubtitleOffset = 4
		*prepared.Played = true
		*prepared.PlayedPercentage = 90
		if !reflect.DeepEqual(input, before) {
			t.Fatalf("prepared output aliases caller state:\n got  %#v\n want %#v", input, before)
		}
	})

	t.Run("rejection leaves caller unchanged", func(t *testing.T) {
		input := mutablePrepareCommand(now)
		input.Policy = PlaybackResumePolicy{MinPct: 80, MaxPct: 10, MinDurationSeconds: 1}
		before := clonePlaybackReportCommand(input)

		_, err := PreparePlaybackReportCommand(input)
		if err == nil || !errors.Is(err, ErrBadRequest) {
			t.Fatalf("error = %v", err)
		}
		if !reflect.DeepEqual(input, before) {
			t.Fatalf("input mutated during rejected prepare:\n got  %#v\n want %#v", input, before)
		}
	})
}

func TestPreparePlaybackResumePolicy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	base := PlaybackReportCommand{
		GatewayTokenHash: "h1",
		Kind:             PlaybackReportStopped,
		ReceivedAt:       now,
		ItemID:           "item-1",
	}

	t.Run("zero min duration defaults", func(t *testing.T) {
		cmd := base
		cmd.Policy = PlaybackResumePolicy{MinPct: 5, MaxPct: 90, MinDurationSeconds: 0}
		got, err := PreparePlaybackReportCommand(cmd)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if got.Policy.MinDurationSeconds != defaultMinResumeDurationSeconds {
			t.Fatalf("MinDurationSeconds = %v, want default %v", got.Policy.MinDurationSeconds, defaultMinResumeDurationSeconds)
		}
	})

	t.Run("explicit thresholds kept", func(t *testing.T) {
		cmd := base
		cmd.Policy = PlaybackResumePolicy{MinPct: 10, MaxPct: 80, MinDurationSeconds: 60}
		got, err := PreparePlaybackReportCommand(cmd)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if got.Policy.MinPct != 10 || got.Policy.MaxPct != 80 || got.Policy.MinDurationSeconds != 60 {
			t.Fatalf("policy = %#v", got.Policy)
		}
	})

	t.Run("min exceeds max rejected", func(t *testing.T) {
		cmd := base
		cmd.Policy = PlaybackResumePolicy{MinPct: 50, MaxPct: 40, MinDurationSeconds: 1}
		_, err := PreparePlaybackReportCommand(cmd)
		if err == nil || !strings.Contains(err.Error(), "min_pct") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("nonfinite policy rejected", func(t *testing.T) {
		cmd := base
		cmd.Policy = PlaybackResumePolicy{MinPct: math.NaN(), MaxPct: 90, MinDurationSeconds: 1}
		_, err := PreparePlaybackReportCommand(cmd)
		if err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("configured policy changes completion", func(t *testing.T) {
		session := Session{GatewayTokenHash: "h1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "syn"}
		// 100s runtime, 50% position: with MinDuration=300 completes as short content;
		// with MinDuration=1 and MaxPct=99 does not complete.
		runtime := int64(100 * embyTicksPerSecond)
		pos := int64(50 * embyTicksPerSecond)

		completePlan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: now, ItemID: "item-1",
				RunTimeTicks: runtime,
				PlayState:    PlaybackPlayState{PositionTicks: &pos},
				Policy:       PlaybackResumePolicy{MinPct: 5, MaxPct: 90, MinDurationSeconds: 300},
			},
			Session: session,
		})
		if err != nil {
			t.Fatalf("reduce complete: %v", err)
		}
		if completePlan.Durable == nil || !completePlan.Durable.Played {
			t.Fatalf("want short-content completion: %#v", completePlan.Durable)
		}

		resumePlan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: now, ItemID: "item-1",
				RunTimeTicks: runtime,
				PlayState:    PlaybackPlayState{PositionTicks: &pos},
				Policy:       PlaybackResumePolicy{MinPct: 5, MaxPct: 99, MinDurationSeconds: 1},
			},
			Session: session,
		})
		if err != nil {
			t.Fatalf("reduce resume: %v", err)
		}
		if resumePlan.Durable == nil || resumePlan.Durable.Played || resumePlan.Durable.PlaybackPositionTicks != pos {
			t.Fatalf("want resume preserved: %#v", resumePlan.Durable)
		}
	})
}

func TestValidatePlaybackMutationPlanRejects(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	session := Session{GatewayTokenHash: "h1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "syn"}
	cmd := PlaybackReportCommand{
		GatewayTokenHash: "h1",
		Kind:             PlaybackReportProgress,
		ReceivedAt:       now,
		ItemID:           "item-1",
		Policy:           DefaultPlaybackResumePolicy(),
	}

	t.Run("upsert missing current", func(t *testing.T) {
		plan := PlaybackMutationPlan{CurrentAction: PlaybackCurrentUpsert, Result: PlaybackReportResult{Applied: true}}
		if err := ValidatePlaybackMutationPlan(plan, cmd, session); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("reversed dates on current", func(t *testing.T) {
		cp := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-1", ItemSnapshot: PlaybackItemSnapshot{ID: "item-1"},
			StartedAt: now.Add(time.Hour), LastReportedAt: now,
		}
		plan := PlaybackMutationPlan{
			CurrentAction: PlaybackCurrentUpsert,
			Current:       cp,
			Result:        PlaybackReportResult{Applied: true, Current: cp},
		}
		if err := ValidatePlaybackMutationPlan(plan, cmd, session); err == nil || !strings.Contains(err.Error(), "started") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("preserve validates corrupt result current", func(t *testing.T) {
		cp := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-active", ItemSnapshot: PlaybackItemSnapshot{ID: "item-active"},
			PlayState: PlaybackPlayState{PositionTicks: int64Ptr(-1)},
			StartedAt: now, LastReportedAt: now,
		}
		plan := PlaybackMutationPlan{
			CurrentAction: PlaybackCurrentPreserve,
			Result:        PlaybackReportResult{Applied: true, Current: cp},
		}
		if err := ValidatePlaybackMutationPlan(plan, cmd, session); err == nil || !strings.Contains(err.Error(), "position_ticks") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("write durable without pointer", func(t *testing.T) {
		plan := PlaybackMutationPlan{WriteDurable: true}
		if err := ValidatePlaybackMutationPlan(plan, cmd, session); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("event user mismatch", func(t *testing.T) {
		plan := PlaybackMutationPlan{
			Event: &PlaybackEvent{
				GatewayUserID: "other", SyntheticUserID: "syn", ItemID: "item-1",
				Event: "progress", CreatedAt: now,
			},
		}
		if err := ValidatePlaybackMutationPlan(plan, cmd, session); err == nil || !strings.Contains(err.Error(), "gateway user") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("ping with event", func(t *testing.T) {
		ping := cmd
		ping.Kind = PlaybackReportPing
		plan := PlaybackMutationPlan{
			Event: &PlaybackEvent{
				GatewayUserID: "u1", SyntheticUserID: "syn", ItemID: "item-1",
				Event: "playing", CreatedAt: now,
			},
		}
		if err := ValidatePlaybackMutationPlan(plan, cmd, session); err == nil {
			// use ping kind
		}
		if err := ValidatePlaybackMutationPlan(plan, ping, session); err == nil || !strings.Contains(err.Error(), "ping") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("valid plan passes", func(t *testing.T) {
		cp := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-1", ItemSnapshot: PlaybackItemSnapshot{ID: "item-1"},
			StartedAt: now, LastReportedAt: now,
		}
		dur := &PlaybackState{
			GatewayUserID: "u1", SyntheticUserID: "syn", ItemID: "item-1",
			UpdatedAt: now, PlaybackPositionTicks: 10,
		}
		ev := &PlaybackEvent{
			GatewayUserID: "u1", SyntheticUserID: "syn", ItemID: "item-1",
			Event: "progress", CreatedAt: now, PositionTicks: 10,
		}
		plan := PlaybackMutationPlan{
			CurrentAction: PlaybackCurrentUpsert,
			Current:       cp,
			WriteDurable:  true,
			Durable:       dur,
			Event:         ev,
			Result: PlaybackReportResult{
				Applied: true, GatewayUserID: "u1", SyntheticUserID: "syn",
				ItemID: "item-1", Current: cp, Durable: dur,
			},
		}
		if err := ValidatePlaybackMutationPlan(plan, cmd, session); err != nil {
			t.Fatalf("valid plan: %v", err)
		}
	})
}

func TestMemoryStoreWhitespaceItemIDPreservesDurableFields(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateSession(ctx, Session{
		GatewayTokenHash: "h1", GatewayUserID: "u1", SyntheticUserID: "syn",
		CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour), LastActivityAt: now,
	}); err != nil {
		t.Fatalf("session: %v", err)
	}
	likes := true
	if err := store.SavePlaybackState(ctx, PlaybackState{
		GatewayUserID: "u1", SyntheticUserID: "syn", ItemID: "item-1",
		IsFavorite: true, Played: true, PlayCount: 4, Likes: &likes,
		PlaybackPositionTicks: 100, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	pos := int64(250)
	res, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1",
		Kind:             PlaybackReportProgress,
		ReceivedAt:       now.Add(time.Minute),
		ItemID:           "  item-1  ",
		PlayState:        PlaybackPlayState{PositionTicks: &pos},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Durable == nil || res.Durable.ItemID != "item-1" || res.Durable.PlaybackPositionTicks != 250 {
		t.Fatalf("durable: %#v", res.Durable)
	}
	if !res.Durable.IsFavorite || !res.Durable.Played || res.Durable.PlayCount != 4 || res.Durable.Likes == nil || !*res.Durable.Likes {
		t.Fatalf("lost durable fields: %#v", res.Durable)
	}
}

func TestMemoryStorePrepareAndPlanFailureNoMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	t.Run("snapshot conflict before mutation", func(t *testing.T) {
		store := NewMemoryStore()
		// Live session with missing profile hole (empty PublicID).
		store.Sessions["h1"] = &Session{
			GatewayTokenHash: "h1", GatewayUserID: "u1", SyntheticUserID: "syn",
			CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
			// PublicID empty, LastActivityAt zero — hole that repair would fill.
		}
		eventsBefore := len(store.PlaybackEvents)
		_, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportProgress,
			ReceivedAt:       now.Add(time.Minute),
			ItemID:           "item-a",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-b"},
		})
		if err == nil || !errors.Is(err, ErrBadRequest) {
			t.Fatalf("err = %v", err)
		}
		if len(store.PlaybackEvents) != eventsBefore {
			t.Fatal("events mutated")
		}
		if len(store.PlaybackStates) != 0 {
			t.Fatal("durable mutated")
		}
		if len(store.CurrentPlaybacks) != 0 {
			t.Fatal("current mutated")
		}
		live := store.Sessions["h1"]
		if live.PublicID != "" {
			t.Fatalf("session hole repaired on prepare failure: %#v", live)
		}
		if !live.LastActivityAt.IsZero() {
			t.Fatalf("activity filled on prepare failure: %#v", live)
		}
	})

	t.Run("invalid policy no session repair", func(t *testing.T) {
		store := NewMemoryStore()
		store.Sessions["h1"] = &Session{
			GatewayTokenHash: "h1", GatewayUserID: "u1", SyntheticUserID: "syn",
			CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
		}
		_, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportStopped,
			ReceivedAt:       now,
			ItemID:           "item-1",
			Policy:           PlaybackResumePolicy{MinPct: 80, MaxPct: 10, MinDurationSeconds: 1},
		})
		if err == nil {
			t.Fatal("want policy error")
		}
		if store.Sessions["h1"].PublicID != "" {
			t.Fatal("session repaired on policy failure")
		}
	})

	t.Run("success repairs hole only after validation", func(t *testing.T) {
		store := NewMemoryStore()
		store.Sessions["h1"] = &Session{
			GatewayTokenHash: "h1", GatewayUserID: "u1", SyntheticUserID: "syn",
			CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
		}
		_, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportPlaying,
			ReceivedAt:       now.Add(time.Minute),
			ItemID:           "item-1",
		})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		live := store.Sessions["h1"]
		if live.PublicID == "" {
			t.Fatal("expected hole repair on success")
		}
		if live.LastActivityAt.IsZero() {
			t.Fatal("expected activity on success")
		}
	})
}

func mutablePrepareCommand(now time.Time) PlaybackReportCommand {
	position := int64(1)
	canSeek := false
	paused := false
	muted := false
	volume := 10
	audio := -1
	subtitle := -1
	mediaSource := "  media-1  "
	playMethod := "  DirectPlay  "
	playbackRate := 1.0
	repeatMode := "  RepeatNone  "
	shuffle := false
	subtitleOffset := -0.5
	played := false
	playedPercentage := 12.5
	return PlaybackReportCommand{
		GatewayTokenHash: "  hash-1  ",
		Kind:             PlaybackReportProgress,
		ReceivedAt:       now,
		RemoteIP:         "  1.2.3.4  ",
		ItemID:           "  item-1  ",
		PlaySessionID:    "  play-1  ",
		ItemSnapshot: PlaybackItemSnapshot{
			ID:        "  item-1  ",
			Name:      "Movie",
			ImageTags: map[string]string{"Primary": "tag-1"},
		},
		PlayState: PlaybackPlayState{
			PositionTicks:       &position,
			CanSeek:             &canSeek,
			IsPaused:            &paused,
			IsMuted:             &muted,
			VolumeLevel:         &volume,
			AudioStreamIndex:    &audio,
			SubtitleStreamIndex: &subtitle,
			MediaSourceID:       &mediaSource,
			PlayMethod:          &playMethod,
			PlaybackRate:        &playbackRate,
			RepeatMode:          &repeatMode,
			Shuffle:             &shuffle,
			SubtitleOffset:      &subtitleOffset,
		},
		Played:           &played,
		PlayedPercentage: &playedPercentage,
		EventName:        "  Unpause  ",
		Policy: PlaybackResumePolicy{
			MinPct:             5,
			MaxPct:             95,
			MinDurationSeconds: 1,
		},
	}
}

func assertPreparedCommandIndependent(t *testing.T, input, prepared PlaybackReportCommand) {
	t.Helper()
	if reflect.ValueOf(input.ItemSnapshot.ImageTags).Pointer() == reflect.ValueOf(prepared.ItemSnapshot.ImageTags).Pointer() {
		t.Fatal("prepared ImageTags aliases input")
	}
	pointers := []struct {
		name string
		in   any
		out  any
	}{
		{"PositionTicks", input.PlayState.PositionTicks, prepared.PlayState.PositionTicks},
		{"CanSeek", input.PlayState.CanSeek, prepared.PlayState.CanSeek},
		{"IsPaused", input.PlayState.IsPaused, prepared.PlayState.IsPaused},
		{"IsMuted", input.PlayState.IsMuted, prepared.PlayState.IsMuted},
		{"VolumeLevel", input.PlayState.VolumeLevel, prepared.PlayState.VolumeLevel},
		{"AudioStreamIndex", input.PlayState.AudioStreamIndex, prepared.PlayState.AudioStreamIndex},
		{"SubtitleStreamIndex", input.PlayState.SubtitleStreamIndex, prepared.PlayState.SubtitleStreamIndex},
		{"MediaSourceID", input.PlayState.MediaSourceID, prepared.PlayState.MediaSourceID},
		{"PlayMethod", input.PlayState.PlayMethod, prepared.PlayState.PlayMethod},
		{"PlaybackRate", input.PlayState.PlaybackRate, prepared.PlayState.PlaybackRate},
		{"RepeatMode", input.PlayState.RepeatMode, prepared.PlayState.RepeatMode},
		{"Shuffle", input.PlayState.Shuffle, prepared.PlayState.Shuffle},
		{"SubtitleOffset", input.PlayState.SubtitleOffset, prepared.PlayState.SubtitleOffset},
		{"Played", input.Played, prepared.Played},
		{"PlayedPercentage", input.PlayedPercentage, prepared.PlayedPercentage},
	}
	for _, pointer := range pointers {
		in := reflect.ValueOf(pointer.in)
		out := reflect.ValueOf(pointer.out)
		if in.IsNil() || out.IsNil() {
			t.Fatalf("%s test pointer unexpectedly nil", pointer.name)
		}
		if in.Pointer() == out.Pointer() {
			t.Fatalf("prepared %s aliases input", pointer.name)
		}
	}
}
