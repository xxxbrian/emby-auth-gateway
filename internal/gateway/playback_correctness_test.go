package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type faultInjectPlaybackStore struct {
	*MemoryStore
	findErr        error
	saveErr        error
	eventErr       error
	resolutionErr  error
	findCalls      int
	saveCalls      int
	resolutionCalls int
}

func (f *faultInjectPlaybackStore) FindPlaybackState(ctx context.Context, gatewayUserID, itemID string) (*PlaybackState, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.MemoryStore.FindPlaybackState(ctx, gatewayUserID, itemID)
}

func (f *faultInjectPlaybackStore) SavePlaybackState(ctx context.Context, state PlaybackState) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	return f.MemoryStore.SavePlaybackState(ctx, state)
}

func (f *faultInjectPlaybackStore) RecordPlaybackEvent(ctx context.Context, event PlaybackEvent) error {
	if f.eventErr != nil {
		return f.eventErr
	}
	return f.MemoryStore.RecordPlaybackEvent(ctx, event)
}

func (f *faultInjectPlaybackStore) SavePlaybackResolution(ctx context.Context, state PlaybackState) error {
	f.resolutionCalls++
	if f.resolutionErr != nil {
		return f.resolutionErr
	}
	return f.MemoryStore.SavePlaybackResolution(ctx, state)
}

func TestPersonalStateWriteDoesNotBlankOnFindStoreOutage(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), findErr: ErrStoreUnavailable}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Users/gateway-user/FavoriteItems/item-1?api_key=gateway-token", nil)
	resp := do(t, req)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", resp.StatusCode, body)
	}
	if store.saveCalls != 0 || len(store.PlaybackStates) != 0 {
		t.Fatalf("saveCalls=%d states=%d, want no blank state created", store.saveCalls, len(store.PlaybackStates))
	}
}

func TestPersonalStateWriteCreatesStateOnNotFound(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), findErr: ErrNotFound}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Users/gateway-user/FavoriteItems/item-1?api_key=gateway-token", nil)
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	state, err := store.MemoryStore.FindPlaybackState(context.Background(), "u1", "item-1")
	if err != nil || state == nil || !state.IsFavorite {
		t.Fatalf("expected favorite state, got %#v err=%v", state, err)
	}
}

func TestPlaybackReportSaveFailureReturns500(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), saveErr: errors.New("save failed")}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PlaybackPositionTicks":250}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", resp.StatusCode, body)
	}
	if _, err := store.MemoryStore.FindPlaybackState(context.Background(), "u1", "item-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected no persisted state, err=%v", err)
	}
}

func TestPlaybackReportFindStoreOutageReturns500WithoutBlankState(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), findErr: ErrStoreUnavailable}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PlaybackPositionTicks":250}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", resp.StatusCode, body)
	}
	if store.saveCalls != 0 || len(store.PlaybackStates) != 0 {
		t.Fatalf("saveCalls=%d states=%d, want no blank state", store.saveCalls, len(store.PlaybackStates))
	}
}

func TestPlaybackReportEventFailureStillSucceedsWhenSaveWorks(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), eventErr: errors.New("event write failed")}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PlaybackPositionTicks":420}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	state, err := store.MemoryStore.FindPlaybackState(context.Background(), "u1", "item-1")
	if err != nil || state.PlaybackPositionTicks != 420 {
		t.Fatalf("state = %#v err=%v", state, err)
	}
	if !hasAuditEvent(store.MemoryStore, "playback_event_persist_failed") {
		t.Fatal("expected playback_event_persist_failed audit event")
	}
	if len(store.PlaybackEvents) != 0 {
		t.Fatalf("events = %d, want 0 after event write failure", len(store.PlaybackEvents))
	}
}

