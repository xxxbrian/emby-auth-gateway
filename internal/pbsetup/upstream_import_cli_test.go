package pbsetup

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

var laneCPreservationCollections = []string{
	"users", "emby_servers", "backend_accounts", "user_mappings", "gateway_sessions",
	"user_item_data", "playback_events", "display_preferences", "audit_logs", "item_child_counts", "path_policies",
}

func TestLaneCImportLegacyCLIApplyPreservesAllCollectionsAndRedactsOutput(t *testing.T) {
	app, legacyServer, account, closeUpstream, cleanupCount := laneCSeedImportApp(t)
	defer closeUpstream()
	before := laneCSnapshotPreservationCollections(t, app)
	expected := laneCExpectedCollectionSummaries(t, app)

	dryRun, stderr, err := laneCExecuteImportCommand(app, legacyServer.Id, account.Id)
	if err != nil {
		t.Fatalf("dry-run command: %v", err)
	}
	if stderr != "" {
		t.Fatalf("dry-run stderr = %q, want empty", stderr)
	}
	drySummary := laneCDecodeSingleSummary(t, dryRun)
	laneCAssertSummary(t, drySummary, legacyServer, account, "dry-run", "create", "logged_out", expected)
	if got := cleanupCount(); got != 1 {
		t.Fatalf("dry-run cleanup count = %d, want 1", got)
	}

	repeated, repeatedStderr, err := laneCExecuteImportCommand(app, legacyServer.Id, account.Id)
	if err != nil {
		t.Fatalf("repeated dry-run command: %v", err)
	}
	if repeated != dryRun || repeatedStderr != "" {
		t.Fatalf("dry-run output was not deterministic: first=%q second=%q stderr=%q", dryRun, repeated, repeatedStderr)
	}

	apply, applyStderr, err := laneCExecuteImportCommand(app, legacyServer.Id, account.Id, "--apply")
	if err != nil {
		t.Fatalf("apply command: %v", err)
	}
	if applyStderr != "" {
		t.Fatalf("apply stderr = %q, want empty", applyStderr)
	}
	applySummary := laneCDecodeSingleSummary(t, apply)
	laneCAssertSummary(t, applySummary, legacyServer, account, "apply", "create", "persisted", expected)
	if got := laneCSnapshotPreservationCollections(t, app); got != before {
		t.Fatalf("preservation records changed after apply:\n got %s\nwant %s", got, before)
	}
	for _, secret := range []string{"gateway-username-secret", "gateway-password-secret", "backend-username-secret", "backend-password-secret", "legacy-token-secret", "new-token-secret", "backend-user-id-secret", "raw-device-id-secret", "payload-secret"} {
		laneCAssertRedacted(t, secret, dryRun, stderr, apply, applyStderr)
	}
}

func TestLaneCImportLegacyCLIValidatesArguments(t *testing.T) {
	app := newTestApp(t)
	for _, args := range [][]string{{}, {"--server-record-id", "server"}, {"--server-record-id", "server", "--account-record-id", "account", "unexpected"}} {
		stdout, _, err := laneCExecuteRawImportCommand(app, args...)
		if err == nil {
			t.Fatalf("args %q succeeded", args)
		}
		if stdout != "" {
			t.Fatalf("args %q wrote success stdout %q", args, stdout)
		}
	}
}

