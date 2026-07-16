package pbstore

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	_ "github.com/xxxbrian/emby-auth-gateway/internal/pbmigrations"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
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

func TestUpstreamRuntimeLoadAndAuthCAS(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	sourceCollection, err := app.FindCollectionByNameOrId("upstream_sources")
	if err != nil {
		t.Fatalf("find source collection: %v", err)
	}
	source := core.NewRecord(sourceCollection)
	for field, value := range map[string]any{
		"key": "default", "server_id": "server", "backend_username": "backend", "backend_password": "password",
		"backend_user_agent": "agent", "backend_authorization_client": "client", "backend_authorization_device": "device", "backend_authorization_device_id": "device-id", "backend_authorization_version": "1.0",
	} {
		source.Set(field, value)
	}
	if err := app.Save(source); err != nil {
		t.Fatalf("save source: %v", err)
	}
	endpointCollection, err := app.FindCollectionByNameOrId("upstream_endpoints")
	if err != nil {
		t.Fatalf("find endpoint collection: %v", err)
	}
	endpoint := core.NewRecord(endpointCollection)
	endpoint.Set("source", source.Id)
	endpoint.Set("key", "primary")
	endpoint.Set("base_url", "https://emby.example")
	endpoint.Set("active", true)
	if err := app.Save(endpoint); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}
	inactive := core.NewRecord(endpointCollection)
	inactive.Set("source", source.Id)
	inactive.Set("key", "backup")
	inactive.Set("base_url", "https://backup.example")
	inactive.Set("active", false)
	if err := app.Save(inactive); err != nil {
		t.Fatalf("save inactive endpoint: %v", err)
	}
	source.Set("server_name", "immutable server")
	source.Set("last_login_error", "old error")
	if err := app.Save(source); err != nil {
		t.Fatalf("update source fixture: %v", err)
	}

	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil || runtime.Source.ID != source.Id || runtime.Endpoint.ID != endpoint.Id {
		t.Fatalf("load runtime = %#v, %v", runtime, err)
	}
	at := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	update := gateway.UpstreamAuthUpdate{SourceID: source.Id, GenerationID: "generation-1", DeviceID: "device-2", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: at}
	if err := store.CompareAndSwapUpstreamAuth(context.Background(), update); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	runtime, err = store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil || runtime.Source.AuthGenerationID != "generation-1" || runtime.Source.ClientIdentity.DeviceID != "device-2" || runtime.Source.TokenUpdatedAt == nil || !runtime.Source.TokenUpdatedAt.Equal(at) || runtime.Source.LastLoginError != "" || runtime.Source.ServerName != "immutable server" {
		t.Fatalf("updated runtime = %#v, %v", runtime, err)
	}
	if got, err := app.FindRecordById("upstream_endpoints", inactive.Id); err != nil || got.GetString("base_url") != "https://backup.example" {
		t.Fatalf("CAS changed inactive endpoint: %#v, %v", got, err)
	}
	if err := store.CompareAndSwapUpstreamAuth(context.Background(), update); !errors.Is(err, gateway.ErrUpstreamAuthConflict) {
		t.Fatalf("stale CAS error = %v", err)
	}
}

func TestUpstreamRuntimeRejectsMalformedInactiveEndpointAndForceQuery(t *testing.T) {
	for _, baseURL := range []string{"https://backup.example?", "https://backup.example?query=value"} {
		t.Run(baseURL, func(t *testing.T) {
			app := newTestApp(t)
			source := createUpstreamSource(t, app)
			createUpstreamEndpoint(t, app, source.Id, "primary", "https://emby.example", true)
			createUpstreamEndpoint(t, app, source.Id, "backup", baseURL, false)
			if _, err := New(app).LoadDefaultUpstreamRuntime(context.Background()); !errors.Is(err, gateway.ErrInvalidUpstreamTopology) {
				t.Fatalf("invalid endpoint error = %v", err)
			}
		})
	}
}

