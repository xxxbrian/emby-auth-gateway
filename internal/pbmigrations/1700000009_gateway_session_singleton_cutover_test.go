package pbmigrations

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

func TestGatewaySessionSingletonCutoverUpgradesHistoricalEncryptedSession(t *testing.T) {
	app := newMigrationTestApp(t)
	user, account := cutoverUserAndAccount(t, app)
	sessions := mustCollection(t, app, "gateway_sessions")
	addHistoricalSessionFields(t, app, sessions, mustCollection(t, app, "backend_accounts").Id)
	counts := restoreLegacyItemChildCounts(t, app)

	session := core.NewRecord(sessions)
	setLegacySession(t, session, user.Id, account.Id, "legacy")
	if err := app.Save(session); err != nil {
		t.Fatalf("save legacy session: %v", err)
	}
	cache := core.NewRecord(counts)
	cache.Set("backend_account_id", account.Id)
	cache.Set("item_id", "item")
	cache.Set("child_count", 2)
	if err := app.Save(cache); err != nil {
		t.Fatalf("save legacy cache: %v", err)
	}

	if err := gatewaySessionSingletonCutoverUp(app); err != nil {
		t.Fatalf("upgrade historical schema: %v", err)
	}
	assertGatewaySessionCutoverFinalSchema(t, app)
	legacy := mustFindRecord(t, app, "gateway_sessions", session.Id)
	if legacy.GetDateTime("revoked_at").IsZero() || legacy.GetDateTime("revoked_at").Time().Location() != time.UTC {
		t.Fatalf("legacy session was not revoked with a UTC timestamp: %#v", legacy.GetRaw("revoked_at"))
	}

	fresh := core.NewRecord(mustCollection(t, app, "gateway_sessions"))
	setFinalSession(fresh, user.Id, "fresh")
	if err := app.Save(fresh); err != nil {
		t.Fatalf("save new gateway-only session: %v", err)
	}
}

func TestGatewaySessionSingletonCutoverPreservesDurableDataAndClearsCache(t *testing.T) {
	app := newMigrationTestApp(t)
	user, accountA := cutoverUserAndAccount(t, app)
	_, accountB := cutoverUserAndAccount(t, app)
	sessions := mustCollection(t, app, "gateway_sessions")
	addCurrentSessionBackendAccountField(t, app, sessions, mustCollection(t, app, "backend_accounts").Id)
	counts := restoreLegacyItemChildCounts(t, app)

	for _, token := range []string{"one", "two"} {
		record := core.NewRecord(sessions)
		setLegacySession(t, record, user.Id, accountA.Id, token)
		if err := app.Save(record); err != nil {
			t.Fatalf("save session %s: %v", token, err)
		}
	}
	for _, accountID := range []string{accountA.Id, accountB.Id} {
		record := core.NewRecord(counts)
		record.Set("backend_account_id", accountID)
		record.Set("item_id", "collision")
		record.Set("child_count", 1)
		if err := app.Save(record); err != nil {
			t.Fatalf("save cache collision: %v", err)
		}
	}
	seedDurableCutoverData(t, app, user.Id)
	snapshot := cutoverSnapshot(t, app, []string{"users", "user_item_data", "playback_events", "display_preferences", "audit_logs", upstreamSourcesCollection, upstreamEndpointsCollection})

	if err := gatewaySessionSingletonCutoverUp(app); err != nil {
		t.Fatalf("upgrade 0008 fixture: %v", err)
	}
	if got := cutoverSnapshot(t, app, []string{"users", "user_item_data", "playback_events", "display_preferences", "audit_logs", upstreamSourcesCollection, upstreamEndpointsCollection}); !slices.Equal(got, snapshot) {
		t.Fatalf("durable records changed: got %#v want %#v", got, snapshot)
	}
	remaining, err := app.FindRecordsByFilter("item_child_counts", "", "", 0, 0, nil)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("cache after cutover = %#v, %v", remaining, err)
	}
	allSessions, err := app.FindRecordsByFilter("gateway_sessions", "", "", 0, 0, nil)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	for _, session := range allSessions {
		if session.GetDateTime("revoked_at").IsZero() {
			t.Fatalf("session %s remained active", session.Id)
		}
	}
	assertGatewaySessionCutoverFinalSchema(t, app)
}

