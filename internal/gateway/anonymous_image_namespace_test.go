package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshUpstreamServerInfoInvalidatesAnonymousImageNamespace(t *testing.T) {
	const serverID = "server-1"
	mode := "valid"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/System/Info/Public" {
			switch mode {
			case "mismatch":
				writeTestJSON(w, map[string]any{"Id": "other-server"})
			case "unavailable":
				w.WriteHeader(http.StatusServiceUnavailable)
			default:
				writeTestJSON(w, map[string]any{"Id": serverID, "Version": "4.9"})
			}
			return
		}
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(anonymousGIF())
	}))
	defer backend.Close()
	s := NewServer(Config{}, anonymousImageTestStore(backend.URL+"/emby", serverID))
	if err := s.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s)
	defer gw.Close()
	requestImage := func() *http.Response {
		return do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/item/Images/Primary", nil))
	}
	for _, failedMode := range []string{"mismatch", "unavailable"} {
		t.Run(failedMode, func(t *testing.T) {
			mode = failedMode
			if err := s.RefreshUpstreamServerInfo(context.Background()); err == nil {
				t.Fatal("periodic refresh unexpectedly succeeded")
			}
			resp := requestImage()
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("stale origin remained available: %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
			}
			mode = "valid"
			if err := s.RefreshUpstreamServerInfo(context.Background()); err != nil {
				t.Fatalf("recovery refresh: %v", err)
			}
			resp = requestImage()
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("recovered image status = %d", resp.StatusCode)
			}
		})
	}
}

func TestAnonymousImageNamespaceUsesOnlyActiveEndpoint(t *testing.T) {
	const serverID = "server-1"
	var inactiveCalls atomic.Int32
	inactive := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { inactiveCalls.Add(1) }))
	defer inactive.Close()
	active := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/System/Info/Public" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Emby-Token") != "" || r.Header.Get("X-Emby-Authorization") == "" {
			t.Fatal("probe forwarded credentials")
		}
		writeTestJSON(w, map[string]any{"Id": serverID})
	}))
	defer active.Close()
	store := anonymousImageTestStore(active.URL+"/emby", serverID)
	store.UpstreamEndpoints["inactive"] = UpstreamEndpoint{ID: "inactive", SourceID: "source", Key: "secondary", BaseURL: inactive.URL + "/emby"}
	s := NewServer(Config{}, store)
	if err := s.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	origin, ok := s.ValidatedAnonymousImageOrigin(context.Background())
	if !ok || origin.BaseURL != active.URL+"/emby" || inactiveCalls.Load() != 0 {
		t.Fatalf("origin=%#v active=%v inactive=%d", origin, ok, inactiveCalls.Load())
	}
}

func TestAnonymousImageNamespaceClassifiesFailuresAndTTL(t *testing.T) {
	const serverID = "server-1"
	status := http.StatusOK
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		writeTestJSON(w, map[string]any{"Id": serverID})
	}))
	defer backend.Close()
	s := NewServer(Config{}, anonymousImageTestStore(backend.URL+"/emby", serverID))
	now := time.Now().UTC()
	s.anonymousImageNow = func() time.Time { return now }
	if err := s.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(anonymousImageNamespaceMaxAge + time.Second)
	if _, ok := s.ValidatedAnonymousImageOrigin(context.Background()); ok {
		t.Fatal("stale namespace served")
	}
	status = http.StatusServiceUnavailable
	if !IsAnonymousImageNamespaceTransient(s.ValidateAnonymousImageNamespace(context.Background())) {
		t.Fatal("5xx not transient")
	}
}

func TestAnonymousImageNamespaceRejectsMismatchAndDrift(t *testing.T) {
	const serverID = "server-1"
	started, release := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		writeTestJSON(w, map[string]any{"Id": serverID})
	}))
	defer backend.Close()
	store := anonymousImageTestStore(backend.URL+"/emby", serverID)
	s := NewServer(Config{}, store)
	done := make(chan error, 1)
	go func() { done <- s.ValidateAnonymousImageNamespace(context.Background()) }()
	<-started
	store.mu.Lock()
	source := store.UpstreamSources["source"]
	source.ClientIdentity.DeviceID = "changed"
	store.UpstreamSources["source"] = source
	store.mu.Unlock()
	close(release)
	err := <-done
	var namespaceErr *AnonymousImageNamespaceError
	if !errors.As(err, &namespaceErr) || namespaceErr.Kind != AnonymousImageNamespaceTransientError {
		t.Fatalf("drift error=%v", err)
	}
}

func anonymousImageTestStore(baseURL, backendServerID string) *MemoryStore {
	store := NewMemoryStore()
	identity := BackendClientIdentity{DeviceID: "device"}.WithDefaults()
	store.UpstreamSources["source"] = UpstreamSource{ID: "source", Key: "default", ServerID: backendServerID, BackendUsername: "backend", BackendPassword: "password", ClientIdentity: identity}
	store.UpstreamEndpoints["endpoint"] = UpstreamEndpoint{ID: "endpoint", SourceID: "source", Key: "primary", BaseURL: baseURL, Active: true}
	return store
}
