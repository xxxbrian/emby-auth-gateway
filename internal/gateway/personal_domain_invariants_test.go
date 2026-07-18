package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recognizedBackendAuthQueryKeys are stripped when asserting neutral metadata query
// cleanliness. Values under these keys must never be a gateway client token.
var recognizedBackendAuthQueryKeys = map[string]struct{}{
	"api_key":      {},
	"access_token": {},
	"X-Emby-Token": {},
	"token":        {},
}

// egressRequest is one observed upstream request (test-only).
type egressRequest struct {
	Method  string
	Path    string
	Query   string
	Body    string
	ReadErr error
}

// egressRecorder records concurrent-safe upstream egress for Phase 0 invariants.
type egressRecorder struct {
	mu       sync.Mutex
	requests []egressRequest
	handler  http.HandlerFunc
}

func newEgressRecorder(handler http.HandlerFunc) *egressRecorder {
	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}
	}
	return &egressRecorder{handler: handler}
}

func (e *egressRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	rec := egressRequest{
		Method:  r.Method,
		Path:    r.URL.Path,
		Query:   r.URL.RawQuery,
		Body:    string(body),
		ReadErr: err,
	}
	e.mu.Lock()
	e.requests = append(e.requests, rec)
	e.mu.Unlock()
	if err != nil {
		http.Error(w, "egress recorder body read failed", http.StatusInternalServerError)
		return
	}
	// Restore full bytes for the wrapped handler.
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	e.handler(w, r)
}

func (e *egressRecorder) snapshot() []egressRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]egressRequest, len(e.requests))
	copy(out, e.requests)
	return out
}

func (e *egressRecorder) reset() {
	e.mu.Lock()
	e.requests = nil
	e.mu.Unlock()
}

func (e *egressRecorder) assertEmpty(t *testing.T) {
	t.Helper()
	got := e.snapshot()
	for _, req := range got {
		if req.ReadErr != nil {
			t.Fatalf("egress body read error: %v (request=%#v)", req.ReadErr, req)
		}
	}
	if len(got) != 0 {
		t.Fatalf("expected zero upstream egress, got %d: %#v", len(got), got)
	}
}

// assertNeutralMetadataReads requires every recorded request to be a GET to one of
// expectedPaths (exact match), with empty body, clean non-auth query, no gateway token
// in auth query values, and no body-read errors.
func (e *egressRecorder) assertNeutralMetadataReads(t *testing.T, gatewayToken string, expectedPaths ...string) {
	t.Helper()
	if len(expectedPaths) == 0 {
		t.Fatal("assertNeutralMetadataReads requires at least one expected path")
	}
	allowed := make(map[string]struct{}, len(expectedPaths))
	for _, p := range expectedPaths {
		allowed[p] = struct{}{}
	}
	got := e.snapshot()
	if len(got) == 0 {
		t.Fatalf("expected neutral metadata GETs to %v, got none", expectedPaths)
	}
	for i, req := range got {
		if req.ReadErr != nil {
			t.Fatalf("egress[%d] body read error: %v", i, req.ReadErr)
		}
		if req.Method != http.MethodGet {
			t.Fatalf("egress[%d] method = %s, want GET (full=%#v)", i, req.Method, req)
		}
		if _, ok := allowed[req.Path]; !ok {
			t.Fatalf("egress[%d] path = %q, want one of %v", i, req.Path, expectedPaths)
		}
		if req.Body != "" {
			t.Fatalf("egress[%d] body must be empty for neutral metadata GET, got %q", i, req.Body)
		}
		assertCleanBackendAuthQuery(t, req.Query, gatewayToken)
	}
}

// assertExactlyOneNeutralMetadataRead asserts a single neutral metadata GET to wantPath.
func (e *egressRecorder) assertExactlyOneNeutralMetadataRead(t *testing.T, gatewayToken, wantPath string) {
	t.Helper()
	got := e.snapshot()
	if len(got) != 1 {
		t.Fatalf("egress count = %d, want exactly 1; got=%#v", len(got), got)
	}
	e.assertNeutralMetadataReads(t, gatewayToken, wantPath)
	if got[0].Path != wantPath {
		t.Fatalf("egress path = %q, want %q", got[0].Path, wantPath)
	}
}

