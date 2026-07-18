package pbstore

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

const testEmbyTicksPerSecond = int64(10_000_000)

func TestApplyPlaybackReportLifecycleAndList(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-life", "syn-apply-life")
	createListSession(t, store, userID, "hash-life", "dev-life")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	pos := int64(1_000)
	res, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-life",
		Kind:             gateway.PlaybackReportPlaying,
		ReceivedAt:       now.Add(time.Minute),
		ItemID:           "item-1",
		PlaySessionID:    "ps-1",
		ItemSnapshot:     gateway.PlaybackItemSnapshot{ID: "item-1", Name: "Movie", Type: "Movie"},
		PlayState:        gateway.PlaybackPlayState{PositionTicks: &pos},
		RunTimeTicks:     100 * testEmbyTicksPerSecond,
		RemoteIP:         "1.2.3.4",
	})
	if err != nil {
		t.Fatalf("Playing: %v", err)
	}
	if !res.Applied || res.Current == nil || res.Durable == nil {
		t.Fatalf("Playing result: %#v", res)
	}
	assertEventCount(t, app, userID, 1)
	assertEventKind(t, app, userID, "playing")

	pos2 := int64(2_000)
	res2, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-life",
		Kind:             gateway.PlaybackReportProgress,
		ReceivedAt:       now.Add(2 * time.Minute),
		ItemID:           "item-1",
		PlaySessionID:    "ps-1",
		PlayState:        gateway.PlaybackPlayState{PositionTicks: &pos2},
	})
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if res2.Current == nil || !res2.Current.StartedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("StartedAt not preserved: %#v", res2.Current)
	}
	if res2.Durable == nil || res2.Durable.PlaybackPositionTicks != 2_000 {
		t.Fatalf("progress durable: %#v", res2.Durable)
	}

	res3, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-life",
		Kind:             gateway.PlaybackReportProgress,
		ReceivedAt:       now.Add(3 * time.Minute),
		ItemID:           "item-1",
		EventName:        "Pause",
	})
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if res3.Current == nil || res3.Current.PlayState.IsPaused == nil || !*res3.Current.PlayState.IsPaused {
		t.Fatalf("paused: %#v", res3.Current)
	}

	eventsBeforePing := countPlaybackEventsForUser(t, app, userID)
	res4, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-life",
		Kind:             gateway.PlaybackReportPing,
		ReceivedAt:       now.Add(4 * time.Minute),
	})
	if err != nil || !res4.Applied {
		t.Fatalf("Ping: res=%#v err=%v", res4, err)
	}
	if countPlaybackEventsForUser(t, app, userID) != eventsBeforePing {
		t.Fatal("Ping wrote event")
	}
	if res4.Current == nil || !res4.Current.LastReportedAt.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("ping current: %#v", res4.Current)
	}

	posDone := int64(96 * testEmbyTicksPerSecond)
	res5, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-life",
		Kind:             gateway.PlaybackReportStopped,
		ReceivedAt:       now.Add(5 * time.Minute),
		ItemID:           "item-1",
		PlaySessionID:    "ps-1",
		RunTimeTicks:     100 * testEmbyTicksPerSecond,
		PlayState:        gateway.PlaybackPlayState{PositionTicks: &posDone},
	})
	if err != nil {
		t.Fatalf("Stopped: %v", err)
	}
	if res5.Current != nil {
		t.Fatalf("current should be cleared: %#v", res5.Current)
	}
	if res5.Durable == nil || !res5.Durable.Played {
		t.Fatalf("durable not complete: %#v", res5.Durable)
	}

	listed, err := store.ListCurrentPlaybacks(ctx, []string{"hash-life"})
	if err != nil || len(listed) != 0 {
		t.Fatalf("list after stop: %#v err=%v", listed, err)
	}

	// Monotonic activity ends at last successful report.
	found, err := store.FindSessionByTokenHash(ctx, "hash-life")
	if err != nil {
		t.Fatal(err)
	}
	if !found.LastActivityAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("LastActivityAt = %v, want %v", found.LastActivityAt, now.Add(5*time.Minute))
	}

	// Durable persisted via store helper path.
	state, err := store.FindPlaybackState(ctx, userID, "item-1")
	if err != nil || !state.Played {
		t.Fatalf("FindPlaybackState: %#v err=%v", state, err)
	}
}