func TestLaneCImportLegacyCLIApplySummaryFailuresCleanUpBeforeWrites(t *testing.T) {
	for _, failure := range []struct {
		name    string
		install func()
		reset   func()
	}{
		{
			name: "read",
			install: func() {
				readImportSummaryRecords = func(core.App, string) ([]*core.Record, error) { return nil, errors.New("lane c summary read failure") }
			},
			reset: func() {
				readImportSummaryRecords = func(app core.App, collection string) ([]*core.Record, error) {
					return app.FindRecordsByFilter(collection, "", "id", 0, 0, nil)
				}
			},
		},
		{
			name: "marshal",
			install: func() {
				marshalImportSummary = func(any) ([]byte, error) { return nil, errors.New("lane c summary marshal failure") }
			},
			reset: func() { marshalImportSummary = json.Marshal },
		},
	} {
		t.Run(failure.name, func(t *testing.T) {
			app, legacyServer, account, closeUpstream, cleanupCount := laneCSeedImportApp(t)
			defer closeUpstream()
			failure.install()
			t.Cleanup(failure.reset)
			stdout, _, err := laneCExecuteImportCommand(app, legacyServer.Id, account.Id, "--apply")
			if err == nil {
				t.Fatal("apply command succeeded")
			}
			if stdout != "" {
				t.Fatalf("failed command wrote success stdout %q", stdout)
			}
			if got := cleanupCount(); got != 1 {
				t.Fatalf("validation token cleanup count = %d, want 1", got)
			}
			if count, countErr := app.CountRecords(upstreamSources); countErr != nil || count != 0 {
				t.Fatalf("source writes = %d, err=%v; want 0", count, countErr)
			}
			if count, countErr := app.CountRecords(upstreamEndpoints); countErr != nil || count != 0 {
				t.Fatalf("endpoint writes = %d, err=%v; want 0", count, countErr)
			}
		})
	}
}

func TestLaneCImportLegacyCLIDryRunLogoutFailuresAreRedactedAndDoNotWrite(t *testing.T) {
	for _, failure := range []struct {
		name   string
		logout func(http.ResponseWriter, *http.Request)
	}{
		{name: "non-2xx", logout: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "logout failure payload-secret", http.StatusBadGateway)
		}},
		{name: "transport", logout: func(_ http.ResponseWriter, _ *http.Request) { panic(http.ErrAbortHandler) }},
	} {
		t.Run(failure.name, func(t *testing.T) {
			app, legacyServer, account, closeUpstream, cleanupCount := laneCSeedImportAppWithLogout(t, failure.logout)
			defer closeUpstream()
			before := laneCSnapshotPreservationCollections(t, app)

			stdout, stderr, err := laneCExecuteImportCommand(app, legacyServer.Id, account.Id)
			if err == nil {
				t.Fatal("dry-run command succeeded despite validation token cleanup failure")
			}
			if stdout != "" {
				t.Fatalf("failed command wrote success stdout %q", stdout)
			}
			if got := cleanupCount(); got != 1 {
				t.Fatalf("validation token cleanup count = %d, want 1", got)
			}
			laneCAssertNoSingletonWrites(t, app)
			if got := laneCSnapshotPreservationCollections(t, app); got != before {
				t.Fatalf("preservation records changed after failed dry-run:\n got %s\nwant %s", got, before)
			}
			laneCAssertRedacted(t, "gateway-username-secret", stdout, stderr, err.Error())
			laneCAssertRedacted(t, "backend-username-secret", stdout, stderr, err.Error())
			laneCAssertRedacted(t, "backend-password-secret", stdout, stderr, err.Error())
			laneCAssertRedacted(t, "legacy-token-secret", stdout, stderr, err.Error())
			laneCAssertRedacted(t, "new-token-secret", stdout, stderr, err.Error())
			laneCAssertRedacted(t, "backend-user-id-secret", stdout, stderr, err.Error())
			laneCAssertRedacted(t, "raw-device-id-secret", stdout, stderr, err.Error())
			laneCAssertRedacted(t, "payload-secret", stdout, stderr, err.Error())
		})
	}
}

