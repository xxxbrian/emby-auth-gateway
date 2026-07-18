package gateway

import (
	"errors"
	"testing"
	"time"
)

func TestReducePlaybackReportValidation(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub-1", GatewayUserID: "u1", SyntheticUserID: "syn-1"}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	t.Run("missing token", func(t *testing.T) {
		_, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{Kind: PlaybackReportPlaying, ReceivedAt: now, ItemID: "i1"},
			Session: session,
		})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("err = %v, want ErrBadRequest", err)
		}
	})
	t.Run("missing received_at", func(t *testing.T) {
		_, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{GatewayTokenHash: "h1", Kind: PlaybackReportPlaying, ItemID: "i1"},
			Session: session,
		})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("err = %v, want ErrBadRequest", err)
		}
	})
	t.Run("invalid kind", func(t *testing.T) {
		_, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{GatewayTokenHash: "h1", Kind: "Nope", ReceivedAt: now, ItemID: "i1"},
			Session: session,
		})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("err = %v, want ErrBadRequest", err)
		}
	})
	t.Run("session hash mismatch", func(t *testing.T) {
		_, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{GatewayTokenHash: "other", Kind: PlaybackReportPlaying, ReceivedAt: now, ItemID: "i1"},
			Session: session,
		})
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("err = %v, want ErrBadRequest", err)
		}
	})
	t.Run("missing item is no-op not error", func(t *testing.T) {
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: now},
			Session: session,
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if plan.Result.Applied || plan.WriteDurable || plan.Event != nil || plan.ActivityAt != nil {
			t.Fatalf("want pure no-op, got %#v", plan)
		}
		if plan.Result.PublicSessionID != "pub-1" || plan.Result.GatewayUserID != "u1" {
			t.Fatalf("identity not filled: %#v", plan.Result)
		}
	})
}

func TestReducePlaybackReportConfirmedMetadataRepairsOrphan(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub-1", GatewayUserID: "u1", SyntheticUserID: "syn-1"}
	orphanedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	lastSeenAt := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	receivedAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	likes := true
	existing := &PlaybackState{
		GatewayUserID:         "u1",
		SyntheticUserID:       "syn-1",
		ItemID:                "item-1",
		ItemName:              "Old",
		ItemType:              "Episode",
		PlaybackPositionTicks: 444,
		Played:                true,
		PlayedPercentage:      floatPtr(80),
		PlayCount:             3,
		IsFavorite:            true,
		Likes:                 &likes,
		Fingerprint:           "type=Movie",
		OrphanedAt:            &orphanedAt,
		LastSeenAt:            &lastSeenAt,
	}

	plan, err := ReducePlaybackReport(PlaybackReduceInput{
		Command: PlaybackReportCommand{
			GatewayTokenHash:  "h1",
			Kind:              PlaybackReportPlaying,
			ReceivedAt:        receivedAt,
			ItemID:            "item-1",
			ItemSnapshot:      PlaybackItemSnapshot{ID: "item-1", Name: "Confirmed", Type: "Episode", SeriesID: "series-1", SeriesName: "Series"},
			MetadataConfirmed: true,
		},
		Session: session,
		Durable: existing,
	})
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	state := plan.Durable
	if state == nil || state.OrphanedAt != nil || state.LastSeenAt == nil || !state.LastSeenAt.Equal(receivedAt) {
		t.Fatalf("confirmed repair state = %#v", state)
	}
	if state.ItemName != "Confirmed" || state.ItemType != "Episode" || state.SeriesID != "series-1" || state.Fingerprint != "type=Episode|seriesid=series-1" {
		t.Fatalf("confirmed metadata not refreshed: %#v", state)
	}
	if !state.IsFavorite || !state.Played || state.PlaybackPositionTicks != 444 || state.PlayCount != 3 || state.Likes == nil || !*state.Likes {
		t.Fatalf("personal fields clobbered: %#v", state)
	}

	withoutConfirmation := *existing
	plan, err = ReducePlaybackReport(PlaybackReduceInput{
		Command: PlaybackReportCommand{
			GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: receivedAt,
			ItemID: "item-1", ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Name: "Client"},
		},
		Session: session,
		Durable: &withoutConfirmation,
	})
	if err != nil {
		t.Fatalf("reduce unconfirmed: %v", err)
	}
	if plan.Durable == nil || plan.Durable.OrphanedAt == nil || !plan.Durable.OrphanedAt.Equal(orphanedAt) || plan.Durable.LastSeenAt == nil || !plan.Durable.LastSeenAt.Equal(lastSeenAt) {
		t.Fatalf("unconfirmed metadata changed orphan markers: %#v", plan.Durable)
	}

	newerSeen := receivedAt.Add(time.Hour)
	newerExisting := *existing
	newerExisting.LastSeenAt = &newerSeen
	plan, err = ReducePlaybackReport(PlaybackReduceInput{
		Command: PlaybackReportCommand{
			GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: receivedAt,
			ItemID: "item-1", ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Type: "Episode"}, MetadataConfirmed: true,
		},
		Session: session,
		Durable: &newerExisting,
	})
	if err != nil {
		t.Fatalf("reduce monotonic last seen: %v", err)
	}
	if plan.Durable.LastSeenAt == nil || !plan.Durable.LastSeenAt.Equal(newerSeen) {
		t.Fatalf("LastSeenAt moved backward: %#v", plan.Durable.LastSeenAt)
	}
}

