package gateway

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryStoreUpstreamRuntimeAndAuthCAS(t *testing.T) {
	store := NewMemoryStore()
	store.UpstreamSources["source"] = validMemoryUpstreamSource()
	store.UpstreamEndpoints["active"] = UpstreamEndpoint{ID: "active", SourceID: "source", Key: "primary", BaseURL: "https://emby.example", Active: true}
	store.UpstreamEndpoints["inactive"] = UpstreamEndpoint{ID: "inactive", SourceID: "source", Key: "backup", BaseURL: "http://backup.example/base", Active: false}

	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if runtime.Source.AuthGenerationID != "" || runtime.Endpoint.ID != "active" {
		t.Fatalf("unexpected runtime: %#v", runtime)
	}
	at := time.Date(2026, 7, 16, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	update := UpstreamAuthUpdate{SourceID: "source", GenerationID: "generation-1", DeviceID: "device-2", BackendUserID: "backend-user", BackendToken: "token", AuthenticatedAt: at}
	if err := store.CompareAndSwapUpstreamAuth(context.Background(), update); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	source := store.UpstreamSources["source"]
	if source.AuthGenerationID != "generation-1" || source.ClientIdentity.DeviceID != "device-2" || source.BackendToken != "token" || source.TokenUpdatedAt == nil || !source.TokenUpdatedAt.Equal(at.UTC()) || source.LastLoginError != "" {
		t.Fatalf("unexpected updated source: %#v", source)
	}
	if store.UpstreamEndpoints["inactive"].BaseURL != "http://backup.example/base" {
		t.Fatal("CAS changed endpoint configuration")
	}
}

func TestMemoryStoreUpstreamCASConflictAndValidationDoNotMutate(t *testing.T) {
	store := NewMemoryStore()
	source := validMemoryUpstreamSource()
	source.AuthGenerationID = "old"
	now := time.Now().UTC()
	source.BackendUserID = "user"
	source.BackendToken = "token"
	source.TokenUpdatedAt = &now
	source.LastLoginAt = &now
	store.UpstreamSources["source"] = source
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: "source", Key: "primary", BaseURL: "https://emby.example", Active: true}
	before := store.UpstreamSources["source"]
	update := UpstreamAuthUpdate{SourceID: "source", ExpectedGenerationID: "stale", GenerationID: "new", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()}
	if err := store.CompareAndSwapUpstreamAuth(context.Background(), update); !errors.Is(err, ErrUpstreamAuthConflict) {
		t.Fatalf("stale CAS error = %v", err)
	}
	if got := store.UpstreamSources["source"]; got != before {
		t.Fatalf("stale CAS mutated source: %#v", got)
	}
	update.ExpectedGenerationID = "old"
	update.GenerationID = "old"
	if err := store.CompareAndSwapUpstreamAuth(context.Background(), update); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("invalid CAS error = %v", err)
	}
	if got := store.UpstreamSources["source"]; got != before {
		t.Fatalf("invalid CAS mutated source: %#v", got)
	}
}

func TestMemoryStoreUpstreamCASHasOneConcurrentWinner(t *testing.T) {
	store := NewMemoryStore()
	store.UpstreamSources["source"] = validMemoryUpstreamSource()
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: "source", Key: "primary", BaseURL: "https://emby.example", Active: true}
	var winners int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(generation string) {
			defer wg.Done()
			err := store.CompareAndSwapUpstreamAuth(context.Background(), UpstreamAuthUpdate{SourceID: "source", GenerationID: generation, DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()})
			if err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			} else if !errors.Is(err, ErrUpstreamAuthConflict) {
				t.Errorf("CAS error = %v", err)
			}
		}(string(rune('a' + i)))
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("CAS winners = %d, want 1", winners)
	}
}

func TestMemoryStoreUpstreamTopologyAndCanceledContext(t *testing.T) {
	store := NewMemoryStore()
	store.UpstreamSources["source"] = validMemoryUpstreamSource()
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: "other", Key: "primary", BaseURL: "https://emby.example", Active: true}
	if _, err := store.LoadDefaultUpstreamRuntime(context.Background()); !errors.Is(err, ErrInvalidUpstreamTopology) {
		t.Fatalf("orphan endpoint error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.CompareAndSwapUpstreamAuth(ctx, UpstreamAuthUpdate{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled CAS error = %v", err)
	}
}

