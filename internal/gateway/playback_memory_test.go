package gateway

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMemoryStorePlaybackLifecycle(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	session, err := store.CreateSession(ctx, Session{
		GatewayTokenHash: "hash-a",
		GatewayUserID:    "u1",
		GatewayUsername:  "alice",
		SyntheticUserID:  "syn-1",
		CreatedAt:        now,
		ExpiresAt:        time.Now().UTC().Add(24 * time.Hour),
		LastActivityAt:   now,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	pos := int64(1_000)
	res, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "hash-a",
		Kind:             PlaybackReportPlaying,
		ReceivedAt:       now.Add(time.Minute),
		ItemID:           "item-1",
		PlaySessionID:    "ps-1",
		ItemSnapshot:     PlaybackItemSnapshot{ID: "item-1", Name: "Movie", Type: "Movie"},
		PlayState:        PlaybackPlayState{PositionTicks: &pos},
		RunTimeTicks:     100 * embyTicksPerSecond,
		RemoteIP:         "1.2.3.4",
	})
	if err != nil {
		t.Fatalf("Playing: %v", err)
	}
	if !res.Applied || res.PublicSessionID != session.PublicID || res.Current == nil {
		t.Fatalf("Playing result: %#v", res)
	}
	if res.Current.ItemSnapshot.ID != "item-1" || res.Durable == nil {
		t.Fatalf("Playing current/durable: %#v", res)
	}
	if len(store.PlaybackEvents) != 1 || store.PlaybackEvents[0].Event != "playing" {
		t.Fatalf("events = %#v", store.PlaybackEvents)
	}

	// Progress match updates position and preserves StartedAt.
	pos2 := int64(2_000)
	res2, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "hash-a",
		Kind:             PlaybackReportProgress,
		ReceivedAt:       now.Add(2 * time.Minute),
		ItemID:           "item-1",
		PlaySessionID:    "ps-1",
		PlayState:        PlaybackPlayState{PositionTicks: &pos2},
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

	// Pause via EventName.
	res3, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "hash-a",
		Kind:             PlaybackReportProgress,
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

	// Ping touches LastReportedAt, no event.
	eventCount := len(store.PlaybackEvents)
	res4, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "hash-a",
		Kind:             PlaybackReportPing,
		ReceivedAt:       now.Add(4 * time.Minute),
	})
	if err != nil || !res4.Applied {
		t.Fatalf("Ping: res=%#v err=%v", res4, err)
	}
	if len(store.PlaybackEvents) != eventCount {
		t.Fatalf("Ping wrote event")
	}
	if res4.Current == nil || !res4.Current.LastReportedAt.Equal(now.Add(4*time.Minute)) {
		t.Fatalf("ping current: %#v", res4.Current)
	}

	// Stop clears current and completes.
	posDone := int64(96 * embyTicksPerSecond)
	res5, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "hash-a",
		Kind:             PlaybackReportStopped,
		ReceivedAt:       now.Add(5 * time.Minute),
		ItemID:           "item-1",
		PlaySessionID:    "ps-1",
		RunTimeTicks:     100 * embyTicksPerSecond,
		PlayState:        PlaybackPlayState{PositionTicks: &posDone},
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
	if _, ok := store.CurrentPlaybacks["hash-a"]; ok {
		t.Fatalf("CurrentPlaybacks still has row")
	}

	// Monotonic activity: last successful report time.
	found, err := store.FindSessionByTokenHash(ctx, "hash-a")
	if err != nil {
		t.Fatalf("FindSession: %v", err)
	}
	if !found.LastActivityAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("LastActivityAt = %v, want %v", found.LastActivityAt, now.Add(5*time.Minute))
	}
}

