package pbsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func TestImportLegacyDryRunCreateApplyAndNoop(t *testing.T) {
	app := newTestApp(t)
	logouts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"live-server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"validation-token","ServerId":"live-server","User":{"Id":"backend-user"}}`))
		case "/Sessions/Logout":
			logouts++
		}
	}))
	defer server.Close()
	legacy := options{GatewayUsername: "gateway", GatewayPassword: "password", SyntheticUserID: "synthetic", EmbyServerName: "selected", EmbyBaseURL: server.URL, BackendAccountName: "selected", BackendUsername: "backend", BackendPassword: "secret"}
	if err := run(app, legacy); err != nil {
		t.Fatal(err)
	}
	legacyServer, _ := app.FindFirstRecordByData("emby_servers", "name", "selected")
	account, _ := app.FindFirstRecordByData("backend_accounts", "name", "selected")
	opts := importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id}
	summary, err := runUpstreamImportLegacy(context.Background(), app, opts, &bytes.Buffer{})
	if err != nil || summary.Action != "create" || summary.Mode != "dry-run" || summary.ValidationToken != "logged_out" {
		t.Fatalf("dry-run summary=%+v err=%v", summary, err)
	}
	if count, _ := app.CountRecords(upstreamSources); count != 0 || logouts != 1 {
		t.Fatalf("dry run mutated source=%d logout=%d", count, logouts)
	}
	opts.Apply = true
	summary, err = runUpstreamImportLegacy(context.Background(), app, opts, &bytes.Buffer{})
	if err != nil || summary.Action != "create" || summary.ValidationToken != "persisted" {
		t.Fatalf("apply summary=%+v err=%v", summary, err)
	}
	source, err := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if err != nil || source.GetString("backend_token") != "validation-token" {
		t.Fatalf("missing persisted import: %v", err)
	}
	opts.Apply = false
	summary, err = runUpstreamImportLegacy(context.Background(), app, opts, &bytes.Buffer{})
	if err != nil || summary.Action != "noop" || logouts != 1 {
		t.Fatalf("noop summary=%+v err=%v logouts=%d", summary, err, logouts)
	}
	data, _ := json.Marshal(summary)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), "validation-token") {
		t.Fatal("summary exposed secret")
	}
}

func TestImportLegacyRejectsAccessExpansion(t *testing.T) {
	app := newTestApp(t)
	server := newUpstreamResponder(t)
	defer server.Close()
	if err := run(app, options{GatewayUsername: "gateway", GatewayPassword: "password", SyntheticUserID: "synthetic", EmbyServerName: "selected", EmbyBaseURL: server.URL, BackendAccountName: "selected", BackendUsername: "backend", BackendPassword: "secret"}); err != nil {
		t.Fatal(err)
	}
	userCollection, _ := app.FindCollectionByNameOrId("users")
	user := core.NewRecord(userCollection)
	user.Set("username", "unmapped")
	user.SetEmail("unmapped@gateway.local")
	user.SetPassword("password")
	user.Set("synthetic_user_id", "unmapped")
	user.Set("enabled", true)
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}
	serverRecord, _ := app.FindFirstRecordByData("emby_servers", "name", "selected")
	account, _ := app.FindFirstRecordByData("backend_accounts", "name", "selected")
	if _, err := runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: serverRecord.Id, AccountRecordID: account.Id}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), user.Id) {
		t.Fatal("unmapped enabled user was accepted or not identified")
	}
}

func TestImportLegacySummaryReadFailureFailsClosed(t *testing.T) {
	app := newTestApp(t)
	readImportSummaryRecords = func(core.App, string) ([]*core.Record, error) { return nil, errors.New("summary read failed") }
	t.Cleanup(func() {
		readImportSummaryRecords = func(app core.App, collection string) ([]*core.Record, error) {
			return app.FindRecordsByFilter(collection, "", "id", 0, 0, nil)
		}
	})
	if _, _, _, _, err := importCollectionSummaries(app, "account"); err == nil {
		t.Fatal("summary read failure was ignored")
	}
}