func TestApplyPlaybackReportDelayedMismatchAndIsolation(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	u1 := createGatewayUser(t, app, "apply-u1", "syn-apply-u1")
	u2 := createGatewayUser(t, app, "apply-u2", "syn-apply-u2")
	createListSession(t, store, u1, "dev-a", "d-a")
	createListSession(t, store, u1, "dev-b", "d-b")
	createListSession(t, store, u2, "dev-c", "d-c")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	pos := int64(100)

	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "dev-a", Kind: gateway.PlaybackReportPlaying, ReceivedAt: now,
		ItemID: "shared", PlaySessionID: "ps-a", PlayState: gateway.PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "dev-b", Kind: gateway.PlaybackReportPlaying, ReceivedAt: now.Add(time.Second),
		ItemID: "shared", PlaySessionID: "ps-b", PlayState: gateway.PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "dev-c", Kind: gateway.PlaybackReportPlaying, ReceivedAt: now.Add(2 * time.Second),
		ItemID: "other", PlayState: gateway.PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatal(err)
	}

	posLate := int64(999)
	res, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "dev-a", Kind: gateway.PlaybackReportProgress, ReceivedAt: now.Add(time.Minute),
		ItemID: "late-item", PlayState: gateway.PlaybackPlayState{PositionTicks: &posLate},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Current == nil || res.Current.ItemID != "shared" {
		t.Fatalf("current replaced by mismatch: %#v", res.Current)
	}
	if res.Durable == nil || res.Durable.ItemID != "late-item" || res.Durable.PlaybackPositionTicks != 999 {
		t.Fatalf("late durable: %#v", res.Durable)
	}

	// Mismatched Stop preserves current.
	resStop, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "dev-a", Kind: gateway.PlaybackReportStopped, ReceivedAt: now.Add(2 * time.Minute),
		ItemID: "late-item",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resStop.Current == nil || resStop.Current.ItemID != "shared" {
		t.Fatalf("stop mismatch current: %#v", resStop.Current)
	}

	// Mismatched Ping no-ops current.
	resPing, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "dev-a", Kind: gateway.PlaybackReportPing, ReceivedAt: now.Add(3 * time.Minute),
		PlaySessionID: "ps-other",
	})
	if err != nil || resPing.Applied {
		t.Fatalf("ping mismatch: %#v err=%v", resPing, err)
	}

	listed, err := store.ListCurrentPlaybacks(ctx, []string{"dev-a", "dev-b", "dev-c"})
	if err != nil || len(listed) != 3 {
		t.Fatalf("listed = %#v err=%v", listed, err)
	}
	if listed["dev-a"].ItemID != "shared" || listed["dev-b"].PlaySessionID != "ps-b" || listed["dev-c"].ItemID != "other" {
		t.Fatalf("isolation: %#v", listed)
	}
}

func TestApplyPlaybackReportNoOpMissingItemAndUnknownRuntime(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-noop", "syn-apply-noop")
	createListSession(t, store, userID, "h1", "dev")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	pos := int64(100)
	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: gateway.PlaybackReportPlaying, ReceivedAt: now,
		ItemID: "item-1", PlaySessionID: "ps-1", PlayState: gateway.PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatal(err)
	}
	eventsBefore := countPlaybackEventsForUser(t, app, userID)

	res, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: gateway.PlaybackReportProgress, ReceivedAt: now.Add(time.Minute),
	})
	if err != nil || res.Applied {
		t.Fatalf("missing item: %#v err=%v", res, err)
	}
	if countPlaybackEventsForUser(t, app, userID) != eventsBefore {
		t.Fatal("missing item wrote event")
	}

	pct := 95.0
	posHigh := int64(8_000_000)
	res2, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: gateway.PlaybackReportStopped, ReceivedAt: now.Add(2 * time.Minute),
		ItemID: "unknown-rt", PlayedPercentage: &pct,
		PlayState: gateway.PlaybackPlayState{PositionTicks: &posHigh},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Current == nil || res2.Current.ItemID != "item-1" {
		t.Fatalf("current = %#v", res2.Current)
	}
	if res2.Durable == nil || res2.Durable.Played || res2.Durable.PlaybackPositionTicks != posHigh {
		t.Fatalf("unknown runtime durable: %#v", res2.Durable)
	}

	played := true
	res3, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: gateway.PlaybackReportStopped, ReceivedAt: now.Add(3 * time.Minute),
		ItemID: "unknown-rt", Played: &played,
		PlayState: gateway.PlaybackPlayState{PositionTicks: &posHigh},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res3.Durable == nil || !res3.Durable.Played || res3.Durable.PlayCount != 1 {
		t.Fatalf("explicit complete: %#v", res3.Durable)
	}
}