func TestMemoryStorePlaybackDelayedMismatchTwoDevices(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	_, err := store.CreateSession(ctx, Session{
		GatewayTokenHash: "dev-a", GatewayUserID: "u1", GatewayUsername: "alice",
		SyntheticUserID: "syn-1", CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("session a: %v", err)
	}
	_, err = store.CreateSession(ctx, Session{
		GatewayTokenHash: "dev-b", GatewayUserID: "u1", GatewayUsername: "alice",
		SyntheticUserID: "syn-1", CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("session b: %v", err)
	}
	// Second user/device.
	_, err = store.CreateSession(ctx, Session{
		GatewayTokenHash: "dev-c", GatewayUserID: "u2", GatewayUsername: "bob",
		SyntheticUserID: "syn-2", CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("session c: %v", err)
	}

	pos := int64(100)
	if _, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "dev-a", Kind: PlaybackReportPlaying, ReceivedAt: now,
		ItemID: "shared", PlaySessionID: "ps-a", PlayState: PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatalf("play a: %v", err)
	}
	if _, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "dev-b", Kind: PlaybackReportPlaying, ReceivedAt: now.Add(time.Second),
		ItemID: "shared", PlaySessionID: "ps-b", PlayState: PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatalf("play b: %v", err)
	}
	if _, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "dev-c", Kind: PlaybackReportPlaying, ReceivedAt: now.Add(2 * time.Second),
		ItemID: "other", PlayState: PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatalf("play c: %v", err)
	}

	// Delayed progress for a different item on dev-a must not replace current.
	posLate := int64(999)
	res, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "dev-a", Kind: PlaybackReportProgress, ReceivedAt: now.Add(time.Minute),
		ItemID: "late-item", PlayState: PlaybackPlayState{PositionTicks: &posLate},
	})
	if err != nil {
		t.Fatalf("late progress: %v", err)
	}
	if res.Current == nil || res.Current.ItemID != "shared" {
		t.Fatalf("current replaced by mismatch: %#v", res.Current)
	}
	if res.Durable == nil || res.Durable.ItemID != "late-item" || res.Durable.PlaybackPositionTicks != 999 {
		t.Fatalf("late durable: %#v", res.Durable)
	}

	listed, err := store.ListCurrentPlaybacks(ctx, []string{"dev-a", "dev-b", "dev-c", "missing", ""})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed = %#v", listed)
	}
	if listed["dev-a"].ItemID != "shared" || listed["dev-b"].PlaySessionID != "ps-b" || listed["dev-c"].ItemID != "other" {
		t.Fatalf("listed values: %#v", listed)
	}

	// Clone isolation: mutate returned map values.
	cp := listed["dev-a"]
	cp.ItemID = "mutated"
	cp.ItemSnapshot.Name = "mutated"
	if tags := cp.ItemSnapshot.ImageTags; tags != nil {
		tags["Primary"] = "x"
	}
	again, err := store.ListCurrentPlaybacks(ctx, []string{"dev-a"})
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if again["dev-a"].ItemID == "mutated" {
		t.Fatal("ListCurrentPlaybacks leaked mutable state")
	}

	// Durable isolation per user for same item id.
	stateA, err := store.FindPlaybackState(ctx, "u1", "shared")
	if err != nil {
		t.Fatalf("stateA: %v", err)
	}
	stateC, err := store.FindPlaybackState(ctx, "u2", "other")
	if err != nil {
		t.Fatalf("stateC: %v", err)
	}
	if stateA.GatewayUserID != "u1" || stateC.GatewayUserID != "u2" {
		t.Fatalf("user isolation: a=%#v c=%#v", stateA, stateC)
	}
}

