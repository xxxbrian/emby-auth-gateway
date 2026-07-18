package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"
)

func TestLoginSessionInfoGoldenDefaults(t *testing.T) {
	store := testStore("http://backend.invalid/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	req := mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`)
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="SenPlayer", Device="Mac", DeviceId="dev-1", Version="6.1.3"`)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// Required empty arrays must be exact JSON arrays, never null.
	for _, key := range []string{"SupportedCommands", "PlayableMediaTypes", "AdditionalUsers"} {
		if !strings.Contains(string(rawBody), `"`+key+`":[]`) {
			t.Fatalf("SessionInfo.%s must be exact empty JSON array [] in body: %s", key, rawBody)
		}
		if strings.Contains(string(rawBody), `"`+key+`":null`) {
			t.Fatalf("SessionInfo.%s must not be null: %s", key, rawBody)
		}
	}

	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rawBody)
	}
	info, ok := body["SessionInfo"].(map[string]any)
	if !ok {
		t.Fatalf("SessionInfo missing: %#v", body)
	}
	id, _ := info["Id"].(string)
	if !sessionid.Valid(id) {
		t.Fatalf("SessionInfo.Id = %q, want valid public id", id)
	}
	if info["ServerId"] != "gateway-server" {
		t.Fatalf("ServerId = %#v", info["ServerId"])
	}
	if info["UserId"] != "gateway-user" || info["UserName"] != "alice" {
		t.Fatalf("user fields = %#v", info)
	}
	if info["Client"] != "SenPlayer" || info["DeviceName"] != "Mac" || info["DeviceId"] != "dev-1" || info["ApplicationVersion"] != "6.1.3" {
		t.Fatalf("client/device projection = %#v", info)
	}
	if info["SupportsRemoteControl"] != false {
		t.Fatalf("SupportsRemoteControl = %#v", info["SupportsRemoteControl"])
	}
	if _, hasNowPlaying := info["NowPlayingItem"]; hasNowPlaying {
		t.Fatalf("NowPlayingItem must be omitted while idle: %#v", info)
	}
	for _, key := range []string{"SupportedCommands", "PlayableMediaTypes", "AdditionalUsers"} {
		arr, ok := info[key].([]any)
		if !ok {
			t.Fatalf("%s type = %T, want []any", key, info[key])
		}
		if arr == nil || len(arr) != 0 {
			t.Fatalf("%s = %#v, want empty non-null array", key, info[key])
		}
		got, err := json.Marshal(info[key])
		if err != nil || string(got) != "[]" {
			t.Fatalf("%s marshaled = %s, want []", key, got)
		}
	}
	playState, ok := info["PlayState"].(map[string]any)
	if !ok {
		t.Fatalf("PlayState missing: %#v", info)
	}
	if playState["CanSeek"] != false || playState["IsPaused"] != false || playState["IsMuted"] != false {
		t.Fatalf("idle PlayState = %#v", playState)
	}
	if _, err := time.Parse(time.RFC3339Nano, info["LastActivityDate"].(string)); err != nil {
		t.Fatalf("LastActivityDate parse: %v (%#v)", err, info["LastActivityDate"])
	}
}

func TestSessionCapabilitiesSlimAndFullRoundTrip(t *testing.T) {
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-cccccccccccccccccccccccccccccccc"
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	// Slim: query-only media/commands/bools; preserve later Full unknown fields.
	slim := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities?api_key=gateway-token&PlayableMediaTypes=Video,Audio&PlayableMediaTypes=Video&SupportedCommands=Play,Pause&SupportsMediaControl=true&SupportsSync=false&Id="+session.PublicID, nil)
	resp := do(t, slim)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("slim status/cache = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}

	found, err := store.FindSessionByTokenHash(context.Background(), HashToken("gateway-token"))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !found.Capabilities.SupportsMediaControl || found.Capabilities.SupportsSync {
		t.Fatalf("slim bools = %#v", found.Capabilities)
	}
	if len(found.Capabilities.PlayableMediaTypes) != 2 || found.Capabilities.PlayableMediaTypes[0] != "Video" {
		t.Fatalf("media dedup = %#v", found.Capabilities.PlayableMediaTypes)
	}

	// Full: unknown fields + DeviceProfile object preserved; body Id accepted.
	fullBody := `{"Id":"` + session.PublicID + `","PlayableMediaTypes":["Photo"],"SupportedCommands":["MoveUp"],"SupportsMediaControl":false,"SupportsSync":true,"DeviceProfile":{"Name":"Sen"},"CustomFlag":true}`
	full := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities/Full?api_key=gateway-token", strings.NewReader(fullBody))
	full.Header.Set("Content-Type", "application/json")
	resp = do(t, full)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("full status = %d", resp.StatusCode)
	}
	found, err = store.FindSessionByTokenHash(context.Background(), HashToken("gateway-token"))
	if err != nil {
		t.Fatalf("find after full: %v", err)
	}
	if found.Capabilities.SupportsSync != true || found.Capabilities.SupportsMediaControl {
		t.Fatalf("full bools = %#v", found.Capabilities)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(found.Capabilities.RawJSON), &doc); err != nil {
		t.Fatalf("raw json: %v", err)
	}
	if doc["CustomFlag"] != true {
		t.Fatalf("unknown field lost: %#v", doc)
	}
	dp, ok := doc["DeviceProfile"].(map[string]any)
	if !ok || dp["Name"] != "Sen" {
		t.Fatalf("DeviceProfile = %#v", doc["DeviceProfile"])
	}
	if _, hasID := doc["Id"]; hasID {
		t.Fatalf("Id must not be stored in capabilities JSON: %#v", doc)
	}
}

func TestSessionCapabilitiesIDAndDeviceProfileErrors(t *testing.T) {
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-dddddddddddddddddddddddddddddddd"
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	// Missing Id accepted.
	resp := do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("missing id status = %d", resp.StatusCode)
	}

	// Malformed Id => 400
	resp = do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities?api_key=gateway-token&Id=not-a-session-id", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed id status = %d", resp.StatusCode)
	}

	// Foreign valid Id => 404
	foreign, _ := sessionid.New()
	resp = do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities?api_key=gateway-token&Id="+foreign, nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign id status = %d", resp.StatusCode)
	}

	// Query/body conflict => 400
	other, _ := sessionid.New()
	body := `{"Id":"` + other + `"}`
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities/Full?api_key=gateway-token&Id="+session.PublicID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp = do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("conflict status = %d", resp.StatusCode)
	}

	// DeviceProfile array => 400
	req = mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities/Full?api_key=gateway-token", strings.NewReader(`{"DeviceProfile":[]}`))
	req.Header.Set("Content-Type", "application/json")
	resp = do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("device profile array status = %d", resp.StatusCode)
	}
}

func TestSessionCapabilitiesOversizeAndPersistenceFailure(t *testing.T) {
	base := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	base.Sessions[HashToken("gateway-token")] = session

	// Oversized body
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, base))
	defer gw.Close()
	huge := `{"Pad":"` + strings.Repeat("x", sessionCapabilitiesMaxBytes) + `"}`
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities/Full?api_key=gateway-token", strings.NewReader(huge))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d, want 413", resp.StatusCode)
	}

	// Persistence failure => 500
	failing := &failingCapabilitiesStore{MemoryStore: base}
	gw2 := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, failing))
	defer gw2.Close()
	req = mustRequest(t, http.MethodPost, gw2.URL+"/emby/Sessions/Capabilities?api_key=gateway-token&PlayableMediaTypes=Video", nil)
	resp = do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("persist fail status = %d", resp.StatusCode)
	}
}

type failingCapabilitiesStore struct {
	*MemoryStore
}

func (f *failingCapabilitiesStore) UpdateSessionCapabilities(ctx context.Context, tokenHash string, capabilities SessionCapabilities, at time.Time) (*Session, error) {
	return nil, errors.New("persist failed")
}

func TestSessionCapabilitiesArrayBounds(t *testing.T) {
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-ffffffffffffffffffffffffffffffff"
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	// Too many media types (must be unique so dedup does not shrink the list).
	media := make([]string, maxPlayableMediaTypes+1)
	for i := range media {
		media[i] = "Media" + strconv.Itoa(i)
	}
	body, _ := json.Marshal(map[string]any{"PlayableMediaTypes": media})
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities/Full?api_key=gateway-token", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("too many media status = %d", resp.StatusCode)
	}

	// Value too long
	long := strings.Repeat("z", maxPlayableMediaTypeLen+1)
	req = mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities?api_key=gateway-token&PlayableMediaTypes="+long, nil)
	resp = do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("long media status = %d", resp.StatusCode)
	}
}

func TestSessionCapabilitiesFullRejectsNullArrayMalformed(t *testing.T) {
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	for _, body := range []string{`null`, `[]`, `{`, `{}{}`, `{"DeviceProfile":[]}`, `{"DeviceProfile":"nope"}`} {
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities/Full?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestSessionCapabilitiesFullPreservesLargeUnknownInteger(t *testing.T) {
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	// 2^53+1 cannot survive float64 round-trip.
	const huge = "9007199254740993"
	body := `{"PlayableMediaTypes":[],"SupportedCommands":[],"SupportsMediaControl":false,"SupportsSync":false,"Huge":` + huge + `,"DeviceProfile":{"Bitrate":` + huge + `}}`
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities/Full?api_key=gateway-token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	found, err := store.FindSessionByTokenHash(context.Background(), HashToken("gateway-token"))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if !strings.Contains(found.Capabilities.RawJSON, huge) {
		t.Fatalf("large integer not preserved: %q", found.Capabilities.RawJSON)
	}
	// Count occurrences: top-level Huge and nested DeviceProfile.Bitrate.
	if strings.Count(found.Capabilities.RawJSON, huge) < 2 {
		t.Fatalf("expected both Huge and DeviceProfile bitrate preserved: %q", found.Capabilities.RawJSON)
	}
}

func TestSessionCapabilitiesSlimIgnoresFormBody(t *testing.T) {
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-99999999999999999999999999999999"
	// Seed existing media so we can detect if form body incorrectly overrides.
	seed, err := ParseSessionCapabilities(`{"PlayableMediaTypes":["Audio"],"SupportedCommands":[],"SupportsMediaControl":false,"SupportsSync":false}`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	session.Capabilities = seed
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	// Query says Video; form body tries to set Photo and SupportsMediaControl=true.
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities?api_key=gateway-token&PlayableMediaTypes=Video", strings.NewReader("PlayableMediaTypes=Photo&SupportsMediaControl=true&Id=not-a-session-id"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (form body Id must be ignored)", resp.StatusCode)
	}
	found, err := store.FindSessionByTokenHash(context.Background(), HashToken("gateway-token"))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(found.Capabilities.PlayableMediaTypes) != 1 || found.Capabilities.PlayableMediaTypes[0] != "Video" {
		t.Fatalf("media = %#v, want [Video] from query only", found.Capabilities.PlayableMediaTypes)
	}
	if found.Capabilities.SupportsMediaControl {
		t.Fatalf("SupportsMediaControl must remain false; form body must not apply")
	}
}

func TestLookupActiveSessionCanceledNoBodyNoAudit(t *testing.T) {
	base := NewMemoryStore()
	store := &canceledFindSessionStore{MemoryStore: base}
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/Me?api_key=gateway-token", nil)
	req = req.WithContext(canceledContext())
	writer := httptest.NewRecorder()

	requireAbortHandler(t, func() {
		_, _ = server.lookupActiveSession(writer, req, "gateway-token")
	})
	if writer.Body.Len() != 0 {
		t.Fatalf("canceled lookup wrote body: %q", writer.Body.String())
	}
	if len(base.AuditLogs) != 0 {
		t.Fatalf("canceled lookup wrote audit: %#v", base.AuditLogs)
	}
	if writer.Code != http.StatusOK && writer.Code != 200 {
		// httptest.ResponseRecorder defaults Code to 200 until WriteHeader; abort must not set 401/500.
		if writer.Code == http.StatusUnauthorized || writer.Code == http.StatusInternalServerError {
			t.Fatalf("canceled lookup set status %d", writer.Code)
		}
	}
}

type canceledFindSessionStore struct {
	*MemoryStore
}

func (c *canceledFindSessionStore) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, context.Canceled
}

func TestGetSessionsLocalZeroEgressAndFilters(t *testing.T) {
	recorder := newEgressRecorder(func(http.ResponseWriter, *http.Request) {
		// should never be called
	})
	backend := httptest.NewServer(recorder)
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")

	now := time.Now().UTC()
	a := testSession()
	a.GatewayTokenHash = HashToken("token-a")
	a.PublicID = "session-11111111111111111111111111111111"
	a.DeviceID = "device-a"
	a.LastActivityAt = now.Add(-time.Minute)
	a.CreatedAt = now.Add(-time.Hour)
	a.ExpiresAt = now.Add(time.Hour)
	store.Sessions[a.GatewayTokenHash] = a

	b := testSession()
	b.GatewayTokenHash = HashToken("token-b")
	b.PublicID = "session-22222222222222222222222222222222"
	b.DeviceID = "device-b"
	b.LastActivityAt = now
	b.CreatedAt = now.Add(-time.Hour)
	b.ExpiresAt = now.Add(time.Hour)
	store.Sessions[b.GatewayTokenHash] = b

	// Other user session must not appear.
	other := testSession()
	other.GatewayTokenHash = HashToken("token-other")
	other.GatewayUserID = "u2"
	other.PublicID = "session-33333333333333333333333333333333"
	other.DeviceID = "device-other"
	other.ExpiresAt = now.Add(time.Hour)
	store.Sessions[other.GatewayTokenHash] = other

	// Revoked / expired excluded.
	revoked := testSession()
	revoked.GatewayTokenHash = HashToken("token-revoked")
	revoked.PublicID = "session-44444444444444444444444444444444"
	rev := now.Add(-time.Minute)
	revoked.RevokedAt = &rev
	store.Sessions[revoked.GatewayTokenHash] = revoked

	expired := testSession()
	expired.GatewayTokenHash = HashToken("token-expired")
	expired.PublicID = "session-55555555555555555555555555555555"
	expired.ExpiresAt = now.Add(-time.Minute)
	store.Sessions[expired.GatewayTokenHash] = expired

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gw"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=token-a", nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var sessions []map[string]any
	decodeJSON(t, resp.Body, &sessions)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %#v, want 2 active for u1", sessions)
	}
	// Sorted LastActivityAt desc: b then a
	if sessions[0]["Id"] != b.PublicID || sessions[1]["Id"] != a.PublicID {
		t.Fatalf("order = %#v", sessions)
	}
	recorder.assertEmpty(t)

	// DeviceId filter exact
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=token-a&DeviceId=device-a", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var filtered []map[string]any
	if err := json.Unmarshal(body, &filtered); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(filtered) != 1 || filtered[0]["DeviceId"] != "device-a" {
		t.Fatalf("device filter = %#v", filtered)
	}

	// Id filter exact case-sensitive
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=token-a&Id="+b.PublicID, nil))
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err := json.Unmarshal(body, &filtered); err != nil {
		t.Fatalf("decode id: %v", err)
	}
	if len(filtered) != 1 || filtered[0]["Id"] != b.PublicID {
		t.Fatalf("id filter = %#v", filtered)
	}

	// ControllableByUserId foreign => []
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=token-a&ControllableByUserId=someone-else", nil))
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "[]\n" && string(body) != "[]" {
		// writeJSON encodes with newline
		var empty []any
		_ = json.Unmarshal(body, &empty)
		if len(empty) != 0 {
			t.Fatalf("foreign controllable = %s", body)
		}
	}
	recorder.assertEmpty(t)
}

func TestGetSessionsRepositoryError500(t *testing.T) {
	store := &failingListSessionsStore{MemoryStore: NewMemoryStore()}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

type failingListSessionsStore struct {
	*MemoryStore
}

func (f *failingListSessionsStore) ListActiveSessions(ctx context.Context, gatewayUserID string, now time.Time) ([]Session, error) {
	return nil, errors.New("list failed")
}

func TestActiveSessionLookupErrNotFoundVsOperational(t *testing.T) {
	// ErrNotFound / inactive => 401
	store := NewMemoryStore()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/Me?api_key=missing", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing status = %d, want 401", resp.StatusCode)
	}

	// Operational error => 500
	failing := &failingFindSessionStore{MemoryStore: NewMemoryStore()}
	gw2 := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, failing))
	defer gw2.Close()
	resp = do(t, mustRequest(t, http.MethodGet, gw2.URL+"/emby/Users/Me?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("operational status = %d, want 500", resp.StatusCode)
	}
}

type failingFindSessionStore struct {
	*MemoryStore
}

func (f *failingFindSessionStore) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	return nil, errors.New("db down")
}

func TestSessionActivityTouchExclusionsAndCoalescing(t *testing.T) {
	base := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-66666666666666666666666666666666"
	session.LastActivityAt = time.Now().UTC().Add(-time.Hour)
	base.Sessions[HashToken("gateway-token")] = session

	touch := &countingTouchStore{MemoryStore: base}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, touch))
	defer gw.Close()

	// Accepted current-user should touch.
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/Me?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d", resp.StatusCode)
	}
	if touch.touches() != 1 {
		t.Fatalf("touches after me = %d, want 1", touch.touches())
	}

	// Capabilities should NOT call TouchSessionActivity (UpdateSessionCapabilities handles activity).
	before := touch.touches()
	beforeUpdates := touch.updates()
	resp = do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Capabilities?api_key=gateway-token&PlayableMediaTypes=Video", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("caps status = %d", resp.StatusCode)
	}
	if touch.touches() != before {
		t.Fatalf("capabilities must not TouchSessionActivity: touches %d -> %d", before, touch.touches())
	}
	if touch.updates() != beforeUpdates+1 {
		t.Fatalf("capabilities must UpdateSessionCapabilities once")
	}

	// Logout notes but must not touch (and revokes).
	before = touch.touches()
	resp = do(t, mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Logout?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status = %d", resp.StatusCode)
	}
	if touch.touches() != before {
		t.Fatalf("logout must not touch activity")
	}
}

type countingTouchStore struct {
	*MemoryStore
	mu          sync.Mutex
	touchCalls  int
	updateCalls int
}

func (c *countingTouchStore) TouchSessionActivity(ctx context.Context, tokenHash string, at time.Time, minInterval time.Duration) (bool, error) {
	c.mu.Lock()
	c.touchCalls++
	c.mu.Unlock()
	return c.MemoryStore.TouchSessionActivity(ctx, tokenHash, at, minInterval)
}

func (c *countingTouchStore) UpdateSessionCapabilities(ctx context.Context, tokenHash string, capabilities SessionCapabilities, at time.Time) (*Session, error) {
	c.mu.Lock()
	c.updateCalls++
	c.mu.Unlock()
	return c.MemoryStore.UpdateSessionCapabilities(ctx, tokenHash, capabilities, at)
}

func (c *countingTouchStore) touches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.touchCalls
}

func (c *countingTouchStore) updates() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.updateCalls
}