func TestApplyPlaybackReportStoppedCanonicalItemLookupPreservesDurableUserData(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-canonical", "syn-apply-canonical")
	createListSession(t, store, userID, "hash-canonical", "dev")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	likes := true

	if err := store.SavePlaybackState(ctx, gateway.PlaybackState{
		GatewayUserID:   userID,
		SyntheticUserID: "syn-list",
		ItemID:          "item-1",
		ItemName:        "Existing",
		Played:          true,
		PlayCount:       7,
		IsFavorite:      true,
		Likes:           &likes,
	}); err != nil {
		t.Fatalf("seed durable: %v", err)
	}

	res, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: " hash-canonical ",
		Kind:             gateway.PlaybackReportStopped,
		ReceivedAt:       now,
		ItemID:           " item-1 ",
		ItemSnapshot:     gateway.PlaybackItemSnapshot{ID: " item-1 "},
	})
	if err != nil {
		t.Fatalf("ApplyPlaybackReport: %v", err)
	}
	if res.ItemID != "item-1" || res.Durable == nil {
		t.Fatalf("result = %#v", res)
	}
	if !res.Durable.Played || res.Durable.PlayCount != 7 || !res.Durable.IsFavorite || res.Durable.Likes == nil || !*res.Durable.Likes {
		t.Fatalf("result durable user data clobbered: %#v", res.Durable)
	}

	state, err := store.FindPlaybackState(ctx, userID, "item-1")
	if err != nil {
		t.Fatalf("FindPlaybackState: %v", err)
	}
	if !state.Played || state.PlayCount != 7 || !state.IsFavorite || state.Likes == nil || !*state.Likes {
		t.Fatalf("persisted durable user data clobbered: %#v", state)
	}
	if got := countUserItemData(t, app, userID); got != 1 {
		t.Fatalf("durable row count = %d, want 1", got)
	}
}

