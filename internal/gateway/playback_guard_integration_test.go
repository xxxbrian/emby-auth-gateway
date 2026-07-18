package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlaybackGuardScopesAndAllowsDirectPlayedItems(t *testing.T) {
	const tokenOne = "gateway-token"
	const tokenTwo = "gateway-token-two"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/emby/Items/item-1/PlaybackInfo" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"reason_code":"max_concurrent_sessions_exceeded"}`)
			return
		}
		writeTestJSON(w, map[string]any{})
	}))
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken(tokenOne)] = testSession()
	second := testSession()
	second.GatewayTokenHash = HashToken(tokenTwo)
	store.Sessions[HashToken(tokenTwo)] = second
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	mustPlaybackInfoStatus(t, gw.URL, tokenOne, "item-1", http.StatusForbidden)
	mustPlaybackReportStatus(t, gw.URL, tokenOne, "item-1", http.StatusOK)
	mustPlaybackReportStatus(t, gw.URL, tokenOne, "item-2", http.StatusOK)
	mustPlaybackReportStatus(t, gw.URL, tokenTwo, "item-1", http.StatusOK)
	if len(store.PlaybackEvents) != 2 {
		t.Fatalf("playback events = %d, want reports for other item and session", len(store.PlaybackEvents))
	}

	direct := mustRequest(t, http.MethodPost, gw.URL+"/emby/Users/gateway-user/PlayedItems/item-1?api_key="+tokenOne, nil)
	resp := do(t, direct)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct played item status = %d", resp.StatusCode)
	}
	state, err := store.FindPlaybackState(t.Context(), "u1", "item-1")
	if err != nil || !state.Played {
		t.Fatalf("direct played item was not persisted: %#v err=%v", state, err)
	}
}

func TestSuppressedPlaybackReportDoesNotRequireUpstream(t *testing.T) {
	const token = "gateway-token"
	store := NewMemoryStore()
	session := testSession()
	store.Sessions[HashToken(token)] = session
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: "item-1"})
	recorder := httptest.NewRecorder()
	req := mustRequest(t, http.MethodPost, "http://gateway/emby/Sessions/Playing/Stopped?api_key="+token, strings.NewReader(`{"ItemId":"item-1","Played":true}`))
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || len(store.PlaybackEvents) != 0 {
		t.Fatalf("status/events = %d/%d", recorder.Code, len(store.PlaybackEvents))
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", recorder.Header().Get("Cache-Control"))
	}
}

func TestPlaybackGuardUsesCanonicalPreparedItemID(t *testing.T) {
	const token = "gateway-token"
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore()}
	session := testSession()
	store.Sessions[HashToken(token)] = session
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: "item-1"})

	recorder := httptest.NewRecorder()
	req := mustRequest(t, http.MethodPost, "http://gateway/emby/Sessions/Playing/Progress?api_key="+token, strings.NewReader(`{"ItemId":" item-1 ","PositionTicks":10}`))
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if store.applyCalls != 0 {
		t.Fatalf("ApplyPlaybackReport calls = %d, want 0", store.applyCalls)
	}
}

