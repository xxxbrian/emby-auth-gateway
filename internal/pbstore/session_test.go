package pbstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestCreateSessionAtomicSuccessAndProfileFailure(t *testing.T) {
	app := newSessionTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	created, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "hash-ok",
		GatewayUserID:    userID,
		GatewayUsername:  "alice",
		SyntheticUserID:  "gateway-user",
		Client:           "Emby Web",
		Device:           "Desktop",
		DeviceID:         "device-1",
		Version:          "4.8.0",
		RemoteIP:         "192.0.2.1",
		CreatedAt:        now,
		ExpiresAt:        now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !sessionid.Valid(created.PublicID) {
		t.Fatalf("PublicID invalid: %q", created.PublicID)
	}
	if created.Capabilities.RawJSON != "{}" {
		t.Fatalf("RawJSON = %q", created.Capabilities.RawJSON)
	}
	if !created.LastActivityAt.Equal(now) {
		t.Fatalf("LastActivityAt = %v, want %v", created.LastActivityAt, now)
	}

	auth, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "hash-ok")
	if err != nil {
		t.Fatalf("find auth: %v", err)
	}
	profile, err := app.FindFirstRecordByData("gateway_session_profiles", "gateway_session", auth.Id)
	if err != nil {
		t.Fatalf("find profile: %v", err)
	}
	if profile.GetString("public_session_id") != created.PublicID {
		t.Fatalf("profile public id = %q, want %q", profile.GetString("public_session_id"), created.PublicID)
	}

	// Missing profile collection: create must fail and leave no auth row.
	profiles, err := app.FindCollectionByNameOrId("gateway_session_profiles")
	if err != nil {
		t.Fatalf("find profiles collection: %v", err)
	}
	if err := app.Delete(profiles); err != nil {
		t.Fatalf("delete profiles collection: %v", err)
	}
	if _, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "hash-fail",
		GatewayUserID:    userID,
		GatewayUsername:  "alice",
		SyntheticUserID:  "gateway-user",
		ExpiresAt:        now.Add(time.Hour),
	}); err == nil {
		t.Fatal("CreateSession with missing profiles collection: want error")
	}
	if _, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "hash-fail"); err == nil {
		t.Fatal("auth row leaked after failed atomic create")
	}
}

func TestCreateSessionGeneratedIDAndDuplicatePublicID(t *testing.T) {
	app := newSessionTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	now := time.Now().UTC()

	first, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "hash-1",
		GatewayUserID:    userID,
		SyntheticUserID:  "gateway-user",
		ExpiresAt:        now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !sessionid.Valid(first.PublicID) || len(first.PublicID) != sessionid.Length {
		t.Fatalf("generated id = %q", first.PublicID)
	}

	// Provided valid ID is preserved.
	fixed := "session-" + strings.Repeat("1", 32)
	second, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "hash-2",
		GatewayUserID:    userID,
		SyntheticUserID:  "gateway-user",
		PublicID:         fixed,
		ExpiresAt:        now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if second.PublicID != fixed {
		t.Fatalf("PublicID = %q, want %q", second.PublicID, fixed)
	}

	if _, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "hash-3",
		GatewayUserID:    userID,
		SyntheticUserID:  "gateway-user",
		PublicID:         fixed,
		ExpiresAt:        now.Add(time.Hour),
	}); err == nil {
		t.Fatal("duplicate public id: want error")
	}
	if _, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "hash-3"); err == nil {
		t.Fatal("auth row leaked after duplicate public id")
	}
}

