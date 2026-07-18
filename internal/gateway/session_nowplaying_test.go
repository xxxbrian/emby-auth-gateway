package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGetSessionsIdleOmitsNowPlaying(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	session.DeviceID = "dev-idle"
	session.Client = "SenPlayer"
	session.Device = "Mac"
	session.Version = "6.1.3"
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gw"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache = %d/%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	var sessions []map[string]any
	decodeJSON(t, resp.Body, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %#v", sessions)
	}
	info := sessions[0]
	if _, has := info["NowPlayingItem"]; has {
		t.Fatalf("idle NowPlayingItem must be absent: %#v", info)
	}
	ps, ok := info["PlayState"].(map[string]any)
	if !ok {
		t.Fatalf("PlayState missing: %#v", info)
	}
	if _, hasPos := ps["PositionTicks"]; hasPos {
		t.Fatalf("idle PlayState must omit PositionTicks: %#v", ps)
	}
	if ps["CanSeek"] != false || ps["IsPaused"] != false || ps["IsMuted"] != false {
		t.Fatalf("idle PlayState = %#v", ps)
	}
	if info["SupportsRemoteControl"] != false {
		t.Fatalf("SupportsRemoteControl = %#v", info["SupportsRemoteControl"])
	}
	for _, forbidden := range []string{"PlaySessionId", "LastPlaybackCheckIn", "NowPlayingQueue", "NowPlayingQueueFullItems", "PlaylistItemId"} {
		if _, has := info[forbidden]; has {
			t.Fatalf("forbidden field %s present: %#v", forbidden, info)
		}
		if _, has := ps[forbidden]; has {
			t.Fatalf("forbidden PlayState field %s present: %#v", forbidden, ps)
		}
	}
}