func assertCleanBackendAuthQuery(t *testing.T, rawQuery, gatewayToken string) {
	t.Helper()
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("parse egress query %q: %v", rawQuery, err)
	}
	for key, vals := range q {
		if _, isAuth := recognizedBackendAuthQueryKeys[key]; !isAuth {
			// Non-auth query keys are not expected on neutral enrichment/proxy metadata reads.
			t.Fatalf("unexpected non-auth query key %q=%v in %q", key, vals, rawQuery)
		}
		for _, val := range vals {
			if gatewayToken != "" && val == gatewayToken {
				t.Fatalf("gateway token leaked into auth query key %q", key)
			}
		}
	}
}

func personalDomainSession(token, gatewayUserID, username, syntheticUserID string) *Session {
	return &Session{
		GatewayTokenHash: HashToken(token),
		GatewayUserID:    gatewayUserID,
		GatewayUsername:  username,
		SyntheticUserID:  syntheticUserID,
		CreatedAt:        time.Now().UTC(),
		ExpiresAt:        time.Now().UTC().Add(time.Hour),
	}
}

func personalDomainStore(backendBaseURL string, sessions ...*Session) *MemoryStore {
	store := NewMemoryStore()
	configureTestUpstream(store, backendBaseURL)
	for _, session := range sessions {
		store.Sessions[session.GatewayTokenHash] = session
	}
	return store
}

// backendItemMetadataPath is the exact upstream path used by enrichPlaybackStateMetadata
// after SyntheticUserID is rewritten to the shared backend user id.
func backendItemMetadataPath(itemID string) string {
	return "/emby/Users/backend-user/Items/" + itemID
}

