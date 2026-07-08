package pbstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	_ "github.com/xxxbrian/emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestRevokeSessionMissingReturnsErrNotFound(t *testing.T) {
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	defer app.Cleanup()

	if _, err := app.FindCollectionByNameOrId("gateway_sessions"); err != nil {
		t.Fatalf("find gateway_sessions collection: %v", err)
	}

	err = New(app).RevokeSession(context.Background(), "missing-token-hash")
	if !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("RevokeSession error = %v, want ErrNotFound", err)
	}
}

func TestPlaybackStateIsScopedByGatewayUserAndItem(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	u1 := createGatewayUser(t, app, "alice", "gateway-user-1")
	u2 := createGatewayUser(t, app, "bob", "gateway-user-2")
	pct1 := 42.5
	pct2 := 88.25
	lastPlayed := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	if err := store.SavePlaybackState(context.Background(), gateway.PlaybackState{GatewayUserID: u1, SyntheticUserID: "gateway-user-1", ItemID: "item-1", PlaybackPositionTicks: 6000000000, PlayedPercentage: &pct1, PlayCount: 1}); err != nil {
		t.Fatalf("save u1 playback state: %v", err)
	}
	if err := store.SavePlaybackState(context.Background(), gateway.PlaybackState{GatewayUserID: u2, SyntheticUserID: "gateway-user-2", ItemID: "item-1", PlaybackPositionTicks: 8800, PlayedPercentage: &pct2, Played: true, LastPlayedDate: &lastPlayed, PlayCount: 3}); err != nil {
		t.Fatalf("save u2 playback state: %v", err)
	}

	state1, err := store.FindPlaybackState(context.Background(), u1, "item-1")
	if err != nil {
		t.Fatalf("find u1 playback state: %v", err)
	}
	state2, err := store.FindPlaybackState(context.Background(), u2, "item-1")
	if err != nil {
		t.Fatalf("find u2 playback state: %v", err)
	}
	if state1.PlaybackPositionTicks != 6000000000 || state1.Played || state1.PlayCount != 1 || state1.PlayedPercentage == nil || *state1.PlayedPercentage != pct1 {
		t.Fatalf("unexpected u1 state: %#v", state1)
	}
	if state2.PlaybackPositionTicks != 8800 || !state2.Played || state2.PlayCount != 3 || state2.PlayedPercentage == nil || *state2.PlayedPercentage != pct2 || state2.LastPlayedDate == nil {
		t.Fatalf("unexpected u2 state: %#v", state2)
	}

	pctUpdated := 95.0
	if err := store.SavePlaybackState(context.Background(), gateway.PlaybackState{GatewayUserID: u1, SyntheticUserID: "gateway-user-1", ItemID: "item-1", PlaybackPositionTicks: 9900, PlayedPercentage: &pctUpdated, Played: true, PlayCount: 2}); err != nil {
		t.Fatalf("update u1 playback state: %v", err)
	}
	records, err := app.FindRecordsByFilter("user_item_data", "gateway_user = {:gatewayUserID} && item_id = 'item-1'", "", 0, 0, dbx.Params{"gatewayUserID": u1})
	if err != nil {
		t.Fatalf("query u1 playback states: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("u1 playback state records = %d, want 1", len(records))
	}
}

func TestUserItemDataFieldsAndDisplayPreferencesArePersisted(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	likes := true
	lastSeen := time.Date(2026, 7, 8, 13, 0, 0, 0, time.UTC)

	if err := store.SavePlaybackState(context.Background(), gateway.PlaybackState{
		GatewayUserID:         userID,
		SyntheticUserID:       "gateway-user",
		ItemID:                "episode-1",
		ItemName:              "Episode 1",
		ItemType:              "Episode",
		SeriesID:              "series-1",
		SeriesName:            "Show",
		SeasonID:              "season-1",
		IndexNumber:           1,
		ParentIndexNumber:     1,
		RunTimeTicks:          1000,
		PlaybackPositionTicks: 500,
		IsFavorite:            true,
		Likes:                 &likes,
		Fingerprint:           "type=Episode|name=Episode 1|seriesId=series-1",
		LastSeenAt:            &lastSeen,
	}); err != nil {
		t.Fatalf("save user item data: %v", err)
	}

	favorite := true
	states, err := store.ListPlaybackStates(context.Background(), userID, gateway.PlaybackStateFilter{Favorite: &favorite})
	if err != nil {
		t.Fatalf("list favorite states: %v", err)
	}
	if len(states) != 1 || states[0].ItemName != "Episode 1" || states[0].SeriesID != "series-1" || states[0].SeasonID != "season-1" || states[0].RunTimeTicks != 1000 || states[0].Likes == nil || !*states[0].Likes || states[0].LastSeenAt == nil {
		t.Fatalf("unexpected user item data: %#v", states)
	}

	if err := store.SaveDisplayPreference(context.Background(), gateway.DisplayPreference{GatewayUserID: userID, SyntheticUserID: "gateway-user", PreferenceID: "home", Client: "web", PayloadJSON: `{"SortBy":"DateCreated"}`}); err != nil {
		t.Fatalf("save display preference: %v", err)
	}
	preference, err := store.FindDisplayPreference(context.Background(), userID, "home", "web")
	if err != nil {
		t.Fatalf("find display preference: %v", err)
	}
	if preference.PayloadJSON != `{"SortBy":"DateCreated"}` || preference.SyntheticUserID != "gateway-user" {
		t.Fatalf("unexpected display preference: %#v", preference)
	}
}

func TestPlaybackAggregatesAreScopedBySeriesAndSeason(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	otherUserID := createGatewayUser(t, app, "bob", "gateway-user-2")
	lastPlayed := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

	states := []gateway.PlaybackState{
		{GatewayUserID: userID, SyntheticUserID: "gateway-user", ItemID: "ep-1", SeriesID: "series-1", SeasonID: "season-1", Played: true, LastPlayedDate: &lastPlayed},
		{GatewayUserID: userID, SyntheticUserID: "gateway-user", ItemID: "ep-2", SeriesID: "series-1", SeasonID: "season-1", Played: false},
		{GatewayUserID: userID, SyntheticUserID: "gateway-user", ItemID: "ep-3", SeriesID: "series-1", SeasonID: "season-2", Played: true},
		{GatewayUserID: otherUserID, SyntheticUserID: "gateway-user-2", ItemID: "ep-4", SeriesID: "series-1", SeasonID: "season-1", Played: true},
	}
	for _, state := range states {
		if err := store.SavePlaybackState(context.Background(), state); err != nil {
			t.Fatalf("save playback state: %v", err)
		}
	}

	aggregates, err := store.ListPlaybackAggregates(context.Background(), userID, []string{"series-1"}, []string{"season-1"})
	if err != nil {
		t.Fatalf("list playback aggregates: %v", err)
	}
	series := aggregates.Series["series-1"]
	season := aggregates.Seasons["season-1"]
	if series.KnownItemCount != 3 || series.PlayedCount != 2 || series.LastPlayedDate == nil {
		t.Fatalf("unexpected series aggregate: %#v", series)
	}
	if season.KnownItemCount != 2 || season.PlayedCount != 1 || season.LastPlayedDate == nil {
		t.Fatalf("unexpected season aggregate: %#v", season)
	}
}

func TestPlaybackStateBatchLookupSkipsOrphanedRecords(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	orphanedAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	if err := store.SavePlaybackState(context.Background(), gateway.PlaybackState{GatewayUserID: userID, SyntheticUserID: "gateway-user", ItemID: "episode-1", PlaybackPositionTicks: 1000, OrphanedAt: &orphanedAt}); err != nil {
		t.Fatalf("save orphaned playback state: %v", err)
	}
	states, err := store.ListPlaybackStatesByItemIDs(context.Background(), userID, []string{"episode-1"})
	if err != nil {
		t.Fatalf("batch lookup playback state: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("orphaned state should be skipped: %#v", states)
	}
}

func TestPathPolicyDefaultAllowAndDeny(t *testing.T) {
	app := newTestApp(t)
	store := New(app)

	decision, err := store.CheckPathPolicy(context.Background(), "GET", "/Videos/1")
	if err != nil {
		t.Fatalf("check default path policy: %v", err)
	}
	if !decision.Allowed || decision.Action != "allow" {
		t.Fatalf("default decision = %#v, want allow", decision)
	}

	policies, err := app.FindCollectionByNameOrId("path_policies")
	if err != nil {
		t.Fatalf("find path_policies: %v", err)
	}
	record := core.NewRecord(policies)
	record.Set("method", "GET")
	record.Set("path", "/Videos/*")
	record.Set("action", "deny")
	record.Set("priority", 10)
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		t.Fatalf("save path policy: %v", err)
	}

	decision, err = store.CheckPathPolicy(context.Background(), "GET", "/Videos/1")
	if err != nil {
		t.Fatalf("check denied path policy: %v", err)
	}
	if decision.Allowed || decision.Action != "deny" || decision.PolicyID == "" {
		t.Fatalf("denied decision = %#v, want deny", decision)
	}

	record = core.NewRecord(policies)
	record.Set("method", "POST")
	record.Set("path", "/Sessions/Playing")
	record.Set("action", "allow")
	record.Set("priority", 10)
	record.Set("enabled", true)
	if err := app.Save(record); err != nil {
		t.Fatalf("save allow path policy: %v", err)
	}
	decision, err = store.CheckPathPolicy(context.Background(), "POST", "/Sessions/Playing")
	if err != nil {
		t.Fatalf("check allowed path policy: %v", err)
	}
	if !decision.Allowed || decision.Action != "allow" || decision.PolicyID == "" {
		t.Fatalf("allowed decision = %#v, want allow with policy id", decision)
	}
}

func TestAuditAndPlaybackEventAreWritable(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	pct := 12.5

	if err := store.RecordAudit(context.Background(), gateway.AuditLog{GatewayUserID: userID, SyntheticUserID: "gateway-user", Event: "login_success", Message: "login succeeded", RemoteIP: "127.0.0.1", Method: "POST", Path: "/Users/AuthenticateByName", Status: 200}); err != nil {
		t.Fatalf("record audit: %v", err)
	}
	if err := store.RecordPlaybackEvent(context.Background(), gateway.PlaybackEvent{GatewayUserID: userID, SyntheticUserID: "gateway-user", ItemID: "item-1", Event: "progress", PositionTicks: 1234, PlayedPercentage: &pct, RemoteIP: "127.0.0.1"}); err != nil {
		t.Fatalf("record playback event: %v", err)
	}

	audits, err := app.FindRecordsByFilter("audit_logs", "gateway_user = {:gatewayUserID} && event = 'login_success'", "", 0, 0, dbx.Params{"gatewayUserID": userID})
	if err != nil {
		t.Fatalf("query audit logs: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audits))
	}
	if audits[0].GetString("synthetic_user_id") != "gateway-user" || audits[0].GetString("method") != "POST" || audits[0].GetString("path") != "/Users/AuthenticateByName" || audits[0].GetInt("status") != 200 {
		t.Fatalf("audit details not persisted: %#v", audits[0])
	}
	events, err := app.FindRecordsByFilter("playback_events", "gateway_user = {:gatewayUserID} && item_id = 'item-1'", "", 0, 0, dbx.Params{"gatewayUserID": userID})
	if err != nil {
		t.Fatalf("query playback events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("playback event records = %d, want 1", len(events))
	}
}

func TestBackendAccountAndSessionUsePlainCredentialsAndClientIdentity(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	accountID := createBackendAccount(t, app)

	account, err := store.DefaultBackend(context.Background())
	if err != nil {
		t.Fatalf("default backend: %v", err)
	}
	if account.ID != accountID || account.Password != "backend-pass" {
		t.Fatalf("unexpected backend account credentials: %#v", account)
	}
	if account.ClientIdentity.UserAgent != "Custom/1.0" || account.ClientIdentity.Client != "Custom" || account.ClientIdentity.Device != "Desktop" || account.ClientIdentity.DeviceID != "device-1" || account.ClientIdentity.Version != "1.0" {
		t.Fatalf("unexpected backend identity: %#v", account.ClientIdentity)
	}

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	session := &gateway.Session{
		GatewayTokenHash: "hash",
		GatewayUserID:    userID,
		GatewayUsername:  "alice",
		SyntheticUserID:  "gateway-user",
		BackendAccountID: accountID,
		BackendServerID:  "server",
		BackendBaseURL:   "https://emby.example.com",
		BackendUserID:    "backend-user",
		BackendUsername:  "real-alice",
		BackendToken:     "backend-token",
		BackendIdentity:  account.ClientIdentity,
		ExpiresAt:        now.Add(time.Hour),
	}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatalf("save session: %v", err)
	}
	saved, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "hash")
	if err != nil {
		t.Fatalf("find raw session: %v", err)
	}
	if saved.GetString("backend_token") != "backend-token" {
		t.Fatalf("stored backend_token = %q, want plaintext backend-token", saved.GetString("backend_token"))
	}

	found, err := store.FindSessionByTokenHash(context.Background(), "hash")
	if err != nil {
		t.Fatalf("find session: %v", err)
	}
	if found.BackendToken != "backend-token" || found.BackendIdentity.UserAgent != "Custom/1.0" || found.BackendIdentity.Client != "Custom" || found.BackendIdentity.Device != "Desktop" || found.BackendIdentity.DeviceID != "device-1" || found.BackendIdentity.Version != "1.0" {
		t.Fatalf("unexpected found session: %#v", found)
	}
}

func TestBackendServerIdentityFieldsAreOptionalAndDefaulted(t *testing.T) {
	app := newTestApp(t)
	store := New(app)

	servers, err := app.FindCollectionByNameOrId("emby_servers")
	if err != nil {
		t.Fatalf("find emby_servers: %v", err)
	}
	server := core.NewRecord(servers)
	server.Set("name", "server")
	server.Set("base_url", "https://emby.example.com")
	server.Set("enabled", true)
	if err := app.Save(server); err != nil {
		t.Fatalf("save server with empty identity fields: %v", err)
	}

	accounts, err := app.FindCollectionByNameOrId("backend_accounts")
	if err != nil {
		t.Fatalf("find backend_accounts: %v", err)
	}
	accountRecord := core.NewRecord(accounts)
	accountRecord.Set("server", server.Id)
	accountRecord.Set("name", "backend")
	accountRecord.Set("backend_username", "real-alice")
	accountRecord.Set("backend_password", "backend-pass")
	accountRecord.Set("enabled", true)
	if err := app.Save(accountRecord); err != nil {
		t.Fatalf("save backend account: %v", err)
	}

	account, err := store.DefaultBackend(context.Background())
	if err != nil {
		t.Fatalf("default backend: %v", err)
	}
	defaults := gateway.DefaultBackendClientIdentity()
	if account.ClientIdentity.UserAgent != defaults.UserAgent || account.ClientIdentity.Client != defaults.Client || account.ClientIdentity.Device != defaults.Device || account.ClientIdentity.Version != defaults.Version {
		t.Fatalf("backend identity defaults not applied: %#v", account.ClientIdentity)
	}
	if account.ClientIdentity.DeviceID != gateway.StableBackendDeviceID(server.Id) {
		t.Fatalf("backend identity device id = %q, want stable default", account.ClientIdentity.DeviceID)
	}
}

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
	return app
}