func TestLaneCImportLegacyCLINoopWritesOneSummaryAndDoesNotMutate(t *testing.T) {
	app, legacyServer, account, closeUpstream, cleanupCount := laneCSeedImportApp(t)
	defer closeUpstream()

	if _, stderr, err := laneCExecuteImportCommand(app, legacyServer.Id, account.Id, "--apply"); err != nil || stderr != "" {
		t.Fatalf("setup apply: stderr=%q err=%v", stderr, err)
	}
	beforeSingleton := laneCSnapshotSingleton(t, app)
	beforePreservation := laneCSnapshotPreservationCollections(t, app)

	stdout, stderr, err := laneCExecuteImportCommand(app, legacyServer.Id, account.Id, "--apply")
	if err != nil {
		t.Fatalf("no-op command: %v", err)
	}
	if stderr != "" {
		t.Fatalf("no-op stderr = %q, want empty", stderr)
	}
	summary := laneCDecodeSingleSummary(t, stdout)
	laneCAssertSummary(t, summary, legacyServer, account, "apply", "noop", "not_created", laneCExpectedCollectionSummaries(t, app))
	if got := cleanupCount(); got != 0 {
		t.Fatalf("no-op cleanup count = %d, want 0 for protected persisted token", got)
	}
	if got := laneCSnapshotSingleton(t, app); got != beforeSingleton {
		t.Fatalf("singleton records changed after no-op:\n got %s\nwant %s", got, beforeSingleton)
	}
	if got := laneCSnapshotPreservationCollections(t, app); got != beforePreservation {
		t.Fatalf("preservation records changed after no-op:\n got %s\nwant %s", got, beforePreservation)
	}
}

func laneCSeedImportApp(t *testing.T) (core.App, *core.Record, *core.Record, func(), func() int) {
	return laneCSeedImportAppWithLogout(t, func(_ http.ResponseWriter, _ *http.Request) {})
}

func laneCSeedImportAppWithLogout(t *testing.T, logout func(http.ResponseWriter, *http.Request)) (core.App, *core.Record, *core.Record, func(), func() int) {
	t.Helper()
	app := newTestApp(t)
	logouts := 0
	var logoutMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"live-server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"new-token-secret","ServerId":"live-server","User":{"Id":"backend-user-id-secret"}}`))
		case "/Sessions/Logout":
			logoutMu.Lock()
			logouts++
			logoutMu.Unlock()
			logout(w, r)
		}
	}))
	if err := run(app, options{GatewayUsername: "gateway-username-secret", GatewayPassword: "gateway-password-secret", SyntheticUserID: "synthetic", EmbyServerName: "selected", EmbyBaseURL: upstream.URL, BackendAccountName: "selected", BackendUsername: "backend-username-secret", BackendPassword: "backend-password-secret"}); err != nil {
		upstream.Close()
		t.Fatalf("seed legacy setup: %v", err)
	}
	legacyServer, err := app.FindFirstRecordByData("emby_servers", "name", "selected")
	if err != nil {
		t.Fatal(err)
	}
	account, err := app.FindFirstRecordByData("backend_accounts", "name", "selected")
	if err != nil {
		t.Fatal(err)
	}
	legacyServer.Set("backend_authorization_device_id", "raw-device-id-secret")
	legacyServer.Set("backend_user_agent", "")
	legacyServer.Set("backend_authorization_client", "")
	legacyServer.Set("backend_authorization_device", "")
	legacyServer.Set("backend_authorization_version", "")
	if err := app.Save(legacyServer); err != nil {
		t.Fatal(err)
	}
	account.Set("backend_token", "legacy-token-secret")
	account.Set("backend_user_id", "backend-user-id-secret")
	if err := app.Save(account); err != nil {
		t.Fatal(err)
	}
	user, err := app.FindFirstRecordByData("users", "username", "gateway-username-secret")
	if err != nil {
		t.Fatal(err)
	}
	laneCSaveRecord(t, app, "gateway_sessions", map[string]any{"gateway_token_hash": "session-hash", "gateway_user": user.Id, "gateway_username": "gateway-username-secret", "synthetic_user_id": "synthetic", "backend_account": account.Id, "client": "client", "device": "device", "device_id": "device-id", "version": "1", "remote_ip": "127.0.0.1", "expires_at": time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)})
	laneCSaveRecord(t, app, "user_item_data", map[string]any{"gateway_user": user.Id, "synthetic_user_id": "synthetic", "item_id": "item", "item_name": "Item", "item_type": "Episode", "series_id": "series", "series_name": "Series", "index_number": 1, "parent_index_number": 2, "played": true, "playback_position_ticks": 100, "played_percentage": 50, "played_percentage_set": true, "last_played_date": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "play_count": 3, "is_favorite": true, "likes": true, "likes_set": true, "fingerprint": "fingerprint", "last_seen_at": time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)})
	laneCSaveRecord(t, app, "playback_events", map[string]any{"gateway_user": user.Id, "synthetic_user_id": "synthetic", "item_id": "item", "item_name": "Item", "event": "progress", "playback_position_ticks": 100, "played": true, "played_percentage": 50, "remote_ip": "127.0.0.1", "occurred_at": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	laneCSaveRecord(t, app, "display_preferences", map[string]any{"gateway_user": user.Id, "synthetic_user_id": "synthetic", "preference_id": "pref", "client": "client", "payload_json": `{"secret":"payload-secret"}`})
	laneCSaveRecord(t, app, "audit_logs", map[string]any{"gateway_user": user.Id, "synthetic_user_id": "synthetic", "event": "event", "message": "audit message", "method": "GET", "path": "/Items", "status": 200, "remote_ip": "127.0.0.1"})
	laneCSaveRecord(t, app, "item_child_counts", map[string]any{"item_id": "parent", "child_count": 2})
	laneCSaveRecord(t, app, "path_policies", map[string]any{"method": "GET", "path": "/Items/*", "action": "allow", "priority": 7, "reason": "test", "enabled": true})
	return app, legacyServer, account, upstream.Close, func() int { logoutMu.Lock(); defer logoutMu.Unlock(); return logouts }
}

func laneCSaveRecord(t *testing.T, app core.App, collectionName string, fields map[string]any) {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId(collectionName)
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(collection)
	for name, value := range fields {
		record.Set(name, value)
	}
	if err := app.Save(record); err != nil {
		t.Fatalf("save %s: %v", collectionName, err)
	}
}

func laneCExecuteImportCommand(app core.App, args ...string) (string, string, error) {
	if len(args) < 2 {
		return "", "", fmt.Errorf("lane C command requires server and account IDs")
	}
	return laneCExecuteRawImportCommand(app, append([]string{"--server-record-id", args[0], "--account-record-id", args[1]}, args[2:]...)...)
}

func laneCExecuteRawImportCommand(app core.App, args ...string) (string, string, error) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cmd := NewCommand(app)
	cmd.SetArgs(append([]string{"upstream", "import-legacy"}, args...))
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func laneCDecodeSingleSummary(t *testing.T, stdout string) importSummary {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout must contain one JSON document and one newline: %q", stdout)
	}
	var summary importSummary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	return summary
}