func TestSavePlaybackResolutionDoesNotClobberUserData(t *testing.T) {
	store := NewMemoryStore()
	orphanedAt := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	if err := store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID:         "u1",
		SyntheticUserID:       "gateway-user",
		ItemID:                "item-1",
		ItemName:              "Old Name",
		ItemType:              "Movie",
		PlaybackPositionTicks: 999,
		Played:                true,
		IsFavorite:            true,
		PlayCount:             4,
		Fingerprint:           "type=Movie|name=Old Name",
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	if err := store.SavePlaybackResolution(context.Background(), PlaybackState{
		GatewayUserID:         "u1",
		SyntheticUserID:       "gateway-user",
		ItemID:                "item-1",
		ItemName:              "New Name",
		ItemType:              "Movie",
		SeriesID:              "series-1",
		SeriesName:            "Show",
		SeasonID:              "season-1",
		IndexNumber:           2,
		ParentIndexNumber:     1,
		RunTimeTicks:          5000,
		Fingerprint:           "type=Movie|name=New Name",
		OrphanedAt:            &orphanedAt,
		PlaybackPositionTicks: 0,
		Played:                false,
		IsFavorite:            false,
		PlayCount:             0,
	}); err != nil {
		t.Fatalf("SavePlaybackResolution: %v", err)
	}

	state, err := store.FindPlaybackState(context.Background(), "u1", "item-1")
	if err != nil {
		t.Fatalf("FindPlaybackState: %v", err)
	}
	if !state.IsFavorite || !state.Played || state.PlaybackPositionTicks != 999 || state.PlayCount != 4 {
		t.Fatalf("user data clobbered: %#v", state)
	}
	if state.ItemName != "New Name" || state.SeriesID != "series-1" || state.SeasonID != "season-1" || state.RunTimeTicks != 5000 || state.Fingerprint != "type=Movie|name=New Name" {
		t.Fatalf("metadata not updated: %#v", state)
	}
	if state.OrphanedAt == nil || !state.OrphanedAt.Equal(orphanedAt) {
		t.Fatalf("orphaned_at not updated: %#v", state.OrphanedAt)
	}
}

func TestSavePlaybackResolutionCreatesMissingRow(t *testing.T) {
	store := NewMemoryStore()
	if err := store.SavePlaybackResolution(context.Background(), PlaybackState{
		GatewayUserID:   "u1",
		SyntheticUserID: "gateway-user",
		ItemID:          "item-new",
		ItemName:        "Fresh",
		ItemType:        "Movie",
		Fingerprint:     "type=Movie|name=Fresh",
	}); err != nil {
		t.Fatalf("SavePlaybackResolution: %v", err)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "item-new")
	if err != nil || state.ItemName != "Fresh" || state.IsFavorite || state.Played {
		t.Fatalf("unexpected new state: %#v err=%v", state, err)
	}
}

func TestReconcileResolvedItem(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	t.Run("missing", func(t *testing.T) {
		state := &PlaybackState{ItemID: "item-1", PlaybackPositionTicks: 100, IsFavorite: true}
		outcome := reconcileResolvedItem(state, nil, false, now)
		if outcome != resolutionMissing {
			t.Fatalf("outcome = %v, want missing", outcome)
		}
		if state.OrphanedAt == nil || !state.OrphanedAt.Equal(now) {
			t.Fatalf("OrphanedAt = %#v", state.OrphanedAt)
		}
		if state.PlaybackPositionTicks != 100 || !state.IsFavorite {
			t.Fatalf("user data mutated on missing: %#v", state)
		}
	})

	t.Run("fingerprint mismatch", func(t *testing.T) {
		state := &PlaybackState{ItemID: "item-1", Fingerprint: "type=Episode|name=A|seriesid=s1", PlaybackPositionTicks: 50}
		item := map[string]any{"Id": "item-1", "Type": "Movie", "Name": "B"}
		outcome := reconcileResolvedItem(state, item, true, now)
		if outcome != resolutionFingerprintMismatch {
			t.Fatalf("outcome = %v, want mismatch", outcome)
		}
		if state.OrphanedAt == nil || !state.OrphanedAt.Equal(now) {
			t.Fatalf("OrphanedAt = %#v", state.OrphanedAt)
		}
		if state.PlaybackPositionTicks != 50 {
			t.Fatalf("position mutated on mismatch: %#v", state)
		}
	})

	t.Run("keep merges metadata", func(t *testing.T) {
		state := &PlaybackState{ItemID: "item-1", Fingerprint: "type=Episode", PlaybackPositionTicks: 77, IsFavorite: true}
		item := map[string]any{
			"Id": "item-1", "Type": "Episode", "Name": "Ep 1", "SeriesId": "show-1", "SeriesName": "Show",
			"SeasonId": "season-1", "IndexNumber": float64(3), "ParentIndexNumber": float64(1), "RunTimeTicks": float64(9000),
		}
		outcome := reconcileResolvedItem(state, item, true, now)
		if outcome != resolutionKeep {
			t.Fatalf("outcome = %v, want keep", outcome)
		}
		if state.OrphanedAt != nil {
			t.Fatalf("OrphanedAt should be cleared, got %#v", state.OrphanedAt)
		}
		if state.LastSeenAt == nil || !state.LastSeenAt.Equal(now) {
			t.Fatalf("LastSeenAt = %#v", state.LastSeenAt)
		}
		if state.ItemName != "Ep 1" || state.SeriesID != "show-1" || state.SeasonID != "season-1" || state.IndexNumber != 3 || state.RunTimeTicks != 9000 {
			t.Fatalf("metadata not merged: %#v", state)
		}
		if state.PlaybackPositionTicks != 77 || !state.IsFavorite {
			t.Fatalf("user data mutated on keep: %#v", state)
		}
	})

	t.Run("partial fingerprint compatible", func(t *testing.T) {
		state := &PlaybackState{ItemID: "item-1", Fingerprint: "type=Episode"}
		item := map[string]any{"Id": "item-1", "Type": "Episode", "Name": "Ep 2", "SeriesId": "show-2"}
		outcome := reconcileResolvedItem(state, item, true, now)
		if outcome != resolutionKeep {
			t.Fatalf("outcome = %v, want keep for partial fingerprint", outcome)
		}
		if state.Fingerprint == "" || !strings.Contains(state.Fingerprint, "type=Episode") {
			t.Fatalf("fingerprint not updated: %q", state.Fingerprint)
		}
	})
}