// TestPersonalDomainLocalMutationZeroEgressMatrix asserts personal mutations and
// local playback/session keepalives terminate at the gateway without mutation egress.
//
// Personal state writes may still issue neutral metadata GETs for enrichment.
// Pure local playback/capability paths must be fully silent.
func TestPersonalDomainLocalMutationZeroEgressMatrix(t *testing.T) {
	const itemID = "item-1"
	wantMetaPath := backendItemMetadataPath(itemID)

	recorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == wantMetaPath {
			writeTestJSON(w, map[string]any{
				"Id":           itemID,
				"Name":         "Item 1",
				"Type":         "Movie",
				"RunTimeTicks": float64(10_000_000),
			})
			return
		}
		t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNoContent)
	})
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	const gatewayToken = "gateway-token"
	session := personalDomainSession(gatewayToken, "u1", "alice", "gateway-user")
	store := personalDomainStore(backend.URL+"/emby", session)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	type tc struct {
		name           string
		method         string
		path           string
		body           string
		wantStatus     int
		allowMetaReads bool
	}
	cases := []tc{
		{name: "post_played", method: http.MethodPost, path: "/Users/gateway-user/PlayedItems/" + itemID, wantStatus: http.StatusOK, allowMetaReads: true},
		{name: "delete_played", method: http.MethodDelete, path: "/Users/gateway-user/PlayedItems/" + itemID, wantStatus: http.StatusOK, allowMetaReads: true},
		{name: "post_favorite", method: http.MethodPost, path: "/Users/gateway-user/FavoriteItems/" + itemID, wantStatus: http.StatusOK, allowMetaReads: true},
		{name: "delete_favorite", method: http.MethodDelete, path: "/Users/gateway-user/FavoriteItems/" + itemID, wantStatus: http.StatusOK, allowMetaReads: true},
		{name: "post_rating_likes_true", method: http.MethodPost, path: "/Users/gateway-user/Items/" + itemID + "/Rating?Likes=true", wantStatus: http.StatusOK, allowMetaReads: true},
		{name: "post_rating_likes_false", method: http.MethodPost, path: "/Users/gateway-user/Items/" + itemID + "/Rating?Likes=false", wantStatus: http.StatusOK, allowMetaReads: true},
		{name: "delete_rating", method: http.MethodDelete, path: "/Users/gateway-user/Items/" + itemID + "/Rating", wantStatus: http.StatusOK, allowMetaReads: true},
		{name: "post_userdata", method: http.MethodPost, path: "/Users/gateway-user/Items/" + itemID + "/UserData", body: `{"PlaybackPositionTicks":321,"PlayedPercentage":33.3}`, wantStatus: http.StatusOK, allowMetaReads: true},
		{name: "sessions_playing", method: http.MethodPost, path: "/Sessions/Playing", body: `{"Item":{"Id":"item-1","RunTimeTicks":10000000},"PositionTicks":100}`, wantStatus: http.StatusNoContent},
		{name: "sessions_progress", method: http.MethodPost, path: "/Sessions/Playing/Progress", body: `{"ItemId":"item-1","PlaybackPositionTicks":250,"RunTimeTicks":10000000,"PlayedPercentage":50.5}`, wantStatus: http.StatusNoContent},
		{name: "sessions_stopped", method: http.MethodPost, path: "/Sessions/Playing/Stopped", body: `{"Item":{"Id":"item-1","RunTimeTicks":10000000},"PositionTicks":5000000}`, wantStatus: http.StatusNoContent},
		{name: "sessions_ping", method: http.MethodPost, path: "/Sessions/Playing/Ping", body: `{}`, wantStatus: http.StatusNoContent},
		{name: "sessions_capabilities", method: http.MethodPost, path: "/Sessions/Capabilities", body: `{}`, wantStatus: http.StatusNoContent},
		{name: "sessions_capabilities_full", method: http.MethodPost, path: "/Sessions/Capabilities/Full", body: `{}`, wantStatus: http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder.reset()
			reqURL := gw.URL + "/emby" + tc.path
			if strings.Contains(tc.path, "?") {
				reqURL += "&api_key=" + gatewayToken
			} else {
				reqURL += "?api_key=" + gatewayToken
			}
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := mustRequest(t, tc.method, reqURL, body)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.allowMetaReads {
				recorder.assertNeutralMetadataReads(t, gatewayToken, wantMetaPath)
				return
			}
			recorder.assertEmpty(t)
		})
	}
}

