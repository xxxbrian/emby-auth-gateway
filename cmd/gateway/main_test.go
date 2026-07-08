package main

import (
	"testing"
	"time"

	_ "emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestCleanupPlaybackEventsKeepsOnlyRecentEvents(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()

	userID := createTestUser(t, app)
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	createPlaybackEvent(t, app, userID, "old", now.Add(-7*time.Hour))
	createPlaybackEvent(t, app, userID, "recent", now.Add(-5*time.Hour))

	if err := cleanupPlaybackEvents(app, now); err != nil {
		t.Fatalf("cleanup playback events: %v", err)
	}

	records, err := app.FindAllRecords("playback_events")
	if err != nil {
		t.Fatalf("query playback events: %v", err)
	}
	if len(records) != 1 || records[0].GetString("item_id") != "recent" {
		t.Fatalf("remaining playback events = %#v, want only recent", records)
	}
}

func createTestUser(t *testing.T, app core.App) string {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	record := core.NewRecord(users)
	record.Set("username", "alice")
	record.Set("email", "alice@example.com")
	record.Set("synthetic_user_id", "gateway-user")
	record.Set("enabled", true)
	record.SetPassword("test-pass")
	if err := app.Save(record); err != nil {
		t.Fatalf("save user: %v", err)
	}
	return record.Id
}

func createPlaybackEvent(t *testing.T, app core.App, userID, itemID string, occurredAt time.Time) {
	t.Helper()
	events, err := app.FindCollectionByNameOrId("playback_events")
	if err != nil {
		t.Fatalf("find playback_events: %v", err)
	}
	record := core.NewRecord(events)
	record.Set("gateway_user", userID)
	record.Set("synthetic_user_id", "gateway-user")
	record.Set("item_id", itemID)
	record.Set("event", "progress")
	record.Set("occurred_at", occurredAt)
	if err := app.Save(record); err != nil {
		t.Fatalf("save playback event: %v", err)
	}
}