func TestReducePlaybackReportConfirmedMetadataClearsFingerprintAndRuntime(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub-1", GatewayUserID: "u1", SyntheticUserID: "syn-1"}
	startedAt := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	receivedAt := startedAt.Add(time.Hour)
	orphanedAt := startedAt.Add(-24 * time.Hour)
	lastSeenAt := startedAt.Add(-time.Hour)
	likes := true

	t.Run("confirmed omission clears fingerprint and both runtimes", func(t *testing.T) {
		existing := &PlaybackState{
			GatewayUserID: "u1", SyntheticUserID: "syn-1", ItemID: "item-1",
			Fingerprint: "type=Rogue|seriesid=rogue", RunTimeTicks: 9_000,
			PlaybackPositionTicks: 444, Played: true, PlayCount: 3, IsFavorite: true, Likes: &likes,
			OrphanedAt: &orphanedAt, LastSeenAt: &lastSeenAt,
		}
		current := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-1", PlaySessionID: "ps-1",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Type: "Rogue", SeriesID: "rogue", RunTimeTicks: 9_000},
			RunTimeTicks: 9_000, StartedAt: startedAt, LastReportedAt: startedAt,
		}
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportPlaying, ReceivedAt: receivedAt,
				ItemID: "item-1", PlaySessionID: "ps-1", ItemSnapshot: PlaybackItemSnapshot{ID: "item-1"}, MetadataConfirmed: true,
			},
			Session: session,
			Current: current,
			Durable: existing,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.Durable == nil || plan.Durable.Fingerprint != "" || plan.Durable.RunTimeTicks != 0 || plan.Durable.OrphanedAt != nil || plan.Durable.LastSeenAt == nil || !plan.Durable.LastSeenAt.Equal(receivedAt) {
			t.Fatalf("confirmed durable omission = %#v", plan.Durable)
		}
		if !plan.Durable.IsFavorite || !plan.Durable.Played || plan.Durable.PlaybackPositionTicks != 444 || plan.Durable.PlayCount != 3 || plan.Durable.Likes == nil || !*plan.Durable.Likes {
			t.Fatalf("personal fields clobbered: %#v", plan.Durable)
		}
		if plan.Current == nil || plan.Current.RunTimeTicks != 0 || plan.Current.ItemSnapshot.RunTimeTicks != 0 || plan.Current.ItemSnapshot.Type != "" || plan.Current.ItemSnapshot.SeriesID != "" {
			t.Fatalf("confirmed current omission = %#v", plan.Current)
		}
	})

	t.Run("confirmed nonempty metadata replaces prior values", func(t *testing.T) {
		current := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-1",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Type: "Old", SeriesID: "old", RunTimeTicks: 9_000},
			RunTimeTicks: 9_000, StartedAt: startedAt, LastReportedAt: startedAt,
		}
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportPlaying, ReceivedAt: receivedAt,
				ItemID: "item-1", ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Type: "Episode", SeriesID: "series-1", RunTimeTicks: 12_345},
				RunTimeTicks: 12_345, MetadataConfirmed: true,
			},
			Session: session,
			Current: current,
			Durable: &PlaybackState{GatewayUserID: "u1", SyntheticUserID: "syn-1", ItemID: "item-1", Fingerprint: "type=Old", RunTimeTicks: 9_000},
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.Durable == nil || plan.Durable.Fingerprint != "type=Episode|seriesid=series-1" || plan.Durable.RunTimeTicks != 12_345 {
			t.Fatalf("confirmed durable replacement = %#v", plan.Durable)
		}
		if plan.Current == nil || plan.Current.RunTimeTicks != 12_345 || plan.Current.ItemSnapshot.RunTimeTicks != 12_345 {
			t.Fatalf("confirmed current replacement = %#v", plan.Current)
		}
	})

	t.Run("unconfirmed partial preserves prior runtime and fingerprint", func(t *testing.T) {
		current := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-1", PlaySessionID: "ps-1",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Type: "Episode", RunTimeTicks: 9_000},
			RunTimeTicks: 9_000, StartedAt: startedAt, LastReportedAt: startedAt,
		}
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: receivedAt,
				ItemID: "item-1", PlaySessionID: "ps-1", ItemSnapshot: PlaybackItemSnapshot{ID: "item-1"},
			},
			Session: session,
			Current: current,
			Durable: &PlaybackState{GatewayUserID: "u1", SyntheticUserID: "syn-1", ItemID: "item-1", Fingerprint: "type=Episode", RunTimeTicks: 9_000},
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.Durable == nil || plan.Durable.Fingerprint != "type=Episode" || plan.Durable.RunTimeTicks != 9_000 {
			t.Fatalf("unconfirmed durable fallback = %#v", plan.Durable)
		}
		if plan.Current == nil || plan.Current.RunTimeTicks != 9_000 || plan.Current.ItemSnapshot.RunTimeTicks != 9_000 {
			t.Fatalf("unconfirmed current fallback = %#v", plan.Current)
		}
	})
}