func TestApplyPlaybackReportConfiguredPolicyMatchesMemoryStore(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-policy", "syn-apply-policy")
	createListSession(t, store, userID, "hash-policy", "dev")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	runtime := int64(600 * testEmbyTicksPerSecond)
	position := runtime * 95 / 100
	cmd := gateway.PlaybackReportCommand{
		GatewayTokenHash: " hash-policy ",
		Kind:             gateway.PlaybackReportStopped,
		ReceivedAt:       now,
		ItemID:           " item-policy ",
		ItemSnapshot: gateway.PlaybackItemSnapshot{
			ID:           "item-policy",
			Type:         "Movie",
			RunTimeTicks: runtime,
		},
		PlayState: gateway.PlaybackPlayState{PositionTicks: &position},
		Policy: gateway.PlaybackResumePolicy{
			MinPct:             5,
			MaxPct:             99,
			MinDurationSeconds: 1,
		},
	}

	pbResult, err := store.ApplyPlaybackReport(ctx, cmd)
	if err != nil {
		t.Fatalf("pbstore apply: %v", err)
	}

	memory := gateway.NewMemoryStore()
	if _, err := memory.CreateSession(ctx, gateway.Session{
		GatewayTokenHash: "hash-policy",
		GatewayUserID:    userID,
		GatewayUsername:  "apply-policy",
		SyntheticUserID:  "syn-list",
		CreatedAt:        now.Add(-time.Hour),
		ExpiresAt:        time.Now().UTC().Add(24 * time.Hour),
		LastActivityAt:   now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("memory session: %v", err)
	}
	memoryResult, err := memory.ApplyPlaybackReport(ctx, cmd)
	if err != nil {
		t.Fatalf("memory apply: %v", err)
	}

	if pbResult.Durable == nil || memoryResult.Durable == nil {
		t.Fatalf("durable results: pb=%#v memory=%#v", pbResult.Durable, memoryResult.Durable)
	}
	if pbResult.Durable.Played != memoryResult.Durable.Played ||
		pbResult.Durable.PlaybackPositionTicks != memoryResult.Durable.PlaybackPositionTicks ||
		pbResult.Durable.PlayCount != memoryResult.Durable.PlayCount {
		t.Fatalf("policy parity: pb=%#v memory=%#v", pbResult.Durable, memoryResult.Durable)
	}
	if pbResult.Durable.Played || pbResult.Durable.PlaybackPositionTicks != position {
		t.Fatalf("configured max=99/min-duration=1 should preserve 95%% resume: %#v", pbResult.Durable)
	}

	persisted, err := store.FindPlaybackState(ctx, userID, "item-policy")
	if err != nil {
		t.Fatalf("FindPlaybackState: %v", err)
	}
	if persisted.Played || persisted.PlaybackPositionTicks != position {
		t.Fatalf("persisted configured policy result: %#v", persisted)
	}
	if pbResult.Current != nil || memoryResult.Current != nil {
		t.Fatalf("stopped current parity: pb=%#v memory=%#v", pbResult.Current, memoryResult.Current)
	}
	if countPlaybackEventsForUser(t, app, userID) != len(memory.PlaybackEvents) {
		t.Fatalf("event count parity: pb=%d memory=%d", countPlaybackEventsForUser(t, app, userID), len(memory.PlaybackEvents))
	}
}

func TestApplyPlaybackReportConflictingItemIDsRejectBeforeMutation(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-conflict", "syn-apply-conflict")
	createListSession(t, store, userID, "hash-conflict", "dev")
	deleteSessionProfileForToken(t, app, "hash-conflict")

	_, err := store.ApplyPlaybackReport(context.Background(), gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-conflict",
		Kind:             gateway.PlaybackReportPlaying,
		ReceivedAt:       time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		ItemID:           "item-a",
		ItemSnapshot:     gateway.PlaybackItemSnapshot{ID: "item-b"},
	})
	if !errors.Is(err, gateway.ErrBadRequest) || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error = %v", err)
	}
	assertApplyPlaybackNoMutation(t, app, userID, "hash-conflict")
}

func TestApplyPlaybackReportInvalidCommandsDoNotRepairOrMutate(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		cmd  gateway.PlaybackReportCommand
	}{
		{
			name: "invalid_kind",
			cmd: gateway.PlaybackReportCommand{
				Kind:       "bogus",
				ReceivedAt: now,
				ItemID:     "item-1",
			},
		},
		{
			name: "nonfinite_playback_rate",
			cmd: gateway.PlaybackReportCommand{
				Kind:       gateway.PlaybackReportPlaying,
				ReceivedAt: now,
				ItemID:     "item-1",
				PlayState:  gateway.PlaybackPlayState{PlaybackRate: float64Ptr(math.NaN())},
			},
		},
		{
			name: "overlong_item_id",
			cmd: gateway.PlaybackReportCommand{
				Kind:       gateway.PlaybackReportPlaying,
				ReceivedAt: now,
				ItemID:     strings.Repeat("x", currentPlaybackItemIDMaxBytes+1),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newCurrentPlaybackListApp(t)
			store := New(app)
			userID := createGatewayUser(t, app, "apply-invalid-"+tt.name, "syn-"+tt.name)
			token := "hash-invalid-" + tt.name
			createListSession(t, store, userID, token, "dev")
			deleteSessionProfileForToken(t, app, token)
			tt.cmd.GatewayTokenHash = token

			_, err := store.ApplyPlaybackReport(context.Background(), tt.cmd)
			if !errors.Is(err, gateway.ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
			assertApplyPlaybackNoMutation(t, app, userID, token)
		})
	}
}