func TestUpstreamAuthCASCanceledDoesNotMutate(t *testing.T) {
	app := newTestApp(t)
	source := createUpstreamSource(t, app)
	createUpstreamEndpoint(t, app, source.Id, "primary", "https://emby.example", true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New(app).CompareAndSwapUpstreamAuth(ctx, gateway.UpstreamAuthUpdate{SourceID: source.Id, GenerationID: "generation", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled CAS error = %v", err)
	}
	record, err := app.FindRecordById("upstream_sources", source.Id)
	if err != nil || record.GetString("auth_generation_id") != "" || record.GetString("backend_token") != "" {
		t.Fatalf("canceled CAS mutated source: %#v, %v", record, err)
	}
}

func TestUpstreamAuthCASZeroRowsClassifiesNotFoundTopologyAndConflict(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		app := newTestApp(t)
		err := New(app).CompareAndSwapUpstreamAuth(context.Background(), gateway.UpstreamAuthUpdate{SourceID: "missing", GenerationID: "generation", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()})
		if !errors.Is(err, gateway.ErrUpstreamNotFound) {
			t.Fatalf("not found CAS error = %v", err)
		}
	})
	t.Run("topology", func(t *testing.T) {
		app := newTestApp(t)
		source := createUpstreamSource(t, app)
		err := New(app).CompareAndSwapUpstreamAuth(context.Background(), gateway.UpstreamAuthUpdate{SourceID: source.Id, ExpectedGenerationID: "old", GenerationID: "generation", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()})
		if !errors.Is(err, gateway.ErrInvalidUpstreamTopology) {
			t.Fatalf("topology CAS error = %v", err)
		}
	})
	t.Run("conflict", func(t *testing.T) {
		app := newTestApp(t)
		source := createUpstreamSource(t, app)
		createUpstreamEndpoint(t, app, source.Id, "primary", "https://emby.example", true)
		err := New(app).CompareAndSwapUpstreamAuth(context.Background(), gateway.UpstreamAuthUpdate{SourceID: source.Id, ExpectedGenerationID: "old", GenerationID: "generation", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()})
		if !errors.Is(err, gateway.ErrUpstreamAuthConflict) {
			t.Fatalf("conflict CAS error = %v", err)
		}
	})
}

func TestUpstreamRuntimePreContractAndManagedAuthValidation(t *testing.T) {
	app := newTestApp(t)
	source := createUpstreamSource(t, app)
	createUpstreamEndpoint(t, app, source.Id, "primary", "https://emby.example", true)
	source.Set("backend_user_id", "stale-user")
	source.Set("backend_token", "stale-token")
	if err := app.Save(source); err != nil {
		t.Fatalf("save pre-contract source: %v", err)
	}
	if _, err := New(app).LoadDefaultUpstreamRuntime(context.Background()); err != nil {
		t.Fatalf("load pre-contract source: %v", err)
	}
	source.Set("auth_generation_id", "managed")
	if err := app.Save(source); err != nil {
		t.Fatalf("save managed source: %v", err)
	}
	if _, err := New(app).LoadDefaultUpstreamRuntime(context.Background()); !errors.Is(err, gateway.ErrInvalidUpstreamTopology) {
		t.Fatalf("malformed managed source error = %v", err)
	}
}

func TestUpstreamAuthCASConcurrentOneWinner(t *testing.T) {
	app := newTestApp(t)
	source := createUpstreamSource(t, app)
	createUpstreamEndpoint(t, app, source.Id, "primary", "https://emby.example", true)
	store := New(app)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, generation := range []string{"generation-a", "generation-b"} {
		wg.Add(1)
		go func(generation string) {
			defer wg.Done()
			<-start
			errs <- store.CompareAndSwapUpstreamAuth(context.Background(), gateway.UpstreamAuthUpdate{SourceID: source.Id, GenerationID: generation, DeviceID: "device-2", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()})
		}(generation)
	}
	close(start)
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, gateway.ErrUpstreamAuthConflict) {
			t.Fatalf("concurrent CAS error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent CAS winners = %d, want 1", winners)
	}
	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil || (runtime.Source.AuthGenerationID != "generation-a" && runtime.Source.AuthGenerationID != "generation-b") || runtime.Endpoint.BaseURL != "https://emby.example" || runtime.Source.ServerID != "server" {
		t.Fatalf("unexpected post-concurrent runtime %#v, %v", runtime, err)
	}
}