func TestReducePlaybackPlayingReplacesAndPreservesStartedAt(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub-1", GatewayUserID: "u1", SyntheticUserID: "syn-1"}
	t0 := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
	pos := int64(100)

	current := &CurrentPlayback{
		GatewayTokenHash: "h1",
		ItemID:           "item-a",
		PlaySessionID:    "ps-1",
		ItemSnapshot:     PlaybackItemSnapshot{ID: "item-a", Name: "Rich", Type: "Movie"},
		PlayState:        PlaybackPlayState{CanSeek: boolPtr(true)},
		RunTimeTicks:     9_000_000,
		StartedAt:        t0,
		LastReportedAt:   t0,
	}
	plan, err := ReducePlaybackReport(PlaybackReduceInput{
		Command: PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportPlaying,
			ReceivedAt:       t1,
			ItemID:           "item-a",
			PlaySessionID:    "ps-1",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-a", Name: "Patch"},
			PlayState:        PlaybackPlayState{PositionTicks: &pos},
			RunTimeTicks:     10_000_000,
		},
		Session: session,
		Current: current,
	})
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if !plan.Result.Applied || plan.CurrentAction != PlaybackCurrentUpsert || plan.Current == nil {
		t.Fatalf("plan = %#v", plan)
	}
	if !plan.Current.StartedAt.Equal(t0) {
		t.Fatalf("StartedAt = %v, want preserved %v", plan.Current.StartedAt, t0)
	}
	if plan.Current.ItemSnapshot.Name != "Patch" || plan.Current.ItemSnapshot.Type != "Movie" {
		t.Fatalf("snapshot merge: %#v", plan.Current.ItemSnapshot)
	}
	if plan.Current.PlayState.PositionTicks == nil || *plan.Current.PlayState.PositionTicks != 100 {
		t.Fatalf("play state: %#v", plan.Current.PlayState)
	}
	if plan.Current.PlayState.CanSeek == nil || !*plan.Current.PlayState.CanSeek {
		t.Fatalf("canseek not preserved: %#v", plan.Current.PlayState)
	}
	if plan.Event == nil || plan.Event.Event != "playing" {
		t.Fatalf("event = %#v", plan.Event)
	}
	if !plan.WriteDurable || plan.Durable == nil || plan.Durable.PlaybackPositionTicks != 100 {
		t.Fatalf("durable = %#v", plan.Durable)
	}
}