func TestFindSessionRepairsMissingProfileIdempotent(t *testing.T) {
	app := newSessionTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)

	// Insert auth row only (rollback-forward hole).
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find sessions: %v", err)
	}
	auth := core.NewRecord(sessions)
	auth.Set("gateway_token_hash", "orphan-hash")
	auth.Set("gateway_user", userID)
	auth.Set("gateway_username", "alice")
	auth.Set("synthetic_user_id", "gateway-user")
	auth.Set("client", "SenPlayer")
	auth.Set("device", "Mac")
	auth.Set("device_id", "dev")
	auth.Set("version", "6.1.3")
	auth.Set("remote_ip", "127.0.0.1")
	auth.Set("expires_at", now.Add(time.Hour))
	if err := app.Save(auth); err != nil {
		t.Fatalf("save orphan auth: %v", err)
	}

	first, err := store.FindSessionByTokenHash(context.Background(), "orphan-hash")
	if err != nil {
		t.Fatalf("first find: %v", err)
	}
	if !sessionid.Valid(first.PublicID) {
		t.Fatalf("repaired PublicID invalid: %q", first.PublicID)
	}
	if first.Capabilities.RawJSON != "{}" {
		t.Fatalf("repaired caps = %q", first.Capabilities.RawJSON)
	}
	if first.Client != "SenPlayer" || first.GatewayUserID != userID {
		t.Fatalf("auth fields lost: %#v", first)
	}
	if first.LastActivityAt.IsZero() {
		t.Fatal("repaired last_activity_at must be non-zero")
	}

	second, err := store.FindSessionByTokenHash(context.Background(), "orphan-hash")
	if err != nil {
		t.Fatalf("second find: %v", err)
	}
	if second.PublicID != first.PublicID {
		t.Fatalf("repair not stable: %q vs %q", first.PublicID, second.PublicID)
	}
	if !second.LastActivityAt.Equal(first.LastActivityAt) {
		t.Fatalf("repair activity not stable: %v vs %v", first.LastActivityAt, second.LastActivityAt)
	}

	profiles, err := app.FindRecordsByFilter("gateway_session_profiles", "gateway_session = {:id}", "", 0, 0, dbx.Params{"id": auth.Id})
	if err != nil {
		t.Fatalf("list profiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("profile count = %d, want 1", len(profiles))
	}
	if profiles[0].GetDateTime("last_activity_at").IsZero() {
		t.Fatal("repaired profile last_activity_at must be persisted non-zero")
	}
}

func TestFindSessionRejectsMissingOrZeroLastActivityAt(t *testing.T) {
	app := newSessionTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	created, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "activity-integrity",
		GatewayUserID:    userID,
		SyntheticUserID:  "gateway-user",
		CreatedAt:        base,
		ExpiresAt:        base.Add(time.Hour),
		LastActivityAt:   base.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	auth, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "activity-integrity")
	if err != nil {
		t.Fatalf("find auth: %v", err)
	}
	profile, err := app.FindFirstRecordByData("gateway_session_profiles", "gateway_session", auth.Id)
	if err != nil {
		t.Fatalf("find profile: %v", err)
	}

	// Zero last_activity_at on an existing profile is an integrity error; must
	// not silently fall back to auth.created.
	if _, execErr := app.DB().NewQuery(`
		UPDATE gateway_session_profiles SET last_activity_at = '' WHERE id = {:id}
	`).Bind(dbx.Params{"id": profile.Id}).Execute(); execErr != nil {
		t.Fatalf("force empty last_activity_at: %v", execErr)
	}
	_, err = store.FindSessionByTokenHash(context.Background(), "activity-integrity")
	if err == nil {
		t.Fatal("empty last_activity_at: want integrity error")
	}
	if !strings.Contains(err.Error(), "last_activity_at") {
		t.Fatalf("error = %v, want last_activity_at integrity failure", err)
	}
	// Ensure we did not invent activity from auth creation.
	if strings.Contains(err.Error(), created.CreatedAt.String()) {
		t.Fatalf("error must not mention created fallback: %v", err)
	}

	// Explicit zero-time timestamp is also rejected.
	if _, execErr := app.DB().NewQuery(`
		UPDATE gateway_session_profiles SET last_activity_at = '0001-01-01 00:00:00.000Z' WHERE id = {:id}
	`).Bind(dbx.Params{"id": profile.Id}).Execute(); execErr != nil {
		t.Fatalf("force zero last_activity_at: %v", execErr)
	}
	_, err = store.FindSessionByTokenHash(context.Background(), "activity-integrity")
	if err == nil {
		t.Fatal("zero last_activity_at: want integrity error")
	}
	if !strings.Contains(err.Error(), "last_activity_at") {
		t.Fatalf("error = %v, want last_activity_at integrity failure", err)
	}

	// List path must also fail closed on corrupt profile (not skip/substitute).
	if _, err := store.ListActiveSessions(context.Background(), userID, base.Add(2*time.Minute)); err == nil {
		t.Fatal("list with zero last_activity_at: want error")
	}

	// An existing profile with an empty capability document is corrupt; the
	// empty-to-default rule applies only while creating or repairing a profile.
	if _, execErr := app.DB().NewQuery(`
		UPDATE gateway_session_profiles
		SET last_activity_at = {:activity}, capabilities_json = ''
		WHERE id = {:id}
	`).Bind(dbx.Params{"activity": base.Add(time.Minute), "id": profile.Id}).Execute(); execErr != nil {
		t.Fatalf("force empty capabilities_json: %v", execErr)
	}
	if _, err := store.FindSessionByTokenHash(context.Background(), "activity-integrity"); err == nil || !strings.Contains(err.Error(), "capabilities_json") {
		t.Fatalf("empty capabilities_json error = %v", err)
	}
	if _, err := store.ListActiveSessions(context.Background(), userID, base.Add(2*time.Minute)); err == nil {
		t.Fatal("list with empty capabilities_json: want error")
	}

	// Hole repair remains green after integrity failure path.
	// Create a separate auth-only row and ensure repair still works.
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find sessions: %v", err)
	}
	orphan := core.NewRecord(sessions)
	orphan.Set("gateway_token_hash", "activity-hole")
	orphan.Set("gateway_user", userID)
	orphan.Set("synthetic_user_id", "gateway-user")
	orphan.Set("expires_at", base.Add(time.Hour))
	if err := app.Save(orphan); err != nil {
		t.Fatalf("save hole auth: %v", err)
	}
	repaired, err := store.FindSessionByTokenHash(context.Background(), "activity-hole")
	if err != nil {
		t.Fatalf("hole repair: %v", err)
	}
	if repaired.LastActivityAt.IsZero() {
		t.Fatal("hole repair last_activity_at must be non-zero")
	}
}