func TestApplyPlaybackReportRejectsInactiveSessionBeforeProfileRepair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.Record)
	}{
		{
			name: "revoked",
			mutate: func(auth *core.Record) {
				auth.Set("revoked_at", time.Now().UTC().Add(-time.Second))
			},
		},
		{
			name: "expired",
			mutate: func(auth *core.Record) {
				auth.Set("expires_at", time.Now().UTC().Add(-time.Second))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newCurrentPlaybackListApp(t)
			store := New(app)
			userID := createGatewayUser(t, app, "inactive-"+tc.name, "syn-inactive-"+tc.name)
			createListSession(t, store, userID, "inactive-hash", "dev")
			auth, err := app.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", "inactive-hash")
			if err != nil {
				t.Fatal(err)
			}
			future := time.Now().UTC().Add(time.Hour)
			auth.Set("expires_at", future)
			if err := app.Save(auth); err != nil {
				t.Fatal(err)
			}
			saveListCurrent(t, app, "inactive-hash", "item-1")
			if err := store.SavePlaybackState(context.Background(), gateway.PlaybackState{
				GatewayUserID: userID, SyntheticUserID: "syn-list", ItemID: "item-1",
				PlaybackPositionTicks: 10,
			}); err != nil {
				t.Fatal(err)
			}
			tc.mutate(auth)
			if err := app.Save(auth); err != nil {
				t.Fatal(err)
			}
			deleteSessionProfileForToken(t, app, "inactive-hash")

			eventsBefore := countPlaybackEventsForUser(t, app, userID)
			durableBefore := countUserItemData(t, app, userID)
			currentBefore := countAllCurrentPlaybacks(t, app)
			_, err = store.ApplyPlaybackReport(context.Background(), gateway.PlaybackReportCommand{
				GatewayTokenHash: "inactive-hash", Kind: gateway.PlaybackReportPlaying,
				ReceivedAt: time.Now().UTC(), ItemID: "item-1",
			})
			if !errors.Is(err, gateway.ErrUnauthorized) {
				t.Fatalf("error = %v, want gateway.ErrUnauthorized", err)
			}
			if events := countPlaybackEventsForUser(t, app, userID); events != eventsBefore {
				t.Fatalf("events changed: %d -> %d", eventsBefore, events)
			}
			if durable := countUserItemData(t, app, userID); durable != durableBefore {
				t.Fatalf("durable rows changed: %d -> %d", durableBefore, durable)
			}
			if current := countAllCurrentPlaybacks(t, app); current != currentBefore {
				t.Fatalf("current rows changed: %d -> %d", currentBefore, current)
			}
			profileRows, err := app.FindRecordsByFilter(pbschema.SessionProfilesCollection, "gateway_session = {:sid}", "", 0, 0, map[string]any{"sid": auth.Id})
			if err != nil {
				t.Fatal(err)
			}
			if len(profileRows) != 0 {
				t.Fatalf("inactive session profile was repaired: %d rows", len(profileRows))
			}
		})
	}
}

func TestApplyPlaybackReportRechecksRevocationAfterPreparation(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-race", "syn-apply-race")
	createListSession(t, store, userID, "race-hash", "dev")
	cmd := gateway.PlaybackReportCommand{
		GatewayTokenHash: "race-hash", Kind: gateway.PlaybackReportPlaying,
		ReceivedAt: time.Now().UTC(), ItemID: "item-race",
	}
	if _, err := gateway.PreparePlaybackReportCommand(cmd); err != nil {
		t.Fatal(err)
	}
	auth, err := app.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", "race-hash")
	if err != nil {
		t.Fatal(err)
	}
	auth.Set("revoked_at", time.Now().UTC())
	if err := app.Save(auth); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyPlaybackReport(context.Background(), cmd); !errors.Is(err, gateway.ErrUnauthorized) {
		t.Fatalf("error = %v, want gateway.ErrUnauthorized", err)
	}
	if countAllCurrentPlaybacks(t, app) != 0 || countUserItemData(t, app, userID) != 0 || countPlaybackEventsForUser(t, app, userID) != 0 {
		t.Fatal("revoked prepared command resurrected playback state")
	}
}

