package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testSeriesEpisodes(count int) []any {
	items := make([]any, 0, count)
	for i := 1; i <= count; i++ {
		items = append(items, map[string]any{
			"Id":                "ep-" + strconv.Itoa(i),
			"Name":              "Episode " + strconv.Itoa(i),
			"Type":              "Episode",
			"SeriesId":          "show-1",
			"SeriesName":        "Show",
			"SeasonId":          "season-1",
			"ParentIndexNumber": 1,
			"IndexNumber":       i,
			"UserData":          map[string]any{},
		})
	}
	return items
}

func seedPlayedEpisode(t *testing.T, store *MemoryStore, n int, complete bool) PlaybackState {
	t.Helper()
	playedAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)
	state := PlaybackState{
		GatewayUserID:   "u1",
		SyntheticUserID: "gateway-user",
		ItemID:          "ep-" + strconv.Itoa(n),
		Played:          true,
		PlayCount:       2,
		LastPlayedDate:  &playedAt,
		UpdatedAt:       playedAt,
	}
	if complete {
		state.ItemName = "Episode " + strconv.Itoa(n)
		state.ItemType = "Episode"
		state.SeriesID = "show-1"
		state.SeriesName = "Show"
		state.SeasonID = "season-1"
		state.ParentIndexNumber = 1
		state.IndexNumber = n
		state.Fingerprint = "type=Episode|name=Episode " + strconv.Itoa(n) + "|seriesid=show-1"
	}
	if err := store.SavePlaybackState(context.Background(), state); err != nil {
		t.Fatalf("seed ep-%d: %v", n, err)
	}
	return state
}

func nextUpItemIDs(t *testing.T, gw *httptest.Server, rawQuery string) []string {
	t.Helper()
	resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby/Shows/NextUp?api_key=gateway-token&"+rawQuery, nil))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("next up status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	decodeJSON(t, resp.Body, &body)
	items, _ := body["Items"].([]any)
	return itemIDsFromResponse(items)
}

func requireInternalEpisodePage(t *testing.T, r *http.Request, start int) {
	t.Helper()
	if got := r.URL.Query().Get("Limit"); got != strconv.Itoa(personalScanBatchLimit) {
		t.Fatalf("episode Limit = %q, want internal %d (not client pagination)", got, personalScanBatchLimit)
	}
	if got := r.URL.Query().Get("StartIndex"); got != strconv.Itoa(start) {
		t.Fatalf("episode StartIndex = %q, want %d", got, start)
	}
}

func TestNextUpExplicitSeriesRepairsBlankPlayedEpisodeMetadata(t *testing.T) {
	episodes := testSeriesEpisodes(10)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/Users/backend-user/Items" {
			t.Fatalf("explicit SeriesId next up should not batch-resolve items: %s", r.URL.String())
		}
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": episodes, "TotalRecordCount": 10})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	for _, n := range []int{1, 2, 3} {
		seedPlayedEpisode(t, store, n, true)
	}
	seed := seedPlayedEpisode(t, store, 9, false)
	seed.IsFavorite = true
	seed.HideFromResume = true
	seed.PlayCount = 4
	seed.PlaybackPositionTicks = 0
	if err := store.SavePlaybackState(context.Background(), seed); err != nil {
		t.Fatalf("update incomplete ep-9: %v", err)
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if strings.Join(ids, ",") != "ep-10" {
		t.Fatalf("next up ids = %v, want ep-10", ids)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil {
		t.Fatalf("find repaired state: %v", err)
	}
	if state.SeriesID != "show-1" || state.SeasonID != "season-1" || state.ItemName != "Episode 9" || state.ItemType != "Episode" || state.IndexNumber != 9 || state.ParentIndexNumber != 1 {
		t.Fatalf("ep-9 metadata not repaired: %#v", state)
	}
	if !state.Played || !state.IsFavorite || !state.HideFromResume || state.PlayCount != 4 || state.PlaybackPositionTicks != 0 || state.LastPlayedDate == nil {
		t.Fatalf("repair clobbered user data: %#v", state)
	}

	ids = nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if strings.Join(ids, ",") != "ep-10" {
		t.Fatalf("idempotent next up ids = %v, want ep-10", ids)
	}
	again, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil {
		t.Fatalf("find after idempotent next up: %v", err)
	}
	if !again.Played || !again.IsFavorite || !again.HideFromResume || again.PlayCount != 4 || again.PlaybackPositionTicks != 0 {
		t.Fatalf("idempotent repair clobbered user data: %#v", again)
	}
}

func TestNextUpUsesRepairedStateWhenResolutionSaveFails(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": testSeriesEpisodes(10), "TotalRecordCount": 10})
	}))
	defer backend.Close()

	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore(), resolutionErr: errors.New("save failed")}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	for _, n := range []int{1, 2, 3} {
		seedPlayedEpisode(t, store.MemoryStore, n, true)
	}
	seedPlayedEpisode(t, store.MemoryStore, 9, false)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if strings.Join(ids, ",") != "ep-10" {
		t.Fatalf("next up ids = %v, want ep-10 from in-memory repair", ids)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || state.SeriesID != "" || !state.Played {
		t.Fatalf("failed save should leave persisted metadata untouched: %#v err=%v", state, err)
	}
}

