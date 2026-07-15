package gateway

import (
	"context"
	"errors"
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

func validMemoryUpstreamSource() UpstreamSource {
	return UpstreamSource{ID: "source", Key: "default", ServerID: "server", BackendUsername: "backend", BackendPassword: "password", ClientIdentity: BackendClientIdentity{UserAgent: "agent", Client: "client", Device: "device", DeviceID: "device-id", Version: "1.0"}}
}