// TestPersonalDomainTwoUserStateIsolationMatrix asserts gateway-local personal state
// is isolated across two gateway users that share one upstream backend identity.
func TestPersonalDomainTwoUserStateIsolationMatrix(t *testing.T) {
	const itemID = "shared-item"
	wantMetaPath := backendItemMetadataPath(itemID)

	recorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == wantMetaPath {
			writeTestJSON(w, map[string]any{"Id": itemID, "Name": "Shared", "Type": "Movie", "RunTimeTicks": float64(20_000_000)})
			return
		}
		t.Errorf("unexpected mutation egress: %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNoContent)
	})
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	const tokenA = "token-a"
	const tokenB = "token-b"
	sessionA := personalDomainSession(tokenA, "u1", "alice", "gateway-user-a")
	sessionB := personalDomainSession(tokenB, "u2", "bob", "gateway-user-b")
	store := personalDomainStore(backend.URL+"/emby", sessionA, sessionB)

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	postJSON := func(t *testing.T, token, path, body string, want int) {
		t.Helper()
		reqURL := gw.URL + "/emby" + path
		if strings.Contains(path, "?") {
			reqURL += "&api_key=" + token
		} else {
			reqURL += "?api_key=" + token
		}
		req := mustRequest(t, http.MethodPost, reqURL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s status = %d, want %d", path, resp.StatusCode, want)
		}
	}

	// User A mutates a representative personal matrix on the shared item.
	recorder.reset()
	postJSON(t, tokenA, "/Users/gateway-user-a/PlayedItems/"+itemID, "", http.StatusOK)
	postJSON(t, tokenA, "/Users/gateway-user-a/FavoriteItems/"+itemID, "", http.StatusOK)
	postJSON(t, tokenA, "/Users/gateway-user-a/Items/"+itemID+"/Rating?Likes=true", "", http.StatusOK)
	postJSON(t, tokenA, "/Sessions/Playing/Progress", `{"ItemId":"shared-item","PlaybackPositionTicks":7777,"RunTimeTicks":20000000,"PlayedPercentage":12.5}`, http.StatusNoContent)
	// Progress after played-mark may still update position; force resume-like userdata write.
	postJSON(t, tokenA, "/Users/gateway-user-a/Items/"+itemID+"/UserData", `{"PlaybackPositionTicks":4321,"Played":false,"PlayedPercentage":21.6}`, http.StatusOK)
	postJSON(t, tokenA, "/DisplayPreferences/home?Client=web", `{"SortBy":"DateCreated","Owner":"alice"}`, http.StatusOK)
	// Personal writes enrich via neutral GETs; progress/prefs are local-only.
	recorder.assertNeutralMetadataReads(t, tokenA, wantMetaPath)

	// Direct store isolation for played/favorite/rating/resume.
	stateA, err := store.FindPlaybackState(context.Background(), "u1", itemID)
	if err != nil {
		t.Fatalf("user A state: %v", err)
	}
	if stateA.IsFavorite != true || stateA.Likes == nil || !*stateA.Likes || stateA.PlaybackPositionTicks != 4321 {
		t.Fatalf("user A expected own matrix state, got %#v", stateA)
	}
	if _, err := store.FindPlaybackState(context.Background(), "u2", itemID); err == nil {
		t.Fatal("user B must not inherit user A playback state via shared backend identity")
	}

	// User B reads overlays / preferences and must not observe A state.
	backendItem := map[string]any{
		"Id":   itemID,
		"Name": "Shared",
		"Type": "Movie",
		"UserData": map[string]any{
			"Played":                true,
			"IsFavorite":            true,
			"PlaybackPositionTicks": float64(9999),
			"PlayedPercentage":      float64(99),
			"PlayCount":             float64(9),
			"Likes":                 true,
		},
	}
	// Swap backend to return conflicting upstream UserData for neutral reads.
	metaPath := "/emby/Items/" + itemID
	metaRecorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"Item":  backendItem,
			"Items": []any{backendItem},
		})
	})
	metaBackend := httptest.NewServer(metaRecorder)
	defer metaBackend.Close()
	configureTestUpstream(store, metaBackend.URL+"/emby")

	userDataB := fetchUserData(t, gw.URL+"/emby/Items/"+itemID+"?api_key="+tokenB)
	if userDataB["Played"] != false || userDataB["IsFavorite"] != false || int(userDataB["PlaybackPositionTicks"].(float64)) != 0 {
		t.Fatalf("user B observed foreign/upstream state: %#v", userDataB)
	}
	if _, ok := userDataB["Likes"]; ok {
		t.Fatalf("user B should not see user A likes: %#v", userDataB)
	}
	metaRecorder.assertNeutralMetadataReads(t, tokenB, metaPath)

	metaRecorder.reset()
	userDataA := fetchUserData(t, gw.URL+"/emby/Items/"+itemID+"?api_key="+tokenA)
	if userDataA["IsFavorite"] != true || int(userDataA["PlaybackPositionTicks"].(float64)) != 4321 {
		t.Fatalf("user A lost own state: %#v", userDataA)
	}
	if userDataA["Likes"] != true {
		t.Fatalf("user A likes not overlaid: %#v", userDataA)
	}
	metaRecorder.assertNeutralMetadataReads(t, tokenA, metaPath)

	// Display preferences isolation via direct store + HTTP read.
	prefA, err := store.FindDisplayPreference(context.Background(), "u1", "home", "web")
	if err != nil || prefA == nil || !strings.Contains(prefA.PayloadJSON, "alice") {
		t.Fatalf("user A preference missing: %#v err=%v", prefA, err)
	}
	if _, err := store.FindDisplayPreference(context.Background(), "u2", "home", "web"); err == nil {
		t.Fatal("user B must not see user A display preferences")
	}
	getPrefB := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/DisplayPreferences/home?api_key="+tokenB+"&Client=web", nil))
	defer getPrefB.Body.Close()
	if getPrefB.StatusCode != http.StatusOK {
		t.Fatalf("user B preference status = %d, want 200", getPrefB.StatusCode)
	}
	prefBodyB, err := io.ReadAll(getPrefB.Body)
	if err != nil {
		t.Fatalf("read user B preference body: %v", err)
	}
	var prefJSON map[string]any
	if err := json.Unmarshal(prefBodyB, &prefJSON); err != nil {
		t.Fatalf("decode user B preference JSON: %v body=%s", err, prefBodyB)
	}
	if len(prefJSON) != 0 {
		t.Fatalf("user B preference must be empty object, got %#v", prefJSON)
	}
}

