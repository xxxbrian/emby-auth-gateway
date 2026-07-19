package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
)

func TestPlaybackReportHTTPEmpty200NoStore(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	paths := []struct {
		path string
		body string
	}{
		{"/Sessions/Playing", `{"ItemId":"item-1","PositionTicks":10,"Item":{"Id":"item-1","Name":"Movie","Type":"Movie","RunTimeTicks":1000}}`},
		{"/Sessions/Playing/Progress", `{"ItemId":"item-1","PlaybackPositionTicks":20,"RunTimeTicks":1000}`},
		{"/Sessions/Playing/Stopped", `{"ItemId":"item-1","PositionTicks":30,"RunTimeTicks":1000}`},
		{"/Sessions/Playing/Ping", `{}`},
	}
	for _, tc := range paths {
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby"+tc.path+"?api_key=gateway-token", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", tc.path, resp.StatusCode)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s Cache-Control = %q", tc.path, resp.Header.Get("Cache-Control"))
		}
		if len(bytes.TrimSpace(body)) != 0 {
			t.Fatalf("%s body = %q, want empty", tc.path, body)
		}
	}
}

func TestPlaybackReportMissingItemIsNoOp200(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"PositionTicks":99}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" || len(bytes.TrimSpace(body)) != 0 {
		t.Fatalf("missing-item status/headers/body = %d %q %q", resp.StatusCode, resp.Header.Get("Cache-Control"), body)
	}
	if len(store.PlaybackEvents) != 0 || len(store.PlaybackStates) != 0 || len(store.CurrentPlaybacks) != 0 {
		t.Fatalf("missing item must not write: events=%d states=%d current=%d", len(store.PlaybackEvents), len(store.PlaybackStates), len(store.CurrentPlaybacks))
	}
}

func TestPlaybackReportConflictingJSONItemIDsRejectBeforeGuardOrEgress(t *testing.T) {
	var upstreamHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		writeTestJSON(w, map[string]any{"Id": "item-1", "Name": "Resolved", "Type": "Movie"})
	}))
	defer backend.Close()

	base := NewMemoryStore()
	configureTestUpstream(base, backend.URL+"/emby")
	session := testSession()
	base.Sessions[HashToken("gateway-token")] = session
	store := &faultInjectPlaybackStore{MemoryStore: base}
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: "item-1"})

	recorder := httptest.NewRecorder()
	req := mustRequest(t, http.MethodPost, "http://gateway/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(`{"ItemId":" item-1 ","Item":{"Id":"item-2"},"PositionTicks":1}`))
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache = %d/%q, want 400/no-store", recorder.Code, recorder.Header().Get("Cache-Control"))
	}
	if upstreamHits.Load() != 0 || store.applyCalls != 0 {
		t.Fatalf("upstream/apply calls = %d/%d, want 0/0", upstreamHits.Load(), store.applyCalls)
	}
	if hasAuditEvent(base, "playback_report_apply_failed") || hasAuditEvent(base, "playback_report_prepare_failed") || hasAuditEvent(base, "playback_report_suppressed") {
		t.Fatalf("malformed identity produced operational/suppression audit: %#v", base.AuditLogs)
	}
}