func TestListActiveSessionsRepairIsolationSortAndMalformed(t *testing.T) {
	app := newSessionTestApp(t)
	store := New(app)
	user1 := createGatewayUser(t, app, "alice", "syn-1")
	user2 := createGatewayUser(t, app, "bob", "syn-2")
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	mk := func(hash, uid string, activity, expires time.Time, revoked *time.Time, publicID string) *gateway.Session {
		t.Helper()
		s, err := store.CreateSession(context.Background(), gateway.Session{
			GatewayTokenHash: hash,
			GatewayUserID:    uid,
			SyntheticUserID:  "syn",
			PublicID:         publicID,
			CreatedAt:        base,
			ExpiresAt:        expires,
			LastActivityAt:   activity,
			RevokedAt:        revoked,
		})
		if err != nil {
			t.Fatalf("create %s: %v", hash, err)
		}
		return s
	}

	a := mk("a", user1, base, base.Add(2*time.Hour), nil, "")
	b := mk("b", user1, base.Add(time.Minute), base.Add(2*time.Hour), nil, "")
	// Tie on activity with deterministic PublicID ordering.
	lowID := "session-" + strings.Repeat("0", 32)
	highID := "session-" + strings.Repeat("f", 32)
	_ = mk("c-low", user1, b.LastActivityAt, base.Add(2*time.Hour), nil, lowID)
	_ = mk("c-high", user1, b.LastActivityAt, base.Add(2*time.Hour), nil, highID)
	_ = mk("other-user", user2, base.Add(time.Hour), base.Add(2*time.Hour), nil, "")
	revokedAt := base.Add(30 * time.Minute)
	_ = mk("revoked", user1, base.Add(time.Hour), base.Add(2*time.Hour), &revokedAt, "")
	_ = mk("expired", user1, base, base.Add(-time.Minute), nil, "")

	// Orphan auth row for user1 should be repaired during list.
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find sessions: %v", err)
	}
	orphan := core.NewRecord(sessions)
	orphan.Set("gateway_token_hash", "list-orphan")
	orphan.Set("gateway_user", user1)
	orphan.Set("synthetic_user_id", "syn-1")
	orphan.Set("expires_at", base.Add(2*time.Hour))
	if err := app.Save(orphan); err != nil {
		t.Fatalf("save list orphan: %v", err)
	}

	list, err := store.ListActiveSessions(context.Background(), user1, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ListActiveSessions: %v", err)
	}
	// a, b, c-low, c-high, list-orphan (revoked/expired excluded; other-user isolated)
	if len(list) != 5 {
		ids := make([]string, len(list))
		for i, s := range list {
			ids[i] = s.GatewayTokenHash
		}
		t.Fatalf("active = %d (%v), want 5", len(list), ids)
	}
	for _, s := range list {
		if s.GatewayUserID != user1 {
			t.Fatalf("isolation broken: %#v", s)
		}
		if !sessionid.Valid(s.PublicID) {
			t.Fatalf("invalid public id in list: %#v", s)
		}
	}
	// Sort: activity desc, then PublicID asc.
	for i := 1; i < len(list); i++ {
		prev, cur := list[i-1], list[i]
		if prev.LastActivityAt.Before(cur.LastActivityAt) {
			t.Fatalf("activity order broken at %d: %v then %v", i, prev.LastActivityAt, cur.LastActivityAt)
		}
		if prev.LastActivityAt.Equal(cur.LastActivityAt) && prev.PublicID > cur.PublicID {
			t.Fatalf("public id order broken: %q then %q", prev.PublicID, cur.PublicID)
		}
	}
	_ = a

	// Malformed profile fails entire list.
	bad, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "bad-caps",
		GatewayUserID:    user1,
		SyntheticUserID:  "syn-1",
		ExpiresAt:        base.Add(2 * time.Hour),
		LastActivityAt:   base.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create bad-caps session: %v", err)
	}
	auth, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "bad-caps")
	if err != nil {
		t.Fatalf("find bad auth: %v", err)
	}
	profile, err := app.FindFirstRecordByData("gateway_session_profiles", "gateway_session", auth.Id)
	if err != nil {
		t.Fatalf("find bad profile: %v", err)
	}
	// Bypass validation by raw SQL if needed; try Set first.
	profile.Set("capabilities_json", "{not-json")
	if err := app.Save(profile); err != nil {
		// Field validation may reject; force via DB.
		if _, execErr := app.DB().NewQuery("UPDATE gateway_session_profiles SET capabilities_json = {:v} WHERE id = {:id}").
			Bind(dbx.Params{"v": "{not-json", "id": profile.Id}).Execute(); execErr != nil {
			t.Fatalf("force malformed caps (save=%v, sql=%v)", err, execErr)
		}
	}
	if _, err := store.ListActiveSessions(context.Background(), user1, base.Add(2*time.Minute)); err == nil {
		t.Fatal("list with malformed profile: want error")
	}
	_ = bad
}