func laneCAssertSummary(t *testing.T, summary importSummary, server, account *core.Record, mode, action, disposition string, expected []importCollectionSummary) {
	t.Helper()
	if summary.Mode != mode || summary.Action != action || summary.ValidationToken != disposition {
		t.Fatalf("summary mode/action/disposition = %q/%q/%q, want %q/%q/%q", summary.Mode, summary.Action, summary.ValidationToken, mode, action, disposition)
	}
	normalizedURL, err := normalizeUpstreamURL(server.GetString("base_url"))
	if err != nil {
		t.Fatalf("normalize expected endpoint URL: %v", err)
	}
	if summary.SchemaVersion != importSummaryVersion || summary.ServerRecordID != server.Id || summary.AccountRecordID != account.Id || summary.EndpointURL != normalizedURL || summary.LiveServerID != "live-server" || summary.EndpointKey != primaryEndpointKey {
		t.Fatalf("summary identity fields = %#v", summary)
	}
	if action == "noop" {
		if summary.DeviceIDSource != "stored" || summary.DeviceIDDisposition != "stored" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(summary.DeviceIDFingerprint) {
			t.Fatalf("noop summary device ID fields = %q/%q", summary.DeviceIDSource, summary.DeviceIDFingerprint)
		}
	} else if mode == "dry-run" {
		if summary.DeviceIDDisposition != "generate_on_apply" || summary.DeviceIDFingerprint != "" {
			t.Fatalf("dry-run device ID fields = %q/%q", summary.DeviceIDDisposition, summary.DeviceIDFingerprint)
		}
	} else if summary.DeviceIDSource != "generated" || summary.DeviceIDDisposition != "generated" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(summary.DeviceIDFingerprint) || summary.DeviceIDFingerprint == fmt.Sprintf("%x", sha256.Sum256([]byte("raw-device-id-secret"))) {
		t.Fatalf("summary device ID fields = %q/%q", summary.DeviceIDSource, summary.DeviceIDFingerprint)
	}
	if got, want := strings.Join(summary.IdentityDefaultsApplied, ","), "user_agent,client,device,version"; got != want {
		t.Fatalf("identity defaults = %q, want %q", got, want)
	}
	if summary.SingletonState != map[string]string{"create": "empty", "noop": "present"}[action] || summary.EnabledUsers != 1 || summary.EligibleUsers != 1 || summary.EnabledMappings != 1 || summary.SelectedMappings != 1 || summary.UnrevokedSessions != 1 || summary.ItemChildCountRows != 1 || summary.ItemChildCountScope != "all_items" {
		t.Fatalf("summary singleton/access fields = %#v", summary)
	}
	if len(summary.Collections) != len(expected) {
		t.Fatalf("collection summary count = %d, want %d", len(summary.Collections), len(expected))
	}
	for i := range expected {
		if summary.Collections[i] != expected[i] {
			t.Fatalf("collection summary %d = %#v, want %#v", i, summary.Collections[i], expected[i])
		}
	}
}