func TestPlaybackReportMixedSourceIdentityConflictsRejectWithoutEffects(t *testing.T) {
	cases := []struct {
		name        string
		query       url.Values
		contentType string
		body        string
	}{
		{
			name:  "query-only top versus nested",
			query: url.Values{"ItemId": {"item-a"}, "Item.Id": {"item-b"}, "Item.Name": {"Item B"}},
		},
		{
			name:        "form-body top versus nested",
			contentType: "application/x-www-form-urlencoded",
			body:        url.Values{"ItemId": {"item-a"}, "Item.Id": {"item-b"}, "Item.Name": {"Item B"}}.Encode(),
		},
		{
			name:        "json top versus query nested",
			query:       url.Values{"Item.Id": {"item-b"}, "Item.Name": {"Item B"}, "Item.Type": {"Movie"}},
			contentType: "application/json",
			body:        `{"ItemId":"item-a","PositionTicks":1}`,
		},
		{
			name:        "json nested versus query top",
			query:       url.Values{"ItemId": {"item-b"}},
			contentType: "application/json",
			body:        `{"Item":{"Id":"item-a","Name":"Item A","Type":"Movie"},"PositionTicks":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamHits atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				writeTestJSON(w, map[string]any{"Id": "item-a", "Name": "Upstream", "Type": "Movie"})
			}))
			defer backend.Close()

			base := NewMemoryStore()
			configureTestUpstream(base, backend.URL+"/emby")
			session := testSession()
			base.Sessions[HashToken("gateway-token")] = session
			store := &faultInjectPlaybackStore{MemoryStore: base}
			server := NewServer(Config{GatewayBasePath: "/emby"}, store)
			server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: "item-a"})
			server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: "item-b"})

			target := "http://gateway/emby/Sessions/Playing?api_key=gateway-token"
			if encoded := tc.query.Encode(); encoded != "" {
				target += "&" + encoded
			}
			recorder := httptest.NewRecorder()
			req := mustRequest(t, http.MethodPost, target, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			server.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status/cache = %d/%q, want 400/no-store", recorder.Code, recorder.Header().Get("Cache-Control"))
			}
			if upstreamHits.Load() != 0 || store.applyCalls != 0 || len(base.PlaybackStates) != 0 || len(base.PlaybackEvents) != 0 || len(base.CurrentPlaybacks) != 0 {
				t.Fatalf("conflict caused effects: upstream=%d apply=%d states=%d events=%d current=%d", upstreamHits.Load(), store.applyCalls, len(base.PlaybackStates), len(base.PlaybackEvents), len(base.CurrentPlaybacks))
			}
			if hasAuditEvent(base, "playback_report_suppressed") || hasAuditEvent(base, "playback_report_apply_failed") || hasAuditEvent(base, "playback_report_prepare_failed") {
				t.Fatalf("conflict produced suppression/operational audit: %#v", base.AuditLogs)
			}
		})
	}
}

func TestPlaybackReportCaseVariantIdentityAmbiguityRejectsWithoutEffects(t *testing.T) {
	cases := []struct {
		name        string
		query       url.Values
		contentType string
		body        string
	}{
		{name: "json exact top duplicate", contentType: "application/json", body: `{"ItemId":"item-1","ItemId":"item-1"}`},
		{name: "json exact nested duplicate", contentType: "application/json", body: `{"Item":{"Id":"item-1","Id":"item-1"}}`},
		{name: "json exact item object duplicate", contentType: "application/json", body: `{"Item":{"Id":"item-1"},"Item":{"Id":"item-1"}}`},
		{name: "json top duplicate", contentType: "application/json", body: `{"ItemId":"item-1","itemid":"item-1"}`},
		{name: "json nested duplicate", contentType: "application/json", body: `{"Item":{"Id":"item-1","id":"item-1"}}`},
		{name: "json item object duplicate", contentType: "application/json", body: `{"Item":{"Id":"item-1"},"item":{"Id":"item-1"}}`},
		{name: "form top duplicate", contentType: "application/x-www-form-urlencoded", body: url.Values{"ItemId": {"item-1"}, "itemid": {"item-1"}}.Encode()},
		{name: "form nested duplicate", contentType: "application/x-www-form-urlencoded", body: url.Values{"Item.Id": {"item-1"}, "item.id": {"item-1"}}.Encode()},
		{name: "query top duplicate", query: url.Values{"ItemId": {"item-1"}, "itemid": {"item-1"}}},
		{name: "query nested duplicate", query: url.Values{"Item.Id": {"item-1"}, "item.id": {"item-1"}}},
		{name: "form repeated top values", contentType: "application/x-www-form-urlencoded", body: "ItemId=item-1&ItemId=item-2"},
		{name: "form repeated identical top values", contentType: "application/x-www-form-urlencoded", body: "ItemId=item-1&ItemId=item-1"},
		{name: "form repeated nested values", contentType: "application/x-www-form-urlencoded", body: "Item.Id=item-1&Item.Id=item-2"},
		{name: "query repeated top values", query: url.Values{"ItemId": {"item-1", "item-2"}}},
		{name: "query repeated identical top values", query: url.Values{"ItemId": {"item-1", "item-1"}}},
		{name: "query repeated nested values", query: url.Values{"Item.Id": {"item-1", "item-2"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamHits atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				writeTestJSON(w, map[string]any{"Id": "item-1"})
			}))
			defer backend.Close()

			base := NewMemoryStore()
			configureTestUpstream(base, backend.URL+"/emby")
			session := testSession()
			base.Sessions[HashToken("gateway-token")] = session
			store := &faultInjectPlaybackStore{MemoryStore: base}
			server := NewServer(Config{GatewayBasePath: "/emby"}, store)
			server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: "item-1"})

			target := "http://gateway/emby/Sessions/Playing?api_key=gateway-token"
			if encoded := tc.query.Encode(); encoded != "" {
				target += "&" + encoded
			}
			recorder := httptest.NewRecorder()
			req := mustRequest(t, http.MethodPost, target, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			server.ServeHTTP(recorder, req)

			if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status/cache = %d/%q, want 400/no-store", recorder.Code, recorder.Header().Get("Cache-Control"))
			}
			if upstreamHits.Load() != 0 || store.applyCalls != 0 || len(base.PlaybackStates) != 0 || len(base.PlaybackEvents) != 0 || len(base.CurrentPlaybacks) != 0 {
				t.Fatalf("ambiguous identity caused effects: upstream=%d apply=%d states=%d events=%d current=%d", upstreamHits.Load(), store.applyCalls, len(base.PlaybackStates), len(base.PlaybackEvents), len(base.CurrentPlaybacks))
			}
			if hasAuditEvent(base, "playback_report_suppressed") || hasAuditEvent(base, "playback_report_apply_failed") || hasAuditEvent(base, "playback_report_prepare_failed") {
				t.Fatalf("ambiguous identity produced suppression/operational audit: %#v", base.AuditLogs)
			}
		})
	}
}

func TestPlaybackReportMixedSourceIdentitiesAcceptedWhenIdentical(t *testing.T) {
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession()
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()

	query := url.Values{"Item.Id": {"item-a"}, "Item.Name": {"Mixed Name"}, "Item.Type": {"Movie"}}
	req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing?api_key=gateway-token&"+query.Encode(), strings.NewReader(`{"ItemId":" item-a ","PositionTicks":14}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-a")
	if err != nil || state.ItemName != "Mixed Name" || state.ItemType != "Movie" || state.PlaybackPositionTicks != 14 {
		t.Fatalf("identical mixed-source state = %#v err=%v", state, err)
	}
}

func TestPlaybackReportNestedOnlyFormAndQueryWork(t *testing.T) {
	cases := []struct {
		name        string
		query       url.Values
		contentType string
		body        string
		itemID      string
		itemName    string
	}{
		{
			name:        "form",
			contentType: "application/x-www-form-urlencoded",
			body:        url.Values{"Item.Id": {"form-nested"}, "Item.Name": {"Form Name"}, "Item.Type": {"Movie"}, "PositionTicks": {"21"}}.Encode(),
			itemID:      "form-nested",
			itemName:    "Form Name",
		},
		{
			name:     "query",
			query:    url.Values{"Item.Id": {"query-nested"}, "Item.Name": {"Query Name"}, "Item.Type": {"Movie"}, "PositionTicks": {"22"}},
			itemID:   "query-nested",
			itemName: "Query Name",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			store.Sessions[HashToken("gateway-token")] = testSession()
			gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
			defer gateway.Close()

			target := gateway.URL + "/emby/Sessions/Playing?api_key=gateway-token"
			if encoded := tc.query.Encode(); encoded != "" {
				target += "&" + encoded
			}
			req := mustRequest(t, http.MethodPost, target, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			state, err := store.FindPlaybackState(context.Background(), "u1", tc.itemID)
			if err != nil || state.ItemName != tc.itemName || state.ItemType != "Movie" || state.PlaybackPositionTicks == 0 {
				t.Fatalf("nested-only state = %#v err=%v", state, err)
			}
		})
	}
}

func TestPlaybackReportNestedOnlyItemIDWorks(t *testing.T) {
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession()
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()

	req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(`{"Item":{"Id":"nested-1","Name":"Nested","Type":"Movie","RunTimeTicks":1000},"PositionTicks":12}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "nested-1")
	if err != nil || state.ItemName != "Nested" || state.PlaybackPositionTicks != 12 {
		t.Fatalf("nested-only state = %#v err=%v", state, err)
	}
}

func TestPlaybackReportConfirmedMetadataRepairsOrphanAndRestoresUserData(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		item := map[string]any{"Id": "item-repair", "Name": "Confirmed Name", "Type": "Movie", "RunTimeTicks": float64(90_000_000), "UserData": map[string]any{}}
		if strings.HasSuffix(r.URL.Path, "/Items/item-repair") {
			writeTestJSON(w, item)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/Items") {
			writeTestJSON(w, map[string]any{"Items": []any{item}, "TotalRecordCount": 1})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()

	store := testStore(backend.URL + "/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	orphanedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	lastSeenAt := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	likes := true
	if err := store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-repair",
		ItemName: "Old", ItemType: "Episode", PlaybackPositionTicks: 444,
		PlayedPercentage: floatPtr(25), PlayCount: 2, IsFavorite: true, Likes: &likes,
		Fingerprint: "type=Episode", OrphanedAt: &orphanedAt, LastSeenAt: &lastSeenAt,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()
	req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-repair"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("playback status = %d", resp.StatusCode)
	}

	state, err := store.FindPlaybackState(context.Background(), "u1", "item-repair")
	if err != nil || state.OrphanedAt != nil || state.LastSeenAt == nil || !state.LastSeenAt.After(lastSeenAt) {
		t.Fatalf("repaired state = %#v err=%v", state, err)
	}
	if state.ItemName != "Confirmed Name" || state.ItemType != "Movie" || state.Fingerprint != "type=Movie" {
		t.Fatalf("confirmed metadata not applied: %#v", state)
	}
	if !state.IsFavorite || state.PlaybackPositionTicks != 444 || state.PlayCount != 2 || state.Likes == nil || !*state.Likes {
		t.Fatalf("personal fields clobbered: %#v", state)
	}

	resp = do(t, mustRequest(t, http.MethodGet, gateway.URL+"/emby/Users/gateway-user/Items?Ids=item-repair&api_key=gateway-token", nil))
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode batch response: %v", err)
	}
	items, _ := payload["Items"].([]any)
	if len(items) != 1 {
		t.Fatalf("batch items = %#v", payload)
	}
	userData, _ := items[0].(map[string]any)["UserData"].(map[string]any)
	if userData["IsFavorite"] != true || int64(userData["PlaybackPositionTicks"].(float64)) != 444 {
		t.Fatalf("repaired UserData overlay = %#v", userData)
	}
}

func TestPlaybackReportConfirmedMetadataIsAuthoritative(t *testing.T) {
	canonicalItem := func(id string, includeSeries bool) map[string]any {
		item := map[string]any{
			"Id": id, "Name": "Upstream Name", "Type": "Episode", "MediaType": "Video",
			"SeasonId": "season-upstream", "ParentId": "parent-upstream",
			"IndexNumber": float64(3), "ParentIndexNumber": float64(2),
			"RunTimeTicks": float64(60 * embyTicksPerSecond), "ProductionYear": float64(2025),
			"PremiereDate": "2025-01-02", "CommunityRating": float64(8.5),
			"OfficialRating": "TV-14", "ImageTags": map[string]any{"Primary": "upstream-tag"},
		}
		if includeSeries {
			item["SeriesId"] = "series-upstream"
			item["SeriesName"] = "Upstream Series"
		}
		return item
	}

	t.Run("stable identity and catalog override client", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, canonicalItem("item-authoritative", true))
		}))
		defer backend.Close()
		store := testStore(backend.URL + "/emby")
		store.Sessions[HashToken("gateway-token")] = testSession()
		orphanedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		lastSeenAt := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
		likes := true
		if err := store.SavePlaybackState(context.Background(), PlaybackState{
			GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-authoritative",
			ItemName: "Old", ItemType: "OldType", SeriesID: "old-series",
			PlayCount: 4, IsFavorite: true, Likes: &likes, Fingerprint: "type=OldType|seriesid=old-series",
			OrphanedAt: &orphanedAt, LastSeenAt: &lastSeenAt,
		}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gateway.Close()

		body := `{"ItemId":"item-authoritative","Item":{"Id":"item-authoritative","Type":"ClientType","MediaType":"Audio","SeriesId":"series-client","SeasonId":"season-client","ParentId":"parent-client","IndexNumber":99,"ParentIndexNumber":88}}`
		req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		body = `{"ItemId":"item-authoritative","Item":{"Id":"item-authoritative","MediaType":"Audio","SeriesId":"series-client","SeasonId":"season-client","ParentId":"parent-client","IndexNumber":99,"ParentIndexNumber":88,"RunTimeTicks":123}}`
		req = mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp = do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("playing status = %d", resp.StatusCode)
		}

		state, err := store.FindPlaybackState(context.Background(), "u1", "item-authoritative")
		if err != nil || state.OrphanedAt != nil || state.LastSeenAt == nil || !state.LastSeenAt.After(lastSeenAt) {
			t.Fatalf("authoritative durable state = %#v err=%v", state, err)
		}
		if state.ItemName != "Upstream Name" || state.ItemType != "Episode" || state.SeriesID != "series-upstream" || state.SeasonID != "season-upstream" || state.IndexNumber != 3 || state.ParentIndexNumber != 2 || state.RunTimeTicks != 60*embyTicksPerSecond || state.Fingerprint != "type=Episode|seriesid=series-upstream" {
			t.Fatalf("client metadata survived confirmed merge: %#v", state)
		}
		if !state.IsFavorite || state.PlaybackPositionTicks != 0 || state.PlayCount != 4 || state.Likes == nil || !*state.Likes {
			t.Fatalf("personal fields clobbered: %#v", state)
		}
		current := store.CurrentPlaybacks[HashToken("gateway-token")]
		if current == nil || current.ItemSnapshot.Type != "Episode" || current.ItemSnapshot.MediaType != "Video" || current.ItemSnapshot.SeriesID != "series-upstream" || current.ItemSnapshot.SeasonID != "season-upstream" || current.ItemSnapshot.ParentID != "parent-upstream" || current.ItemSnapshot.IndexNumber != 3 || current.ItemSnapshot.ParentIndexNumber != 2 || current.ItemSnapshot.RunTimeTicks != 60*embyTicksPerSecond {
			t.Fatalf("current snapshot is not canonical: %#v", current)
		}
	})

	t.Run("upstream display and runtime override client", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, canonicalItem("item-display", true))
		}))
		defer backend.Close()
		store := testStore(backend.URL + "/emby")
		store.Sessions[HashToken("gateway-token")] = testSession()
		gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gateway.Close()

		body := `{"ItemId":"item-display","RunTimeTicks":0,"Item":{"Id":"item-display","Name":"Client Name","Type":"ClientType","SeriesName":"Client Series"}}`
		req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		state, err := store.FindPlaybackState(context.Background(), "u1", "item-display")
		if resp.StatusCode != http.StatusOK || err != nil || state.ItemName != "Upstream Name" || state.SeriesName != "Upstream Series" || state.RunTimeTicks != 60*embyTicksPerSecond {
			t.Fatalf("display/runtime authority: status=%d state=%#v err=%v", resp.StatusCode, state, err)
		}
	})

	t.Run("upstream omission clears rogue series identity", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, canonicalItem("item-no-series", false))
		}))
		defer backend.Close()
		store := testStore(backend.URL + "/emby")
		store.Sessions[HashToken("gateway-token")] = testSession()
		orphanedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		if err := store.SavePlaybackState(context.Background(), PlaybackState{
			GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-no-series",
			SeriesID: "old-rogue", IsFavorite: true, OrphanedAt: &orphanedAt,
		}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gateway.Close()
		body := `{"ItemId":"item-no-series","Item":{"Id":"item-no-series","SeriesId":"client-rogue"}}`
		req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		state, err := store.FindPlaybackState(context.Background(), "u1", "item-no-series")
		current := store.CurrentPlaybacks[HashToken("gateway-token")]
		if resp.StatusCode != http.StatusOK || err != nil || state.OrphanedAt != nil || state.SeriesID != "" || state.Fingerprint != "type=Episode" || !state.IsFavorite {
			t.Fatalf("omitted series durable state: status=%d state=%#v err=%v", resp.StatusCode, state, err)
		}
		if current == nil || current.ItemSnapshot.SeriesID != "" {
			t.Fatalf("omitted series survived current snapshot: %#v", current)
		}
	})
}

func TestPlaybackReportStoppedConfirmedMetadataRepairsOrphan(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Id": "item-stopped", "Name": "Stopped Item", "Type": "Movie", "RunTimeTicks": float64(60 * embyTicksPerSecond)})
	}))
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	orphanedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if err := store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-stopped",
		PlaybackPositionTicks: embyTicksPerSecond, IsFavorite: true, OrphanedAt: &orphanedAt,
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()

	req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-stopped"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-stopped")
	if err != nil || state.OrphanedAt != nil || state.LastSeenAt == nil || state.ItemName != "Stopped Item" || !state.IsFavorite {
		t.Fatalf("stopped repair state = %#v err=%v", state, err)
	}
}

func TestPlaybackReportUnconfirmedMetadataPreservesOrphan(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{name: "outage", handler: func(w http.ResponseWriter, r *http.Request) {
			hijacker := w.(http.Hijacker)
			conn, _, _ := hijacker.Hijack()
			_ = conn.Close()
		}},
		{name: "404", handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }},
		{name: "bad json", handler: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{bad-json`))
		}},
		{name: "id mismatch", handler: func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, map[string]any{"Id": "other-item", "Name": "Wrong", "Type": "Movie"})
		}},
		{name: "missing id", handler: func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, map[string]any{"Name": "No Identity", "Type": "Movie"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(tc.handler)
			defer backend.Close()
			store := testStore(backend.URL + "/emby")
			store.Sessions[HashToken("gateway-token")] = testSession()
			orphanedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
			lastSeenAt := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
			if err := store.SavePlaybackState(context.Background(), PlaybackState{
				GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-orphan",
				PlaybackPositionTicks: 333, IsFavorite: true, OrphanedAt: &orphanedAt, LastSeenAt: &lastSeenAt,
			}); err != nil {
				t.Fatalf("seed state: %v", err)
			}
			gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
			defer gateway.Close()

			req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-orphan"}`))
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			state, err := store.FindPlaybackState(context.Background(), "u1", "item-orphan")
			if err != nil || state.OrphanedAt == nil || !state.OrphanedAt.Equal(orphanedAt) || state.LastSeenAt == nil || !state.LastSeenAt.Equal(lastSeenAt) {
				t.Fatalf("unconfirmed state = %#v err=%v", state, err)
			}
			if !state.IsFavorite || state.PlaybackPositionTicks != 333 || state.ItemName == "Wrong" {
				t.Fatalf("unconfirmed report changed durable data: %#v", state)
			}
		})
	}
}