func TestReducePlaybackPlayingConflictStillReplacesButResetsStartedAt(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "syn"}
	t0 := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
	current := &CurrentPlayback{
		GatewayTokenHash: "h1",
		ItemID:           "item-a",
		PlaySessionID:    "ps-old",
		StartedAt:        t0,
		LastReportedAt:   t0,
		ItemSnapshot:     PlaybackItemSnapshot{ID: "item-a", Name: "Old"},
	}
	plan, err := ReducePlaybackReport(PlaybackReduceInput{
		Command: PlaybackReportCommand{
			GatewayTokenHash: "h1",
			Kind:             PlaybackReportPlaying,
			ReceivedAt:       t1,
			ItemID:           "item-a",
			PlaySessionID:    "ps-new",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-a"},
		},
		Session: session,
		Current: current,
	})
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if plan.Current == nil || !plan.Current.StartedAt.Equal(t1) {
		t.Fatalf("conflict should not preserve StartedAt: %#v", plan.Current)
	}
	if plan.Current.PlaySessionID != "ps-new" {
		t.Fatalf("PlaySessionID = %q", plan.Current.PlaySessionID)
	}
}

func TestReducePlaybackProgressCreateMatchAndMismatch(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "syn"}
	t0 := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
	pos := int64(500)

	t.Run("create when absent", func(t *testing.T) {
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: t1,
				ItemID: "item-1", PlayState: PlaybackPlayState{PositionTicks: &pos},
			},
			Session: session,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.CurrentAction != PlaybackCurrentUpsert || plan.Current == nil || plan.Current.ItemID != "item-1" {
			t.Fatalf("current = %#v", plan.Current)
		}
		if plan.Current.ItemSnapshot.ID != "item-1" {
			t.Fatalf("snapshot must include Id: %#v", plan.Current.ItemSnapshot)
		}
		if plan.Event == nil || plan.Event.Event != "progress" {
			t.Fatalf("event = %#v", plan.Event)
		}
	})

	t.Run("match merges", func(t *testing.T) {
		current := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-1", PlaySessionID: "ps-1",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Name: "Rich", Type: "Episode", SeriesID: "s1"},
			StartedAt:    t0, LastReportedAt: t0, RunTimeTicks: 20_000_000,
		}
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: t1,
				ItemID: "item-1", PlaySessionID: "ps-1",
				ItemSnapshot: PlaybackItemSnapshot{ID: "item-1", Name: "Patched"},
				PlayState:    PlaybackPlayState{PositionTicks: &pos},
			},
			Session: session,
			Current: current,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.CurrentAction != PlaybackCurrentUpsert {
			t.Fatalf("action = %v", plan.CurrentAction)
		}
		if plan.Current.ItemSnapshot.Name != "Patched" || plan.Current.ItemSnapshot.Type != "Episode" {
			t.Fatalf("snapshot = %#v", plan.Current.ItemSnapshot)
		}
		if plan.Current.RunTimeTicks != 20_000_000 {
			t.Fatalf("runtime not preserved: %d", plan.Current.RunTimeTicks)
		}
		if !plan.Current.StartedAt.Equal(t0) {
			t.Fatalf("StartedAt = %v", plan.Current.StartedAt)
		}
	})

	t.Run("item mismatch preserves current and updates durable", func(t *testing.T) {
		current := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-active", PlaySessionID: "ps-1",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-active", Name: "Active"},
			StartedAt:    t0, LastReportedAt: t0,
		}
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: t1,
				ItemID: "item-other", PlayState: PlaybackPlayState{PositionTicks: &pos},
				ItemSnapshot: PlaybackItemSnapshot{ID: "item-other", Name: "Other"},
			},
			Session: session,
			Current: current,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.CurrentAction != PlaybackCurrentPreserve {
			t.Fatalf("action = %v", plan.CurrentAction)
		}
		if plan.Result.Current == nil || plan.Result.Current.ItemID != "item-active" {
			t.Fatalf("preserved current = %#v", plan.Result.Current)
		}
		if !plan.WriteDurable || plan.Durable == nil || plan.Durable.ItemID != "item-other" {
			t.Fatalf("durable = %#v", plan.Durable)
		}
		if plan.Durable.PlaybackPositionTicks != 500 {
			t.Fatalf("durable position = %d", plan.Durable.PlaybackPositionTicks)
		}
		if plan.Event == nil || plan.Event.ItemID != "item-other" {
			t.Fatalf("event = %#v", plan.Event)
		}
	})

	t.Run("play session conflict preserves current", func(t *testing.T) {
		current := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-1", PlaySessionID: "ps-a",
			StartedAt: t0, LastReportedAt: t0, ItemSnapshot: PlaybackItemSnapshot{ID: "item-1"},
		}
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: t1,
				ItemID: "item-1", PlaySessionID: "ps-b",
				PlayState: PlaybackPlayState{PositionTicks: &pos},
			},
			Session: session,
			Current: current,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.CurrentAction != PlaybackCurrentPreserve {
			t.Fatalf("action = %v", plan.CurrentAction)
		}
		if !plan.WriteDurable {
			t.Fatal("want durable update on conflict")
		}
	})
}