func TestMemoryStoreValidatesEveryEndpointAndReturnsDetachedSnapshot(t *testing.T) {
	store := NewMemoryStore()
	source := validMemoryUpstreamSource()
	now := time.Now()
	source.TokenUpdatedAt = &now
	store.UpstreamSources["source"] = source
	store.UpstreamEndpoints["active"] = UpstreamEndpoint{ID: "active", SourceID: "source", Key: "primary", BaseURL: "https://emby.example", Active: true}
	store.UpstreamEndpoints["inactive"] = UpstreamEndpoint{ID: "inactive", SourceID: "source", Key: "backup", BaseURL: "https://backup.example?", Active: false}
	if _, err := store.LoadDefaultUpstreamRuntime(context.Background()); !errors.Is(err, ErrInvalidUpstreamTopology) {
		t.Fatalf("malformed inactive endpoint error = %v", err)
	}
	store.UpstreamEndpoints["inactive"] = UpstreamEndpoint{ID: "inactive", SourceID: "source", Key: "backup", BaseURL: "https://backup.example", Active: false}
	runtime, err := store.LoadDefaultUpstreamRuntime(context.Background())
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	*runtime.Source.TokenUpdatedAt = time.Time{}
	if store.UpstreamSources["source"].TokenUpdatedAt.IsZero() {
		t.Fatal("runtime timestamp aliases stored source")
	}
}

func TestMemoryStoreAcceptsStalePreContractAuthAndRejectsMalformedManagedAuth(t *testing.T) {
	store := NewMemoryStore()
	source := validMemoryUpstreamSource()
	source.BackendUserID = "stale-user"
	source.BackendToken = "stale-token"
	store.UpstreamSources["source"] = source
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: "source", Key: "primary", BaseURL: "https://emby.example", Active: true}
	if _, err := store.LoadDefaultUpstreamRuntime(context.Background()); err != nil {
		t.Fatalf("pre-contract runtime: %v", err)
	}
	source.AuthGenerationID = "managed"
	store.UpstreamSources["source"] = source
	if _, err := store.LoadDefaultUpstreamRuntime(context.Background()); !errors.Is(err, ErrInvalidUpstreamTopology) {
		t.Fatalf("malformed managed auth error = %v", err)
	}
}

func TestMemoryStoreUpstreamAuthUpdateRejectsInvalidFieldsWithoutMutation(t *testing.T) {
	store := NewMemoryStore()
	store.UpstreamSources["source"] = validMemoryUpstreamSource()
	before := store.UpstreamSources["source"]
	for _, update := range []UpstreamAuthUpdate{
		{SourceID: "source", GenerationID: " generation", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()},
		{SourceID: "source", GenerationID: string(make([]byte, 129)), DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()},
		{SourceID: "source", GenerationID: "generation", DeviceID: string(make([]byte, 256)), BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()},
		{SourceID: "source", GenerationID: "generation", DeviceID: "device", BackendUserID: string(make([]byte, 81)), BackendToken: "token", AuthenticatedAt: time.Now()},
	} {
		if err := store.CompareAndSwapUpstreamAuth(context.Background(), update); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("invalid update error = %v", err)
		}
		if got := store.UpstreamSources["source"]; got != before {
			t.Fatalf("invalid update mutated source: %#v", got)
		}
	}
}

func TestMemoryStoreNoSourceWithEndpointIsTopologyError(t *testing.T) {
	store := NewMemoryStore()
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: "missing", Key: "primary", BaseURL: "https://emby.example", Active: true}
	if _, err := store.LoadDefaultUpstreamRuntime(context.Background()); !errors.Is(err, ErrInvalidUpstreamTopology) {
		t.Fatalf("endpoint without source error = %v", err)
	}
}

func TestMemoryStoreRejectsMapIdentityMismatches(t *testing.T) {
	store := NewMemoryStore()
	store.UpstreamSources["wrong-key"] = validMemoryUpstreamSource()
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: "source", Key: "primary", BaseURL: "https://emby.example", Active: true}
	if _, err := store.LoadDefaultUpstreamRuntime(context.Background()); !errors.Is(err, ErrInvalidUpstreamTopology) {
		t.Fatalf("source identity error = %v", err)
	}
	store.UpstreamSources = map[string]UpstreamSource{"source": validMemoryUpstreamSource()}
	store.UpstreamEndpoints = map[string]UpstreamEndpoint{"wrong-key": {ID: "endpoint", SourceID: "source", Key: "primary", BaseURL: "https://emby.example", Active: true}}
	if err := store.CompareAndSwapUpstreamAuth(context.Background(), UpstreamAuthUpdate{SourceID: "source", ExpectedGenerationID: "stale", GenerationID: "generation", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()}); !errors.Is(err, ErrInvalidUpstreamTopology) {
		t.Fatalf("endpoint identity CAS error = %v", err)
	}
}