func TestUpdateSessionCapabilitiesAndTouchCoalescing(t *testing.T) {
	app := newSessionTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	created, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "caps-hash",
		GatewayUserID:    userID,
		SyntheticUserID:  "gateway-user",
		CreatedAt:        base,
		ExpiresAt:        base.Add(time.Hour),
		LastActivityAt:   base,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	raw := `{"PlayableMediaTypes":["Video","Audio"],"SupportedCommands":["Play","Stop"],"SupportsMediaControl":true,"SupportsSync":true,"Extra":1}`
	parsed, err := gateway.ParseSessionCapabilities(raw)
	if err != nil {
		t.Fatalf("parse capabilities: %v", err)
	}
	updated, err := store.UpdateSessionCapabilities(context.Background(), "caps-hash", gateway.SessionCapabilities{RawJSON: raw}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("UpdateSessionCapabilities: %v", err)
	}
	if updated.Capabilities.RawJSON != parsed.RawJSON {
		t.Fatalf("raw = %q", updated.Capabilities.RawJSON)
	}
	if len(updated.Capabilities.PlayableMediaTypes) != 2 || updated.Capabilities.PlayableMediaTypes[1] != "Audio" {
		t.Fatalf("media types = %#v", updated.Capabilities.PlayableMediaTypes)
	}
	if len(updated.Capabilities.SupportedCommands) != 2 {
		t.Fatalf("commands = %#v", updated.Capabilities.SupportedCommands)
	}
	if !updated.Capabilities.SupportsMediaControl || !updated.Capabilities.SupportsSync {
		t.Fatalf("bools = %#v", updated.Capabilities)
	}
	if !updated.LastActivityAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("activity = %v", updated.LastActivityAt)
	}

	// Hydration on find.
	found, err := store.FindSessionByTokenHash(context.Background(), "caps-hash")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.Capabilities.RawJSON != parsed.RawJSON || !found.Capabilities.SupportsMediaControl {
		t.Fatalf("hydrated caps = %#v", found.Capabilities)
	}

	// 30s coalescing.
	changed, err := store.TouchSessionActivity(context.Background(), "caps-hash", base.Add(time.Minute+10*time.Second), 30*time.Second)
	if err != nil || changed {
		t.Fatalf("touch within 30s = (%v, %v)", changed, err)
	}
	mid, err := store.FindSessionByTokenHash(context.Background(), "caps-hash")
	if err != nil {
		t.Fatalf("find mid: %v", err)
	}
	if !mid.LastActivityAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("activity changed within coalesce window: %v", mid.LastActivityAt)
	}

	changed, err = store.TouchSessionActivity(context.Background(), "caps-hash", base.Add(time.Minute+30*time.Second), 30*time.Second)
	if err != nil || !changed {
		// at - last == 30s; condition is at.Sub(last) < minInterval, so 30s is NOT < 30s → should change.
		t.Fatalf("touch at exactly 30s = (%v, %v), want (true, nil)", changed, err)
	}
	// Also verify a clear >30s case from original base if needed.
	changed, err = store.TouchSessionActivity(context.Background(), "caps-hash", base.Add(time.Minute+61*time.Second), 30*time.Second)
	if err != nil || !changed {
		t.Fatalf("touch after 30s = (%v, %v)", changed, err)
	}

	if _, err := store.UpdateSessionCapabilities(context.Background(), "missing", gateway.SessionCapabilities{RawJSON: "{}"}, base); !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("missing caps update = %v", err)
	}
	if _, err := store.TouchSessionActivity(context.Background(), "missing", base, time.Second); !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("missing touch = %v", err)
	}
	_ = created
}