func TestUpdateUpstreamServerInfo(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	source := createUpstreamSource(t, app)
	createUpstreamEndpoint(t, app, source.Id, "primary", "https://emby.example", true)
	source.Set("server_name", "old name")
	source.Set("server_version", "old version")
	source.Set("backend_token", "old-token")
	if err := app.Save(source); err != nil {
		t.Fatalf("save fixture: %v", err)
	}
	at := time.Date(2026, 7, 16, 12, 0, 0, 123456789, time.FixedZone("offset", 3600))
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "server", ServerName: "new name", CheckedAt: at}); err != nil {
		t.Fatalf("update name: %v", err)
	}
	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	wantCheckedAt := at.UTC().Truncate(time.Millisecond)
	if err != nil || runtime.Source.ServerName != "new name" || runtime.Source.ServerVersion != "old version" || runtime.Source.VersionCheckedAt == nil || !runtime.Source.VersionCheckedAt.Equal(wantCheckedAt) || runtime.Source.BackendToken != "old-token" {
		t.Fatalf("updated runtime = %#v, %v", runtime, err)
	}
	record, err := app.FindRecordById("upstream_sources", source.Id)
	rawCheckedAt, ok := record.GetRaw("version_checked_at").(types.DateTime)
	if err != nil || !ok || rawCheckedAt.String() != wantCheckedAt.Format(types.DefaultDateLayout) {
		t.Fatalf("canonical checked timestamp = %#v, %v", record.GetRaw("version_checked_at"), err)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "server", ServerVersion: "new version", CheckedAt: at.Add(time.Hour)}); err != nil {
		t.Fatalf("update version: %v", err)
	}
	runtime, err = store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil || runtime.Source.ServerName != "new name" || runtime.Source.ServerVersion != "new version" {
		t.Fatalf("empty preserve runtime = %#v, %v", runtime, err)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "server", ServerName: "new name", ServerVersion: "new version", CheckedAt: at.Add(time.Hour)}); err != nil {
		t.Fatalf("exact no-op update: %v", err)
	}
	unicodeName := strings.Repeat("界", 255)
	unicodeVersion := strings.Repeat("界", 80)
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "server", ServerName: unicodeName, ServerVersion: unicodeVersion, CheckedAt: at}); err != nil {
		t.Fatalf("unicode boundary update: %v", err)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "server", ServerName: unicodeName + "界", CheckedAt: at}); !errors.Is(err, gateway.ErrBadRequest) {
		t.Fatalf("unicode name max+1 error = %v", err)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "server", ServerVersion: unicodeVersion + "界", CheckedAt: at}); !errors.Is(err, gateway.ErrBadRequest) {
		t.Fatalf("unicode version max+1 error = %v", err)
	}
}