func TestApplyPlaybackReportMonotonicActivity(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-mono", "syn-apply-mono")
	createListSession(t, store, userID, "h1", "dev")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	// Force activity ahead of later report timestamps.
	auth, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "h1")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := app.FindFirstRecordByData(pbschema.SessionProfilesCollection, "gateway_session", auth.Id)
	if err != nil {
		t.Fatal(err)
	}
	ahead := now.Add(10 * time.Minute)
	profile.Set("last_activity_at", ahead)
	if err := app.Save(profile); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: gateway.PlaybackReportPlaying, ReceivedAt: now.Add(time.Minute),
		ItemID: "item-1",
	}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.FindSessionByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.LastActivityAt.Equal(ahead) {
		t.Fatalf("activity went backward: %v", sess.LastActivityAt)
	}

	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: gateway.PlaybackReportProgress, ReceivedAt: now.Add(20 * time.Minute),
		ItemID: "item-1",
	}); err != nil {
		t.Fatal(err)
	}
	sess, err = store.FindSessionByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.LastActivityAt.Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("activity not advanced: %v", sess.LastActivityAt)
	}
}

func TestApplyPlaybackReportRevokeCleansCurrent(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-rev", "syn-apply-rev")
	createListSession(t, store, userID, "h1", "dev")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	pos := int64(50)
	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: gateway.PlaybackReportPlaying, ReceivedAt: now,
		ItemID: "item-1", PlayState: gateway.PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeSession(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ListCurrentPlaybacks(ctx, []string{"h1"})
	if err != nil || len(listed) != 0 {
		t.Fatalf("current after revoke: %#v err=%v", listed, err)
	}
	// Table should have zero current rows for the session.
	authID := listSessionAuthID(t, app, "h1")
	var count int
	if err := app.DB().NewQuery(`SELECT COUNT(*) FROM gateway_current_playbacks WHERE gateway_session = {:sid}`).
		Bind(map[string]any{"sid": authID}).Row(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("current rows remaining after revoke: %d", count)
	}
	sess, err := store.FindSessionByTokenHash(ctx, "h1")
	if err != nil || sess.RevokedAt == nil {
		t.Fatalf("session not revoked: %#v err=%v", sess, err)
	}
}

func TestApplyPlaybackReportFaultInjectionRollsBack(t *testing.T) {
	cases := []struct {
		name      string
		hook      string // event | durable | current | profile
		wantErr   string
		setupPlay bool
	}{
		{name: "event_create", hook: "event", wantErr: "injected event failure", setupPlay: false},
		{name: "durable_create", hook: "durable", wantErr: "injected durable failure", setupPlay: false},
		{name: "current_create", hook: "current", wantErr: "injected current failure", setupPlay: false},
		{name: "profile_update", hook: "profile", wantErr: "injected profile failure", setupPlay: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := newCurrentPlaybackListApp(t)
			store := New(app)
			userID := createGatewayUser(t, app, "apply-fault-"+tc.name, "syn-fault-"+tc.name)
			createListSession(t, store, userID, "hash-fault", "dev")
			ctx := context.Background()
			now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

			// Snapshot pre-state.
			eventsBefore := countPlaybackEventsForUser(t, app, userID)
			activityBefore := sessionActivity(t, store, "hash-fault")
			currentsBefore := countAllCurrentPlaybacks(t, app)
			statesBefore := countUserItemData(t, app, userID)

			unbind := bindPlaybackFaultHook(t, app, tc.hook)
			defer unbind()

			pos := int64(10)
			res, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
				GatewayTokenHash: "hash-fault",
				Kind:             gateway.PlaybackReportPlaying,
				ReceivedAt:       now.Add(time.Minute),
				ItemID:           "item-fault",
				PlayState:        gateway.PlaybackPlayState{PositionTicks: &pos},
				RunTimeTicks:     100 * testEmbyTicksPerSecond,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if res.Applied {
				t.Fatalf("Applied=true after rollback: %#v", res)
			}

			if countPlaybackEventsForUser(t, app, userID) != eventsBefore {
				t.Fatal("events changed after rollback")
			}
			if countAllCurrentPlaybacks(t, app) != currentsBefore {
				t.Fatal("current rows changed after rollback")
			}
			if countUserItemData(t, app, userID) != statesBefore {
				t.Fatal("durable rows changed after rollback")
			}
			activityAfter := sessionActivity(t, store, "hash-fault")
			if !activityAfter.Equal(activityBefore) {
				t.Fatalf("activity changed: %v -> %v", activityBefore, activityAfter)
			}
		})
	}
}