func TestPlaybackInfoSuccessClearsGuard(t *testing.T) {
	const token = "gateway-token"
	var deny atomic.Bool
	deny.Store(true)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if deny.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"reason_code":"max_concurrent_sessions_exceeded"}`)
			return
		}
		writeTestJSON(w, map[string]any{})
	}))
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken(token)] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()
	mustPlaybackInfoStatus(t, gw.URL, token, "item-1", http.StatusForbidden)
	deny.Store(false)
	mustPlaybackInfoStatus(t, gw.URL, token, "item-1", http.StatusOK)
	mustPlaybackReportStatus(t, gw.URL, token, "item-1", http.StatusOK)
	if len(store.PlaybackEvents) != 1 {
		t.Fatalf("cleared guard still suppressed lifecycle report: %#v", store.PlaybackEvents)
	}
}

func TestOldPlaybackInfoSuccessCannotClearNewDenial(t *testing.T) {
	const token = "gateway-token"
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			close(oldStarted)
			<-releaseOld
			writeTestJSON(w, map[string]any{})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"reason_code":"max_concurrent_sessions_exceeded"}`)
	}))
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	session := testSession()
	store.Sessions[HashToken(token)] = session
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: "item-1"})
	gw := httptest.NewServer(server)
	defer gw.Close()

	oldStatus := make(chan int, 1)
	go func() {
		resp, err := http.Get(gw.URL + "/emby/Items/item-1/PlaybackInfo?api_key=" + token)
		if err != nil {
			oldStatus <- 0
			return
		}
		_ = resp.Body.Close()
		oldStatus <- resp.StatusCode
	}()
	<-oldStarted
	mustPlaybackInfoStatus(t, gw.URL, token, "item-1", http.StatusForbidden)
	close(releaseOld)
	if status := <-oldStatus; status != http.StatusOK {
		t.Fatalf("old PlaybackInfo status = %d, want %d", status, http.StatusOK)
	}
	mustPlaybackReportStatus(t, gw.URL, token, "item-1", http.StatusOK)
	if len(store.PlaybackEvents) != 0 {
		t.Fatal("old success cleared newer denial guard")
	}
}

func mustPlaybackInfoStatus(t *testing.T, gatewayURL, token, itemID string, want int) {
	t.Helper()
	resp := do(t, mustRequest(t, http.MethodGet, gatewayURL+"/emby/Items/"+itemID+"/PlaybackInfo?api_key="+token, nil))
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PlaybackInfo status = %d, want %d: %s", resp.StatusCode, want, body)
	}
}

func mustPlaybackReportStatus(t *testing.T, gatewayURL, token, itemID string, want int) {
	t.Helper()
	req := mustRequest(t, http.MethodPost, gatewayURL+"/emby/Sessions/Playing/Stopped?api_key="+token, strings.NewReader(`{"ItemId":"`+itemID+`","Played":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("playback report status = %d, want %d", resp.StatusCode, want)
	}
}

func TestPlaybackConcurrencyAuditCooldown(t *testing.T) {
	const token = "gateway-token"
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"reason_code":"max_concurrent_sessions_exceeded"}`)
	}))
	defer backend.Close()
	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken(token)] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	server.playbackGuards.now = func() time.Time { return now }
	gw := httptest.NewServer(server)
	defer gw.Close()

	for range 2 {
		mustPlaybackInfoStatus(t, gw.URL, token, "item-1", http.StatusForbidden)
		mustPlaybackReportStatus(t, gw.URL, token, "item-1", http.StatusOK)
	}
	assertConcurrencyAudits(t, store, 1, 1)
	now = now.Add(playbackAuditCooldown)
	mustPlaybackInfoStatus(t, gw.URL, token, "item-1", http.StatusForbidden)
	mustPlaybackReportStatus(t, gw.URL, token, "item-1", http.StatusOK)
	assertConcurrencyAudits(t, store, 2, 2)
}

func assertConcurrencyAudits(t *testing.T, store *MemoryStore, denials, suppressions int) {
	t.Helper()
	gotDenials, gotSuppressions := 0, 0
	for _, entry := range store.AuditLogs {
		switch entry.Event {
		case "playback_concurrency_denied":
			gotDenials++
			if entry.Status != http.StatusForbidden || entry.Message != "playback denied because the concurrent playback limit was exceeded" || strings.Contains(entry.Path, "?") {
				t.Fatalf("bad denial audit: %#v", entry)
			}
		case "playback_report_suppressed":
			gotSuppressions++
			if entry.Status != http.StatusOK || entry.Message != "playback report suppressed after concurrent playback denial" || strings.Contains(entry.Path, "?") {
				t.Fatalf("bad suppression audit: %#v", entry)
			}
		}
	}
	if gotDenials != denials || gotSuppressions != suppressions {
		t.Fatalf("denial/suppression audit count = %d/%d, want %d/%d", gotDenials, gotSuppressions, denials, suppressions)
	}
}