func TestUpdateUpstreamServerInfoErrorsAndConcurrentAuth(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	source := createUpstreamSource(t, app)
	endpoint := createUpstreamEndpoint(t, app, source.Id, "primary", "https://emby.example", true)
	backup := createUpstreamEndpoint(t, app, source.Id, "backup", "https://backup.example", false)
	source.Set("server_name", "old name")
	source.Set("server_version", "old version")
	source.Set("last_login_error", "old error")
	if err := app.Save(source); err != nil {
		t.Fatalf("save concurrent fixture: %v", err)
	}
	before, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatalf("load concurrent fixture: %v", err)
	}
	endpointSnapshots := map[string]upstreamEndpointSnapshot{}
	for _, record := range []*core.Record{endpoint, backup} {
		endpointSnapshots[record.Id] = upstreamEndpointSnapshot{source: record.GetString("source"), key: record.GetString("key"), baseURL: record.GetString("base_url"), active: record.GetBool("active")}
	}
	metadataAt := time.Date(2026, 7, 16, 12, 0, 0, 123456789, time.FixedZone("offset", 3600))
	authenticatedAt := time.Date(2026, 7, 16, 12, 1, 0, 987654321, time.FixedZone("offset", -3600))
	update := gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "server", CheckedAt: time.Now()}
	for _, invalid := range []gateway.UpstreamServerInfoUpdate{{SourceID: " source", ServerID: "server", CheckedAt: time.Now()}, {SourceID: source.Id, ServerID: " server", CheckedAt: time.Now()}, {SourceID: source.Id, ServerID: "server", ServerName: string(make([]byte, 256)), CheckedAt: time.Now()}, {SourceID: source.Id, ServerID: "server", ServerVersion: string(make([]byte, 81)), CheckedAt: time.Now()}, {SourceID: source.Id, ServerID: "server"}} {
		if err := store.UpdateUpstreamServerInfo(context.Background(), invalid); !errors.Is(err, gateway.ErrBadRequest) {
			t.Fatalf("invalid update error = %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.UpdateUpstreamServerInfo(ctx, update); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled update error = %v", err)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: "missing", ServerID: "server", CheckedAt: time.Now()}); !errors.Is(err, gateway.ErrUpstreamNotFound) {
		t.Fatalf("missing source error = %v", err)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "other", CheckedAt: time.Now()}); !errors.Is(err, gateway.ErrUpstreamServerInfoConflict) {
		t.Fatalf("namespace mismatch error = %v", err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "server", ServerName: "new", ServerVersion: "1.2", CheckedAt: metadataAt})
	}()
	go func() {
		<-start
		errs <- store.CompareAndSwapUpstreamAuth(context.Background(), gateway.UpstreamAuthUpdate{SourceID: source.Id, GenerationID: "generation", DeviceID: "new-device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: authenticatedAt})
	}()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	metadataCheckedAt := metadataAt.UTC().Truncate(time.Millisecond)
	authenticatedUTC := authenticatedAt.UTC()
	want := before.Source
	want.ServerName, want.ServerVersion, want.VersionCheckedAt = "new", "1.2", &metadataCheckedAt
	want.AuthGenerationID, want.ClientIdentity.DeviceID, want.BackendUserID, want.BackendToken = "generation", "new-device", "user", "token"
	want.TokenUpdatedAt, want.LastLoginAt, want.LastLoginError = &authenticatedUTC, &authenticatedUTC, ""
	if err != nil || !reflect.DeepEqual(runtime.Source, want) {
		t.Fatalf("concurrent updates lost data: %#v, %v", runtime, err)
	}
	for id, wantEndpoint := range endpointSnapshots {
		record, err := app.FindRecordById("upstream_endpoints", id)
		if err != nil {
			t.Fatalf("find endpoint %s: %v", id, err)
		}
		gotEndpoint := upstreamEndpointSnapshot{source: record.GetString("source"), key: record.GetString("key"), baseURL: record.GetString("base_url"), active: record.GetBool("active")}
		if gotEndpoint != wantEndpoint {
			t.Fatalf("endpoint %s changed: %#v, %v", id, gotEndpoint, err)
		}
	}
	if err := app.Delete(endpoint); err != nil {
		t.Fatalf("delete endpoint: %v", err)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: source.Id, ServerID: "other", CheckedAt: time.Now()}); !errors.Is(err, gateway.ErrInvalidUpstreamTopology) {
		t.Fatalf("malformed topology error = %v", err)
	}
}

