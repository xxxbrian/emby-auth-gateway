package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

func newTestApp(t *testing.T) core.App {
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
	// Control plane runs after migrations: require the current-playbacks sidecar.
	// Base Ensure does not create it; install the exact builder for Phase 4 fixtures.
	ensureCurrentPlaybacksCollection(t, app)
	return app
}

func ensureCurrentPlaybacksCollection(t *testing.T, app core.App) *core.Collection {
	t.Helper()
	if collection, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection); err == nil {
		return collection
	}
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find gateway_sessions: %v", err)
	}
	if err := app.Save(pbschema.CurrentPlaybacks(sessions.Id)); err != nil {
		t.Fatalf("save %s: %v", pbschema.CurrentPlaybacksCollection, err)
	}
	collection, err := app.FindCollectionByNameOrId(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatalf("reload %s: %v", pbschema.CurrentPlaybacksCollection, err)
	}
	return collection
}

func TestCreateUserFailsIfExistsWithoutChangingPassword(t *testing.T) {
	app := newTestApp(t)
	in := UpsertUserInput{
		Username:        "alice",
		Password:        "first-password",
		SyntheticUserID: "synthetic-alice",
	}
	if err := CreateUser(context.Background(), app, in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	rec, err := app.FindFirstRecordByData("users", "username", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.ValidatePassword("first-password") {
		t.Fatal("expected first password to validate")
	}
	hash := rec.GetString("password")

	err = CreateUser(context.Background(), app, UpsertUserInput{
		Username:        "alice",
		Password:        "second-password",
		SyntheticUserID: "synthetic-alice-2",
	})
	if !errors.Is(err, ErrUserExists) {
		t.Fatalf("second create: got %v, want ErrUserExists", err)
	}

	rec, err = app.FindFirstRecordByData("users", "username", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetString("password") != hash {
		t.Fatal("password hash changed on duplicate create")
	}
	if !rec.ValidatePassword("first-password") {
		t.Fatal("original password no longer validates")
	}
	if rec.ValidatePassword("second-password") {
		t.Fatal("second password must not have been applied")
	}
	if rec.GetString("synthetic_user_id") != "synthetic-alice" {
		t.Fatalf("synthetic id changed: %q", rec.GetString("synthetic_user_id"))
	}
}

func TestUpsertUserStillUpdatesPassword(t *testing.T) {
	app := newTestApp(t)
	in := UpsertUserInput{
		Username:        "bob",
		Password:        "first-password",
		SyntheticUserID: "synthetic-bob",
	}
	if err := UpsertUser(context.Background(), app, in); err != nil {
		t.Fatal(err)
	}
	if err := UpsertUser(context.Background(), app, UpsertUserInput{
		Username:        "bob",
		Password:        "second-password",
		SyntheticUserID: "synthetic-bob",
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := app.FindFirstRecordByData("users", "username", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.ValidatePassword("second-password") {
		t.Fatal("upsert should update password")
	}
}

func TestRevokeSessionByIDDeletesTargetCurrentPlaybackAndLeavesUnrelated(t *testing.T) {
	app := newTestApp(t)
	userA := createTestUser(t, app, "alice", "synthetic-alice")
	userB := createTestUser(t, app, "bob", "synthetic-bob")
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	targetID := createTestSession(t, app, userA, "hash-target", now.Add(24*time.Hour), nil)
	otherID := createTestSession(t, app, userB, "hash-other", now.Add(24*time.Hour), nil)
	createTestCurrentPlayback(t, app, targetID, "item-target", now)
	createTestCurrentPlayback(t, app, otherID, "item-other", now)

	if err := RevokeSessionByID(context.Background(), app, targetID); err != nil {
		t.Fatalf("RevokeSessionByID: %v", err)
	}

	target, err := app.FindRecordById("gateway_sessions", targetID)
	if err != nil {
		t.Fatalf("reload target session: %v", err)
	}
	if target.GetDateTime("revoked_at").IsZero() {
		t.Fatal("target session not revoked")
	}
	other, err := app.FindRecordById("gateway_sessions", otherID)
	if err != nil {
		t.Fatalf("reload other session: %v", err)
	}
	if !other.GetDateTime("revoked_at").IsZero() {
		t.Fatal("unrelated session was revoked")
	}

	currents, err := app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatalf("list current playbacks: %v", err)
	}
	if len(currents) != 1 || currents[0].GetString("gateway_session") != otherID {
		t.Fatalf("remaining current playbacks = %#v, want only unrelated %q", currents, otherID)
	}
	if currents[0].GetString("item_id") != "item-other" {
		t.Fatalf("unrelated current item_id = %q", currents[0].GetString("item_id"))
	}
}

func TestRevokeUserSessionsAndPasswordResetClearTargetCurrents(t *testing.T) {
	app := newTestApp(t)
	userA := createTestUser(t, app, "alice", "synthetic-alice")
	userB := createTestUser(t, app, "bob", "synthetic-bob")
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	a1 := createTestSession(t, app, userA, "hash-a1", now.Add(24*time.Hour), nil)
	a2 := createTestSession(t, app, userA, "hash-a2", now.Add(48*time.Hour), nil)
	b1 := createTestSession(t, app, userB, "hash-b1", now.Add(24*time.Hour), nil)
	createTestCurrentPlayback(t, app, a1, "item-a1", now)
	createTestCurrentPlayback(t, app, a2, "item-a2", now)
	createTestCurrentPlayback(t, app, b1, "item-b1", now)

	n, err := RevokeUserSessions(context.Background(), app, userA)
	if err != nil {
		t.Fatalf("RevokeUserSessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("revoked count = %d, want 2", n)
	}

	for _, id := range []string{a1, a2} {
		session, err := app.FindRecordById("gateway_sessions", id)
		if err != nil {
			t.Fatalf("reload session %q: %v", id, err)
		}
		if session.GetDateTime("revoked_at").IsZero() {
			t.Fatalf("session %q not revoked", id)
		}
	}
	other, err := app.FindRecordById("gateway_sessions", b1)
	if err != nil {
		t.Fatalf("reload unrelated session: %v", err)
	}
	if !other.GetDateTime("revoked_at").IsZero() {
		t.Fatal("unrelated session was revoked")
	}
	currents, err := app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatalf("list current playbacks: %v", err)
	}
	if len(currents) != 1 || currents[0].GetString("gateway_session") != b1 {
		t.Fatalf("after RevokeUserSessions remaining currents = %#v, want only %q", currents, b1)
	}

	// Password-change path shares revokeUserSessionsTx; re-seed active sessions for user B.
	b2 := createTestSession(t, app, userB, "hash-b2", now.Add(36*time.Hour), nil)
	createTestCurrentPlayback(t, app, b2, "item-b2", now)
	// Unrelated user A already revoked; leave their empty current set alone and keep b1.
	if err := ResetUserPassword(context.Background(), app, userB, "new-password"); err != nil {
		t.Fatalf("ResetUserPassword: %v", err)
	}
	for _, id := range []string{b1, b2} {
		session, err := app.FindRecordById("gateway_sessions", id)
		if err != nil {
			t.Fatalf("reload bob session %q: %v", id, err)
		}
		if session.GetDateTime("revoked_at").IsZero() {
			t.Fatalf("bob session %q not revoked after password reset", id)
		}
	}
	currents, err = app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatalf("list current playbacks after password reset: %v", err)
	}
	if len(currents) != 0 {
		t.Fatalf("after password reset remaining currents = %#v, want none", currents)
	}
	bob, err := app.FindRecordById("users", userB)
	if err != nil {
		t.Fatalf("reload bob: %v", err)
	}
	if !bob.ValidatePassword("new-password") {
		t.Fatal("password was not updated")
	}
}

func TestRevokeSessionByIDAlreadyRevokedCleansOrphanCurrent(t *testing.T) {
	app := newTestApp(t)
	userID := createTestUser(t, app, "alice", "synthetic-alice")
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	revokedAt := now.Add(-time.Hour)
	sessionID := createTestSession(t, app, userID, "hash-already", now.Add(24*time.Hour), &revokedAt)
	createTestCurrentPlayback(t, app, sessionID, "item-orphan", now)
	originalRevoked := mustLoadSession(t, app, sessionID).GetDateTime("revoked_at").Time()

	if err := RevokeSessionByID(context.Background(), app, sessionID); err != nil {
		t.Fatalf("RevokeSessionByID already-revoked: %v", err)
	}

	session := mustLoadSession(t, app, sessionID)
	if !session.GetDateTime("revoked_at").Time().Equal(originalRevoked) {
		t.Fatalf("revoked_at changed on idempotent revoke: got %v want %v",
			session.GetDateTime("revoked_at"), originalRevoked)
	}
	currents, err := app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatalf("list current playbacks: %v", err)
	}
	if len(currents) != 0 {
		t.Fatalf("orphan current rows after already-revoked cleanup = %#v, want none", currents)
	}
}

func TestRevokeSessionByIDCurrentDeleteFailureRollsBack(t *testing.T) {
	app := newTestApp(t)
	userID := createTestUser(t, app, "alice", "synthetic-alice")
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	sessionID := createTestSession(t, app, userID, "hash-fail", now.Add(24*time.Hour), nil)
	createTestCurrentPlayback(t, app, sessionID, "item-fail", now)

	// Forced deletion failure must abort the whole revocation transaction.
	app.OnRecordDelete(pbschema.CurrentPlaybacksCollection).BindFunc(func(e *core.RecordEvent) error {
		return errors.New("forced current playback delete failure")
	})

	err := RevokeSessionByID(context.Background(), app, sessionID)
	if err == nil {
		t.Fatal("RevokeSessionByID expected to fail when current delete fails")
	}

	session := mustLoadSession(t, app, sessionID)
	if !session.GetDateTime("revoked_at").IsZero() {
		t.Fatal("session was revoked despite current delete failure")
	}
	currents, err := app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		t.Fatalf("list current playbacks: %v", err)
	}
	if len(currents) != 1 || currents[0].GetString("gateway_session") != sessionID {
		t.Fatalf("current playbacks after failed revoke = %#v, want rolled-back target row", currents)
	}
}

func createTestUser(t *testing.T, app core.App, username, syntheticID string) string {
	t.Helper()
	if err := CreateUser(context.Background(), app, UpsertUserInput{
		Username:        username,
		Password:        "test-password",
		SyntheticUserID: syntheticID,
	}); err != nil {
		t.Fatalf("create user %q: %v", username, err)
	}
	user, err := app.FindFirstRecordByData("users", "username", username)
	if err != nil {
		t.Fatalf("find user %q: %v", username, err)
	}
	return user.Id
}

func createTestSession(t *testing.T, app core.App, userID, tokenHash string, expiresAt time.Time, revokedAt *time.Time) string {
	t.Helper()
	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find gateway_sessions: %v", err)
	}
	record := core.NewRecord(sessions)
	record.Set("gateway_token_hash", tokenHash)
	record.Set("gateway_user", userID)
	record.Set("gateway_username", "test")
	record.Set("synthetic_user_id", "synthetic")
	record.Set("expires_at", expiresAt)
	if revokedAt != nil {
		record.Set("revoked_at", *revokedAt)
	}
	if err := app.Save(record); err != nil {
		t.Fatalf("save gateway session: %v", err)
	}
	return record.Id
}

func createTestCurrentPlayback(t *testing.T, app core.App, sessionID, itemID string, reportedAt time.Time) string {
	t.Helper()
	currents := ensureCurrentPlaybacksCollection(t, app)
	record := core.NewRecord(currents)
	record.Set("gateway_session", sessionID)
	record.Set("item_id", itemID)
	record.Set("item_snapshot_json", `{"Id":"`+itemID+`"}`)
	record.Set("play_state_json", `{"PositionTicks":0}`)
	record.Set("started_at", reportedAt)
	record.Set("last_reported_at", reportedAt)
	if err := app.Save(record); err != nil {
		t.Fatalf("save current playback: %v", err)
	}
	return record.Id
}

func mustLoadSession(t *testing.T, app core.App, sessionID string) *core.Record {
	t.Helper()
	record, err := app.FindRecordById("gateway_sessions", sessionID)
	if err != nil {
		t.Fatalf("find session %q: %v", sessionID, err)
	}
	return record
}