// TestPersonalDomainNeutralMetadataLocalUserDataWins asserts neutral metadata reads
// may egress once, but gateway-local UserData always wins over conflicting upstream data.
func TestPersonalDomainNeutralMetadataLocalUserDataWins(t *testing.T) {
	const itemID = "movie-42"
	const gatewayToken = "gateway-token"
	const syntheticUserID = "gateway-user"
	wantMetaPath := "/emby/Items/" + itemID

	recorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"Id":           itemID,
			"Name":         "Upstream Movie",
			"Type":         "Movie",
			"ServerId":     "backend-server",
			"UserId":       "backend-user",
			"RunTimeTicks": float64(50_000_000),
			"UserData": map[string]any{
				"Played":                true,
				"IsFavorite":            true,
				"PlaybackPositionTicks": float64(99999),
				"PlayedPercentage":      float64(99),
				"PlayCount":             float64(42),
				"Likes":                 true,
			},
		})
	})
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	session := personalDomainSession(gatewayToken, "u1", "alice", syntheticUserID)
	store := personalDomainStore(backend.URL+"/emby", session)
	// Stored percentage is intentionally high/wrong; overlay recomputes from local
	// position + item RunTimeTicks when runtime is known (must not use upstream 99).
	stalePct := 99.0
	likes := false
	const localPosition int64 = 6_250_000 // 12.5% of 50_000_000
	if err := store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID:         "u1",
		SyntheticUserID:       syntheticUserID,
		ItemID:                itemID,
		Played:                false,
		IsFavorite:            false,
		PlaybackPositionTicks: localPosition,
		PlayedPercentage:      &stalePct,
		PlayCount:             1,
		Likes:                 &likes,
	}); err != nil {
		t.Fatalf("SavePlaybackState: %v", err)
	}

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodGet, gw.URL+"/emby/Items/"+itemID+"?api_key="+gatewayToken, nil)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	decodeJSON(t, resp.Body, &body)

	// Exactly one neutral metadata GET with allowed backend auth query only.
	recorder.assertExactlyOneNeutralMetadataRead(t, gatewayToken, wantMetaPath)

	// Identity rewrite: upstream UserId becomes synthetic gateway user.
	if got, _ := body["UserId"].(string); got != syntheticUserID {
		t.Fatalf("UserId = %q, want synthetic %q", got, syntheticUserID)
	}

	userData, ok := body["UserData"].(map[string]any)
	if !ok {
		t.Fatalf("missing UserData in %#v", body)
	}
	if userData["Played"] != false || userData["IsFavorite"] != false {
		t.Fatalf("upstream UserData overrode local played/favorite: %#v", userData)
	}
	if int64(userData["PlaybackPositionTicks"].(float64)) != localPosition {
		t.Fatalf("position = %#v, want local %d", userData["PlaybackPositionTicks"], localPosition)
	}
	// Local position + item runtime => 12.5; must not keep upstream 99 or stale stored 99.
	if got := userData["PlayedPercentage"].(float64); got < 12.4 || got > 12.6 {
		t.Fatalf("percentage = %#v, want ~12.5 from local position (not upstream/stale 99)", userData["PlayedPercentage"])
	}
	if int(userData["PlayCount"].(float64)) != 1 {
		t.Fatalf("playCount = %#v, want local 1", userData["PlayCount"])
	}
	if userData["Likes"] != false {
		t.Fatalf("likes = %#v, want local false", userData["Likes"])
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	payload := string(raw)
	// Secret/token leakage is separate from identity rewrite assertions above.
	for _, secret := range []string{"backend-token", "alice-pass", "backend-pass"} {
		if strings.Contains(payload, secret) {
			t.Fatalf("response leaked secret %q: %s", secret, payload)
		}
	}
}