func TestPlaybackReportInvalidInputRejectsBeforeMetadataOrApply(t *testing.T) {
	invalidBodies := []struct {
		name string
		body string
	}{
		{name: "slash", body: `{"ItemId":"folder/item","PositionTicks":1}`},
		{name: "backslash", body: `{"ItemId":"folder\\item","PositionTicks":1}`},
		{name: "dot", body: `{"ItemId":".","PositionTicks":1}`},
		{name: "dotdot", body: `{"ItemId":"..","PositionTicks":1}`},
		{name: "traversal", body: `{"ItemId":"../item","PositionTicks":1}`},
		{name: "control", body: `{"ItemId":"item\u0000x","PositionTicks":1}`},
		{name: "overlong", body: `{"ItemId":` + strconv.Quote(strings.Repeat("x", currentPlaybackItemIDMaxBytes+1)) + `,"PositionTicks":1}`},
		{name: "negative position", body: `{"ItemId":"item-1","PositionTicks":-1}`},
	}
	for _, tc := range invalidBodies {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamHits atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamHits.Add(1)
				writeTestJSON(w, map[string]any{"Id": "item-1"})
			}))
			defer backend.Close()

			base := NewMemoryStore()
			configureTestUpstream(base, backend.URL+"/emby")
			base.Sessions[HashToken("gateway-token")] = testSession()
			store := &faultInjectPlaybackStore{MemoryStore: base}
			gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
			defer gateway.Close()

			req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if upstreamHits.Load() != 0 || store.applyCalls != 0 || len(base.PlaybackStates) != 0 || len(base.PlaybackEvents) != 0 || len(base.CurrentPlaybacks) != 0 {
				t.Fatalf("invalid report caused effects: upstream=%d apply=%d states=%d events=%d current=%d", upstreamHits.Load(), store.applyCalls, len(base.PlaybackStates), len(base.PlaybackEvents), len(base.CurrentPlaybacks))
			}
		})
	}
}