func TestMemoryStorePlaybackPingMismatchAndUnknownRuntime(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	_, err := store.CreateSession(ctx, Session{
		GatewayTokenHash: "h1", GatewayUserID: "u1", SyntheticUserID: "syn",
		CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	pos := int64(100)
	if _, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportPlaying, ReceivedAt: now,
		ItemID: "item-1", PlaySessionID: "ps-1", PlayState: PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatalf("playing: %v", err)
	}

	eventsBefore := len(store.PlaybackEvents)
	res, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportPing, ReceivedAt: now.Add(time.Minute),
		PlaySessionID: "ps-other",
	})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if res.Applied {
		t.Fatalf("ping mismatch should no-op: %#v", res)
	}
	if len(store.PlaybackEvents) != eventsBefore {
		t.Fatal("ping mismatch wrote event")
	}

	pct := 95.0
	posHigh := int64(8_000_000)
	res2, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: now.Add(2 * time.Minute),
		ItemID: "unknown-rt", PlayedPercentage: &pct,
		PlayState: PlaybackPlayState{PositionTicks: &posHigh},
	})
	if err != nil {
		t.Fatalf("stopped unknown: %v", err)
	}
	// Current for item-1 preserved (stopped different item).
	if res2.Current == nil || res2.Current.ItemID != "item-1" {
		t.Fatalf("current = %#v", res2.Current)
	}
	if res2.Durable == nil || res2.Durable.Played || res2.Durable.PlaybackPositionTicks != posHigh {
		t.Fatalf("unknown runtime durable: %#v", res2.Durable)
	}

	played := true
	res3, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportStopped, ReceivedAt: now.Add(3 * time.Minute),
		ItemID: "unknown-rt", Played: &played,
		PlayState: PlaybackPlayState{PositionTicks: &posHigh},
	})
	if err != nil {
		t.Fatalf("explicit played: %v", err)
	}
	if res3.Durable == nil || !res3.Durable.Played || res3.Durable.PlayCount != 1 {
		t.Fatalf("explicit complete: %#v", res3.Durable)
	}
}

func TestMemoryStorePlaybackAtomicValidationAndRevoke(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	_, err := store.CreateSession(ctx, Session{
		GatewayTokenHash: "h1", GatewayUserID: "u1", SyntheticUserID: "syn",
		CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour), LastActivityAt: now,
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	pos := int64(50)
	if _, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportPlaying, ReceivedAt: now.Add(time.Minute),
		ItemID: "item-1", PlayState: PlaybackPlayState{PositionTicks: &pos},
	}); err != nil {
		t.Fatalf("playing: %v", err)
	}
	eventsBefore := len(store.PlaybackEvents)
	activityBefore, _ := store.FindSessionByTokenHash(ctx, "h1")

	// Invalid command must not mutate.
	_, err = store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: "bogus", ReceivedAt: now.Add(2 * time.Minute), ItemID: "item-1",
	})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
	if len(store.PlaybackEvents) != eventsBefore {
		t.Fatal("validation failure wrote event")
	}
	activityAfter, _ := store.FindSessionByTokenHash(ctx, "h1")
	if !activityAfter.LastActivityAt.Equal(activityBefore.LastActivityAt) {
		t.Fatalf("activity mutated on validation failure")
	}

	// Missing session.
	_, err = store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "missing", Kind: PlaybackReportProgress, ReceivedAt: now, ItemID: "x",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session err = %v", err)
	}

	// Missing item is success no-op.
	res, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: now.Add(3 * time.Minute),
	})
	if err != nil || res.Applied {
		t.Fatalf("missing item: res=%#v err=%v", res, err)
	}
	if len(store.PlaybackEvents) != eventsBefore {
		t.Fatal("missing item wrote event")
	}

	// Result clone isolation.
	resPlay, err := store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: now.Add(4 * time.Minute),
		ItemID: "item-1", PlayState: PlaybackPlayState{PositionTicks: int64Ptr(77)},
	})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if resPlay.Current != nil {
		resPlay.Current.ItemID = "mutated"
	}
	if resPlay.Durable != nil {
		resPlay.Durable.ItemID = "mutated"
	}
	listed, err := store.ListCurrentPlaybacks(ctx, []string{"h1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if listed["h1"].ItemID == "mutated" {
		t.Fatal("result current leaked into store")
	}
	state, err := store.FindPlaybackState(ctx, "u1", "item-1")
	if err != nil || state.ItemID == "mutated" {
		t.Fatalf("durable isolation: %#v err=%v", state, err)
	}

	// Revoke removes current playback.
	if err := store.RevokeSession(ctx, "h1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	listed, err = store.ListCurrentPlaybacks(ctx, []string{"h1"})
	if err != nil {
		t.Fatalf("list after revoke: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("current not cleaned on revoke: %#v", listed)
	}
	sess, err := store.FindSessionByTokenHash(ctx, "h1")
	if err != nil || sess.RevokedAt == nil {
		t.Fatalf("session not revoked: %#v err=%v", sess, err)
	}
}

func TestMemoryStorePlaybackMonotonicActivity(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	_, err := store.CreateSession(ctx, Session{
		GatewayTokenHash: "h1", GatewayUserID: "u1", SyntheticUserID: "syn",
		CreatedAt: now, ExpiresAt: time.Now().UTC().Add(24 * time.Hour), LastActivityAt: now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	// Older report must not move activity backward.
	_, err = store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportPlaying, ReceivedAt: now.Add(time.Minute),
		ItemID: "item-1",
	})
	if err != nil {
		t.Fatalf("playing: %v", err)
	}
	sess, err := store.FindSessionByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !sess.LastActivityAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("activity went backward: %v", sess.LastActivityAt)
	}

	_, err = store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
		GatewayTokenHash: "h1", Kind: PlaybackReportProgress, ReceivedAt: now.Add(20 * time.Minute),
		ItemID: "item-1",
	})
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	sess, err = store.FindSessionByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatalf("find2: %v", err)
	}
	if !sess.LastActivityAt.Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("activity not advanced: %v", sess.LastActivityAt)
	}
}

