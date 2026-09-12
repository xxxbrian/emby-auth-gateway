package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAdminMediaReadUsesSharedIdentityAndReadOnlyEndpoints(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("X-Emby-Token") != "backend-token" || r.URL.Query().Get("api_key") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected auth/method %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/emby/Users/backend-user/Items":
			if r.URL.Query().Get("Ids") != "one,two" || r.URL.Query().Get("EnableUserData") != "false" {
				t.Errorf("query=%v", r.URL.Query())
			}
			fmt.Fprint(w, `{"Items":[{"Id":"one","Name":"Movie"}]}`)
		case "/emby/Users/backend-user/Items/one":
			if r.URL.RawQuery != "" {
				t.Error("detail used unsupported query parameters")
			}
			fmt.Fprint(w, `{"Id":"one","Name":"Movie"}`)
		case "/emby/Items/one/Images/Primary":
			if r.URL.Query().Get("MaxWidth") != "720" {
				t.Error("size not bounded")
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte{0xff, 0xd8, 0xff})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	server := NewServer(Config{HTTPClient: backend.Client()}, store)
	ref, err := server.AdminMediaSourceRef(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	data, status, err := server.AdminMediaItems(context.Background(), ref, []string{"one", "two"}, false)
	if err != nil || status != 200 || !strings.Contains(string(data), "Movie") {
		t.Fatalf("summary status=%d err=%v", status, err)
	}
	_, status, err = server.AdminMediaItems(context.Background(), ref, []string{"one"}, true)
	if err != nil || status != 200 {
		t.Fatalf("detail status=%d err=%v", status, err)
	}
	_, kind, status, err := server.AdminMediaImage(context.Background(), ref, "one", "Primary", "large")
	if err != nil || status != 200 || kind != "image/jpeg" {
		t.Fatalf("image=%s/%d err=%v", kind, status, err)
	}
	if calls.Load() != 3 || len(store.Sessions) != 0 {
		t.Fatalf("calls=%d sessions=%d", calls.Load(), len(store.Sessions))
	}
	if len(store.PlaybackStates) != 0 || len(store.PlaybackEvents) != 0 || len(store.DisplayPreferences) != 0 {
		t.Fatal("read-only Admin metadata mutated personal state")
	}
	store.mu.Lock()
	source := store.UpstreamSources["source"]
	source.BackendUserID = "different-user"
	store.UpstreamSources["source"] = source
	store.mu.Unlock()
	_, status, err = server.AdminMediaItems(context.Background(), ref, []string{"one"}, true)
	if err != nil || status != 409 || calls.Load() != 3 {
		t.Fatalf("source switch sent old ID to new user: status=%d calls=%d", status, calls.Load())
	}
}

func TestAdminMediaImageBlocksCrossOriginRedirect(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		t.Error("cross-origin image redirect was followed")
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/image", http.StatusFound)
	}))
	defer origin.Close()
	server := NewServer(Config{HTTPClient: origin.Client()}, testStore(origin.URL))
	ref, _ := server.AdminMediaSourceRef(context.Background())
	_, _, status, err := server.AdminMediaImage(context.Background(), ref, "one", "Primary", "small")
	if err != nil || status != 302 || targetCalls.Load() != 0 {
		t.Fatalf("status=%d err=%v target calls=%d", status, err, targetCalls.Load())
	}
}

func TestAdminMediaRejectsOversizeAndInvalidPaths(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, strings.Repeat("x", (2<<20)+1))
	}))
	defer backend.Close()
	server := NewServer(Config{HTTPClient: backend.Client()}, testStore(backend.URL))
	ref, _ := server.AdminMediaSourceRef(context.Background())
	_, status, err := server.AdminMediaItems(context.Background(), ref, []string{"one"}, true)
	if err == nil || status != 503 {
		t.Fatalf("oversize status=%d err=%v", status, err)
	}
	_, status, err = server.AdminMediaItems(context.Background(), ref, []string{"../System/Info"}, true)
	if err == nil || status != 400 || calls.Load() != 1 {
		t.Fatal("invalid ID accessed upstream")
	}
}

func TestAdminMediaRefreshSharesAuthenticatorAndStableReference(t *testing.T) {
	var logins, gets atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/backend-user/Items/one":
			gets.Add(1)
			if r.Header.Get("X-Emby-Token") == "backend-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprint(w, `{"Id":"one","Name":"Movie"}`)
		case "/System/Info":
			w.WriteHeader(http.StatusUnauthorized)
		case "/Users/AuthenticateByName":
			logins.Add(1)
			fmt.Fprint(w, `{"ServerId":"backend-server","AccessToken":"new-backend-token","User":{"Id":"backend-user"}}`)
		case "/Sessions/Logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	server := NewServer(Config{HTTPClient: backend.Client()}, testStore(backend.URL))
	before, _ := server.AdminMediaSourceRef(context.Background())
	data, status, err := server.AdminMediaItems(context.Background(), before, []string{"one"}, true)
	after, _ := server.AdminMediaSourceRef(context.Background())
	if err != nil || status != 200 || before != after || logins.Load() != 1 || gets.Load() != 2 || !strings.Contains(string(data), "Movie") {
		t.Fatalf("status=%d err=%v logins=%d gets=%d ref stable=%v", status, err, logins.Load(), gets.Load(), before == after)
	}
}

func TestAdminMediaRejectsSourceChangeDuringResponse(t *testing.T) {
	var store *MemoryStore
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store.mu.Lock()
		source := store.UpstreamSources["source"]
		source.BackendUserID = "new-user"
		store.UpstreamSources["source"] = source
		store.mu.Unlock()
		fmt.Fprint(w, `{"Id":"one","Name":"Old movie"}`)
	}))
	defer backend.Close()
	store = testStore(backend.URL)
	server := NewServer(Config{HTTPClient: backend.Client()}, store)
	ref, _ := server.AdminMediaSourceRef(context.Background())
	data, status, err := server.AdminMediaItems(context.Background(), ref, []string{"one"}, true)
	if err != nil || status != 409 || len(data) != 0 {
		t.Fatalf("source changed while reading: status=%d err=%v data=%s", status, err, data)
	}
}