func TestNextUpGlobalResolvesIncompleteRecentState(t *testing.T) {
	var itemBatches []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/backend-user/Items":
			itemBatches = append(itemBatches, r.URL.Query().Get("Ids"))
			if r.URL.Query().Get("Limit") != "1" {
				t.Fatalf("global item resolution Limit = %q, want 1", r.URL.Query().Get("Limit"))
			}
			if r.URL.Query().Get("SeriesId") != "" || r.URL.Query().Get("ParentId") != "" {
				t.Fatalf("global next up resolution should be unfiltered: %s", r.URL.RawQuery)
			}
			writeTestJSON(w, map[string]any{"Items": []any{
				map[string]any{
					"Id":                "ep-9",
					"Name":              "Episode 9",
					"Type":              "Episode",
					"SeriesId":          "show-1",
					"SeriesName":        "Show",
					"SeasonId":          "season-1",
					"ParentIndexNumber": 1,
					"IndexNumber":       9,
				},
			}, "TotalRecordCount": 1})
		case "/emby/Shows/show-1/Episodes":
			writeTestJSON(w, map[string]any{"Items": testSeriesEpisodes(10), "TotalRecordCount": 10})
		default:
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	for _, n := range []int{1, 2, 3} {
		seedPlayedEpisode(t, store, n, true)
	}
	seedPlayedEpisode(t, store, 9, false)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "Limit=1")
	if strings.Join(ids, ",") != "ep-10" {
		t.Fatalf("global next up ids = %v, want ep-10", ids)
	}
	if len(itemBatches) != 1 || itemBatches[0] != "ep-9" {
		t.Fatalf("global resolution batches = %v, want one unfiltered ep-9 batch", itemBatches)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || state.SeriesID != "show-1" || state.IndexNumber != 9 || !state.Played {
		t.Fatalf("global repair did not persist episode metadata: %#v err=%v", state, err)
	}
}

func TestNextUpGlobalResolutionFailureLeavesStateUntouched(t *testing.T) {
	var episodeLookups int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/backend-user/Items":
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		case "/emby/Shows/show-1/Episodes":
			episodeLookups++
			writeTestJSON(w, map[string]any{"Items": testSeriesEpisodes(10), "TotalRecordCount": 10})
		default:
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	seedPlayedEpisode(t, store, 9, false)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "Limit=1")
	if len(ids) != 0 {
		t.Fatalf("failed global resolution next up ids = %v, want empty", ids)
	}
	if episodeLookups != 0 {
		t.Fatalf("episode lookups = %d, want 0 when series remains unknown", episodeLookups)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || state.SeriesID != "" || state.ItemType != "" || !state.Played || state.OrphanedAt != nil {
		t.Fatalf("failed global resolution mutated state: %#v err=%v", state, err)
	}
}