func TestResumeFindStoreOutageReturns500(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "item-1", "Name": "Movie", "Type": "Movie", "UserData": map[string]any{}},
		}})
	}))
	defer backend.Close()

	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), findErr: ErrStoreUnavailable}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	_ = store.MemoryStore.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-1", PlaybackPositionTicks: 1200,
	})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Resume?api_key=gateway-token", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("resume status = %d body=%s, want 500", resp.StatusCode, body)
	}
}

func TestPersonalFilterFindStoreOutageReturns500(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "fav-1", "Name": "Favorite", "Type": "Movie", "UserData": map[string]any{}},
		}, "TotalRecordCount": 1})
	}))
	defer backend.Close()

	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), findErr: ErrStoreUnavailable}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	_ = store.MemoryStore.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "fav-1", IsFavorite: true, ItemType: "Movie",
	})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&Filters=IsFavorite", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("personal filter status = %d body=%s, want 500 not 502", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "backend unavailable") {
		t.Fatalf("store outage should not report backend unavailable: %s", body)
	}
}

func TestSavePlaybackResolutionFailureFailsClosedOnResume(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "item-1", "Name": "Movie", "Type": "Movie", "UserData": map[string]any{}},
		}, "TotalRecordCount": 1})
	}))
	defer backend.Close()

	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), resolutionErr: errors.New("resolution write failed")}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	_ = store.MemoryStore.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "item-1", PlaybackPositionTicks: 1200,
	})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items/Resume?api_key=gateway-token", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("resume status = %d body=%s, want 500 (fail closed, not partial success)", resp.StatusCode, body)
	}
	if store.resolutionCalls == 0 {
		t.Fatal("expected SavePlaybackResolution to be attempted")
	}
}

func TestSavePlaybackResolutionFailureFailsClosedOnPersonalFavorite(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Items": []any{
			map[string]any{"Id": "fav-1", "Name": "Favorite", "Type": "Movie", "UserData": map[string]any{}},
		}, "TotalRecordCount": 1})
	}))
	defer backend.Close()

	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), resolutionErr: errors.New("resolution write failed")}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	_ = store.MemoryStore.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID: "u1", SyntheticUserID: "gateway-user", ItemID: "fav-1", IsFavorite: true, ItemType: "Movie",
	})
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user/Items?api_key=gateway-token&Filters=IsFavorite", nil))
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("personal favorite status = %d body=%s, want 500 (fail closed, not partial success)", resp.StatusCode, body)
	}
	if store.resolutionCalls == 0 {
		t.Fatal("expected SavePlaybackResolution to be attempted")
	}
	if strings.Contains(string(body), "backend unavailable") {
		t.Fatalf("store resolution failure should not report backend unavailable: %s", body)
	}
}