type upstreamEndpointSnapshot struct {
	source  string
	key     string
	baseURL string
	active  bool
}

func TestUpdateUpstreamServerInfoMissingSchemaIsUnavailable(t *testing.T) {
	app := newTestApp(t)
	endpoints, err := app.FindCollectionByNameOrId("upstream_endpoints")
	if err != nil {
		t.Fatalf("find endpoint collection: %v", err)
	}
	if err := app.Delete(endpoints); err != nil {
		t.Fatalf("delete endpoint collection: %v", err)
	}
	collection, err := app.FindCollectionByNameOrId("upstream_sources")
	if err != nil {
		t.Fatalf("find source collection: %v", err)
	}
	if err := app.Delete(collection); err != nil {
		t.Fatalf("delete source collection: %v", err)
	}
	err = New(app).UpdateUpstreamServerInfo(context.Background(), gateway.UpstreamServerInfoUpdate{SourceID: "123456789012345", ServerID: "server", CheckedAt: time.Now()})
	if !errors.Is(err, gateway.ErrStoreUnavailable) {
		t.Fatalf("missing schema error = %v", err)
	}
}

func TestUpstreamRuntimeReadIsCoherentDuringTransactionUpdate(t *testing.T) {
	app := newTestApp(t)
	source := createUpstreamSource(t, app)
	source.Set("server_name", "old-server")
	if err := app.Save(source); err != nil {
		t.Fatalf("save source: %v", err)
	}
	endpoint := createUpstreamEndpoint(t, app, source.Id, "primary", "https://old.example", true)
	sourceUpdated := make(chan struct{})
	continueUpdate := make(chan struct{})
	writerErr := make(chan error, 1)
	go func() {
		writerErr <- app.RunInTransaction(func(tx core.App) error {
			txSource, err := tx.FindRecordById("upstream_sources", source.Id)
			if err != nil {
				return err
			}
			txSource.Set("server_name", "new-server")
			if err := tx.Save(txSource); err != nil {
				return err
			}
			close(sourceUpdated)
			<-continueUpdate
			txEndpoint, err := tx.FindRecordById("upstream_endpoints", endpoint.Id)
			if err != nil {
				return err
			}
			txEndpoint.Set("base_url", "https://new.example")
			return tx.Save(txEndpoint)
		})
	}()
	select {
	case <-sourceUpdated:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not update source")
	}
	loaderStarted := make(chan struct{})
	loaderResult := make(chan *gateway.UpstreamRuntime, 1)
	loaderErr := make(chan error, 1)
	go func() {
		close(loaderStarted)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runtime, err := New(app).LoadDefaultUpstreamRuntime(ctx)
		loaderResult <- runtime
		loaderErr <- err
	}()
	select {
	case <-loaderStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("loader did not start")
	}
	close(continueUpdate)
	select {
	case err := <-writerErr:
		if err != nil {
			t.Fatalf("writer transaction: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer transaction did not finish")
	}
	var runtime *gateway.UpstreamRuntime
	var err error
	select {
	case runtime = <-loaderResult:
		err = <-loaderErr
	case <-time.After(5 * time.Second):
		t.Fatal("loader did not finish")
	}
	if err != nil {
		t.Fatalf("concurrent loader: %v", err)
	}
	old := runtime.Source.ServerName == "old-server" && runtime.Endpoint.BaseURL == "https://old.example"
	new := runtime.Source.ServerName == "new-server" && runtime.Endpoint.BaseURL == "https://new.example"
	if !old && !new {
		t.Fatalf("mixed runtime snapshot: %#v", runtime)
	}
}

func createUpstreamSource(t *testing.T, app core.App) *core.Record {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId("upstream_sources")
	if err != nil {
		t.Fatalf("find source collection: %v", err)
	}
	record := core.NewRecord(collection)
	for field, value := range map[string]any{
		"key": "default", "server_id": "server", "backend_username": "backend", "backend_password": "password",
		"backend_user_agent": "agent", "backend_authorization_client": "client", "backend_authorization_device": "device", "backend_authorization_device_id": "device-id", "backend_authorization_version": "1.0",
	} {
		record.Set(field, value)
	}
	if err := app.Save(record); err != nil {
		t.Fatalf("save source: %v", err)
	}
	return record
}

func createUpstreamEndpoint(t *testing.T, app core.App, sourceID, key, baseURL string, active bool) *core.Record {
	t.Helper()
	collection, err := app.FindCollectionByNameOrId("upstream_endpoints")
	if err != nil {
		t.Fatalf("find endpoint collection: %v", err)
	}
	record := core.NewRecord(collection)
	record.Set("source", sourceID)
	record.Set("key", key)
	record.Set("base_url", baseURL)
	record.Set("active", active)
	if err := app.Save(record); err != nil {
		t.Fatalf("save endpoint: %v", err)
	}
	return record
}

func TestUpstreamRuntimeMissingSchemaIsUnavailable(t *testing.T) {
	app := newTestApp(t)
	collection, err := app.FindCollectionByNameOrId("upstream_endpoints")
	if err != nil {
		t.Fatalf("find endpoint collection: %v", err)
	}
	if err := app.Delete(collection); err != nil {
		t.Fatalf("delete endpoint collection: %v", err)
	}
	if _, err := New(app).LoadDefaultUpstreamRuntime(context.Background()); !errors.Is(err, gateway.ErrStoreUnavailable) {
		t.Fatalf("missing schema error = %v", err)
	}
	if err := New(app).CompareAndSwapUpstreamAuth(context.Background(), gateway.UpstreamAuthUpdate{SourceID: "source", GenerationID: "generation", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()}); !errors.Is(err, gateway.ErrStoreUnavailable) {
		t.Fatalf("missing schema CAS error = %v", err)
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

func TestPlaybackStateBatchLookupChunksLargeIDLists(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	for i := 0; i < playbackStateItemIDBatchLimit+1; i++ {
		id := "item-" + strconv.Itoa(i)
		if err := store.SavePlaybackState(context.Background(), gateway.PlaybackState{GatewayUserID: userID, SyntheticUserID: "gateway-user", ItemID: id, PlaybackPositionTicks: int64(i + 1)}); err != nil {
			t.Fatalf("save playback state: %v", err)
		}
	}
	ids := make([]string, 0, playbackStateItemIDBatchLimit+1)
	for i := 0; i < playbackStateItemIDBatchLimit+1; i++ {
		ids = append(ids, "item-"+strconv.Itoa(i))
	}
	states, err := store.ListPlaybackStatesByItemIDs(context.Background(), userID, ids)
	if err != nil {
		t.Fatalf("batch lookup playback states: %v", err)
	}
	lastID := "item-" + strconv.Itoa(playbackStateItemIDBatchLimit)
	last := states[lastID]
	if len(states) != playbackStateItemIDBatchLimit+1 || last == nil || last.PlaybackPositionTicks != int64(playbackStateItemIDBatchLimit+1) {
		t.Fatalf("unexpected chunked states len=%d last=%#v", len(states), last)
	}
}

func TestItemChildCountsAreSingletonByItemID(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	ctx := context.Background()

	if err := store.SaveItemChildCount(ctx, gateway.ItemChildCount{ItemID: "show-1", ChildCount: 12}); err != nil {
		t.Fatalf("save initial child count: %v", err)
	}
	if err := store.SaveItemChildCount(ctx, gateway.ItemChildCount{ItemID: "show-1", ChildCount: 99}); err != nil {
		t.Fatalf("save replacement child count: %v", err)
	}
	if err := store.SaveItemChildCount(ctx, gateway.ItemChildCount{ItemID: "movie-1", ChildCount: 4}); err != nil {
		t.Fatalf("save second item child count: %v", err)
	}
	if err := store.SaveItemChildCount(ctx, gateway.ItemChildCount{ItemID: "show-1", ChildCount: 13}); err != nil {
		t.Fatalf("update child count: %v", err)
	}

	itemIDs := make([]string, playbackStateItemIDBatchLimit+1)
	for i := range itemIDs {
		itemIDs[i] = "missing-" + strconv.Itoa(i)
	}
	itemIDs[0] = "show-1"
	itemIDs[len(itemIDs)-1] = "movie-1"
	counts, err := store.ListItemChildCounts(ctx, itemIDs)
	if err != nil {
		t.Fatalf("list child counts: %v", err)
	}
	if len(counts) != 2 || counts["show-1"].ChildCount != 13 || counts["movie-1"].ChildCount != 4 {
		t.Fatalf("unexpected singleton counts: %#v", counts)
	}
	records, err := app.FindRecordsByFilter("item_child_counts", "item_id = 'show-1'", "", 0, 0, nil)
	if err != nil {
		t.Fatalf("query show child counts: %v", err)
	}
	if len(records) != 1 || records[0].GetInt("child_count") != 13 {
		t.Fatalf("show child count records = %#v, want one current record", records)
	}
	collection, err := app.FindCollectionByNameOrId("item_child_counts")
	if err != nil || collection.Fields.GetByName("backend_account_id") != nil {
		t.Fatalf("item_child_counts retained account scope, collection=%#v err=%v", collection, err)
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

	if err := store.RecordAudit(context.Background(), gateway.AuditLog{GatewayUserID: userID, SyntheticUserID: "gateway-user", Event: "login_success", Message: "login succeeded", RemoteIP: "127.0.0.1", Method: "POST", Path: "/Users/AuthenticateByName", Status: 200, ErrorKind: "upstream_read_error", Direction: "upstream", BytesTransferred: 123, DurationMS: 45, UpstreamStatus: 206, ResponseCommitted: true}); err != nil {
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
	if audits[0].GetString("synthetic_user_id") != "gateway-user" || audits[0].GetString("method") != "POST" || audits[0].GetString("path") != "/Users/AuthenticateByName" || audits[0].GetInt("status") != 200 || audits[0].GetString("error_kind") != "upstream_read_error" || audits[0].GetString("direction") != "upstream" || audits[0].GetInt("bytes_transferred") != 123 || audits[0].GetInt("duration_ms") != 45 || audits[0].GetInt("upstream_status") != 206 || !audits[0].GetBool("response_committed") {
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

func TestBackendAccountUsesPlainCredentialsAndClientIdentity(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	accountID := createBackendAccount(t, app)

	account, err := store.FindBackendAccountByID(context.Background(), accountID)
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
	if err := store.UpdateBackendToken(context.Background(), accountID, "backend-token", "backend-user", now); err != nil {
		t.Fatalf("update backend token: %v", err)
	}
}

func TestSessionPersistsGatewayOnlyFieldsAndRevokes(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	session := &gateway.Session{
		GatewayTokenHash: "hash",
		GatewayUserID:    userID,
		GatewayUsername:  "alice",
		SyntheticUserID:  "gateway-user",
		Client:           "Emby Web",
		Device:           "Desktop",
		DeviceID:         "device-1",
		Version:          "4.8.0",
		RemoteIP:         "192.0.2.1",
		ExpiresAt:        now.Add(time.Hour),
	}
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatalf("save session: %v", err)
	}
	saved, err := app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", "hash")
	if err != nil {
		t.Fatalf("find raw session: %v", err)
	}
	collection, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find gateway_sessions collection: %v", err)
	}
	for _, field := range []string{
		"backend_account", "backend_token", "backend_server_id", "backend_base_url", "backend_user_id", "backend_username",
		"backend_user_agent", "backend_authorization_client", "backend_authorization_device", "backend_authorization_device_id", "backend_authorization_version", "backend_token_encrypted",
	} {
		if collection.Fields.GetByName(field) != nil || saved.GetRaw(field) != nil {
			t.Fatalf("gateway_sessions retained upstream field %q: schema=%#v raw=%#v", field, collection.Fields.GetByName(field), saved.GetRaw(field))
		}
	}

	found, err := store.FindSessionByTokenHash(context.Background(), "hash")
	if err != nil {
		t.Fatalf("find session: %v", err)
	}
	if found.GatewayUserID != userID || found.GatewayUsername != "alice" || found.SyntheticUserID != "gateway-user" || found.Client != "Emby Web" || found.Device != "Desktop" || found.DeviceID != "device-1" || found.Version != "4.8.0" || found.RemoteIP != "192.0.2.1" || !found.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("unexpected hydrated gateway session: %#v", found)
	}
	if err := store.RevokeSession(context.Background(), "hash"); err != nil {
		t.Fatalf("revoke session: %v", err)
	}
	revoked, err := store.FindSessionByTokenHash(context.Background(), "hash")
	if err != nil || revoked.RevokedAt == nil || !revoked.RevokedAt.After(now) {
		t.Fatalf("revoked session = %#v, %v", revoked, err)
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

	account, err := store.FindBackendAccountByID(context.Background(), accountRecord.Id)
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

func TestSessionTokenExistsExistingMissingAndOperationalError(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	userID := createGatewayUser(t, app, "alice", "gateway-user")

	const tokenHash = "exists-hash-value"
	if err := store.SaveSession(context.Background(), &gateway.Session{
		GatewayTokenHash: tokenHash,
		GatewayUserID:    userID,
		GatewayUsername:  "alice",
		SyntheticUserID:  "gateway-user",
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}

	exists, err := store.SessionTokenExists(context.Background(), tokenHash)
	if err != nil || !exists {
		t.Fatalf("existing SessionTokenExists = (%v, %v), want (true, nil)", exists, err)
	}

	exists, err = store.SessionTokenExists(context.Background(), "missing-hash-value")
	if err != nil || exists {
		t.Fatalf("missing SessionTokenExists = (%v, %v), want (false, nil)", exists, err)
	}

	// Deterministic schema break: delete the sessions collection so existence
	// checks surface an operational error rather than false.
	collection, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		t.Fatalf("find gateway_sessions: %v", err)
	}
	if err := app.Delete(collection); err != nil {
		t.Fatalf("delete gateway_sessions collection: %v", err)
	}
	exists, err = store.SessionTokenExists(context.Background(), tokenHash)
	if err == nil {
		t.Fatal("operational SessionTokenExists error = nil, want non-nil")
	}
	if exists {
		t.Fatal("operational SessionTokenExists returned exists=true")
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

func TestUpdateServerInfoPreservesExistingValuesWhenInputsAreEmpty(t *testing.T) {
	app := newTestApp(t)
	store := New(app)
	servers, err := app.FindCollectionByNameOrId("emby_servers")
	if err != nil {
		t.Fatalf("find emby_servers: %v", err)
	}
	server := core.NewRecord(servers)
	server.Set("name", "server")
	server.Set("base_url", "https://emby.example.com")
	server.Set("server_id", "real-server")
	server.Set("server_name", "Real Emby")
	server.Set("server_version", "4.9.5.0")
	server.Set("enabled", true)
	if err := app.Save(server); err != nil {
		t.Fatalf("save server: %v", err)
	}

	if err := store.UpdateServerInfo(context.Background(), server.Id, "", "", "", time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("update server info: %v", err)
	}
	updated, err := app.FindRecordById("emby_servers", server.Id)
	if err != nil {
		t.Fatalf("find updated server: %v", err)
	}
	if updated.GetString("server_id") != "real-server" || updated.GetString("server_name") != "Real Emby" || updated.GetString("server_version") != "4.9.5.0" {
		t.Fatalf("server info was cleared by empty update: %#v", updated)
	}
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