func TestApplyPlaybackReportCorruptPrestateFailsClosed(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-corrupt", "syn-apply-corrupt")
	createListSession(t, store, userID, "hash-c", "dev")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	// Seed a valid current then corrupt JSON.
	pos := int64(1)
	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-c", Kind: gateway.PlaybackReportPlaying, ReceivedAt: now,
		ItemID: "item-1", PlayState: gateway.PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatal(err)
	}
	authID := listSessionAuthID(t, app, "hash-c")
	if _, err := app.DB().NewQuery(`
		UPDATE gateway_current_playbacks SET item_snapshot_json = 'null' WHERE gateway_session = {:sid}
	`).Bind(map[string]any{"sid": authID}).Execute(); err != nil {
		t.Fatal(err)
	}

	eventsBefore := countPlaybackEventsForUser(t, app, userID)
	_, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-c", Kind: gateway.PlaybackReportProgress, ReceivedAt: now.Add(time.Minute),
		ItemID: "item-1", PlayState: gateway.PlaybackPlayState{PositionTicks: int64Ptr(2)},
	})
	if err == nil || !strings.Contains(err.Error(), "item_snapshot_json") {
		t.Fatalf("corrupt prestate err = %v", err)
	}
	if countPlaybackEventsForUser(t, app, userID) != eventsBefore {
		t.Fatal("corrupt prestate still wrote event")
	}
}

func TestApplyPlaybackReportDuplicateCurrentFailsClosed(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-dup", "syn-apply-dup")
	createListSession(t, store, userID, "hash-dup", "dev")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	pos := int64(1)
	if _, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-dup", Kind: gateway.PlaybackReportPlaying, ReceivedAt: now,
		ItemID: "item-1", PlayState: gateway.PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatal(err)
	}
	sessID := listSessionAuthID(t, app, "hash-dup")
	if _, err := app.DB().NewQuery(`DROP INDEX IF EXISTS idx_gateway_current_playbacks_session`).Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB().NewQuery(`
		INSERT INTO gateway_current_playbacks
			(id, gateway_session, item_id, play_session_id, media_source_id, item_snapshot_json, play_state_json, run_time_ticks, started_at, last_reported_at, created, updated)
		VALUES
			('dupcurrentapply2', {:sid}, 'item-2', '', '', '{"Id":"item-2"}', '{}', 0,
			 '2030-01-02 03:04:05.000Z', '2030-01-02 03:04:05.000Z',
			 '2030-01-02 03:04:05.000Z', '2030-01-02 03:04:05.000Z')
	`).Bind(map[string]any{"sid": sessID}).Execute(); err != nil {
		t.Fatalf("insert dup: %v", err)
	}

	eventsBefore := countPlaybackEventsForUser(t, app, userID)
	_, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "hash-dup", Kind: gateway.PlaybackReportProgress, ReceivedAt: now.Add(time.Minute),
		ItemID: "item-1",
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("dup prestate err = %v", err)
	}
	if countPlaybackEventsForUser(t, app, userID) != eventsBefore {
		t.Fatal("dup prestate wrote event")
	}
}

func TestApplyPlaybackReportMissingSessionAndBadRequest(t *testing.T) {
	app := newCurrentPlaybackListApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "apply-bad", "syn-apply-bad")
	createListSession(t, store, userID, "h1", "dev")
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	_, err := store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "missing", Kind: gateway.PlaybackReportProgress, ReceivedAt: now, ItemID: "x",
	})
	if !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("missing session err = %v", err)
	}

	eventsBefore := countPlaybackEventsForUser(t, app, userID)
	_, err = store.ApplyPlaybackReport(ctx, gateway.PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: "bogus", ReceivedAt: now, ItemID: "x",
	})
	if !errors.Is(err, gateway.ErrBadRequest) {
		t.Fatalf("bad kind err = %v", err)
	}
	if countPlaybackEventsForUser(t, app, userID) != eventsBefore {
		t.Fatal("bad request wrote event")
	}
}