func TestMemoryStoreUpstreamCASMatchesPocketBaseBeforeTopologyValidation(t *testing.T) {
	store := NewMemoryStore()
	store.UpstreamSources["source"] = validMemoryUpstreamSource()
	store.UpstreamEndpoints["wrong-key"] = UpstreamEndpoint{ID: "endpoint", SourceID: "source", Key: "primary", BaseURL: "https://emby.example", Active: true}

	update := UpstreamAuthUpdate{SourceID: "source", GenerationID: "generation", DeviceID: "device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: time.Now()}
	if err := store.CompareAndSwapUpstreamAuth(context.Background(), update); err != nil {
		t.Fatalf("matching CAS with malformed topology: %v", err)
	}
	if got := store.UpstreamSources["source"].AuthGenerationID; got != "generation" {
		t.Fatalf("generation = %q, want generation", got)
	}
}

func TestMemoryStoreUpdateUpstreamServerInfo(t *testing.T) {
	store := NewMemoryStore()
	source := validMemoryUpstreamSource()
	source.ServerName = "old name"
	source.ServerVersion = "old version"
	source.AuthGenerationID = "old-generation"
	source.BackendToken = "old-token"
	store.UpstreamSources[source.ID] = source
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: source.ID, Key: "primary", BaseURL: "https://emby.example", Active: true}
	at := time.Date(2026, 7, 16, 12, 0, 0, 123456789, time.FixedZone("offset", 3600))
	if err := store.UpdateUpstreamServerInfo(context.Background(), UpstreamServerInfoUpdate{SourceID: source.ID, ServerID: source.ServerID, ServerName: "new name", CheckedAt: at}); err != nil {
		t.Fatalf("update name: %v", err)
	}
	got := store.UpstreamSources[source.ID]
	wantCheckedAt := at.UTC().Truncate(time.Millisecond)
	if got.ServerName != "new name" || got.ServerVersion != "old version" || got.VersionCheckedAt == nil || !got.VersionCheckedAt.Equal(wantCheckedAt) || got.VersionCheckedAt.Location() != time.UTC || got.AuthGenerationID != "old-generation" || got.BackendToken != "old-token" {
		t.Fatalf("unexpected metadata update: %#v", got)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), UpstreamServerInfoUpdate{SourceID: source.ID, ServerID: source.ServerID, ServerVersion: "new version", CheckedAt: at.Add(time.Hour)}); err != nil {
		t.Fatalf("update version: %v", err)
	}
	got = store.UpstreamSources[source.ID]
	if got.ServerName != "new name" || got.ServerVersion != "new version" {
		t.Fatalf("empty metadata values were not preserved independently: %#v", got)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), UpstreamServerInfoUpdate{SourceID: source.ID, ServerID: source.ServerID, ServerName: "new name", ServerVersion: "new version", CheckedAt: at.Add(time.Hour)}); err != nil {
		t.Fatalf("exact no-op update: %v", err)
	}
}

func TestUpstreamServerInfoUpdateValidationCountsRunes(t *testing.T) {
	valid := UpstreamServerInfoUpdate{
		SourceID:      strings.Repeat("界", upstreamSourceIDMaxLength),
		ServerID:      strings.Repeat("界", upstreamServerIDMaxLength),
		ServerName:    strings.Repeat("界", upstreamServerNameMaxLength),
		ServerVersion: strings.Repeat("界", upstreamServerVersionMaxLength),
		CheckedAt:     time.Now(),
	}
	if err := ValidateUpstreamServerInfoUpdate(valid); err != nil {
		t.Fatalf("unicode boundary update: %v", err)
	}
	for _, field := range []func(*UpstreamServerInfoUpdate){
		func(update *UpstreamServerInfoUpdate) { update.SourceID += "界" },
		func(update *UpstreamServerInfoUpdate) { update.ServerID += "界" },
		func(update *UpstreamServerInfoUpdate) { update.ServerName += "界" },
		func(update *UpstreamServerInfoUpdate) { update.ServerVersion += "界" },
	} {
		update := valid
		field(&update)
		if err := ValidateUpstreamServerInfoUpdate(update); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("unicode max+1 validation error = %v", err)
		}
	}
}