func createBackendAccount(t *testing.T, app core.App) string {
	t.Helper()
	servers, err := app.FindCollectionByNameOrId("emby_servers")
	if err != nil {
		t.Fatalf("find emby_servers: %v", err)
	}
	server := core.NewRecord(servers)
	server.Set("name", "server")
	server.Set("base_url", "https://emby.example.com")
	server.Set("backend_user_agent", "Custom/1.0")
	server.Set("backend_authorization_client", "Custom")
	server.Set("backend_authorization_device", "Desktop")
	server.Set("backend_authorization_device_id", "device-1")
	server.Set("backend_authorization_version", "1.0")
	server.Set("enabled", true)
	if err := app.Save(server); err != nil {
		t.Fatalf("save server: %v", err)
	}

	accounts, err := app.FindCollectionByNameOrId("backend_accounts")
	if err != nil {
		t.Fatalf("find backend_accounts: %v", err)
	}
	account := core.NewRecord(accounts)
	account.Set("server", server.Id)
	account.Set("name", "backend")
	account.Set("backend_username", "real-alice")
	account.Set("backend_password", "backend-pass")
	account.Set("enabled", true)
	if err := app.Save(account); err != nil {
		t.Fatalf("save account: %v", err)
	}
	return account.Id
}

func createGatewayUser(t *testing.T, app core.App, username, syntheticUserID string) string {
	t.Helper()
	users, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users: %v", err)
	}
	record := core.NewRecord(users)
	record.Set("username", username)
	record.Set("email", username+"@example.com")
	record.Set("synthetic_user_id", syntheticUserID)
	record.Set("enabled", true)
	record.SetPassword("test-pass")
	if err := app.Save(record); err != nil {
		t.Fatalf("save gateway user: %v", err)
	}
	return record.Id
}