func TestGetSessionsActiveProjectsSnapshotAndPlayState(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	session := testSession()
	session.PublicID = "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	session.DeviceID = "dev-active"
	session.Client = "SenPlayer"
	session.Device = "Mac"
	session.Version = "6.1.3"
	session.LastActivityAt = now
	store.Sessions[HashToken("gateway-token")] = session

	pos := int64(12345)
	canSeek := true
	paused := false
	muted := true
	vol := 0
	audio := 0
	sub := -1
	rate := 1.0
	shuffle := false
	offset := 0.0
	method := "DirectPlay"
	repeat := "RepeatNone"
	ms := "ms-1"

	store.CurrentPlaybacks = map[string]*CurrentPlayback{
		HashToken("gateway-token"): {
			GatewayTokenHash: HashToken("gateway-token"),
			ItemID:           "item-1",
			PlaySessionID:    "ps-must-omit",
			MediaSourceID:    ms,
			ItemSnapshot: PlaybackItemSnapshot{
				ID: "item-1", Name: "Episode 1", Type: "Episode", MediaType: "Video",
				SeriesID: "show-1", SeriesName: "Show", SeasonID: "season-1", ParentID: "par-1",
				IndexNumber: 3, ParentIndexNumber: 1, RunTimeTicks: 9_000_000,
				ProductionYear: 2024, PremiereDate: "2024-01-02", CommunityRating: 8.5,
				OfficialRating: "TV-14", ImageTags: map[string]string{"Primary": "tag"},
			},
			PlayState: PlaybackPlayState{
				PositionTicks: &pos, CanSeek: &canSeek, IsPaused: &paused, IsMuted: &muted,
				VolumeLevel: &vol, AudioStreamIndex: &audio, SubtitleStreamIndex: &sub,
				MediaSourceID: &ms, PlayMethod: &method, PlaybackRate: &rate,
				RepeatMode: &repeat, Shuffle: &shuffle, SubtitleOffset: &offset,
			},
			RunTimeTicks:   9_000_000,
			StartedAt:      now.Add(-time.Hour),
			LastReportedAt: now.Add(-time.Minute),
		},
	}
	// Local UserData (not backend).
	_ = store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-1",
		PlaybackPositionTicks: 999, Played: false, IsFavorite: true, PlayCount: 2,
		PlayedPercentage: floatPtr(11),
	})

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gw"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var sessions []map[string]any
	decodeJSON(t, resp.Body, &sessions)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %#v", sessions)
	}
	info := sessions[0]
	np, ok := info["NowPlayingItem"].(map[string]any)
	if !ok {
		t.Fatalf("NowPlayingItem missing: %#v", info)
	}
	if np["Id"] != "item-1" || np["Name"] != "Episode 1" || np["Type"] != "Episode" || np["MediaType"] != "Video" {
		t.Fatalf("snapshot core: %#v", np)
	}
	if np["SeriesId"] != "show-1" || np["SeasonId"] != "season-1" || np["ParentId"] != "par-1" {
		t.Fatalf("snapshot hierarchy: %#v", np)
	}
	if int(np["IndexNumber"].(float64)) != 3 || int(np["ParentIndexNumber"].(float64)) != 1 {
		t.Fatalf("indexes: %#v", np)
	}
	if int64(np["RunTimeTicks"].(float64)) != 9_000_000 {
		t.Fatalf("runtime: %#v", np["RunTimeTicks"])
	}
	// UserData is gateway-local overlay.
	ud, ok := np["UserData"].(map[string]any)
	if !ok {
		t.Fatalf("UserData missing: %#v", np)
	}
	if ud["IsFavorite"] != true || ud["Played"] != false || int(ud["PlaybackPositionTicks"].(float64)) != 999 {
		t.Fatalf("local UserData overlay: %#v", ud)
	}
	if int(ud["PlayCount"].(float64)) != 2 {
		t.Fatalf("PlayCount = %#v", ud["PlayCount"])
	}

	ps, ok := info["PlayState"].(map[string]any)
	if !ok {
		t.Fatalf("PlayState missing")
	}
	if int64(ps["PositionTicks"].(float64)) != 12345 {
		t.Fatalf("PositionTicks = %#v", ps["PositionTicks"])
	}
	if ps["CanSeek"] != true || ps["IsPaused"] != false || ps["IsMuted"] != true {
		t.Fatalf("bool triad: %#v", ps)
	}
	if int(ps["VolumeLevel"].(float64)) != 0 || int(ps["AudioStreamIndex"].(float64)) != 0 || int(ps["SubtitleStreamIndex"].(float64)) != -1 {
		t.Fatalf("explicit zero optionals: %#v", ps)
	}
	if ps["MediaSourceId"] != "ms-1" || ps["PlayMethod"] != "DirectPlay" || ps["RepeatMode"] != "RepeatNone" {
		t.Fatalf("string optionals: %#v", ps)
	}
	if ps["Shuffle"] != false || ps["PlaybackRate"].(float64) != 1.0 || ps["SubtitleOffset"].(float64) != 0 {
		t.Fatalf("optional false/zero: %#v", ps)
	}
	// Forbidden fields
	for _, forbidden := range []string{"PlaySessionId", "LastPlaybackCheckIn", "NowPlayingQueue", "NowPlayingQueueFullItems", "MediaSource"} {
		if _, has := info[forbidden]; has {
			t.Fatalf("forbidden %s on session: %#v", forbidden, info)
		}
		if _, has := ps[forbidden]; has {
			t.Fatalf("forbidden %s on PlayState: %#v", forbidden, ps)
		}
		if _, has := np[forbidden]; has {
			t.Fatalf("forbidden %s on NowPlayingItem: %#v", forbidden, np)
		}
	}
	if info["SupportsRemoteControl"] != false {
		t.Fatalf("SupportsRemoteControl = %#v", info["SupportsRemoteControl"])
	}
}

func TestGetSessionsOptionalOmissionAndExplicitZeros(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	session := testSession()
	session.PublicID = "session-cccccccccccccccccccccccccccccccc"
	store.Sessions[HashToken("gateway-token")] = session

	// Minimal active: no optional play fields, empty snapshot beyond Id.
	store.CurrentPlaybacks = map[string]*CurrentPlayback{
		HashToken("gateway-token"): {
			GatewayTokenHash: HashToken("gateway-token"),
			ItemID:           "only-id",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "only-id"},
			PlayState:        PlaybackPlayState{}, // all nil
			StartedAt:        now.Add(-time.Minute),
			LastReportedAt:   now,
		},
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	defer resp.Body.Close()
	var sessions []map[string]any
	decodeJSON(t, resp.Body, &sessions)
	np := sessions[0]["NowPlayingItem"].(map[string]any)
	if np["Id"] != "only-id" {
		t.Fatalf("Id = %#v", np["Id"])
	}
	for _, key := range []string{"Name", "Type", "SeriesId", "RunTimeTicks", "IndexNumber", "ImageTags"} {
		if _, has := np[key]; has {
			t.Fatalf("empty field %s should be omitted: %#v", key, np)
		}
	}
	ps := sessions[0]["PlayState"].(map[string]any)
	if int64(ps["PositionTicks"].(float64)) != 0 {
		t.Fatalf("absent position projects as 0: %#v", ps["PositionTicks"])
	}
	if ps["CanSeek"] != false || ps["IsPaused"] != false || ps["IsMuted"] != false {
		t.Fatalf("default bools: %#v", ps)
	}
	for _, key := range []string{"VolumeLevel", "AudioStreamIndex", "SubtitleStreamIndex", "MediaSourceId", "PlayMethod", "PlaybackRate", "RepeatMode", "Shuffle", "SubtitleOffset"} {
		if _, has := ps[key]; has {
			t.Fatalf("unreported optional %s must be omitted: %#v", key, ps)
		}
	}

	// Explicit zero position + false CanSeek.
	pos := int64(0)
	canSeek := false
	store.CurrentPlaybacks[HashToken("gateway-token")].PlayState = PlaybackPlayState{
		PositionTicks: &pos, CanSeek: &canSeek,
	}
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	decodeJSON(t, resp.Body, &sessions)
	_ = resp.Body.Close()
	ps = sessions[0]["PlayState"].(map[string]any)
	if int64(ps["PositionTicks"].(float64)) != 0 || ps["CanSeek"] != false {
		t.Fatalf("explicit zero/false: %#v", ps)
	}
}

