package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

type audioBindingStore struct {
	*MemoryStore
	loaded chan struct{}
}

func (s *audioBindingStore) LoadDefaultUpstreamRuntime(ctx context.Context) (*UpstreamRuntime, error) {
	select {
	case s.loaded <- struct{}{}:
	default:
	}
	return s.MemoryStore.LoadDefaultUpstreamRuntime(ctx)
}

func TestAudioSourceValidatesBindingUnderReconfigureGate(t *testing.T) {
	var requests atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.ServeContent(w, r, "media", time.Time{}, strings.NewReader("01234567"))
	}))
	defer backend.Close()
	store := &audioBindingStore{MemoryStore: NewMemoryStore(), loaded: make(chan struct{}, 10)}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	s := NewServer(Config{}, store)
	source := s.audioSource(httptest.NewRequest("GET", "http://gateway/emby", nil), testSession(), testUpstreamSnapshot(backend.URL+"/emby"), "gateway-token", "item", transcode.MediaSource{ID: "source", Size: 8, DirectStreamURL: "/Videos/item/original.mkv?api_key=backend-token"})
	release, err := s.TryAcquireReconfigure(true)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		body, err := source.Open(context.Background(), 0, 4)
		if body != nil {
			_ = body.Close()
		}
		result <- err
	}()
	<-started
	select {
	case <-store.loaded:
		release()
		t.Fatal("source binding read before acquiring its transfer gate")
	case <-time.After(20 * time.Millisecond):
	}
	store.mu.Lock()
	changed := store.UpstreamSources["source"]
	changed.ServerID = "different-server"
	store.UpstreamSources["source"] = changed
	store.mu.Unlock()
	release()
	select {
	case err := <-result:
		if !errors.Is(err, transcode.ErrSource) {
			t.Fatalf("source change: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source request did not leave reconfiguration")
	}
	if requests.Load() != 0 || s.ActiveMediaCopies() != 0 {
		t.Fatal("stale source I/O or gate ownership remained")
	}
}

func TestAudioSourceAllowsCredentialRotation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "rotated-token" || r.URL.Query().Get("api_key") != "rotated-token" {
			http.Error(w, "wrong credential", 401)
			return
		}
		http.ServeContent(w, r, "media", time.Time{}, strings.NewReader("01234567"))
	}))
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	s := NewServer(Config{}, store)
	source := s.audioSource(httptest.NewRequest("GET", "http://gateway/emby", nil), testSession(), testUpstreamSnapshot(backend.URL+"/emby"), "gateway-token", "item", transcode.MediaSource{ID: "source", Size: 8, DirectStreamURL: "/Videos/item/original.mkv?api_key=backend-token"})
	current := store.UpstreamSources["source"]
	current.BackendToken = "rotated-token"
	current.AuthGenerationID = "next-generation"
	current.ClientIdentity.DeviceID = "rotated-device"
	store.UpstreamSources["source"] = current
	body, err := source.Open(context.Background(), 2, 4)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(data) != "2345" {
		t.Fatalf("rotated source %q %v", data, err)
	}
}