func TestSessionTokenExistsAndRevokeWithProfiles(t *testing.T) {
	app := newSessionTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	now := time.Now().UTC()

	created, err := store.CreateSession(context.Background(), gateway.Session{
		GatewayTokenHash: "exists-hash",
		GatewayUserID:    userID,
		GatewayUsername:  "alice",
		SyntheticUserID:  "gateway-user",
		Client:           "Emby Web",
		Device:           "Desktop",
		DeviceID:         "device-1",
		Version:          "4.8.0",
		RemoteIP:         "192.0.2.1",
		ExpiresAt:        now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Gateway-only fields still hold; no upstream columns.
	saved, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "exists-hash")
	if err != nil {
		t.Fatalf("find raw: %v", err)
	}
	collection, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find collection: %v", err)
	}
	for _, field := range []string{
		"backend_account", "backend_token", "backend_server_id", "backend_base_url", "backend_user_id", "backend_username",
		"backend_user_agent", "backend_authorization_client", "backend_authorization_device", "backend_authorization_device_id", "backend_authorization_version", "backend_token_encrypted",
	} {
		if collection.Fields.GetByName(field) != nil || saved.GetRaw(field) != nil {
			t.Fatalf("gateway_sessions retained upstream field %q", field)
		}
	}

	found, err := store.FindSessionByTokenHash(context.Background(), "exists-hash")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if found.PublicID != created.PublicID || found.GatewayUsername != "alice" || found.DeviceID != "device-1" {
		t.Fatalf("unexpected session: %#v", found)
	}

	exists, err := store.SessionTokenExists(context.Background(), "exists-hash")
	if err != nil || !exists {
		t.Fatalf("exists = (%v, %v)", exists, err)
	}
	exists, err = store.SessionTokenExists(context.Background(), "missing-hash")
	if err != nil || exists {
		t.Fatalf("missing exists = (%v, %v)", exists, err)
	}

	if err := store.RevokeSession(context.Background(), "exists-hash"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	revoked, err := store.FindSessionByTokenHash(context.Background(), "exists-hash")
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoked = %#v, %v", revoked, err)
	}
	// Profile retained with revoked auth row.
	if _, err := app.FindFirstRecordByData("gateway_session_profiles", "public_session_id", created.PublicID); err != nil {
		t.Fatalf("profile missing after revoke: %v", err)
	}

	if err := store.RevokeSession(context.Background(), "missing-hash"); !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("revoke missing = %v", err)
	}

	// Operational error when sessions collection is gone.
	// Drop dependent profiles collection first (relation prevents parent delete).
	if profiles, err := app.FindCollectionByNameOrId("gateway_session_profiles"); err == nil {
		if err := app.Delete(profiles); err != nil {
			t.Fatalf("delete profiles: %v", err)
		}
	}
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find sessions: %v", err)
	}
	if err := app.Delete(sessions); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}
	exists, err = store.SessionTokenExists(context.Background(), "exists-hash")
	if err == nil {
		t.Fatal("operational SessionTokenExists error = nil")
	}
	if exists {
		t.Fatal("operational SessionTokenExists returned exists=true")
	}
}