func bindPlaybackFaultHook(t *testing.T, app core.App, hook string) func() {
	t.Helper()
	switch hook {
	case "event":
		id := app.OnRecordCreate().BindFunc(func(e *core.RecordEvent) error {
			if e.Record.Collection().Name == "playback_events" {
				return errors.New("injected event failure")
			}
			return e.Next()
		})
		return func() { app.OnRecordCreate().Unbind(id) }
	case "durable":
		id := app.OnRecordCreate().BindFunc(func(e *core.RecordEvent) error {
			if e.Record.Collection().Name == "user_item_data" {
				return errors.New("injected durable failure")
			}
			return e.Next()
		})
		return func() { app.OnRecordCreate().Unbind(id) }
	case "current":
		id := app.OnRecordCreate().BindFunc(func(e *core.RecordEvent) error {
			if e.Record.Collection().Name == pbschema.CurrentPlaybacksCollection {
				return errors.New("injected current failure")
			}
			return e.Next()
		})
		return func() { app.OnRecordCreate().Unbind(id) }
	case "profile":
		id := app.OnRecordUpdate().BindFunc(func(e *core.RecordEvent) error {
			if e.Record.Collection().Name == pbschema.SessionProfilesCollection {
				return errors.New("injected profile failure")
			}
			return e.Next()
		})
		return func() { app.OnRecordUpdate().Unbind(id) }
	default:
		t.Fatalf("unknown hook %q", hook)
		return func() {}
	}
}

func assertEventCount(t *testing.T, app core.App, userID string, want int) {
	t.Helper()
	if got := countPlaybackEventsForUser(t, app, userID); got != want {
		t.Fatalf("playback_events count = %d, want %d", got, want)
	}
}

func assertEventKind(t *testing.T, app core.App, userID, want string) {
	t.Helper()
	records, err := app.FindRecordsByFilter("playback_events", "gateway_user = {:uid}", "-created", 1, 0, map[string]any{"uid": userID})
	if err != nil || len(records) == 0 {
		t.Fatalf("events: %v len=%d", err, len(records))
	}
	if records[0].GetString("event") != want {
		t.Fatalf("event = %q, want %q", records[0].GetString("event"), want)
	}
}

func countPlaybackEventsForUser(t *testing.T, app core.App, userID string) int {
	t.Helper()
	records, err := app.FindRecordsByFilter("playback_events", "gateway_user = {:uid}", "", 0, 0, map[string]any{"uid": userID})
	if err != nil {
		t.Fatal(err)
	}
	return len(records)
}

func countAllCurrentPlaybacks(t *testing.T, app core.App) int {
	t.Helper()
	records, err := app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatal(err)
	}
	return len(records)
}

func countUserItemData(t *testing.T, app core.App, userID string) int {
	t.Helper()
	records, err := app.FindRecordsByFilter("user_item_data", "gateway_user = {:uid}", "", 0, 0, map[string]any{"uid": userID})
	if err != nil {
		t.Fatal(err)
	}
	return len(records)
}

func sessionActivity(t *testing.T, store *Store, tokenHash string) time.Time {
	t.Helper()
	sess, err := store.FindSessionByTokenHash(context.Background(), tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	return sess.LastActivityAt
}

func deleteSessionProfileForToken(t *testing.T, app core.App, tokenHash string) {
	t.Helper()
	auth, err := app.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := app.FindFirstRecordByData(pbschema.SessionProfilesCollection, "gateway_session", auth.Id)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(profile); err != nil {
		t.Fatal(err)
	}
}

func assertApplyPlaybackNoMutation(t *testing.T, app core.App, userID, tokenHash string) {
	t.Helper()
	auth, err := app.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := app.FindRecordsByFilter(
		pbschema.SessionProfilesCollection,
		"gateway_session = {:sessionID}",
		"",
		0,
		0,
		map[string]any{"sessionID": auth.Id},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 0 {
		t.Fatalf("profile hole repaired on rejected command: %d rows", len(profiles))
	}
	if got := countPlaybackEventsForUser(t, app, userID); got != 0 {
		t.Fatalf("events changed: %d", got)
	}
	if got := countUserItemData(t, app, userID); got != 0 {
		t.Fatalf("durable rows changed: %d", got)
	}
	if got := countAllCurrentPlaybacks(t, app); got != 0 {
		t.Fatalf("current rows changed: %d", got)
	}
}

func float64Ptr(v float64) *float64 { return &v }