func TestMemoryStoreCurrentPlaybackIntegrityFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	seedSession := func(t *testing.T, store *MemoryStore, hash string) {
		t.Helper()
		if _, err := store.CreateSession(ctx, Session{
			GatewayTokenHash: hash,
			GatewayUserID:    "u1",
			SyntheticUserID:  "syn",
			CreatedAt:        now,
			ExpiresAt:        time.Now().UTC().Add(24 * time.Hour),
			LastActivityAt:   now,
		}); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}
	validCurrent := func(hash, itemID string) *CurrentPlayback {
		return &CurrentPlayback{
			GatewayTokenHash: hash,
			ItemID:           itemID,
			ItemSnapshot:     PlaybackItemSnapshot{ID: itemID},
			StartedAt:        now,
			LastReportedAt:   now.Add(time.Minute),
		}
	}

	cases := []struct {
		name    string
		mutate  func(*CurrentPlayback)
		wantSub string
	}{
		{
			name: "zero started_at",
			mutate: func(cp *CurrentPlayback) {
				cp.StartedAt = time.Time{}
			},
			wantSub: "started_at",
		},
		{
			name: "reversed dates",
			mutate: func(cp *CurrentPlayback) {
				cp.StartedAt = now.Add(time.Hour)
				cp.LastReportedAt = now
			},
			wantSub: "before started_at",
		},
		{
			name: "item snapshot mismatch",
			mutate: func(cp *CurrentPlayback) {
				cp.ItemSnapshot.ID = "other-item"
			},
			wantSub: "snapshot Id",
		},
		{
			name: "token map mismatch",
			mutate: func(cp *CurrentPlayback) {
				cp.GatewayTokenHash = "other-hash"
			},
			wantSub: "does not match",
		},
		{
			name: "negative runtime",
			mutate: func(cp *CurrentPlayback) {
				cp.RunTimeTicks = -5
			},
			wantSub: "negative",
		},
		{
			name: "empty item id",
			mutate: func(cp *CurrentPlayback) {
				cp.ItemID = ""
				cp.ItemSnapshot.ID = ""
			},
			wantSub: "item_id",
		},
		{
			name: "negative nested position",
			mutate: func(cp *CurrentPlayback) {
				cp.PlayState.PositionTicks = int64Ptr(-1)
			},
			wantSub: "position_ticks",
		},
		{
			name: "negative snapshot runtime",
			mutate: func(cp *CurrentPlayback) {
				cp.ItemSnapshot.RunTimeTicks = -1
			},
			wantSub: "negative",
		},
		{
			name: "overlong snapshot name",
			mutate: func(cp *CurrentPlayback) {
				cp.ItemSnapshot.Name = strings.Repeat("n", playbackSnapshotNameMaxBytes+1)
			},
			wantSub: "item_snapshot.name",
		},
		{
			name: "invalid snapshot rating",
			mutate: func(cp *CurrentPlayback) {
				cp.ItemSnapshot.CommunityRating = 11
			},
			wantSub: "community_rating",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run("apply_"+tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewMemoryStore()
			seedSession(t, store, "hash-c")
			row := validCurrent("hash-c", "item-1")
			tc.mutate(row)
			store.CurrentPlaybacks["hash-c"] = row

			eventsBefore := len(store.PlaybackEvents)
			activityBefore, err := store.FindSessionByTokenHash(ctx, "hash-c")
			if err != nil {
				t.Fatalf("find: %v", err)
			}
			// Snapshot durable map size for no-mutation proof.
			durableBefore := len(store.PlaybackStates)

			_, err = store.ApplyPlaybackReport(ctx, PlaybackReportCommand{
				GatewayTokenHash: "hash-c",
				Kind:             PlaybackReportProgress,
				ReceivedAt:       now.Add(2 * time.Minute),
				ItemID:           "item-1",
				PlayState:        PlaybackPlayState{PositionTicks: int64Ptr(42)},
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Apply err = %v, want substring %q", err, tc.wantSub)
			}
			if len(store.PlaybackEvents) != eventsBefore {
				t.Fatalf("Apply mutated events: before=%d after=%d", eventsBefore, len(store.PlaybackEvents))
			}
			if len(store.PlaybackStates) != durableBefore {
				t.Fatalf("Apply mutated durable states")
			}
			// Current row must remain the injected corrupt prestate (no upsert/delete).
			if got := store.CurrentPlaybacks["hash-c"]; got != row {
				t.Fatalf("Apply replaced current prestate pointer")
			}
			activityAfter, err := store.FindSessionByTokenHash(ctx, "hash-c")
			if err != nil {
				t.Fatalf("find after: %v", err)
			}
			if !activityAfter.LastActivityAt.Equal(activityBefore.LastActivityAt) {
				t.Fatalf("Apply mutated activity on integrity failure")
			}
		})

		t.Run("list_"+tc.name, func(t *testing.T) {
			t.Parallel()
			store := NewMemoryStore()
			seedSession(t, store, "hash-good")
			seedSession(t, store, "hash-bad")
			// Valid sibling must not be returned when any requested row is corrupt.
			store.CurrentPlaybacks["hash-good"] = validCurrent("hash-good", "item-good")
			bad := validCurrent("hash-bad", "item-bad")
			tc.mutate(bad)
			store.CurrentPlaybacks["hash-bad"] = bad

			got, err := store.ListCurrentPlaybacks(ctx, []string{"hash-good", "hash-bad", "missing"})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("List err = %v, want substring %q", err, tc.wantSub)
			}
			if got != nil {
				t.Fatalf("List must return nil map on integrity failure, got %#v", got)
			}
		})
	}

	t.Run("list missing remains omission", func(t *testing.T) {
		t.Parallel()
		store := NewMemoryStore()
		seedSession(t, store, "hash-only")
		store.CurrentPlaybacks["hash-only"] = validCurrent("hash-only", "item-1")
		got, err := store.ListCurrentPlaybacks(ctx, []string{"hash-only", "missing", ""})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 || got["hash-only"].ItemID != "item-1" {
			t.Fatalf("got = %#v", got)
		}
	})

	t.Run("list nil row fails closed", func(t *testing.T) {
		t.Parallel()
		store := NewMemoryStore()
		seedSession(t, store, "hash-nil")
		store.CurrentPlaybacks["hash-nil"] = nil
		_, err := store.ListCurrentPlaybacks(ctx, []string{"hash-nil"})
		if err == nil || !strings.Contains(err.Error(), "nil") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMemoryStoreApplyRejectsInactiveSessionBeforeRepair(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	for _, tc := range []struct {
		name   string
		mutate func(*Session)
	}{
		{
			name: "revoked",
			mutate: func(session *Session) {
				revoked := now.Add(-time.Second)
				session.RevokedAt = &revoked
			},
		},
		{
			name: "expired",
			mutate: func(session *Session) {
				session.ExpiresAt = now.Add(-time.Second)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			if _, err := store.CreateSession(context.Background(), Session{
				GatewayTokenHash: "inactive-hash",
				GatewayUserID:    "u1",
				SyntheticUserID:  "syn",
				CreatedAt:        now.Add(-time.Hour),
				ExpiresAt:        now.Add(time.Hour),
				LastActivityAt:   now.Add(-time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			live := store.Sessions["inactive-hash"]
			live.PublicID = ""
			live.LastActivityAt = time.Time{}
			tc.mutate(live)
			current := &CurrentPlayback{
				GatewayTokenHash: "inactive-hash",
				ItemID:           "item-1",
				ItemSnapshot:     PlaybackItemSnapshot{ID: "item-1"},
				StartedAt:        now,
				LastReportedAt:   now,
			}
			store.CurrentPlaybacks["inactive-hash"] = current
			store.PlaybackStates[playbackStateKey("u1", "item-1")] = &PlaybackState{GatewayUserID: "u1", SyntheticUserID: "syn", ItemID: "item-1"}
			beforeSession := cloneSession(live)
			beforeEvents := len(store.PlaybackEvents)
			beforeCurrent := cloneCurrentPlayback(current)
			beforeDurable := clonePlaybackState(store.PlaybackStates[playbackStateKey("u1", "item-1")])

			_, err := store.ApplyPlaybackReport(context.Background(), PlaybackReportCommand{
				GatewayTokenHash: "inactive-hash",
				Kind:             PlaybackReportPlaying,
				ReceivedAt:       now.Add(time.Hour),
				ItemID:           "item-1",
			})
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("error = %v, want ErrUnauthorized", err)
			}
			if !reflect.DeepEqual(store.Sessions["inactive-hash"], beforeSession) {
				t.Fatal("inactive session profile hole was repaired")
			}
			if len(store.PlaybackEvents) != beforeEvents || !reflect.DeepEqual(store.CurrentPlaybacks["inactive-hash"], beforeCurrent) ||
				!reflect.DeepEqual(store.PlaybackStates[playbackStateKey("u1", "item-1")], beforeDurable) {
				t.Fatal("inactive apply mutated playback state")
			}
		})
	}
}

func TestMemoryStoreApplyRechecksRevocationAfterPreparation(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now().UTC()
	if _, err := store.CreateSession(context.Background(), Session{
		GatewayTokenHash: "race-hash", GatewayUserID: "u1", SyntheticUserID: "syn",
		CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), LastActivityAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	cmd := PlaybackReportCommand{
		GatewayTokenHash: "race-hash", Kind: PlaybackReportPlaying, ReceivedAt: now,
		ItemID: "item-race",
	}
	if _, err := PreparePlaybackReportCommand(cmd); err != nil {
		t.Fatal(err)
	}
	revoked := time.Now().UTC()
	store.Sessions["race-hash"].RevokedAt = &revoked
	_, err := store.ApplyPlaybackReport(context.Background(), cmd)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
	if _, ok := store.CurrentPlaybacks["race-hash"]; ok {
		t.Fatal("revoked prepared command resurrected current playback")
	}
}

func int64Ptr(v int64) *int64 { return &v }