func TestReducePlaybackPauseUnpauseEventName(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "syn"}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	plan, err := ReducePlaybackReport(PlaybackReduceInput{
		Command: PlaybackReportCommand{
			GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: now,
			ItemID: "item-1", EventName: "Pause",
		},
		Session: session,
	})
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if plan.Current == nil || plan.Current.PlayState.IsPaused == nil || !*plan.Current.PlayState.IsPaused {
		t.Fatalf("Pause did not set IsPaused: %#v", plan.Current)
	}

	current := plan.Current
	plan2, err := ReducePlaybackReport(PlaybackReduceInput{
		Command: PlaybackReportCommand{
			GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: now.Add(time.Second),
			ItemID: "item-1", EventName: "Unpause",
		},
		Session: session,
		Current: current,
	})
	if err != nil {
		t.Fatalf("reduce2: %v", err)
	}
	if plan2.Current == nil || plan2.Current.PlayState.IsPaused == nil || *plan2.Current.PlayState.IsPaused {
		t.Fatalf("Unpause did not clear IsPaused: %#v", plan2.Current)
	}
}

func TestReducePlaybackPingRules(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "syn"}
	t0 := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
	current := &CurrentPlayback{
		GatewayTokenHash: "h1", ItemID: "item-1", PlaySessionID: "ps-1",
		StartedAt: t0, LastReportedAt: t0, ItemSnapshot: PlaybackItemSnapshot{ID: "item-1"},
	}

	t.Run("no play id touches current", func(t *testing.T) {
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{GatewayTokenHash: "h1", Kind: PlaybackReportPing, ReceivedAt: t1},
			Session: session,
			Current: current,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if !plan.Result.Applied || plan.Event != nil || plan.WriteDurable {
			t.Fatalf("plan = %#v", plan)
		}
		if plan.CurrentAction != PlaybackCurrentUpsert || plan.Current == nil {
			t.Fatalf("current action = %#v", plan)
		}
		if !plan.Current.LastReportedAt.Equal(t1) || !plan.Current.StartedAt.Equal(t0) {
			t.Fatalf("times = %#v", plan.Current)
		}
		if plan.ActivityAt == nil || !plan.ActivityAt.Equal(t1) {
			t.Fatalf("activity = %v", plan.ActivityAt)
		}
	})

	t.Run("matching play id touches", func(t *testing.T) {
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportPing, ReceivedAt: t1,
				PlaySessionID: "ps-1", ItemID: "item-1",
			},
			Session: session,
			Current: current,
		})
		if err != nil || !plan.Result.Applied {
			t.Fatalf("plan=%#v err=%v", plan, err)
		}
	})

	t.Run("mismatched play id is complete no-op", func(t *testing.T) {
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportPing, ReceivedAt: t1,
				PlaySessionID: "ps-other",
			},
			Session: session,
			Current: current,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.Result.Applied || plan.CurrentAction != PlaybackCurrentNone || plan.ActivityAt != nil {
			t.Fatalf("want complete no-op: %#v", plan)
		}
	})

	t.Run("no current is no-op", func(t *testing.T) {
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{GatewayTokenHash: "h1", Kind: PlaybackReportPing, ReceivedAt: t1},
			Session: session,
		})
		if err != nil || plan.Result.Applied {
			t.Fatalf("plan=%#v err=%v", plan, err)
		}
	})
}

