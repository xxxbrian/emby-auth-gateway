package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAnonymousImageNamespaceValidation(t *testing.T) {
	const expected = "namespace-1"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/System/Info/Public" {
			t.Fatalf("probe path = %q", r.URL.Path)
		}
		writeTestJSON(w, map[string]any{"Id": expected})
	}))
	defer backend.Close()

	for _, tc := range []struct {
		name    string
		cfg     Config
		servers []EmbyServer
		wantErr AnonymousImageNamespaceErrorKind
		wantOK  bool
	}{
		{"disabled", Config{}, nil, 0, false},
		{"only record", Config{AnonymousImageServerRecordID: "one"}, nil, AnonymousImageNamespaceStaticError, false},
		{"only expected", Config{AnonymousImageBackendServerID: expected}, nil, AnonymousImageNamespaceStaticError, false},
		{"whitespace", Config{AnonymousImageConfigured: true, AnonymousImageServerRecordID: " ", AnonymousImageBackendServerID: expected}, nil, AnonymousImageNamespaceStaticError, false},
		{"missing selected", Config{AnonymousImageServerRecordID: "missing", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby", expected)}, AnonymousImageNamespaceStaticError, false},
		{"invalid base", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", "ftp://backend", expected)}, AnonymousImageNamespaceStaticError, false},
		{"query base", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby?api_key=value", expected)}, AnonymousImageNamespaceStaticError, false},
		{"force query base", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby?", expected)}, AnonymousImageNamespaceStaticError, false},
		{"fragment base", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby#fragment", expected)}, AnonymousImageNamespaceStaticError, false},
		{"empty stored id", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby", "")}, AnonymousImageNamespaceStaticError, false},
		{"stored mismatch", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby", "other")}, AnonymousImageNamespaceMismatchError, false},
		{"one server", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby", expected)}, 0, true},
		{"duplicate ingress", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby", expected), anonymousImageTestServer("two", backend.URL+"/emby", expected)}, 0, true},
		{"two namespaces", Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, []EmbyServer{anonymousImageTestServer("one", backend.URL+"/emby", expected), anonymousImageTestServer("two", backend.URL+"/emby", "other")}, AnonymousImageNamespaceMismatchError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := anonymousImageTestStore(tc.servers...)
			server := NewServer(tc.cfg, store)
			err := server.ValidateAnonymousImageNamespace(context.Background())
			if tc.wantErr == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var namespaceErr *AnonymousImageNamespaceError
				if !errors.As(err, &namespaceErr) || namespaceErr.Kind != tc.wantErr {
					t.Fatalf("error = %#v, want kind %d", err, tc.wantErr)
				}
			}
			_, ok := server.ValidatedAnonymousImageOrigin(context.Background())
			if ok != tc.wantOK {
				t.Fatalf("origin available = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

func TestAnonymousImageNamespaceProbeIdentityAndTransientState(t *testing.T) {
	const expected = "namespace-1"
	var sawAuth, sawCookie, sawToken bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("X-Emby-Authorization") != ""
		sawCookie = r.Header.Get("Cookie") != ""
		sawToken = r.Header.Get("X-Emby-Token") != ""
		if r.UserAgent() != "Custom/1" {
			t.Fatalf("User-Agent = %q", r.UserAgent())
		}
		writeTestJSON(w, map[string]any{"Id": expected})
	}))
	defer backend.Close()
	configured := anonymousImageTestServer("one", backend.URL+"/emby", expected)
	configured.ClientIdentity = BackendClientIdentity{UserAgent: "Custom/1", Client: "Client", Device: "Device", DeviceID: "device", Version: "1"}
	server := NewServer(Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, anonymousImageTestStore(configured))
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sawAuth || sawCookie || sawToken {
		t.Fatalf("probe auth/cookie/token = %v/%v/%v", sawAuth, sawCookie, sawToken)
	}

	unreachable := anonymousImageTestServer("one", "http://127.0.0.1:1", expected)
	transient := NewServer(Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, anonymousImageTestStore(unreachable))
	err := transient.ValidateAnonymousImageNamespace(context.Background())
	if !IsAnonymousImageNamespaceTransient(err) {
		t.Fatalf("transient error = %#v", err)
	}
	if _, ok := transient.ValidatedAnonymousImageOrigin(context.Background()); ok || transient.AnonymousImageNamespaceDiagnostic() == "" {
		t.Fatalf("transient namespace state is available: %q", transient.AnonymousImageNamespaceDiagnostic())
	}
}

func TestAnonymousImageNamespaceMissingLiveIDIsStatic(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeTestJSON(w, map[string]any{"ServerName": "Emby"}) }))
	defer backend.Close()
	server := NewServer(Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: "namespace-1"}, anonymousImageTestStore(anonymousImageTestServer("one", backend.URL+"/emby", "namespace-1")))
	err := server.ValidateAnonymousImageNamespace(context.Background())
	var namespaceErr *AnonymousImageNamespaceError
	if !errors.As(err, &namespaceErr) || namespaceErr.Kind != AnonymousImageNamespaceStaticError {
		t.Fatalf("missing Id error = %#v", err)
	}
}

func TestAnonymousImageNamespaceRejectsLiveServerIDMismatch(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Id": "live-other"})
	}))
	defer backend.Close()
	server := NewServer(Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: "namespace-1"}, anonymousImageTestStore(anonymousImageTestServer("one", backend.URL+"/emby", "namespace-1")))
	err := server.ValidateAnonymousImageNamespace(context.Background())
	var namespaceErr *AnonymousImageNamespaceError
	if !errors.As(err, &namespaceErr) || namespaceErr.Kind != AnonymousImageNamespaceMismatchError {
		t.Fatalf("live mismatch error = %#v", err)
	}
}

func TestAnonymousImageNamespaceFingerprintInvalidatesAndRevalidates(t *testing.T) {
	const expected = "namespace-1"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeTestJSON(w, map[string]any{"Id": expected}) }))
	defer backend.Close()
	store := anonymousImageTestStore(anonymousImageTestServer("one", backend.URL+"/emby", expected))
	server := NewServer(Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, store)
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if server.AnonymousImageNamespaceFingerprintChanged(context.Background()) {
		t.Fatal("fresh fingerprint reported changed")
	}
	mapping := store.Mappings["mapping-one"]
	mapping.BackendAccount.Server.BaseURL = backend.URL + "/other"
	store.Mappings["mapping-one"] = mapping
	if _, ok := server.ValidatedAnonymousImageOrigin(context.Background()); ok || !server.AnonymousImageNamespaceFingerprintChanged(context.Background()) {
		t.Fatal("base URL mutation did not invalidate origin")
	}
	mapping.BackendAccount.Server.BaseURL = backend.URL + "/emby"
	mapping.BackendAccount.Server.ClientIdentity.UserAgent = "Changed/1"
	store.Mappings["mapping-one"] = mapping
	if _, ok := server.ValidatedAnonymousImageOrigin(context.Background()); ok {
		t.Fatal("identity mutation did not invalidate origin")
	}
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.ValidatedAnonymousImageOrigin(context.Background()); !ok {
		t.Fatal("revalidation did not restore origin")
	}
	mapping.BackendAccount.Server.BackendServerID = "changed"
	store.Mappings["mapping-one"] = mapping
	if _, ok := server.ValidatedAnonymousImageOrigin(context.Background()); ok {
		t.Fatal("server ID mutation did not invalidate origin")
	}
}