func TestImportCollectionSummariesFinalChildCountScope(t *testing.T) {
	app := newTestApp(t)
	counts, err := app.FindCollectionByNameOrId("item_child_counts")
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []string{"one", "two"} {
		record := core.NewRecord(counts)
		record.Set("item_id", itemID)
		record.Set("child_count", 1)
		if err := app.Save(record); err != nil {
			t.Fatalf("save final child count: %v", err)
		}
	}

	_, _, childCounts, scope, err := importCollectionSummaries(app, "selected")
	if err != nil {
		t.Fatal(err)
	}
	if childCounts != 2 || scope != "all_items" {
		t.Fatalf("final child-count summary = %d/%q, want 2/all_items", childCounts, scope)
	}
}

func TestImportCollectionSummariesLegacyChildCountScope(t *testing.T) {
	app := newTestApp(t)
	counts, err := app.FindCollectionByNameOrId("item_child_counts")
	if err != nil {
		t.Fatal(err)
	}
	counts.Fields.Add(&core.TextField{Name: "backend_account_id", Max: 80})
	if err := app.Save(counts); err != nil {
		t.Fatalf("add legacy child-count field: %v", err)
	}
	for _, accountID := range []string{"selected", "other"} {
		record := core.NewRecord(counts)
		record.Set("backend_account_id", accountID)
		record.Set("item_id", accountID+"-item")
		record.Set("child_count", 1)
		if err := app.Save(record); err != nil {
			t.Fatalf("save legacy child count: %v", err)
		}
	}

	_, _, childCounts, scope, err := importCollectionSummaries(app, "selected")
	if err != nil {
		t.Fatal(err)
	}
	if childCounts != 1 || scope != "selected_account" {
		t.Fatalf("legacy child-count summary = %d/%q, want 1/selected_account", childCounts, scope)
	}
}

func TestImportFingerprintMarshalFailurePropagates(t *testing.T) {
	app := newTestApp(t)
	marshalImportSnapshot = func(any) ([]byte, error) { return nil, errors.New("snapshot marshal failed") }
	t.Cleanup(func() { marshalImportSnapshot = json.Marshal })
	server := newUpstreamResponder(t)
	defer server.Close()
	if err := run(app, options{GatewayUsername: "gateway", GatewayPassword: "password", SyntheticUserID: "synthetic", EmbyServerName: "selected", EmbyBaseURL: server.URL, BackendAccountName: "selected", BackendUsername: "backend", BackendPassword: "secret"}); err != nil {
		t.Fatal(err)
	}
	legacyServer, _ := app.FindFirstRecordByData("emby_servers", "name", "selected")
	account, _ := app.FindFirstRecordByData("backend_accounts", "name", "selected")
	if _, err := loadImportPlan(app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id}); err == nil {
		t.Fatal("snapshot marshal failure was ignored")
	}
}

func TestImportLegacySummaryMarshalFailureOccursBeforeApply(t *testing.T) {
	app := newTestApp(t)
	logouts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server"}`))
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"token","ServerId":"server","User":{"Id":"user"}}`))
		case "/Sessions/Logout":
			logouts++
		}
	}))
	defer server.Close()
	if err := run(app, options{GatewayUsername: "gateway", GatewayPassword: "password", SyntheticUserID: "synthetic", EmbyServerName: "selected", EmbyBaseURL: server.URL, BackendAccountName: "selected", BackendUsername: "backend", BackendPassword: "secret"}); err != nil {
		t.Fatal(err)
	}
	legacyServer, _ := app.FindFirstRecordByData("emby_servers", "name", "selected")
	account, _ := app.FindFirstRecordByData("backend_accounts", "name", "selected")
	marshalImportSummary = func(any) ([]byte, error) { return nil, errors.New("summary marshal failed") }
	t.Cleanup(func() { marshalImportSummary = json.Marshal })
	if _, err := runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &bytes.Buffer{}); err == nil {
		t.Fatal("marshal failure succeeded")
	}
	if count, _ := app.CountRecords(upstreamSources); count != 0 || logouts != 1 {
		t.Fatalf("summary failure wrote source=%d cleanup=%d", count, logouts)
	}
}