func TestGatewaySessionSingletonCutoverRepeatedBootstrapAndRefusals(t *testing.T) {
	t.Run("fresh repeated bootstrap", func(t *testing.T) {
		app := newMigrationTestApp(t)
		for range 2 {
			if err := app.RunAllMigrations(); err != nil {
				t.Fatalf("repeat bootstrap: %v", err)
			}
		}
		assertGatewaySessionCutoverFinalSchema(t, app)
	})

	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, app core.App)
	}{
		{
			name:   "partial state",
			mutate: func(t *testing.T, app core.App) { restoreLegacyItemChildCounts(t, app) },
		},
		{
			name: "incompatible retired field",
			mutate: func(t *testing.T, app core.App) {
				sessions := mustCollection(t, app, "gateway_sessions")
				sessions.Fields.Add(&core.TextField{Name: gatewaySessionBackendAccountField, Required: true, Max: 80})
				if err := app.SaveNoValidate(sessions); err != nil {
					t.Fatalf("save incompatible session schema: %v", err)
				}
				restoreLegacyItemChildCounts(t, app)
			},
		},
		{
			name: "wrong final index shape",
			mutate: func(t *testing.T, app core.App) {
				counts := mustCollection(t, app, "item_child_counts")
				counts.RemoveIndex(itemChildCountItemIndex)
				counts.AddIndex(itemChildCountItemIndex, false, "item_id", "")
				if err := app.SaveNoValidate(counts); err != nil {
					t.Fatalf("save incompatible cache index: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newMigrationTestApp(t)
			tc.mutate(t, app)
			before := cutoverStateSnapshot(t, app, []string{"gateway_sessions", "item_child_counts"})
			if err := gatewaySessionSingletonCutoverUp(app); err == nil {
				t.Fatal("accepted incompatible or partial schema")
			}
			if after := cutoverStateSnapshot(t, app, []string{"gateway_sessions", "item_child_counts"}); !slices.Equal(after, before) {
				t.Fatalf("refused migration mutated data: got %#v want %#v", after, before)
			}
		})
	}
}

func TestGatewaySessionSingletonCutoverRejectsWrongGatewayUserTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, app core.App, user, account *core.Record)
	}{
		{
			name: "legacy shape",
			setup: func(t *testing.T, app core.App, user, account *core.Record) {
				sessions := mustCollection(t, app, "gateway_sessions")
				addCurrentSessionBackendAccountField(t, app, sessions, mustCollection(t, app, "backend_accounts").Id)
				record := core.NewRecord(sessions)
				setLegacySession(t, record, user.Id, account.Id, "legacy-wrong-target")
				if err := app.Save(record); err != nil {
					t.Fatalf("save legacy session: %v", err)
				}
				counts := restoreLegacyItemChildCounts(t, app)
				cache := core.NewRecord(counts)
				cache.Set("backend_account_id", account.Id)
				cache.Set("item_id", "item")
				cache.Set("child_count", 1)
				if err := app.Save(cache); err != nil {
					t.Fatalf("save legacy cache: %v", err)
				}
			},
		},
		{
			name: "final shape",
			setup: func(t *testing.T, app core.App, user, _ *core.Record) {
				record := core.NewRecord(mustCollection(t, app, "gateway_sessions"))
				setFinalSession(record, user.Id, "final-wrong-target")
				if err := app.Save(record); err != nil {
					t.Fatalf("save final session: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newMigrationTestApp(t)
			user, account := cutoverUserAndAccount(t, app)
			tc.setup(t, app, user, account)
			setGatewayUserRelationTarget(t, app, mustCollection(t, app, "backend_accounts").Id)
			before := cutoverStateSnapshot(t, app, []string{"gateway_sessions", "item_child_counts"})
			if err := gatewaySessionSingletonCutoverUp(app); err == nil {
				t.Fatal("accepted wrong gateway_user relation target")
			}
			if after := cutoverStateSnapshot(t, app, []string{"gateway_sessions", "item_child_counts"}); !slices.Equal(after, before) {
				t.Fatalf("refused migration mutated state: got %#v want %#v", after, before)
			}
		})
	}
}

func TestGatewaySessionSingletonCutoverUpIsDirectlyIdempotent(t *testing.T) {
	app := newMigrationTestApp(t)
	user, _ := cutoverUserAndAccount(t, app)
	record := core.NewRecord(mustCollection(t, app, "gateway_sessions"))
	setFinalSession(record, user.Id, "idempotent")
	if err := app.Save(record); err != nil {
		t.Fatalf("save final session: %v", err)
	}
	collections := []string{"gateway_sessions", "item_child_counts"}
	beforeFirst := cutoverStateSnapshot(t, app, collections)
	if err := gatewaySessionSingletonCutoverUp(app); err != nil {
		t.Fatalf("first direct final-schema migration: %v", err)
	}
	if afterFirst := cutoverStateSnapshot(t, app, collections); !slices.Equal(afterFirst, beforeFirst) {
		t.Fatalf("first direct final-schema migration changed state: got %#v want %#v", afterFirst, beforeFirst)
	}
	beforeSecond := cutoverStateSnapshot(t, app, collections)
	if err := gatewaySessionSingletonCutoverUp(app); err != nil {
		t.Fatalf("second direct final-schema migration: %v", err)
	}
	if afterSecond := cutoverStateSnapshot(t, app, collections); !slices.Equal(afterSecond, beforeSecond) {
		t.Fatalf("second direct final-schema migration changed state: got %#v want %#v", afterSecond, beforeSecond)
	}
}

func TestGatewaySessionSingletonCutoverDownRefusesWithoutMutation(t *testing.T) {
	app := newMigrationTestApp(t)
	user, _ := cutoverUserAndAccount(t, app)
	record := core.NewRecord(mustCollection(t, app, "gateway_sessions"))
	setFinalSession(record, user.Id, "down")
	if err := app.Save(record); err != nil {
		t.Fatalf("save session: %v", err)
	}
	before := cutoverSnapshot(t, app, []string{"gateway_sessions", "item_child_counts"})
	if err := gatewaySessionSingletonCutoverDown(app); err == nil {
		t.Fatal("down migration succeeded")
	}
	if after := cutoverSnapshot(t, app, []string{"gateway_sessions", "item_child_counts"}); !slices.Equal(after, before) {
		t.Fatalf("down migration mutated data: got %#v want %#v", after, before)
	}
}

func addHistoricalSessionFields(t *testing.T, app core.App, sessions *core.Collection, accountID string) {
	t.Helper()
	sessions.Fields.Add(&core.RelationField{Name: gatewaySessionBackendAccountField, CollectionId: accountID, Required: true, MaxSelect: 1})
	sessions.Fields.Add(&core.TextField{Name: "backend_server_id", Max: 255})
	sessions.Fields.Add(&core.URLField{Name: "backend_base_url", Required: true})
	sessions.Fields.Add(&core.TextField{Name: "backend_user_id", Required: true, Max: 80})
	sessions.Fields.Add(&core.TextField{Name: "backend_username", Max: 255})
	sessions.Fields.Add(&core.TextField{Name: "backend_token_encrypted", Required: true})
	sessions.Fields.Add(&core.TextField{Name: "backend_token", Required: true})
	for _, field := range backendIdentityFieldNames {
		max := 255
		if field == "backend_authorization_version" {
			max = 80
		}
		sessions.Fields.Add(&core.TextField{Name: field, Max: max})
	}
	if err := app.SaveNoValidate(sessions); err != nil {
		t.Fatalf("save historical session schema: %v", err)
	}
}

func addCurrentSessionBackendAccountField(t *testing.T, app core.App, sessions *core.Collection, accountID string) {
	t.Helper()
	sessions.Fields.AddAt(5, &core.RelationField{Name: gatewaySessionBackendAccountField, CollectionId: accountID, Required: true, MaxSelect: 1})
	if err := app.SaveNoValidate(sessions); err != nil {
		t.Fatalf("save current session schema: %v", err)
	}
}

func restoreLegacyItemChildCounts(t *testing.T, app core.App) *core.Collection {
	t.Helper()
	counts := mustCollection(t, app, "item_child_counts")
	counts.Fields.AddAt(1, &core.TextField{Name: itemChildCountAccountField, Required: true, Max: 80})
	counts.RemoveIndex(itemChildCountItemIndex)
	counts.AddIndex(itemChildCountLegacyIndex, true, "backend_account_id, item_id", "")
	if err := app.SaveNoValidate(counts); err != nil {
		t.Fatalf("restore legacy item_child_counts: %v", err)
	}
	return counts
}

func setGatewayUserRelationTarget(t *testing.T, app core.App, targetID string) {
	t.Helper()
	sessions := mustCollection(t, app, "gateway_sessions")
	field, ok := sessions.Fields.GetByName("gateway_user").(*core.RelationField)
	if !ok {
		t.Fatalf("gateway_user field = %T, want relation", sessions.Fields.GetByName("gateway_user"))
	}
	field.CollectionId = targetID
	if err := app.SaveNoValidate(sessions); err != nil {
		t.Fatalf("save wrong gateway_user target: %v", err)
	}
}

func cutoverUserAndAccount(t *testing.T, app core.App) (*core.Record, *core.Record) {
	t.Helper()
	user := core.NewRecord(mustCollection(t, app, "users"))
	user.Set("email", "user-"+time.Now().UTC().Format("150405.000000000")+"@example.test")
	user.Set("username", "user-"+time.Now().UTC().Format("150405.000000000"))
	user.Set("synthetic_user_id", "synthetic-"+time.Now().UTC().Format("150405.000000000"))
	user.Set("enabled", true)
	user.SetPassword("password")
	if err := app.Save(user); err != nil {
		t.Fatalf("save user: %v", err)
	}
	server := core.NewRecord(mustCollection(t, app, "emby_servers"))
	server.Set("name", "server-"+user.Id)
	server.Set("base_url", "https://example.test")
	if err := app.Save(server); err != nil {
		t.Fatalf("save server: %v", err)
	}
	account := core.NewRecord(mustCollection(t, app, "backend_accounts"))
	account.Set("server", server.Id)
	account.Set("name", "account-"+user.Id)
	account.Set("backend_username", "backend")
	account.Set("backend_password", "password")
	if err := app.Save(account); err != nil {
		t.Fatalf("save account: %v", err)
	}
	return user, account
}

func setLegacySession(t *testing.T, record *core.Record, userID, accountID, token string) {
	t.Helper()
	setFinalSession(record, userID, token)
	record.Set("backend_account", accountID)
	record.Set("backend_server_id", "server")
	record.Set("backend_base_url", "https://example.test")
	record.Set("backend_user_id", "backend-user")
	record.Set("backend_username", "backend")
	record.Set("backend_token_encrypted", "encrypted")
	record.Set("backend_token", "token")
	for _, field := range backendIdentityFieldNames {
		record.Set(field, "value")
	}
}

func setFinalSession(record *core.Record, userID, token string) {
	record.Set("gateway_token_hash", token)
	record.Set("gateway_user", userID)
	record.Set("synthetic_user_id", "synthetic")
	record.Set("expires_at", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
}

func seedDurableCutoverData(t *testing.T, app core.App, userID string) {
	t.Helper()
	item := core.NewRecord(mustCollection(t, app, "user_item_data"))
	item.Set("gateway_user", userID)
	item.Set("item_id", "item")
	if err := app.Save(item); err != nil {
		t.Fatalf("save user item data: %v", err)
	}
	playback := core.NewRecord(mustCollection(t, app, "playback_events"))
	playback.Set("gateway_user", userID)
	playback.Set("item_id", "item")
	playback.Set("event", "progress")
	playback.Set("occurred_at", time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := app.Save(playback); err != nil {
		t.Fatalf("save playback event: %v", err)
	}
	pref := core.NewRecord(mustCollection(t, app, "display_preferences"))
	pref.Set("gateway_user", userID)
	pref.Set("preference_id", "pref")
	pref.Set("payload_json", "{}")
	if err := app.Save(pref); err != nil {
		t.Fatalf("save display preference: %v", err)
	}
	audit := core.NewRecord(mustCollection(t, app, "audit_logs"))
	audit.Set("event", "event")
	if err := app.Save(audit); err != nil {
		t.Fatalf("save audit record: %v", err)
	}
	source := newSourceRecord(mustCollection(t, app, upstreamSourcesCollection), "default")
	if err := app.Save(source); err != nil {
		t.Fatalf("save source: %v", err)
	}
	endpoint := newEndpointRecord(mustCollection(t, app, upstreamEndpointsCollection), source.Id, "primary", "https://example.test", true)
	if err := app.Save(endpoint); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}
}

func cutoverSnapshot(t *testing.T, app core.App, collections []string) []string {
	t.Helper()
	var snapshot []string
	for _, name := range collections {
		records, err := app.FindRecordsByFilter(name, "", "id", 0, 0, nil)
		if err != nil {
			t.Fatalf("list %s: %v", name, err)
		}
		for _, record := range records {
			data, err := json.Marshal(record.PublicExport())
			if err != nil {
				t.Fatalf("marshal %s: %v", name, err)
			}
			snapshot = append(snapshot, name+":"+string(data))
		}
	}
	return snapshot
}

func cutoverStateSnapshot(t *testing.T, app core.App, collections []string) []string {
	t.Helper()
	snapshot := cutoverSnapshot(t, app, collections)
	for _, name := range collections {
		collection := mustCollection(t, app, name)
		data, err := json.Marshal(collection)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", name, err)
		}
		snapshot = append(snapshot, name+":schema:"+string(data))
	}
	return snapshot
}

func mustFindRecord(t *testing.T, app core.App, collection, id string) *core.Record {
	t.Helper()
	record, err := app.FindRecordById(collection, id)
	if err != nil {
		t.Fatalf("find %s/%s: %v", collection, id, err)
	}
	return record
}

func assertGatewaySessionCutoverFinalSchema(t *testing.T, app core.App) {
	t.Helper()
	sessions := mustCollection(t, app, "gateway_sessions")
	counts := mustCollection(t, app, "item_child_counts")
	if !isGatewaySessionFinalSchema(sessions, mustCollection(t, app, "users").Id) || !isItemChildCountFinalSchema(counts) {
		t.Fatalf("unexpected final cutover schema: sessions=%#v counts=%#v", sessions, counts)
	}
}