func TestAnonymousImageNamespaceFingerprintIsDeterministic(t *testing.T) {
	first := anonymousImageTestServer("one", "https://one.example/emby", "namespace")
	second := anonymousImageTestServer("two", "https://two.example/emby", "namespace")
	if anonymousImageNamespaceFingerprint([]EmbyServer{first, second}) != anonymousImageNamespaceFingerprint([]EmbyServer{second, first}) {
		t.Fatal("fingerprint depends on enabled-server order")
	}
}

func TestAnonymousImageNamespaceOldReaderCannotInvalidateNewGeneration(t *testing.T) {
	server := NewServer(Config{}, NewMemoryStore())
	server.setAnonymousImageNamespaceSnapshot(anonymousImageNamespaceSnapshot{fingerprint: "same", available: true, diagnostic: "available"})
	server.anonymousImages.mu.RLock()
	old := server.anonymousImages.snapshot
	server.anonymousImages.mu.RUnlock()
	server.setAnonymousImageNamespaceSnapshot(anonymousImageNamespaceSnapshot{fingerprint: "same", available: true, diagnostic: "available"})
	server.invalidateAnonymousImageNamespace(old.generation, old.fingerprint, "configuration changed")
	server.anonymousImages.mu.RLock()
	current := server.anonymousImages.snapshot
	server.anonymousImages.mu.RUnlock()
	if !current.available || current.generation == old.generation || current.diagnostic != "available" {
		t.Fatalf("new snapshot was invalidated by stale reader: %#v", current)
	}
}