func TestGetSessionsTwoDevicesAndTwoUsersIsolate(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()

	devA := testSession()
	devA.GatewayTokenHash = HashToken("token-a")
	devA.PublicID = "session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	devA.DeviceID = "device-a"
	devA.LastActivityAt = now
	store.Sessions[devA.GatewayTokenHash] = devA

	devB := testSession()
	devB.GatewayTokenHash = HashToken("token-b")
	devB.PublicID = "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	devB.DeviceID = "device-b"
	devB.LastActivityAt = now.Add(-time.Minute)
	store.Sessions[devB.GatewayTokenHash] = devB

	otherUser := testSession()
	otherUser.GatewayTokenHash = HashToken("token-other")
	otherUser.GatewayUserID = "u2"
	otherUser.SyntheticUserID = "gateway-user-2"
	otherUser.GatewayUsername = "bob"
	otherUser.PublicID = "session-cccccccccccccccccccccccccccccccc"
	otherUser.DeviceID = "device-other"
	otherUser.LastActivityAt = now
	store.Sessions[otherUser.GatewayTokenHash] = otherUser

	posA, posB, posO := int64(100), int64(200), int64(9999)
	store.CurrentPlaybacks = map[string]*CurrentPlayback{
		devA.GatewayTokenHash: {
			GatewayTokenHash: devA.GatewayTokenHash, ItemID: "item-a",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-a", Name: "A"},
			PlayState:    PlaybackPlayState{PositionTicks: &posA},
			StartedAt:    now.Add(-time.Hour), LastReportedAt: now,
		},
		devB.GatewayTokenHash: {
			GatewayTokenHash: devB.GatewayTokenHash, ItemID: "item-b",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-b", Name: "B"},
			PlayState:    PlaybackPlayState{PositionTicks: &posB},
			StartedAt:    now.Add(-time.Hour), LastReportedAt: now,
		},
		otherUser.GatewayTokenHash: {
			GatewayTokenHash: otherUser.GatewayTokenHash, ItemID: "item-other",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-other", Name: "Other"},
			PlayState:    PlaybackPlayState{PositionTicks: &posO},
			StartedAt:    now.Add(-time.Hour), LastReportedAt: now,
		},
	}
	_ = store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u1", ItemID: "item-a", IsFavorite: true, PlaybackPositionTicks: 100,
	})
	_ = store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u2", ItemID: "item-other", IsFavorite: true, PlaybackPositionTicks: 9999,
	})

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=token-a", nil))
	defer resp.Body.Close()
	var sessions []map[string]any
	decodeJSON(t, resp.Body, &sessions)
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions for u1, got %#v", sessions)
	}
	byDevice := map[string]map[string]any{}
	for _, s := range sessions {
		byDevice[s["DeviceId"].(string)] = s
	}
	npA := byDevice["device-a"]["NowPlayingItem"].(map[string]any)
	npB := byDevice["device-b"]["NowPlayingItem"].(map[string]any)
	if npA["Id"] != "item-a" || npB["Id"] != "item-b" {
		t.Fatalf("device items mixed: a=%#v b=%#v", npA, npB)
	}
	// Other user's item must not appear for u1.
	for _, s := range sessions {
		np := s["NowPlayingItem"].(map[string]any)
		if np["Id"] == "item-other" {
			t.Fatalf("leaked other user item: %#v", s)
		}
	}
	// u1 UserData for item-a is favorite; item-b has no local favorite.
	if npA["UserData"].(map[string]any)["IsFavorite"] != true {
		t.Fatalf("u1 item-a UserData: %#v", npA["UserData"])
	}
	if npB["UserData"].(map[string]any)["IsFavorite"] == true {
		t.Fatalf("item-b should not inherit u2 favorite: %#v", npB["UserData"])
	}

	// Other user list sees only their session/item.
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=token-other", nil))
	decodeJSON(t, resp.Body, &sessions)
	_ = resp.Body.Close()
	if len(sessions) != 1 {
		t.Fatalf("other user sessions = %#v", sessions)
	}
	np := sessions[0]["NowPlayingItem"].(map[string]any)
	if np["Id"] != "item-other" {
		t.Fatalf("other now playing = %#v", np)
	}
	if np["Id"] == "item-a" || np["Id"] == "item-b" {
		t.Fatal("other user saw u1 item")
	}
}