func TestReducePlaybackStoppedClearAndDurableCompletion(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "syn"}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	// Runtime must exceed default MinDurationSeconds (300s) so mid-position
	// stops are not auto-completed by the short-content rule.
	runtime := int64(600 * embyTicksPerSecond)
	posHigh := int64(96 * 6 * embyTicksPerSecond) // ~96% of 600s
	posMid := int64(300 * embyTicksPerSecond)     // 50% of 600s

	t.Run("clears matching current and completes by position", func(t *testing.T) {
		current := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-1", PlaySessionID: "ps-1",
			StartedAt: now.Add(-time.Hour), LastReportedAt: now.Add(-time.Minute),
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-1"},
		}
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: now,
				ItemID: "item-1", PlaySessionID: "ps-1", RunTimeTicks: runtime,
				PlayState: PlaybackPlayState{PositionTicks: &posHigh},
			},
			Session: session,
			Current: current,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.CurrentAction != PlaybackCurrentDelete || plan.Result.Current != nil {
			t.Fatalf("want delete current: %#v", plan)
		}
		if plan.Event == nil || plan.Event.Event != "stopped" {
			t.Fatalf("event = %#v", plan.Event)
		}
		if plan.Durable == nil || !plan.Durable.Played || plan.Durable.PlaybackPositionTicks != 0 {
			t.Fatalf("durable complete: %#v", plan.Durable)
		}
		if plan.Durable.PlayCount != 1 || plan.Durable.LastPlayedDate == nil {
			t.Fatalf("completion side effects: %#v", plan.Durable)
		}
	})

	t.Run("mismatched current preserved; durable still finalized", func(t *testing.T) {
		current := &CurrentPlayback{
			GatewayTokenHash: "h1", ItemID: "item-active",
			StartedAt: now.Add(-time.Hour), LastReportedAt: now.Add(-time.Minute),
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-active"},
		}
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: now,
				ItemID: "item-other", RunTimeTicks: runtime,
				PlayState: PlaybackPlayState{PositionTicks: &posMid},
			},
			Session: session,
			Current: current,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.CurrentAction != PlaybackCurrentPreserve || plan.Result.Current.ItemID != "item-active" {
			t.Fatalf("preserve = %#v", plan)
		}
		if plan.Durable == nil || plan.Durable.ItemID != "item-other" || plan.Durable.Played {
			t.Fatalf("durable = %#v", plan.Durable)
		}
		if plan.Durable.PlaybackPositionTicks != posMid {
			t.Fatalf("resume = %d", plan.Durable.PlaybackPositionTicks)
		}
	})

	t.Run("unknown runtime does not complete from percentage", func(t *testing.T) {
		pct := 95.0
		pos := int64(8_000_000)
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: now,
				ItemID: "unknown-1", PlayedPercentage: &pct,
				PlayState: PlaybackPlayState{PositionTicks: &pos},
			},
			Session: session,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.Durable == nil || plan.Durable.Played {
			t.Fatalf("must not complete: %#v", plan.Durable)
		}
		if plan.Durable.PlaybackPositionTicks != pos {
			t.Fatalf("resume not preserved: %#v", plan.Durable)
		}
		if plan.Durable.PlayCount != 0 || plan.Durable.LastPlayedDate != nil {
			t.Fatalf("side effects: %#v", plan.Durable)
		}
	})

	t.Run("explicit Played completes unknown runtime", func(t *testing.T) {
		played := true
		pos := int64(8_000_000)
		plan, err := ReducePlaybackReport(PlaybackReduceInput{
			Command: PlaybackReportCommand{
				GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: now,
				ItemID: "unknown-2", Played: &played,
				PlayState: PlaybackPlayState{PositionTicks: &pos},
			},
			Session: session,
		})
		if err != nil {
			t.Fatalf("reduce: %v", err)
		}
		if plan.Durable == nil || !plan.Durable.Played || plan.Durable.PlaybackPositionTicks != 0 {
			t.Fatalf("explicit Played should complete: %#v", plan.Durable)
		}
		if plan.Durable.PlayCount != 1 {
			t.Fatalf("PlayCount = %d", plan.Durable.PlayCount)
		}
	})
}

func TestReducePlaybackNewSnapshotHasAtLeastId(t *testing.T) {
	t.Parallel()
	session := Session{GatewayTokenHash: "h1", PublicID: "pub", GatewayUserID: "u1", SyntheticUserID: "syn"}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	plan, err := ReducePlaybackReport(PlaybackReduceInput{
		Command: PlaybackReportCommand{
			GatewayTokenHash: "h1", Kind: PlaybackReportPlaying, ReceivedAt: now, ItemID: "only-id",
		},
		Session: session,
	})
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if plan.Current == nil || plan.Current.ItemSnapshot.ID != "only-id" {
		t.Fatalf("snapshot = %#v", plan.Current)
	}
}

func boolPtr(v bool) *bool { return &v }