func TestAnonymousImageNamespaceFreshnessAndRecovery(t *testing.T) {
	const expected = "namespace-1"
	var transient bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if transient {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeTestJSON(w, map[string]any{"Id": expected})
	}))
	defer backend.Close()
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	server := NewServer(Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, anonymousImageTestStore(anonymousImageTestServer("one", backend.URL+"/emby", expected)))
	server.anonymousImageNow = func() time.Time { return now }
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(anonymousImageNamespaceMaxAge + time.Second)
	if _, ok := server.ValidatedAnonymousImageOrigin(context.Background()); ok || server.AnonymousImageNamespaceDiagnostic() != "validation expired" {
		t.Fatalf("stale origin state = %q", server.AnonymousImageNamespaceDiagnostic())
	}
	transient = true
	if !IsAnonymousImageNamespaceTransient(server.ValidateAnonymousImageNamespace(context.Background())) {
		t.Fatal("transient refresh was not classified transient")
	}
	transient = false
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := server.ValidatedAnonymousImageOrigin(context.Background()); !ok {
		t.Fatal("successful revalidation did not restore stale origin")
	}
}

func TestAnonymousImageNamespaceDuplicateIngressesProbeDistinctOrigins(t *testing.T) {
	const expected = "namespace-1"
	var mu sync.Mutex
	probed := map[string]bool{}
	newBackend := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if auth := r.Header.Get("X-Emby-Authorization"); auth == "" || strings.Contains(auth, "UserId=") || strings.Contains(auth, "Token=") || r.Header.Get("Cookie") != "" || r.Header.Get("X-Emby-Token") != "" {
				t.Fatalf("probe credentials = %q cookie=%q token=%q", auth, r.Header.Get("Cookie"), r.Header.Get("X-Emby-Token"))
			}
			mu.Lock()
			probed[r.Host] = true
			mu.Unlock()
			writeTestJSON(w, map[string]any{"Id": expected})
		}))
	}
	first, second := newBackend(), newBackend()
	defer first.Close()
	defer second.Close()
	server := NewServer(Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, anonymousImageTestStore(anonymousImageTestServer("one", first.URL+"/emby", expected), anonymousImageTestServer("two", second.URL+"/emby", expected)))
	if err := server.ValidateAnonymousImageNamespace(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	count := len(probed)
	mu.Unlock()
	if count != 2 {
		t.Fatalf("probed origins = %d, want 2", count)
	}
}

func TestAnonymousImageNamespaceValidationIsSerialized(t *testing.T) {
	const expected = "namespace-1"
	started := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			close(started)
			<-release
		}
		writeTestJSON(w, map[string]any{"Id": expected})
	}))
	defer backend.Close()
	server := NewServer(Config{AnonymousImageServerRecordID: "one", AnonymousImageBackendServerID: expected}, anonymousImageTestStore(anonymousImageTestServer("one", backend.URL+"/emby", expected)))
	done := make(chan error, 2)
	go func() { done <- server.ValidateAnonymousImageNamespace(context.Background()) }()
	<-started
	go func() { done <- server.ValidateAnonymousImageNamespace(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("concurrent probe calls = %d, want 1 before release", gotCalls)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func anonymousImageTestServer(id, baseURL, backendServerID string) EmbyServer {
	return EmbyServer{ID: id, BaseURL: baseURL, BackendServerID: backendServerID, Enabled: true, ClientIdentity: BackendClientIdentity{DeviceID: "device"}}
}

func anonymousImageTestStore(servers ...EmbyServer) *MemoryStore {
	store := NewMemoryStore()
	for _, server := range servers {
		store.Mappings["mapping-"+server.ID] = UserMapping{ID: "mapping-" + server.ID, Enabled: true, BackendAccount: BackendAccount{ID: "account-" + server.ID, Enabled: true, Server: server}}
	}
	return store
}