type countingCurrentListStore struct {
	*MemoryStore
	listCalls  int
	listHashes [][]string
}

func (c *countingCurrentListStore) ListCurrentPlaybacks(ctx context.Context, tokenHashes []string) (map[string]CurrentPlayback, error) {
	c.listCalls++
	c.listHashes = append(c.listHashes, append([]string(nil), tokenHashes...))
	return c.MemoryStore.ListCurrentPlaybacks(ctx, tokenHashes)
}

func TestGetSessionsFiltersBeforeSingleBatchList(t *testing.T) {
	t.Parallel()
	store := &countingCurrentListStore{MemoryStore: NewMemoryStore()}
	now := time.Now().UTC()

	a := testSession()
	a.GatewayTokenHash = HashToken("token-a")
	a.PublicID = "session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	a.DeviceID = "device-a"
	a.LastActivityAt = now
	store.Sessions[a.GatewayTokenHash] = a

	b := testSession()
	b.GatewayTokenHash = HashToken("token-b")
	b.PublicID = "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	b.DeviceID = "device-b"
	b.LastActivityAt = now.Add(-time.Minute)
	store.Sessions[b.GatewayTokenHash] = b

	pos := int64(1)
	store.CurrentPlaybacks = map[string]*CurrentPlayback{
		a.GatewayTokenHash: {
			GatewayTokenHash: a.GatewayTokenHash, ItemID: "item-a",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-a"},
			PlayState:    PlaybackPlayState{PositionTicks: &pos},
			StartedAt:    now.Add(-time.Hour), LastReportedAt: now,
		},
		b.GatewayTokenHash: {
			GatewayTokenHash: b.GatewayTokenHash, ItemID: "item-b",
			ItemSnapshot: PlaybackItemSnapshot{ID: "item-b"},
			PlayState:    PlaybackPlayState{PositionTicks: &pos},
			StartedAt:    now.Add(-time.Hour), LastReportedAt: now,
		},
	}

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	// DeviceId filter: only device-a remains → one ListCurrentPlaybacks with only a's hash.
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=token-a&DeviceId=device-a", nil))
	_ = resp.Body.Close()
	if store.listCalls != 1 {
		t.Fatalf("ListCurrentPlaybacks calls = %d, want 1", store.listCalls)
	}
	if len(store.listHashes[0]) != 1 || store.listHashes[0][0] != a.GatewayTokenHash {
		t.Fatalf("list hashes = %#v, want only device-a hash", store.listHashes[0])
	}

	// Full list: still one batch call with both hashes (order not critical).
	store.listCalls = 0
	store.listHashes = nil
	resp = do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=token-a", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if store.listCalls != 1 {
		t.Fatalf("full listCalls = %d, want 1", store.listCalls)
	}
	if len(store.listHashes[0]) != 2 {
		t.Fatalf("full hashes = %#v, want 2", store.listHashes[0])
	}
	var sessions []map[string]any
	if err := json.Unmarshal(body, &sessions); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %#v", sessions)
	}
}

type failingCurrentListStore struct {
	*MemoryStore
}

func (f *failingCurrentListStore) ListCurrentPlaybacks(ctx context.Context, tokenHashes []string) (map[string]CurrentPlayback, error) {
	return nil, errors.New("current list failed")
}