func TestMemoryAndPbstoreSessionParity(t *testing.T) {
	app := newSessionTestApp(t)
	pb := New(app)
	mem := gateway.NewMemoryStore()
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	// Memory store uses free-form user ids.
	memUser := userID
	base := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	raw := `{"PlayableMediaTypes":["Video"],"SupportedCommands":["Play"],"SupportsMediaControl":true,"SupportsSync":false}`

	type repo interface {
		CreateSession(context.Context, gateway.Session) (*gateway.Session, error)
		FindSessionByTokenHash(context.Context, string) (*gateway.Session, error)
		UpdateSessionCapabilities(context.Context, string, gateway.SessionCapabilities, time.Time) (*gateway.Session, error)
		TouchSessionActivity(context.Context, string, time.Time, time.Duration) (bool, error)
		ListActiveSessions(context.Context, string, time.Time) ([]gateway.Session, error)
		RevokeSession(context.Context, string) error
	}

	run := func(t *testing.T, name string, r repo, uid string) {
		t.Helper()
		created, err := r.CreateSession(context.Background(), gateway.Session{
			GatewayTokenHash: "parity-" + name,
			GatewayUserID:    uid,
			SyntheticUserID:  "syn",
			CreatedAt:        base,
			ExpiresAt:        base.Add(time.Hour),
			LastActivityAt:   base,
		})
		if err != nil {
			t.Fatalf("%s create: %v", name, err)
		}
		if !sessionid.Valid(created.PublicID) || created.Capabilities.RawJSON != "{}" {
			t.Fatalf("%s create defaults: %#v", name, created)
		}
		updated, err := r.UpdateSessionCapabilities(context.Background(), "parity-"+name, gateway.SessionCapabilities{RawJSON: raw}, base.Add(time.Minute))
		if err != nil {
			t.Fatalf("%s caps: %v", name, err)
		}
		if !updated.Capabilities.SupportsMediaControl || len(updated.Capabilities.PlayableMediaTypes) != 1 {
			t.Fatalf("%s caps hydrate: %#v", name, updated.Capabilities)
		}
		changed, err := r.TouchSessionActivity(context.Background(), "parity-"+name, base.Add(time.Minute+5*time.Second), 30*time.Second)
		if err != nil || changed {
			t.Fatalf("%s touch coalesce: (%v, %v)", name, changed, err)
		}
		changed, err = r.TouchSessionActivity(context.Background(), "parity-"+name, base.Add(time.Minute+40*time.Second), 30*time.Second)
		if err != nil || !changed {
			t.Fatalf("%s touch write: (%v, %v)", name, changed, err)
		}
		list, err := r.ListActiveSessions(context.Background(), uid, base.Add(2*time.Minute))
		if err != nil || len(list) != 1 {
			t.Fatalf("%s list: %d, %v", name, len(list), err)
		}
		if list[0].Capabilities.RawJSON != raw {
			t.Fatalf("%s list caps: %q", name, list[0].Capabilities.RawJSON)
		}
		if err := r.RevokeSession(context.Background(), "parity-"+name); err != nil {
			t.Fatalf("%s revoke: %v", name, err)
		}
		list, err = r.ListActiveSessions(context.Background(), uid, base.Add(2*time.Minute))
		if err != nil || len(list) != 0 {
			t.Fatalf("%s list after revoke: %d, %v", name, len(list), err)
		}
	}

	run(t, "pb", pb, userID)
	run(t, "mem", mem, memUser)
}

func newSessionTestApp(t *testing.T) core.App {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	t.Cleanup(app.Cleanup)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	ensureSessionProfilesCollection(t, app)
	return app
}

func ensureSessionProfilesCollection(t *testing.T, app core.App) {
	t.Helper()
	if _, err := app.FindCollectionByNameOrId(pbschema.SessionProfilesCollection); err == nil {
		return
	}
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find gateway_sessions: %v", err)
	}
	// Prefer the schema-lane builder when present; fall back is not needed once
	// migrations land in the same worktree bootstrap path.
	if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
		t.Fatalf("create gateway_session_profiles: %v", err)
	}
}