func TestMemoryStoreUpdateUpstreamServerInfoErrorsDoNotMutate(t *testing.T) {
	store := NewMemoryStore()
	source := validMemoryUpstreamSource()
	store.UpstreamSources[source.ID] = source
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: source.ID, Key: "primary", BaseURL: "https://emby.example", Active: true}
	before := store.UpstreamSources[source.ID]
	for _, update := range []UpstreamServerInfoUpdate{
		{SourceID: " source", ServerID: source.ServerID, CheckedAt: time.Now()},
		{SourceID: source.ID, ServerID: " server", CheckedAt: time.Now()},
		{SourceID: source.ID, ServerID: string(make([]byte, 256)), CheckedAt: time.Now()},
		{SourceID: source.ID, ServerID: source.ServerID, ServerName: string(make([]byte, 256)), CheckedAt: time.Now()},
		{SourceID: source.ID, ServerID: source.ServerID, ServerVersion: string(make([]byte, 81)), CheckedAt: time.Now()},
		{SourceID: source.ID, ServerID: source.ServerID},
	} {
		if err := store.UpdateUpstreamServerInfo(context.Background(), update); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("invalid update error = %v", err)
		}
		if got := store.UpstreamSources[source.ID]; got != before {
			t.Fatalf("invalid update mutated source: %#v", got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.UpdateUpstreamServerInfo(ctx, UpstreamServerInfoUpdate{SourceID: source.ID, ServerID: source.ServerID, CheckedAt: time.Now()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled update error = %v", err)
	}
	if got := store.UpstreamSources[source.ID]; got != before {
		t.Fatalf("canceled update mutated source: %#v", got)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), UpstreamServerInfoUpdate{SourceID: source.ID, ServerID: "other", CheckedAt: time.Now()}); !errors.Is(err, ErrUpstreamServerInfoConflict) {
		t.Fatalf("server mismatch error = %v", err)
	}
	if err := store.UpdateUpstreamServerInfo(context.Background(), UpstreamServerInfoUpdate{SourceID: "missing", ServerID: source.ServerID, CheckedAt: time.Now()}); !errors.Is(err, ErrUpstreamNotFound) {
		t.Fatalf("missing source error = %v", err)
	}
	store.UpstreamEndpoints = map[string]UpstreamEndpoint{}
	if err := store.UpdateUpstreamServerInfo(context.Background(), UpstreamServerInfoUpdate{SourceID: source.ID, ServerID: "other", CheckedAt: time.Now()}); !errors.Is(err, ErrInvalidUpstreamTopology) {
		t.Fatalf("malformed topology error = %v", err)
	}
}

func TestMemoryStoreMetadataAndAuthCASPreserveEachOther(t *testing.T) {
	store := NewMemoryStore()
	source := validMemoryUpstreamSource()
	source.ServerName = "old name"
	source.ServerVersion = "old version"
	source.LastLoginError = "old error"
	store.UpstreamSources[source.ID] = source
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: source.ID, Key: "primary", BaseURL: "https://emby.example", Active: true}
	store.UpstreamEndpoints["backup"] = UpstreamEndpoint{ID: "backup", SourceID: source.ID, Key: "backup", BaseURL: "https://backup.example", Active: false}
	endpointsBefore := make(map[string]UpstreamEndpoint, len(store.UpstreamEndpoints))
	for id, endpoint := range store.UpstreamEndpoints {
		endpointsBefore[id] = endpoint
	}
	metadataAt := time.Date(2026, 7, 16, 12, 0, 0, 123456789, time.FixedZone("offset", 3600))
	authenticatedAt := time.Date(2026, 7, 16, 12, 1, 0, 987654321, time.FixedZone("offset", -3600))
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- store.UpdateUpstreamServerInfo(context.Background(), UpstreamServerInfoUpdate{SourceID: source.ID, ServerID: source.ServerID, ServerName: "new", ServerVersion: "1.2", CheckedAt: metadataAt})
	}()
	go func() {
		<-start
		errs <- store.CompareAndSwapUpstreamAuth(context.Background(), UpstreamAuthUpdate{SourceID: source.ID, GenerationID: "generation", DeviceID: "new-device", BackendUserID: "user", BackendToken: "token", AuthenticatedAt: authenticatedAt})
	}()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}
	got := store.UpstreamSources[source.ID]
	want := source
	metadataCheckedAt := metadataAt.UTC().Truncate(time.Millisecond)
	authenticatedUTC := authenticatedAt.UTC()
	want.ServerName, want.ServerVersion, want.VersionCheckedAt = "new", "1.2", &metadataCheckedAt
	want.AuthGenerationID, want.ClientIdentity.DeviceID, want.BackendUserID, want.BackendToken = "generation", "new-device", "user", "token"
	want.TokenUpdatedAt, want.LastLoginAt, want.LastLoginError = &authenticatedUTC, &authenticatedUTC, ""
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent updates lost data: %#v", got)
	}
	if !reflect.DeepEqual(store.UpstreamEndpoints, endpointsBefore) {
		t.Fatalf("concurrent updates changed endpoints: %#v", store.UpstreamEndpoints)
	}
}

func validMemoryUpstreamSource() UpstreamSource {
	return UpstreamSource{ID: "source", Key: "default", ServerID: "server", BackendUsername: "backend", BackendPassword: "password", ClientIdentity: BackendClientIdentity{UserAgent: "agent", Client: "client", Device: "device", DeviceID: "device-id", Version: "1.0"}}
}