func TestGetSessionsCurrentListError500NoStore(t *testing.T) {
	t.Parallel()
	store := &failingCurrentListStore{MemoryStore: NewMemoryStore()}
	session := testSession()
	session.PublicID = "session-dddddddddddddddddddddddddddddddd"
	store.Sessions[HashToken("gateway-token")] = session
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

func TestGetSessionsCorruptCurrentFailsClosed(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	session := testSession()
	session.PublicID = "session-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	store.Sessions[HashToken("gateway-token")] = session
	// Token hash mismatch is an integrity failure.
	store.CurrentPlaybacks = map[string]*CurrentPlayback{
		HashToken("gateway-token"): {
			GatewayTokenHash: "wrong-hash",
			ItemID:           "item-1",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-1"},
			StartedAt:        now.Add(-time.Minute),
			LastReportedAt:   now,
		},
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (not idle)", resp.StatusCode)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

func TestGetSessionsNoAutoAdvancePosition(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	session := testSession()
	session.PublicID = "session-ffffffffffffffffffffffffffffffff"
	store.Sessions[HashToken("gateway-token")] = session
	pos := int64(42)
	store.CurrentPlaybacks = map[string]*CurrentPlayback{
		HashToken("gateway-token"): {
			GatewayTokenHash: HashToken("gateway-token"),
			ItemID:           "item-1",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-1", Name: "Movie"},
			PlayState:        PlaybackPlayState{PositionTicks: &pos},
			// Last report was long ago; position must not clock-advance.
			StartedAt:      now.Add(-2 * time.Hour),
			LastReportedAt: now.Add(-time.Hour),
		},
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	defer resp.Body.Close()
	var sessions []map[string]any
	decodeJSON(t, resp.Body, &sessions)
	ps := sessions[0]["PlayState"].(map[string]any)
	if int64(ps["PositionTicks"].(float64)) != 42 {
		t.Fatalf("position auto-advanced: %#v", ps["PositionTicks"])
	}
}

func TestGetSessionsZeroUpstreamSessionsEgress(t *testing.T) {
	t.Parallel()
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		t.Errorf("unexpected upstream: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	now := time.Now().UTC()
	session := testSession()
	session.PublicID = "session-11111111111111111111111111111111"
	store.Sessions[HashToken("gateway-token")] = session
	pos := int64(1)
	store.CurrentPlaybacks = map[string]*CurrentPlayback{
		HashToken("gateway-token"): {
			GatewayTokenHash: HashToken("gateway-token"),
			ItemID:           "item-1",
			ItemSnapshot:     PlaybackItemSnapshot{ID: "item-1", Name: "N", Type: "Movie"},
			PlayState:        PlaybackPlayState{PositionTicks: &pos},
			StartedAt:        now.Add(-time.Minute),
			LastReportedAt:   now,
		},
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Sessions?api_key=gateway-token", nil))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if hits != 0 {
		t.Fatalf("upstream hits = %d, want 0 (no /Sessions, no metadata)", hits)
	}
}

func TestLoginSessionInfoStillIdleGolden(t *testing.T) {
	// Regression: authentication DTO must pass nil current (Phase 3 idle).
	store := testStore("http://backend.invalid/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	req := mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`)
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="SenPlayer", Device="Mac", DeviceId="dev-1", Version="6.1.3"`)
	resp := do(t, req)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), `"NowPlayingItem"`) {
		t.Fatalf("login SessionInfo must omit NowPlayingItem: %s", raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	info := body["SessionInfo"].(map[string]any)
	ps := info["PlayState"].(map[string]any)
	if _, has := ps["PositionTicks"]; has {
		t.Fatalf("login idle PlayState must omit PositionTicks: %#v", ps)
	}
}

func TestSessionInfoDTOUnitProjection(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	session := &Session{
		PublicID: "session-22222222222222222222222222222222", GatewayUsername: "alice",
		SyntheticUserID: "gateway-user", Client: "C", Device: "D", DeviceID: "did", Version: "1",
		LastActivityAt: now, Capabilities: defaultSessionCapabilities(),
	}
	// Idle
	idle := sessionInfoDTO(session, "srv", nil, nil)
	if _, has := idle["NowPlayingItem"]; has {
		t.Fatal("idle has NowPlayingItem")
	}
	// Active with nil UserData still projects item
	cp := &CurrentPlayback{
		GatewayTokenHash: "h", ItemID: "i1",
		ItemSnapshot: PlaybackItemSnapshot{ID: "i1", Name: "N"},
		PlayState:    PlaybackPlayState{},
		StartedAt:    now, LastReportedAt: now,
	}
	active := sessionInfoDTO(session, "srv", cp, nil)
	np := active["NowPlayingItem"].(map[string]any)
	if np["Id"] != "i1" || np["Name"] != "N" {
		t.Fatalf("active item: %#v", np)
	}
	if _, has := np["UserData"]; has {
		t.Fatalf("nil userData should omit UserData key: %#v", np)
	}
	// With UserData
	active = sessionInfoDTO(session, "srv", cp, map[string]any{"Played": false, "IsFavorite": true})
	np = active["NowPlayingItem"].(map[string]any)
	if np["UserData"].(map[string]any)["IsFavorite"] != true {
		t.Fatalf("UserData: %#v", np["UserData"])
	}
}