func laneCAssertNoSingletonWrites(t *testing.T, app core.App) {
	t.Helper()
	for _, collection := range []string{upstreamSources, upstreamEndpoints} {
		if count, err := app.CountRecords(collection); err != nil || count != 0 {
			t.Fatalf("%s writes = %d, err=%v; want 0", collection, count, err)
		}
	}
}

func laneCAssertRedacted(t *testing.T, secret string, output ...string) {
	t.Helper()
	if strings.Contains(strings.Join(output, "\n"), secret) {
		t.Fatalf("command output exposed %q", secret)
	}
}

func laneCSnapshotSingleton(t *testing.T, app core.App) string {
	t.Helper()
	snapshot := map[string][]map[string]any{}
	for _, collection := range []string{upstreamSources, upstreamEndpoints} {
		records, err := app.FindRecordsByFilter(collection, "", "id", 0, 0, nil)
		if err != nil {
			t.Fatalf("read %s: %v", collection, err)
		}
		for _, record := range records {
			export, err := record.DBExport(app)
			if err != nil {
				t.Fatalf("export %s/%s: %v", collection, record.Id, err)
			}
			snapshot[collection] = append(snapshot[collection], export)
		}
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal singleton snapshot: %v", err)
	}
	return string(data)
}

func laneCExpectedCollectionSummaries(t *testing.T, app core.App) []importCollectionSummary {
	t.Helper()
	result := make([]importCollectionSummary, 0, len(laneCPreservationCollections))
	for _, collection := range laneCPreservationCollections {
		records, err := app.FindRecordsByFilter(collection, "", "id", 0, 0, nil)
		if err != nil {
			t.Fatalf("read %s: %v", collection, err)
		}
		parts := make([]string, 0, len(records))
		for _, record := range records {
			parts = append(parts, strings.Join([]string{record.Id, record.GetDateTime("created").String(), record.GetDateTime("updated").String()}, "\x00"))
		}
		sort.Strings(parts)
		result = append(result, importCollectionSummary{Name: collection, Count: len(records), Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x01"))))})
	}
	return result
}

func laneCSnapshotPreservationCollections(t *testing.T, app core.App) string {
	t.Helper()
	snapshot := map[string][]map[string]any{}
	for _, collection := range laneCPreservationCollections {
		records, err := app.FindRecordsByFilter(collection, "", "id", 0, 0, nil)
		if err != nil {
			t.Fatalf("read %s: %v", collection, err)
		}
		for _, record := range records {
			export, err := record.DBExport(app)
			if err != nil {
				t.Fatalf("export %s/%s: %v", collection, record.Id, err)
			}
			snapshot[collection] = append(snapshot[collection], export)
		}
		sort.Slice(snapshot[collection], func(i, j int) bool {
			return fmt.Sprint(snapshot[collection][i]["id"]) < fmt.Sprint(snapshot[collection][j]["id"])
		})
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal preservation snapshot: %v", err)
	}
	return string(data)
}
