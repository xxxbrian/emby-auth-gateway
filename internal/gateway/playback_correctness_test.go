package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
)

type faultInjectPlaybackStore struct {
	*MemoryStore
	findErr         error
	listErr         error
	saveErr         error
	applyErr        error
	resolutionErr   error
	findCalls       int
	saveCalls       int
	applyCalls      int
	resolutionCalls int
}

type playbackOuterActivitySpyStore struct {
	*faultInjectPlaybackStore
	authSession       *Session
	touchCalls        int
	outerRepairCalled bool
}

func (s *playbackOuterActivitySpyStore) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	if s.authSession == nil || tokenHash != s.authSession.GatewayTokenHash {
		return nil, ErrNotFound
	}
	return cloneSession(s.authSession), nil
}

func (s *playbackOuterActivitySpyStore) TouchSessionActivity(ctx context.Context, tokenHash string, at time.Time, minInterval time.Duration) (bool, error) {
	s.touchCalls++
	s.outerRepairCalled = true
	return false, nil
}

func newPlaybackOuterActivitySpyStore(t *testing.T, applyErr error) *playbackOuterActivitySpyStore {
	t.Helper()
	base := NewMemoryStore()
	created, err := base.CreateSession(context.Background(), *testSession())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	authSession := cloneSession(created)
	live := base.Sessions[created.GatewayTokenHash]
	live.PublicID = ""
	live.Capabilities = SessionCapabilities{}
	live.LastActivityAt = time.Time{}
	return &playbackOuterActivitySpyStore{
		faultInjectPlaybackStore: &faultInjectPlaybackStore{MemoryStore: base, applyErr: applyErr},
		authSession:              authSession,
	}
}

func (f *faultInjectPlaybackStore) FindPlaybackState(ctx context.Context, gatewayUserID, itemID string) (*PlaybackState, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.MemoryStore.FindPlaybackState(ctx, gatewayUserID, itemID)
}

func (f *faultInjectPlaybackStore) ListPlaybackStates(ctx context.Context, gatewayUserID string, filter PlaybackStateFilter) ([]PlaybackState, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.MemoryStore.ListPlaybackStates(ctx, gatewayUserID, filter)
}

func (f *faultInjectPlaybackStore) SavePlaybackState(ctx context.Context, state PlaybackState) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	return f.MemoryStore.SavePlaybackState(ctx, state)
}

func (f *faultInjectPlaybackStore) ApplyPlaybackReport(ctx context.Context, cmd PlaybackReportCommand) (PlaybackReportResult, error) {
	f.applyCalls++
	if f.applyErr != nil {
		return PlaybackReportResult{}, f.applyErr
	}
	return f.MemoryStore.ApplyPlaybackReport(ctx, cmd)
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

func TestPlaybackReportApplyFailureReturns500WithoutPartialState(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), applyErr: errors.New("apply failed")}
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
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
	if store.applyCalls != 1 {
		t.Fatalf("applyCalls=%d, want 1", store.applyCalls)
	}
	if _, err := store.MemoryStore.FindPlaybackState(context.Background(), "u1", "item-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected no persisted state, err=%v", err)
	}
	if len(store.PlaybackEvents) != 0 || len(store.CurrentPlaybacks) != 0 {
		t.Fatalf("partial event/current after apply failure: events=%d current=%d", len(store.PlaybackEvents), len(store.CurrentPlaybacks))
	}
	if !hasAuditEvent(store.MemoryStore, "playback_report_apply_failed") {
		t.Fatal("expected playback_report_apply_failed audit")
	}
}

func TestPlaybackReportFailureHasNoOuterSuccessSideEffects(t *testing.T) {
	cases := []struct {
		name       string
		applyErr   error
		wantStatus int
	}{
		{name: "unauthorized", applyErr: fmt.Errorf("revoked: %w", ErrUnauthorized), wantStatus: http.StatusUnauthorized},
		{name: "operational", applyErr: errors.New("storage failed"), wantStatus: http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newPlaybackOuterActivitySpyStore(t, tc.applyErr)
			em := observe.NewEmitter(32)
			defer em.Close()
			gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", Emitter: em}, store))
			defer gateway.Close()

			req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PositionTicks":25}`))
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus || resp.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("status/cache = %d/%q, want %d/no-store", resp.StatusCode, resp.Header.Get("Cache-Control"), tc.wantStatus)
			}
			if store.applyCalls != 1 || store.touchCalls != 0 || store.outerRepairCalled {
				t.Fatalf("apply/touch/repair = %d/%d/%v, want 1/0/false", store.applyCalls, store.touchCalls, store.outerRepairCalled)
			}
			live := store.MemoryStore.Sessions[HashToken("gateway-token")]
			if live.PublicID != "" || live.Capabilities.RawJSON != "" || !live.LastActivityAt.IsZero() {
				t.Fatalf("failed report repaired profile hole: %#v", live)
			}
			if len(store.PlaybackEvents) != 0 || len(store.PlaybackStates) != 0 || len(store.CurrentPlaybacks) != 0 {
				t.Fatalf("failed report mutated playback state: events=%d states=%d current=%d", len(store.PlaybackEvents), len(store.PlaybackStates), len(store.CurrentPlaybacks))
			}
			for _, event := range drainEmitter(em) {
				if event.Kind == observe.KindRequest && event.Outcome == observe.OutcomeOK {
					t.Fatalf("failed report emitted success request event: %#v", event)
				}
			}
			if tc.wantStatus == http.StatusUnauthorized && hasAuditEvent(store.MemoryStore, "playback_report_apply_failed") {
				t.Fatal("unauthorized report emitted operational audit")
			}
		})
	}
}