func TestPlaybackReportInvalidConfiguredPolicyReturns400BeforeApply(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore()}
	store.Sessions[HashToken("gateway-token")] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	server.cfg.MinResumePct = 90
	server.cfg.MaxResumePct = 10

	recorder := httptest.NewRecorder()
	req := mustRequest(t, http.MethodPost, "http://gateway/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PositionTicks":1}`))
	req.Header.Set("Content-Type", "application/json")
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || store.applyCalls != 0 {
		t.Fatalf("status/apply = %d/%d, want 400/0", recorder.Code, store.applyCalls)
	}
}

func TestPlaybackReportBodySessionIdDoesNotRebind(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	session.Capabilities = defaultSessionCapabilities()
	store.Sessions[HashToken("gateway-token")] = session
	// Different session that must not receive the report.
	other := testSession()
	other.GatewayTokenHash = HashToken("other-token")
	other.PublicID = "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	other.Capabilities = defaultSessionCapabilities()
	store.Sessions[other.GatewayTokenHash] = other

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	body := `{"ItemId":"item-1","SessionId":"` + other.PublicID + `","PlaySessionId":"ps-1","PositionTicks":50,"RunTimeTicks":1000}`
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if _, ok := store.CurrentPlaybacks[HashToken("gateway-token")]; !ok {
		t.Fatal("report should bind authoritative gateway token session")
	}
	if _, ok := store.CurrentPlaybacks[other.GatewayTokenHash]; ok {
		t.Fatal("body SessionId must not bind other session")
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-1")
	if err != nil || state.PlaybackPositionTicks != 50 {
		t.Fatalf("durable for caller: %#v err=%v", state, err)
	}
}

func TestPlaybackReportTelemetryAfterCommitUsesPublicSessionID(t *testing.T) {
	t.Parallel()
	em := observe.NewEmitter(64)
	defer em.Close()
	store := NewMemoryStore()
	session := testSession()
	session.PublicID = "session-cccccccccccccccccccccccccccccccc"
	session.Capabilities = defaultSessionCapabilities()
	store.Sessions[HashToken("gateway-token")] = session

	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", Emitter: em}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PositionTicks":11,"RunTimeTicks":100}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var playback observe.Event
	found := false
	deadline := time.After(time.Second)
drain:
	for {
		select {
		case ev, ok := <-em.Events():
			if !ok {
				break drain
			}
			if ev.Kind == observe.KindPlayback {
				playback = ev
				found = true
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if !found {
		t.Fatal("no playback telemetry event")
	}
	if playback.SessionID != session.PublicID {
		t.Fatalf("SessionID = %q, want public id %q (never token hash)", playback.SessionID, session.PublicID)
	}
	if playback.SessionID == HashToken("gateway-token") || playback.SessionID == "gateway-token" {
		t.Fatal("telemetry must not use token hash")
	}
	if playback.ItemID != "item-1" || playback.Outcome != observe.OutcomeOK {
		t.Fatalf("telemetry fields: %#v", playback)
	}
}

func TestPlaybackReportNoTelemetryWhenNotApplied(t *testing.T) {
	t.Parallel()
	em := observe.NewEmitter(64)
	defer em.Close()
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", Emitter: em}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	select {
	case ev := <-em.Events():
		if ev.Kind == observe.KindPlayback {
			t.Fatalf("unexpected playback telemetry for non-applied: %#v", ev)
		}
	case <-time.After(50 * time.Millisecond):
		// expected: no playback event
	}
}

func TestPlaybackReportMetadataResolvePolicy(t *testing.T) {
	t.Parallel()

	t.Run("playing resolves when nested insufficient", func(t *testing.T) {
		var hits atomic.Int32
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/Items/item-play") {
				hits.Add(1)
				writeTestJSON(w, map[string]any{
					"Id": "item-play", "Name": "Resolved", "Type": "Movie", "RunTimeTicks": float64(5000),
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer backend.Close()
		store := testStore(backend.URL + "/emby")
		store.Sessions[HashToken("gateway-token")] = testSession()
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-play","PositionTicks":1}`))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if hits.Load() != 1 {
			t.Fatalf("metadata hits = %d, want 1 for Playing with insufficient nested data", hits.Load())
		}
		state, err := store.FindPlaybackState(context.Background(), "u1", "item-play")
		if err != nil || state.ItemName != "Resolved" || state.ItemType != "Movie" || state.RunTimeTicks != 5000 {
			t.Fatalf("resolved state: %#v err=%v", state, err)
		}
	})

	t.Run("progress and ping never resolve", func(t *testing.T) {
		var hits atomic.Int32
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			writeTestJSON(w, map[string]any{"Id": "item-x", "Name": "X", "Type": "Movie", "RunTimeTicks": float64(9)})
		}))
		defer backend.Close()
		store := testStore(backend.URL + "/emby")
		store.Sessions[HashToken("gateway-token")] = testSession()
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		for _, path := range []string{"/Sessions/Playing/Progress", "/Sessions/Playing/Ping"} {
			body := `{"ItemId":"item-x","PositionTicks":1}`
			if path == "/Sessions/Playing/Ping" {
				body = `{"ItemId":"item-x"}`
			}
			req := mustRequest(t, http.MethodPost, gw.URL+"/emby"+path+"?api_key=gateway-token", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s status = %d", path, resp.StatusCode)
			}
		}
		if hits.Load() != 0 {
			t.Fatalf("Progress/Ping must not resolve metadata, hits=%d", hits.Load())
		}
	})

	t.Run("stopped resolves only when runtime unknown", func(t *testing.T) {
		var hits atomic.Int32
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			writeTestJSON(w, map[string]any{"Id": "item-stop", "Name": "Stop", "Type": "Movie", "RunTimeTicks": float64(8000)})
		}))
		defer backend.Close()
		store := testStore(backend.URL + "/emby")
		store.Sessions[HashToken("gateway-token")] = testSession()
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		// Known runtime: no resolve.
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-stop","PositionTicks":100,"RunTimeTicks":9000}`))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || hits.Load() != 0 {
			t.Fatalf("known runtime status=%d hits=%d", resp.StatusCode, hits.Load())
		}

		// Unknown runtime: resolve.
		req = mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-stop","PositionTicks":100}`))
		req.Header.Set("Content-Type", "application/json")
		resp = do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
			t.Fatalf("unknown runtime status=%d hits=%d", resp.StatusCode, hits.Load())
		}
		state, err := store.FindPlaybackState(context.Background(), "u1", "item-stop")
		if err != nil || state.RunTimeTicks != 8000 {
			t.Fatalf("stopped with resolve: %#v err=%v", state, err)
		}
	})
}