func TestNextUpEpisodeFetchFailureDoesNotOrphan(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		http.Error(w, "episodes unavailable", http.StatusInternalServerError)
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	seedPlayedEpisode(t, store, 9, true)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if len(ids) != 0 {
		t.Fatalf("failed episode fetch next up ids = %v, want empty", ids)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || state.OrphanedAt != nil || !state.Played || state.SeriesID != "show-1" {
		t.Fatalf("episode fetch failure orphaned or mutated state: %#v err=%v", state, err)
	}
}

func TestNextUpPartialEpisodeListDoesNotRewindOrOrphan(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		requireInternalEpisodePage(t, r, 0)
		writeTestJSON(w, map[string]any{"Items": testSeriesEpisodes(8), "TotalRecordCount": 10})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	for _, n := range []int{1, 2, 3} {
		seedPlayedEpisode(t, store, n, true)
	}
	seedPlayedEpisode(t, store, 9, true)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if strings.Join(ids, ",") == "ep-4" {
		t.Fatalf("partial episode page rewound next up to ep-4: %v", ids)
	}
	if len(ids) != 0 {
		t.Fatalf("incomplete episode list should fail soft, got %v", ids)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || state.OrphanedAt != nil || !state.Played || state.IndexNumber != 9 {
		t.Fatalf("partial episode list orphaned ep-9: %#v err=%v", state, err)
	}
}

func TestNextUpFingerprintMismatchIsOrphanedAndDoesNotAdvance(t *testing.T) {
	episodes := testSeriesEpisodes(10)
	episodes[8] = map[string]any{
		"Id":                "ep-9",
		"Name":              "Different Movie",
		"Type":              "Movie",
		"SeriesId":          "other-show",
		"ParentIndexNumber": 1,
		"IndexNumber":       9,
		"UserData":          map[string]any{},
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": episodes, "TotalRecordCount": 10})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	for _, n := range []int{1, 2, 3} {
		seedPlayedEpisode(t, store, n, true)
	}
	state := seedPlayedEpisode(t, store, 9, false)
	state.Fingerprint = "type=Episode|name=Episode 9|seriesid=show-1"
	if err := store.SavePlaybackState(context.Background(), state); err != nil {
		t.Fatalf("seed fingerprint: %v", err)
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if strings.Join(ids, ",") != "ep-4" {
		t.Fatalf("mismatch next up ids = %v, want ep-4 (do not advance from mismatched ep-9)", ids)
	}
	got, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || got.OrphanedAt == nil || !got.Played || got.PlayCount != 2 {
		t.Fatalf("mismatch should orphan without clobbering played data: %#v err=%v", got, err)
	}
}

func TestNextUpExplicitZeroIndexIsValidOrdering(t *testing.T) {
	episodes := []any{
		map[string]any{"Id": "ep-special", "Name": "Special", "Type": "Episode", "SeriesId": "show-1", "ParentIndexNumber": 0, "IndexNumber": 0, "UserData": map[string]any{}},
		map[string]any{"Id": "ep-1", "Name": "Episode 1", "Type": "Episode", "SeriesId": "show-1", "ParentIndexNumber": 1, "IndexNumber": 1, "UserData": map[string]any{}},
		map[string]any{"Id": "ep-2", "Name": "Episode 2", "Type": "Episode", "SeriesId": "show-1", "ParentIndexNumber": 1, "IndexNumber": 2, "UserData": map[string]any{}},
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": episodes, "TotalRecordCount": 3})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	playedAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	if err := store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID:   "u1",
		SyntheticUserID: "gateway-user",
		ItemID:          "ep-special",
		Played:          true,
		LastPlayedDate:  &playedAt,
	}); err != nil {
		t.Fatalf("seed special: %v", err)
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if strings.Join(ids, ",") != "ep-1" {
		t.Fatalf("zero-index next up ids = %v, want ep-1 after explicit index 0", ids)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "ep-special")
	if err != nil || state.IndexNumber != 0 || state.ParentIndexNumber != 0 || state.SeriesID != "show-1" || !state.Played {
		t.Fatalf("explicit zero index was not preserved as valid: %#v err=%v", state, err)
	}
}

func TestNextUpGlobalResolutionUsesOneBoundedBatch(t *testing.T) {
	var batchSizes []int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Users/backend-user/Items" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		ids := splitFilterValues([]string{r.URL.Query().Get("Ids")})
		if r.URL.Query().Get("Limit") != strconv.Itoa(len(ids)) {
			t.Fatalf("global item Limit = %q, want %d", r.URL.Query().Get("Limit"), len(ids))
		}
		batchSizes = append(batchSizes, len(ids))
		writeTestJSON(w, map[string]any{"Items": []any{}, "TotalRecordCount": 0})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	for i := 0; i < personalIDBatchLimit+5; i++ {
		playedAt := now.Add(-time.Duration(i) * time.Minute)
		_ = store.SavePlaybackState(context.Background(), PlaybackState{
			GatewayUserID:   "u1",
			SyntheticUserID: "gateway-user",
			ItemID:          "item-" + strconv.Itoa(i),
			Played:          true,
			LastPlayedDate:  &playedAt,
			UpdatedAt:       playedAt,
		})
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	_ = nextUpItemIDs(t, gw, "Limit=1")
	if len(batchSizes) != 1 || batchSizes[0] != personalIDBatchLimit {
		t.Fatalf("global next up batches = %v, want one batch of %d", batchSizes, personalIDBatchLimit)
	}
}

func TestFetchedEpisodeIndexTreatsExplicitZeroAsValid(t *testing.T) {
	idx := fetchedEpisodeIndex(map[string]any{"ParentIndexNumber": 0, "IndexNumber": 0})
	if !idx.valid || idx.season != 0 || idx.episode != 0 {
		t.Fatalf("explicit zero index = %#v, want valid 0/0", idx)
	}
	missingEpisode := fetchedEpisodeIndex(map[string]any{"ParentIndexNumber": 1, "Name": "Episode"})
	if missingEpisode.valid {
		t.Fatalf("missing IndexNumber should not be valid: %#v", missingEpisode)
	}
	missingSeason := fetchedEpisodeIndex(map[string]any{"IndexNumber": 3, "Name": "Episode"})
	if missingSeason.valid {
		t.Fatalf("missing ParentIndexNumber should not be valid: %#v", missingSeason)
	}
	same := itemEpisodeIndex(map[string]any{"ParentIndexNumber": 0, "IndexNumber": 0})
	if !same.valid || same.season != 0 || same.episode != 0 {
		t.Fatalf("itemEpisodeIndex explicit zero = %#v, want valid 0/0", same)
	}
}

func TestNextUpGlobalTruncatedResponseDoesNotOrphanMissing(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/backend-user/Items":
			ids := splitFilterValues([]string{r.URL.Query().Get("Ids")})
			if r.URL.Query().Get("Limit") != strconv.Itoa(len(ids)) {
				t.Fatalf("Limit = %q, want %d", r.URL.Query().Get("Limit"), len(ids))
			}
			writeTestJSON(w, map[string]any{"Items": []any{
				map[string]any{"Id": "ep-9", "Name": "Episode 9", "Type": "Episode", "SeriesId": "show-1", "SeasonId": "season-1", "ParentIndexNumber": 1, "IndexNumber": 9},
			}, "TotalRecordCount": 2})
		case "/emby/Shows/show-1/Episodes":
			http.Error(w, "episodes unused for this assertion", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	seedPlayedEpisode(t, store, 8, false)
	seedPlayedEpisode(t, store, 9, false)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	_ = nextUpItemIDs(t, gw, "Limit=1")
	missing, err := store.FindPlaybackState(context.Background(), "u1", "ep-8")
	if err != nil || missing.OrphanedAt != nil || missing.SeriesID != "" || !missing.Played {
		t.Fatalf("truncated 2xx orphaned or mutated missing id: %#v err=%v", missing, err)
	}
	repaired, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || repaired.SeriesID != "show-1" || repaired.OrphanedAt != nil {
		t.Fatalf("returned item should still repair: %#v err=%v", repaired, err)
	}
}

func TestNextUpGlobalUnknownCompletenessDoesNotOrphanMissing(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/backend-user/Items":
			writeTestJSON(w, map[string]any{"Items": []any{
				map[string]any{"Id": "ep-9", "Name": "Episode 9", "Type": "Episode", "SeriesId": "show-1", "SeasonId": "season-1", "ParentIndexNumber": 1, "IndexNumber": 9},
			}})
		case "/emby/Shows/show-1/Episodes":
			http.Error(w, "episodes unused for this assertion", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	seedPlayedEpisode(t, store, 8, false)
	seedPlayedEpisode(t, store, 9, false)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	_ = nextUpItemIDs(t, gw, "Limit=1")
	missing, err := store.FindPlaybackState(context.Background(), "u1", "ep-8")
	if err != nil || missing.OrphanedAt != nil || missing.SeriesID != "" || !missing.Played {
		t.Fatalf("unknown completeness orphaned or mutated missing id: %#v err=%v", missing, err)
	}
}

func TestNextUpGlobalFingerprintMismatchDoesNotEmitFirstEpisode(t *testing.T) {
	episodes := testSeriesEpisodes(10)
	episodes[8] = map[string]any{
		"Id": "ep-9", "Name": "Different Movie", "Type": "Movie", "SeriesId": "other-show",
		"ParentIndexNumber": 1, "IndexNumber": 9, "UserData": map[string]any{},
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": episodes, "TotalRecordCount": 10})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	state := seedPlayedEpisode(t, store, 9, true)
	state.Fingerprint = "type=Episode|name=Episode 9|seriesid=show-1"
	if err := store.SavePlaybackState(context.Background(), state); err != nil {
		t.Fatalf("seed fingerprint: %v", err)
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "Limit=1")
	if len(ids) != 0 {
		t.Fatalf("global mismatch next up ids = %v, want empty (no first episode)", ids)
	}
	got, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || got.OrphanedAt == nil || !got.Played {
		t.Fatalf("global mismatch should orphan played row: %#v err=%v", got, err)
	}
}

func TestNextUpSecondRequestDoesNotPersistCompatibleResolutions(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": testSeriesEpisodes(10), "TotalRecordCount": 10})
	}))
	defer backend.Close()

	store := &faultInjectPlaybackStore{MemoryStore: NewMemoryStore()}
	configureTestUpstream(store.MemoryStore, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	for _, n := range []int{1, 2, 3} {
		seedPlayedEpisode(t, store.MemoryStore, n, true)
	}
	seedPlayedEpisode(t, store.MemoryStore, 9, false)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	if ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1"); strings.Join(ids, ",") != "ep-10" {
		t.Fatalf("first next up ids = %v, want ep-10", ids)
	}
	if store.resolutionCalls == 0 {
		t.Fatal("first next up should persist incomplete metadata repair")
	}
	afterFirst := store.resolutionCalls
	if ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1"); strings.Join(ids, ",") != "ep-10" {
		t.Fatalf("second next up ids = %v, want ep-10", ids)
	}
	if store.resolutionCalls != afterFirst {
		t.Fatalf("second next up resolution writes = %d, want 0 additional (had %d)", store.resolutionCalls-afterFirst, afterFirst)
	}
}

func TestNextUpPagesLongSeries(t *testing.T) {
	const total = 250
	var pages []int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		start := intQuery(r.URL.Query(), "StartIndex", 0)
		limit := intQuery(r.URL.Query(), "Limit", 0)
		if limit != personalScanBatchLimit {
			t.Fatalf("page limit = %d, want %d", limit, personalScanBatchLimit)
		}
		pages = append(pages, start)
		all := testSeriesEpisodes(total)
		end := start + limit
		if end > len(all) {
			end = len(all)
		}
		if start > len(all) {
			start = len(all)
		}
		writeTestJSON(w, map[string]any{"Items": all[start:end], "TotalRecordCount": total})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	seedPlayedEpisode(t, store, 200, true)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if strings.Join(ids, ",") != "ep-201" {
		t.Fatalf("long series next up ids = %v, want ep-201", ids)
	}
	if len(pages) < 3 || pages[0] != 0 || pages[1] != personalScanBatchLimit {
		t.Fatalf("episode pages = %v, want at least 3 pages starting at 0/%d", pages, personalScanBatchLimit)
	}
}

func TestMergeItemMetadataDoesNotWeakenFingerprint(t *testing.T) {
	state := &PlaybackState{
		ItemID:      "ep-9",
		ItemName:    "Episode 9",
		ItemType:    "Episode",
		SeriesID:    "show-1",
		Fingerprint: "type=Episode|name=Episode 9|seriesid=show-1",
	}
	mergeItemMetadata(state, map[string]any{"Id": "ep-9", "Type": "Episode"})
	if state.Fingerprint != "type=Episode|name=Episode 9|seriesid=show-1" {
		t.Fatalf("partial item weakened fingerprint: %q", state.Fingerprint)
	}
	if state.ItemName != "Episode 9" || state.ItemType != "Episode" || state.SeriesID != "show-1" {
		t.Fatalf("partial item clobbered identity: %#v", state)
	}

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	kept := &PlaybackState{ItemID: "ep-9", Fingerprint: "type=Episode|name=Episode 9|seriesid=show-1", ItemName: "Episode 9", ItemType: "Episode", SeriesID: "show-1"}
	if outcome := reconcileResolvedItem(kept, map[string]any{"Id": "ep-9", "Type": "Episode"}, true, now); outcome != resolutionKeep {
		t.Fatalf("outcome = %v, want keep", outcome)
	}
	if kept.Fingerprint != "type=Episode|name=Episode 9|seriesid=show-1" {
		t.Fatalf("reconcile weakened fingerprint: %q", kept.Fingerprint)
	}
}

func TestNextUpGlobalDuplicateUnrelatedItemsDoNotOrphan(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/backend-user/Items":
			ep9 := map[string]any{"Id": "ep-9", "Name": "Episode 9", "Type": "Episode", "SeriesId": "show-1", "SeasonId": "season-1", "ParentIndexNumber": 1, "IndexNumber": 9}
			writeTestJSON(w, map[string]any{"Items": []any{
				ep9,
				ep9,
				map[string]any{"Name": "no-id"},
				map[string]any{"Id": "unrelated", "Name": "Other", "Type": "Movie"},
			}, "TotalRecordCount": 4})
		case "/emby/Shows/show-1/Episodes":
			http.Error(w, "episodes unused for this assertion", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	seedPlayedEpisode(t, store, 8, false)
	seedPlayedEpisode(t, store, 9, false)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	_ = nextUpItemIDs(t, gw, "Limit=1")
	missing, err := store.FindPlaybackState(context.Background(), "u1", "ep-8")
	if err != nil || missing.OrphanedAt != nil || missing.SeriesID != "" || !missing.Played {
		t.Fatalf("duplicate/unrelated items orphaned missing id: %#v err=%v", missing, err)
	}
	repaired, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || repaired.SeriesID != "show-1" || repaired.OrphanedAt != nil {
		t.Fatalf("valid returned requested item should still repair: %#v err=%v", repaired, err)
	}
}

func TestNextUpGlobalCompleteResponseOrphansOnlyAbsentRequestedID(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Users/backend-user/Items":
			writeTestJSON(w, map[string]any{"Items": []any{
				map[string]any{"Id": "ep-9", "Name": "Episode 9", "Type": "Episode", "SeriesId": "show-1", "SeasonId": "season-1", "ParentIndexNumber": 1, "IndexNumber": 9},
			}, "TotalRecordCount": 1})
		case "/emby/Shows/show-1/Episodes":
			http.Error(w, "episodes unused for this assertion", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	seedPlayedEpisode(t, store, 8, false)
	seedPlayedEpisode(t, store, 9, false)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	_ = nextUpItemIDs(t, gw, "Limit=1")
	absent, err := store.FindPlaybackState(context.Background(), "u1", "ep-8")
	if err != nil || absent.OrphanedAt == nil || !absent.Played {
		t.Fatalf("complete TRC=1 should orphan absent requested id: %#v err=%v", absent, err)
	}
	repaired, err := store.FindPlaybackState(context.Background(), "u1", "ep-9")
	if err != nil || repaired.SeriesID != "show-1" || repaired.OrphanedAt != nil || !repaired.Played {
		t.Fatalf("present requested id should repair without orphan: %#v err=%v", repaired, err)
	}
}

func TestNextUpOverlappingEpisodePagesFailSoft(t *testing.T) {
	var pages int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		pages++
		if pages > 5 {
			t.Fatalf("overlapping pages did not stall: %d requests", pages)
		}
		writeTestJSON(w, map[string]any{"Items": testSeriesEpisodes(100), "TotalRecordCount": 250})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	for _, n := range []int{1, 2, 3} {
		seedPlayedEpisode(t, store, n, true)
	}
	seedPlayedEpisode(t, store, 200, true)
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "SeriesId=show-1&Limit=1")
	if strings.Join(ids, ",") == "ep-4" {
		t.Fatalf("overlapping pages rewound next up to ep-4: %v", ids)
	}
	if len(ids) != 0 {
		t.Fatalf("overlapping incomplete catalog should fail soft, got %v", ids)
	}
	if pages != 2 {
		t.Fatalf("overlapping pages = %d, want 2 (first page + stalled overlap)", pages)
	}
	state, err := store.FindPlaybackState(context.Background(), "u1", "ep-200")
	if err != nil || state.OrphanedAt != nil || !state.Played {
		t.Fatalf("overlapping pages orphaned ep-200: %#v err=%v", state, err)
	}
}

func TestNextUpDoesNotLookupCompleteSpecialsEpisode(t *testing.T) {
	episodes := []any{
		map[string]any{"Id": "ep-special", "Name": "Special", "Type": "Episode", "SeriesId": "show-1", "ParentIndexNumber": 0, "IndexNumber": 0, "UserData": map[string]any{}},
		map[string]any{"Id": "ep-1", "Name": "Episode 1", "Type": "Episode", "SeriesId": "show-1", "ParentIndexNumber": 1, "IndexNumber": 1, "UserData": map[string]any{}},
		map[string]any{"Id": "ep-2", "Name": "Episode 2", "Type": "Episode", "SeriesId": "show-1", "ParentIndexNumber": 1, "IndexNumber": 2, "UserData": map[string]any{}},
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby/Users/backend-user/Items" {
			t.Fatalf("populated S00E00 should not trigger global item lookup: %s", r.URL.String())
		}
		if r.URL.Path != "/emby/Shows/show-1/Episodes" {
			t.Fatalf("unexpected backend request %s", r.URL.String())
		}
		writeTestJSON(w, map[string]any{"Items": episodes, "TotalRecordCount": 3})
	}))
	defer backend.Close()

	store := NewMemoryStore()
	configureTestUpstream(store, backend.URL+"/emby")
	store.Sessions[HashToken("gateway-token")] = testSession()
	playedAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	if err := store.SavePlaybackState(context.Background(), PlaybackState{
		GatewayUserID:     "u1",
		SyntheticUserID:   "gateway-user",
		ItemID:            "ep-special",
		ItemName:          "Special",
		ItemType:          "Episode",
		SeriesID:          "show-1",
		ParentIndexNumber: 0,
		IndexNumber:       0,
		Played:            true,
		LastPlayedDate:    &playedAt,
		Fingerprint:       "type=Episode|name=Special|seriesid=show-1",
	}); err != nil {
		t.Fatalf("seed special: %v", err)
	}
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby"}, store))
	defer gw.Close()

	ids := nextUpItemIDs(t, gw, "Limit=1")
	if strings.Join(ids, ",") != "ep-1" {
		t.Fatalf("s00e00 next up ids = %v, want ep-1", ids)
	}
}

func TestIDResolutionResponseCompleteUsesUniqueRequestedIDs(t *testing.T) {
	requested := map[string]bool{"ep-8": true, "ep-9": true}
	ep9 := map[string]any{"Id": "ep-9", "Name": "Episode 9"}
	noisy := map[string]any{"Items": []any{ep9, ep9, map[string]any{"Id": "other"}, map[string]any{}}, "TotalRecordCount": 4}
	if idResolutionResponseComplete(noisy, requested, extractItems(noisy)) {
		t.Fatal("duplicate/unrelated items must not certify completeness")
	}
	complete := map[string]any{"Items": []any{ep9}, "TotalRecordCount": 1}
	if !idResolutionResponseComplete(complete, requested, extractItems(complete)) {
		t.Fatal("unique requested id with TRC=1 should be complete")
	}
}