func TestPlaybackReportSuccessNotesWithoutGenericActivityTouch(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		suppress  bool
		wantApply int
	}{
		{name: "applied", body: `{"ItemId":"item-1","PositionTicks":25}`, wantApply: 1},
		{name: "missing item no-op", body: `{}`, wantApply: 1},
		{name: "guard suppressed no-op", body: `{"ItemId":"item-1","PositionTicks":25}`, suppress: true, wantApply: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newPlaybackOuterActivitySpyStore(t, nil)
			em := observe.NewEmitter(32)
			defer em.Close()
			server := NewServer(Config{GatewayBasePath: "/emby", Emitter: em}, store)
			if tc.suppress {
				server.playbackGuards.deny(playbackGuardKey{GatewayTokenHash: store.authSession.GatewayTokenHash, ItemID: "item-1"})
			}
			gateway := httptest.NewServer(server)
			defer gateway.Close()

			req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp := do(t, req)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if store.applyCalls != tc.wantApply || store.touchCalls != 0 || store.outerRepairCalled {
				t.Fatalf("apply/touch/repair = %d/%d/%v, want %d/0/false", store.applyCalls, store.touchCalls, store.outerRepairCalled, tc.wantApply)
			}
			requestOK := 0
			for _, event := range drainEmitter(em) {
				if event.Kind == observe.KindRequest && event.Outcome == observe.OutcomeOK {
					requestOK++
				}
			}
			if requestOK != 1 {
				t.Fatalf("success request events = %d, want 1", requestOK)
			}
		})
	}
}

func TestPlaybackReportApplyBadRequestReturns400WithoutOperationalAudit(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), applyErr: fmt.Errorf("repository rejected command: %w", ErrBadRequest)}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()

	req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PlaybackPositionTicks":250}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache = %d/%q, want 400/no-store", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if store.applyCalls != 1 {
		t.Fatalf("applyCalls = %d, want 1", store.applyCalls)
	}
	if hasAuditEvent(store.MemoryStore, "playback_report_apply_failed") {
		t.Fatal("repository ErrBadRequest must not be audited as an operational storage failure")
	}
	if len(store.PlaybackEvents) != 0 || len(store.PlaybackStates) != 0 || len(store.CurrentPlaybacks) != 0 {
		t.Fatalf("bad request mutated state: events=%d states=%d current=%d", len(store.PlaybackEvents), len(store.PlaybackStates), len(store.CurrentPlaybacks))
	}
}

func TestPlaybackReportApplyUnauthorizedReturns401WithoutOperationalAudit(t *testing.T) {
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), applyErr: fmt.Errorf("inactive session: %w", ErrUnauthorized)}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gateway := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gateway.Close()

	req := mustRequest(t, http.MethodPost, gateway.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PlaybackPositionTicks":250}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache = %d/%q, want 401/no-store", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if store.applyCalls != 1 {
		t.Fatalf("applyCalls = %d, want 1", store.applyCalls)
	}
	if hasAuditEvent(store.MemoryStore, "playback_report_apply_failed") {
		t.Fatal("repository ErrUnauthorized must not be audited as an operational storage failure")
	}
	if len(store.PlaybackEvents) != 0 || len(store.PlaybackStates) != 0 || len(store.CurrentPlaybacks) != 0 {
		t.Fatalf("unauthorized report mutated state: events=%d states=%d current=%d", len(store.PlaybackEvents), len(store.PlaybackStates), len(store.CurrentPlaybacks))
	}
}

func TestPlaybackReportEventFailureNowFailsReport(t *testing.T) {
	// Event write is part of the atomic ApplyPlaybackReport transaction; failure must fail the report
	// (reversed from legacy best-effort event persist).
	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), applyErr: errors.New("event write failed")}
	store.Sessions[HashToken("gateway-token")] = testSession()
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	req := mustRequest(t, http.MethodPost, gw.URL+"/emby/Sessions/Playing/Progress?api_key=gateway-token", strings.NewReader(`{"ItemId":"item-1","PlaybackPositionTicks":420}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when apply/event fails", resp.StatusCode)
	}
	if _, err := store.MemoryStore.FindPlaybackState(context.Background(), "u1", "item-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected no durable state after failed apply, err=%v", err)
	}
	if len(store.PlaybackEvents) != 0 {
		t.Fatalf("events = %d, want 0 after apply failure", len(store.PlaybackEvents))
	}
	if !hasAuditEvent(store.MemoryStore, "playback_report_apply_failed") {
		t.Fatal("expected playback_report_apply_failed audit event")
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
		// Name is not part of fingerprint identity; Type/SeriesId mismatch still orphans.
		state := &PlaybackState{ItemID: "item-1", Fingerprint: "type=Episode|seriesid=s1", PlaybackPositionTicks: 50}
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
		if strings.Contains(state.Fingerprint, "name=") {
			t.Fatalf("Name must not enter fingerprint identity: %q", state.Fingerprint)
		}
	})

	t.Run("rename does not mismatch", func(t *testing.T) {
		state := &PlaybackState{ItemID: "item-1", Fingerprint: "type=Movie", ItemName: "Old Title"}
		item := map[string]any{"Id": "item-1", "Type": "Movie", "Name": "New Title"}
		outcome := reconcileResolvedItem(state, item, true, now)
		if outcome != resolutionKeep {
			t.Fatalf("rename should not fingerprint-mismatch: %v", outcome)
		}
		if state.ItemName != "New Title" {
			t.Fatalf("name not merged: %#v", state)
		}
		if strings.Contains(state.Fingerprint, "name=") {
			t.Fatalf("fingerprint must not include Name: %q", state.Fingerprint)
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

	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), listErr: ErrStoreUnavailable}
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

	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), listErr: ErrStoreUnavailable}
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