func TestPlaybackReportMetadataSoftFailuresPersistPartial(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"outage", func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("no hijack")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}},
		{"404", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"bad json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{not-json`))
		}},
		{"id mismatch", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, map[string]any{"Id": "other-id", "Name": "Wrong", "Type": "Movie", "RunTimeTicks": float64(1)})
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := httptest.NewServer(tc.handler)
			defer backend.Close()
			store := testStore(backend.URL + "/emby")
			store.Sessions[HashToken("gateway-token")] = testSession()
			gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
			defer gw.Close()

			req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(`{"ItemId":"partial-1","PositionTicks":7}`))
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			state, err := store.FindPlaybackState(context.Background(), "u1", "partial-1")
			if err != nil || state.PlaybackPositionTicks != 7 {
				t.Fatalf("partial persist failed for %s: %#v err=%v", tc.name, state, err)
			}
			// Must not adopt mismatched metadata.
			if tc.name == "id mismatch" && state.ItemName == "Wrong" {
				t.Fatalf("ID mismatch must not merge metadata: %#v", state)
			}
		})
	}
}

func TestPlaybackReportCancellationAbortsWithoutWrite(t *testing.T) {
	t.Parallel()
	// Metadata resolve blocks until context cancels.
	started := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer backend.Close()
	store := testStore(backend.URL + "/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby", HTTPClient: backend.Client()}, store)

	ctx, cancel := context.WithCancel(context.Background())
	req := mustRequest(t, http.MethodPost, "http://gateway/emby/Sessions/Playing?api_key=gateway-token", strings.NewReader(`{"ItemId":"cancel-1","PositionTicks":1}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)

	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		done <- rec
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("backend not hit")
	}
	cancel()

	select {
	case got := <-done:
		if got != http.ErrAbortHandler {
			t.Fatalf("recover = %#v, want ErrAbortHandler", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not abort")
	}
	if len(store.PlaybackEvents) != 0 || len(store.PlaybackStates) != 0 || len(store.CurrentPlaybacks) != 0 {
		t.Fatalf("cancel must not write: events=%d states=%d current=%d", len(store.PlaybackEvents), len(store.PlaybackStates), len(store.CurrentPlaybacks))
	}
}

func TestPlaybackReportMissingRepositoryFailsClosed(t *testing.T) {
	t.Parallel()
	// Wrapper embeds Store interface only (not concrete MemoryStore), so
	// type assertion to PlaybackRepository fails and there is no legacy path.
	type noPlaybackStore struct {
		Store
	}
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby"}, &noPlaybackStore{Store: store})
	if server.playback != nil {
		t.Fatal("expected no playback repo from non-PlaybackRepository store wrapper")
	}
	gw := httptest.NewServer(server)
	defer gw.Close()
	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"x","PositionTicks":1}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 without repository", resp.StatusCode)
	}
}

func TestItemFingerprintExcludesName(t *testing.T) {
	t.Parallel()
	fp := itemFingerprint(map[string]any{"Type": "Movie", "Name": "Title", "SeriesId": "s1"})
	if strings.Contains(fp, "name=") {
		t.Fatalf("fingerprint includes Name: %q", fp)
	}
	if fp != "type=Movie|seriesid=s1" {
		t.Fatalf("fingerprint = %q", fp)
	}
	// Rename-only change stays compatible when Type/SeriesId match.
	a := itemFingerprint(map[string]any{"Type": "Movie", "Name": "Old"})
	b := itemFingerprint(map[string]any{"Type": "Movie", "Name": "New"})
	if !fingerprintsCompatible(a, b) {
		t.Fatalf("rename incompatible: %q vs %q", a, b)
	}
}

func TestPlaybackReportConfigPolicyMaxPctThroughHTTP(t *testing.T) {
	// 95% position on a long item: default MaxResumePct=90 completes; MaxResumePct=99 remains resume.
	// Long runtime avoids MinResumeDurationSeconds short-item auto-complete.
	// Proves Config → command.Policy → reducer via real handler + MemoryStore.
	t.Parallel()
	longRuntime := 30 * 60 * embyTicksPerSecond
	longPos95 := int64(float64(longRuntime) * 0.95)

	t.Run("default_max_90_completes", func(t *testing.T) {
		t.Parallel()
		store := NewMemoryStore()
		store.Sessions[HashToken("gateway-token")] = testSession()
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		body := `{"ItemId":"maxpct-default","PositionTicks":` + itoa64(longPos95) + `,"RunTimeTicks":` + itoa64(longRuntime) + `}`
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		state, err := store.FindPlaybackState(context.Background(), "u1", "maxpct-default")
		if err != nil || !state.Played || state.PlaybackPositionTicks != 0 {
			t.Fatalf("default MaxPct=90 should complete 95%%: %#v err=%v", state, err)
		}
	})

	t.Run("max_99_keeps_resume", func(t *testing.T) {
		t.Parallel()
		store := NewMemoryStore()
		store.Sessions[HashToken("gateway-token")] = testSession()
		gw := httptest.NewServer(NewServer(Config{
			GatewayBasePath:          "/emby",
			MaxResumePct:             99,
			MinResumeDurationSeconds: 1, // keep duration rule from interfering
		}, store))
		defer gw.Close()

		body := `{"ItemId":"maxpct-99","PositionTicks":` + itoa64(longPos95) + `,"RunTimeTicks":` + itoa64(longRuntime) + `}`
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		state, err := store.FindPlaybackState(context.Background(), "u1", "maxpct-99")
		if err != nil || state.Played || state.PlaybackPositionTicks != longPos95 {
			t.Fatalf("MaxResumePct=99 should keep 95%% resume: %#v err=%v (pos=%d runtime=%d)", state, err, longPos95, longRuntime)
		}
	})
}

func TestPlaybackReportConfigPolicyMinDurationThroughHTTP(t *testing.T) {
	// Short item (60s) at 50%: default MinResumeDurationSeconds=300 completes;
	// MinResumeDurationSeconds=1 leaves resume (50% is between min and max pct).
	t.Parallel()
	runtime := 60 * embyTicksPerSecond
	pos50 := runtime / 2

	t.Run("default_min_duration_300_completes_short", func(t *testing.T) {
		t.Parallel()
		store := NewMemoryStore()
		store.Sessions[HashToken("gateway-token")] = testSession()
		gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
		defer gw.Close()

		body := `{"ItemId":"mindur-default","PositionTicks":` + itoa64(pos50) + `,"RunTimeTicks":` + itoa64(runtime) + `}`
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		state, err := store.FindPlaybackState(context.Background(), "u1", "mindur-default")
		if err != nil || !state.Played {
			t.Fatalf("default MinDuration=300 should complete 60s item at 50%%: %#v err=%v", state, err)
		}
	})

	t.Run("min_duration_1_keeps_resume", func(t *testing.T) {
		t.Parallel()
		store := NewMemoryStore()
		store.Sessions[HashToken("gateway-token")] = testSession()
		gw := httptest.NewServer(NewServer(Config{
			GatewayBasePath:          "/emby",
			MinResumeDurationSeconds: 1,
			MaxResumePct:             90,
			MinResumePct:             5,
		}, store))
		defer gw.Close()

		body := `{"ItemId":"mindur-1","PositionTicks":` + itoa64(pos50) + `,"RunTimeTicks":` + itoa64(runtime) + `}`
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Stopped?api_key=gateway-token", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		state, err := store.FindPlaybackState(context.Background(), "u1", "mindur-1")
		if err != nil || state.Played || state.PlaybackPositionTicks != pos50 {
			t.Fatalf("MinResumeDurationSeconds=1 should keep 50%% of 60s item: %#v err=%v", state, err)
		}
	})
}

func TestPlaybackReportAllKindsCarryConfigPolicy(t *testing.T) {
	// Spy ApplyPlaybackReport to assert Policy is set for Playing/Progress/Stopped/Ping.
	t.Parallel()
	store := &policySpyStore{MemoryStore: NewMemoryStore()}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{
		GatewayBasePath:          "/emby",
		MinResumePct:             7,
		MaxResumePct:             88,
		MinResumeDurationSeconds: 42,
	}, store))
	defer gw.Close()

	for _, tc := range []struct {
		path string
		body string
	}{
		{"/Sessions/Playing", `{"ItemId":"pol-1","PositionTicks":1,"RunTimeTicks":1000,"Item":{"Id":"pol-1","Name":"N","Type":"Movie"}}`},
		{"/Sessions/Playing/Progress", `{"ItemId":"pol-1","PositionTicks":2,"RunTimeTicks":1000}`},
		{"/Sessions/Playing/Stopped", `{"ItemId":"pol-1","PositionTicks":3,"RunTimeTicks":1000}`},
		{"/Sessions/Playing/Ping", `{}`},
	} {
		req := mustRequest(t, http.MethodPost, gw.URL+"/emby"+tc.path+"?api_key=gateway-token", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		resp := do(t, req)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", tc.path, resp.StatusCode)
		}
	}
	if len(store.policies) != 4 {
		t.Fatalf("Apply calls = %d, want 4", len(store.policies))
	}
	for i, p := range store.policies {
		if p.MinPct != 7 || p.MaxPct != 88 || p.MinDurationSeconds != 42 {
			t.Fatalf("call %d policy = %#v, want Min=7 Max=88 Dur=42", i, p)
		}
	}
}

func TestPlaybackResumePolicyFromConfigMapping(t *testing.T) {
	t.Parallel()
	s := NewServer(Config{
		GatewayBasePath:          "/emby",
		MinResumePct:             12,
		MaxResumePct:             77,
		MinResumeDurationSeconds: 15,
	}, NewMemoryStore())
	p := s.playbackResumePolicyFromConfig()
	if p.MinPct != 12 || p.MaxPct != 77 || p.MinDurationSeconds != 15 {
		t.Fatalf("policy = %#v", p)
	}
	// Zero config fields become NewServer defaults, then map through.
	s2 := NewServer(Config{GatewayBasePath: "/emby"}, NewMemoryStore())
	p2 := s2.playbackResumePolicyFromConfig()
	if p2.MinPct != defaultMinResumePct || p2.MaxPct != defaultMaxResumePct || p2.MinDurationSeconds != defaultMinResumeDurationSeconds {
		t.Fatalf("default policy = %#v", p2)
	}
}

type policySpyStore struct {
	*MemoryStore
	policies []PlaybackResumePolicy
}

func (p *policySpyStore) ApplyPlaybackReport(ctx context.Context, cmd PlaybackReportCommand) (PlaybackReportResult, error) {
	p.policies = append(p.policies, cmd.Policy)
	return p.MemoryStore.ApplyPlaybackReport(ctx, cmd)
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