// TestPersonalDomainUnknownRuntimeDoesNotCompleteFromPlayedPercentage encodes the
// architecture invariant: unknown/zero runtime must not establish completion solely
// from PlayedPercentage (or a high position without runtime).
//
// TODO(gateway-personal-domain-architecture#phase-4-current-playback-aggregate):
// require known RunTimeTicks before percentage-based completion, then enable this
// regression test. Current applyStoppedPlaybackState completes on PlayedPercentage
// alone when runtime is unknown.
func TestPersonalDomainUnknownRuntimeDoesNotCompleteFromPlayedPercentage(t *testing.T) {
	t.Skip("TODO(gateway-personal-domain-architecture#phase-4-current-playback-aggregate): require known RunTimeTicks before percentage-based completion, then enable this regression test")

	t.Run("unit_apply_stopped", func(t *testing.T) {
		now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
		pct := 95.0
		state := &PlaybackState{
			GatewayUserID:         "u1",
			SyntheticUserID:       "gateway-user",
			ItemID:                "unknown-runtime-1",
			PlaybackPositionTicks: 8_000_000,
			PlayedPercentage:      &pct,
			RunTimeTicks:          0,
			Played:                false,
		}
		applyStoppedPlaybackState(state, now, false, resumePolicy{
			MinPct:             defaultMinResumePct,
			MaxPct:             defaultMaxResumePct,
			MinDurationSeconds: defaultMinResumeDurationSeconds,
		})
		if state.Played {
			t.Fatalf("unknown runtime must not complete from PlayedPercentage alone: %#v", state)
		}
		if state.PlaybackPositionTicks != 8_000_000 {
			t.Fatalf("resume position not preserved: %#v", state)
		}
		if state.PlayCount != 0 || state.LastPlayedDate != nil {
			t.Fatalf("completion side effects on unknown runtime: %#v", state)
		}
	})

	t.Run("http_stopped_report", func(t *testing.T) {
		// Backend that never supplies runtime (enrichment returns zero/absent RunTimeTicks).
		recorder := newEgressRecorder(func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, map[string]any{
				"Id":   "unknown-runtime-1",
				"Name": "Mystery",
				"Type": "Movie",
				// Deliberately omit RunTimeTicks so stopped enrichment cannot invent runtime.
			})
		})
		backend := httptest.NewServer(recorder)
		defer backend.Close()

		session := personalDomainSession("gateway-token", "u1", "alice", "gateway-user")
		store := personalDomainStore(backend.URL+"/emby", session)
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		body := `{"ItemId":"unknown-runtime-1","PositionTicks":8000000,"PlayedPercentage":95}`
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("stopped status = %d, want 204", resp.StatusCode)
		}

		state, err := store.FindPlaybackState(context.Background(), "u1", "unknown-runtime-1")
		if err != nil {
			t.Fatalf("find state: %v", err)
		}
		if state.Played {
			t.Fatalf("HTTP stopped with unknown runtime completed from PlayedPercentage: %#v", state)
		}
		if state.PlaybackPositionTicks != 8_000_000 {
			t.Fatalf("HTTP stopped did not preserve resume position: %#v", state)
		}
		if state.PlayCount != 0 {
			t.Fatalf("HTTP stopped incremented play count without known runtime: %#v", state)
		}
	})
}
