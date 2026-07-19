package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type personalPlanSourceStore struct {
	*MemoryStore
	states     []PlaybackState
	listErr    error
	listCalls  int
	findCalls  int
	lastFilter PlaybackStateFilter
	lastUserID string
	saves      []PlaybackState
	saveErr    error
	saveErrAt  int
}

func (s *personalPlanSourceStore) ListPlaybackStates(_ context.Context, userID string, filter PlaybackStateFilter) ([]PlaybackState, error) {
	s.listCalls++
	s.lastUserID = userID
	s.lastFilter = filter
	return append([]PlaybackState(nil), s.states...), s.listErr
}

func (s *personalPlanSourceStore) FindPlaybackState(ctx context.Context, userID, itemID string) (*PlaybackState, error) {
	s.findCalls++
	return s.MemoryStore.FindPlaybackState(ctx, userID, itemID)
}

func (s *personalPlanSourceStore) SavePlaybackResolution(ctx context.Context, state PlaybackState) error {
	s.saves = append(s.saves, state)
	if s.saveErr != nil && (s.saveErrAt == 0 || s.saveErrAt == len(s.saves)) {
		return s.saveErr
	}
	return s.MemoryStore.SavePlaybackResolution(ctx, state)
}

func TestPersonalPlanSourceSnapshotUsesOneScopedList(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ID: "state-a", ItemID: "a", Played: true, PlaybackPositionTicks: 42}}}
	snapshot, err := NewServer(Config{}, store).personalStateSnapshot(context.Background(), &Session{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"})
	if err != nil {
		t.Fatal(err)
	}
	state := snapshot.States["a"]
	if store.listCalls != 1 || store.findCalls != 0 || !store.lastFilter.IncludeOrphaned || store.lastUserID != "gateway-user" {
		t.Fatalf("list=%d find=%d filter=%#v user=%q", store.listCalls, store.findCalls, store.lastFilter, store.lastUserID)
	}
	if state.ID != "state-a" || !state.Played || state.PlaybackPositionTicks != 42 || state.GatewayUserID != "gateway-user" || state.SyntheticUserID != "synthetic-user" {
		t.Fatalf("snapshot state = %#v", state)
	}
}

func TestPersonalPlanSourceSnapshotFailuresAreStoreUnavailable(t *testing.T) {
	for _, store := range []*personalPlanSourceStore{
		{MemoryStore: NewMemoryStore(), listErr: errors.New("offline")},
		{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "duplicate"}, {ItemID: "duplicate"}}},
	} {
		_, err := NewServer(Config{}, store).personalStateSnapshot(context.Background(), &Session{GatewayUserID: "u", SyntheticUserID: "s"})
		if !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("error = %v, want ErrStoreUnavailable", err)
		}
	}
	for _, state := range []PlaybackState{
		{ItemID: "foreign-gateway", GatewayUserID: "other"},
		{ItemID: "foreign-synthetic", SyntheticUserID: "other"},
	} {
		store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{state}}
		_, err := NewServer(Config{}, store).personalStateSnapshot(context.Background(), &Session{GatewayUserID: "u", SyntheticUserID: "s"})
		if !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("foreign state error = %v, want ErrStoreUnavailable", err)
		}
	}
}

type personalPlanSourceMetadataFake struct {
	status    int
	body      string
	err       error
	calls     int
	request   *http.Request
	requests  []*http.Request
	snapshot  upstreamRequestSnapshot
	responses []personalPlanSourceMetadataResponse
}

type personalPlanSourceMetadataResponse struct {
	status   int
	body     string
	err      error
	snapshot upstreamRequestSnapshot
}

func (f *personalPlanSourceMetadataFake) RoundTripMetadata(in metadataUpstreamRequest) (*http.Response, error) {
	response := personalPlanSourceMetadataResponse{status: f.status, body: f.body, err: f.err, snapshot: f.snapshot}
	if f.calls < len(f.responses) {
		response = f.responses[f.calls]
	}
	f.calls++
	f.request = in.Request
	f.requests = append(f.requests, in.Request)
	if in.SnapshotRef != nil {
		*in.SnapshotRef = response.snapshot
	}
	if response.err != nil {
		return nil, response.err
	}
	return &http.Response{StatusCode: response.status, Body: io.NopCloser(strings.NewReader(response.body)), Header: make(http.Header), Request: in.Request}, nil
}

func newPersonalPlanSourceTestSource(t *testing.T, fake *personalPlanSourceMetadataFake) (*personalPlanSource, *phase5AuthSpy) {
	return newPersonalPlanSourceTestSourceWithStore(t, fake, NewMemoryStore())
}

func newPersonalPlanSourceTestSourceWithStore(t *testing.T, fake *personalPlanSourceMetadataFake, store Store) (*personalPlanSource, *phase5AuthSpy) {
	t.Helper()
	server := NewServer(Config{GatewayServerID: "gateway-server", PublicBaseURL: "https://gateway.test/emby"}, store)
	auth := &phase5AuthSpy{runtime: managedRuntime("old-token")}
	server.managedAuthUpstream = auth
	server.metadataUpstream = fake
	request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Items", nil)
	source, err := newPersonalPlanSource(server, request, &Session{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}, "gateway-token")
	if err != nil {
		t.Fatal(err)
	}
	return source, auth
}

func TestPersonalPlanSourceResolveIDsIdentityPreflightHasNoEgress(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[]}`}
	source, auth := newPersonalPlanSourceTestSource(t, fake)
	_, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanNegative}, personalStateSnapshot{GatewayUserID: "other", SyntheticUserID: "synthetic-user"}, []string{"item"})
	if !errors.Is(err, ErrStoreUnavailable) || fake.calls != 0 || auth.ensure != 0 {
		t.Fatalf("err=%v metadata=%d ensure=%d", err, fake.calls, auth.ensure)
	}
}

func personalPlanSourceSnapshot(states map[string]PlaybackState) personalStateSnapshot {
	return personalStateSnapshot{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", States: states}
}

func personalPlanSourceUpstreamSnapshot() upstreamRequestSnapshot {
	return upstreamRequestSnapshot{baseURL: "https://backend.test/emby", userID: "backend-user", serverID: "backend-server", token: "backend-token"}
}

func personalPlanSourceJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func personalPlanSourceItems(ids []string, reverse bool) []any {
	items := make([]any, 0, len(ids))
	for i := range ids {
		index := i
		if reverse {
			index = len(ids) - 1 - i
		}
		items = append(items, map[string]any{"Id": ids[index], "Type": "Movie", "UserId": "backend-user", "ServerId": "backend-server"})
	}
	return items
}

func personalPlanSourceFieldSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[strings.ToLower(value)] = true
	}
	return set
}

func TestPersonalPlanSourceResolveIDsPreflightRejectsCorruptSnapshotWithoutEgress(t *testing.T) {
	for _, test := range []struct {
		name     string
		snapshot personalStateSnapshot
	}{
		{"empty gateway", personalStateSnapshot{SyntheticUserID: "synthetic-user"}},
		{"empty synthetic", personalStateSnapshot{GatewayUserID: "gateway-user"}},
		{"foreign gateway", personalStateSnapshot{GatewayUserID: "other", SyntheticUserID: "synthetic-user"}},
		{"foreign synthetic", personalStateSnapshot{GatewayUserID: "gateway-user", SyntheticUserID: "other"}},
		{"state item mismatch", personalPlanSourceSnapshot(map[string]PlaybackState{"item": {ItemID: "other", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}})},
		{"state empty gateway", personalPlanSourceSnapshot(map[string]PlaybackState{"item": {ItemID: "item", SyntheticUserID: "synthetic-user"}})},
		{"state foreign gateway", personalPlanSourceSnapshot(map[string]PlaybackState{"item": {ItemID: "item", GatewayUserID: "other", SyntheticUserID: "synthetic-user"}})},
		{"state empty synthetic", personalPlanSourceSnapshot(map[string]PlaybackState{"item": {ItemID: "item", GatewayUserID: "gateway-user"}})},
		{"state foreign synthetic", personalPlanSourceSnapshot(map[string]PlaybackState{"item": {ItemID: "item", GatewayUserID: "gateway-user", SyntheticUserID: "other"}})},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &personalPlanSourceMetadataFake{}
			store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
			source, auth := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
			_, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanNegative}, test.snapshot, []string{"item"})
			if !errors.Is(err, ErrStoreUnavailable) || fake.calls != 0 || auth.ensure != 0 || len(store.saves) != 0 {
				t.Fatalf("err=%v metadata=%d ensure=%d saves=%d", err, fake.calls, auth.ensure, len(store.saves))
			}
		})
	}
}

func TestPersonalPlanSourceResolveIDsBatchesAndPreservesRequestedOrder(t *testing.T) {
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf("item-%03d", i)
	}
	upstream := personalPlanSourceUpstreamSnapshot()
	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": personalPlanSourceItems(ids[:200], true)}), snapshot: upstream},
		{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": personalPlanSourceItems(ids[200:], true)}), snapshot: upstream},
	}}
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	plan := personalPlan{
		Kind:       personalPlanNegative,
		Refinement: url.Values{"ParentId": {"parent"}, "Filters": {"IsFolder,Unknown,IsPlayed"}, "IsFavorite": {"true"}, "SortBy": {"PlayCount"}},
		Projection: url.Values{"Fields": {"Name,UserData"}, "EnableUserData": {"true"}},
		Sort:       []personalSortTerm{{Name: "DateCreated", Source: personalSortMetadata}, {Name: "PlayCount", Source: personalSortLocal}},
	}
	resolved, err := source.resolveIDs(context.Background(), plan, personalPlanSourceSnapshot(nil), ids)
	if err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 || len(store.saves) != 201 || store.findCalls != 0 || len(resolved) != 201 {
		t.Fatalf("metadata=%d saves=%d finds=%d resolved=%d", fake.calls, len(store.saves), store.findCalls, len(resolved))
	}
	if got := strings.Split(fake.requests[0].URL.Query().Get("Ids"), ","); len(got) != 200 || !reflect.DeepEqual(got, ids[:200]) {
		t.Fatalf("first batch IDs = %d, ordered=%v", len(got), reflect.DeepEqual(got, ids[:200]))
	}
	if got := strings.Split(fake.requests[1].URL.Query().Get("Ids"), ","); !reflect.DeepEqual(got, ids[200:]) {
		t.Fatalf("second batch IDs = %v", got)
	}
	query := fake.requests[0].URL.Query()
	if query.Get("ParentId") != "" || query.Get("Filters") != "" || query.Get("Fields") != "Name,DateCreated" || query.Get("IsFavorite") != "" || query.Get("EnableUserData") != "" || query.Get("SortBy") != "" || query.Get("SortOrder") != "" {
		t.Fatalf("resolution egress query = %v", query)
	}
	for i, item := range resolved {
		id, ok := personalItemID(item.item)
		if !ok || item.item == nil || id != ids[i] {
			t.Fatalf("resolved[%d] = %#v, want %q", i, item.item, ids[i])
		}
	}
}

func TestPersonalPlanSourceResolveIDsSecondBatchFailureReturnsNoPartialItems(t *testing.T) {
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf("item-%03d", i)
	}
	first := personalPlanSourceMetadataResponse{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": personalPlanSourceItems(ids[:200], false)}), snapshot: personalPlanSourceUpstreamSnapshot()}
	for _, failure := range []personalPlanSourceMetadataResponse{
		{err: errors.New("offline")},
		{status: http.StatusBadGateway, body: `{"Items":[]}`},
	} {
		fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{first, failure}}
		store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
		source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
		resolved, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanNegative}, personalPlanSourceSnapshot(nil), ids)
		if err == nil || resolved != nil || fake.calls != 2 || store.findCalls != 0 {
			t.Fatalf("resolved=%#v err=%v calls=%d finds=%d", resolved, err, fake.calls, store.findCalls)
		}
	}
}

func TestPersonalPlanSourceResolveIDsReconcilesOmittedAndCompatibleItems(t *testing.T) {
	t.Run("omitted", func(t *testing.T) {
		state := PlaybackState{ItemID: "missing", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true, PlaybackPositionTicks: 42}
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
		store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
		source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
		resolved, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanPositive}, personalPlanSourceSnapshot(map[string]PlaybackState{"missing": state}), []string{"missing"})
		if err != nil || len(resolved) != 0 || len(store.saves) != 1 || store.saves[0].OrphanedAt == nil {
			t.Fatalf("resolved=%#v saves=%#v err=%v", resolved, store.saves, err)
		}
	})

	t.Run("compatible", func(t *testing.T) {
		percentage, likes := 37.5, false
		orphaned := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		fingerprintItem := map[string]any{"Type": "Movie", "SeriesId": "series"}
		state := PlaybackState{
			ItemID: "present", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user",
			Fingerprint: itemFingerprint(fingerprintItem), OrphanedAt: &orphaned, Played: true,
			PlaybackPositionTicks: 1234, PlayCount: 8, IsFavorite: true, Likes: &likes, PlayedPercentage: &percentage,
		}
		item := map[string]any{"Id": "present", "Type": "Movie", "SeriesId": "series", "Name": "Resolved", "RunTimeTicks": 9000, "UserId": "backend-user", "ServerId": "backend-server"}
		fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": []any{item}}), snapshot: personalPlanSourceUpstreamSnapshot()}
		store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
		source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
		resolved, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanPositive}, personalPlanSourceSnapshot(map[string]PlaybackState{"present": state}), []string{"present"})
		if err != nil || len(resolved) != 1 || len(store.saves) != 1 {
			t.Fatalf("resolved=%#v saves=%#v err=%v", resolved, store.saves, err)
		}
		saved := store.saves[0]
		if saved.OrphanedAt != nil || saved.LastSeenAt == nil || saved.ItemName != "Resolved" || saved.Fingerprint != itemFingerprint(item) {
			t.Fatalf("resolution metadata = %#v", saved)
		}
		if !saved.Played || saved.PlaybackPositionTicks != 1234 || saved.PlayCount != 8 || !saved.IsFavorite || saved.Likes == nil || *saved.Likes || saved.PlayedPercentage == nil || *saved.PlayedPercentage != percentage {
			t.Fatalf("user fields changed = %#v", saved)
		}
	})
}

func TestPersonalPlanSourceResolveIDsFingerprintMismatchPreservesUserFields(t *testing.T) {
	percentage, likes := 25.0, true
	state := PlaybackState{
		ItemID: "item", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user",
		Fingerprint: itemFingerprint(map[string]any{"Type": "Movie", "SeriesId": "series-a"}), Played: true,
		PlaybackPositionTicks: 777, PlayCount: 3, IsFavorite: true, Likes: &likes, PlayedPercentage: &percentage,
	}
	item := map[string]any{"Id": "item", "Type": "Episode", "SeriesId": "series-b"}
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": []any{item}}), snapshot: personalPlanSourceUpstreamSnapshot()}
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	resolved, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanPositive}, personalPlanSourceSnapshot(map[string]PlaybackState{"item": state}), []string{"item"})
	if err != nil || len(resolved) != 0 || len(store.saves) != 1 || store.saves[0].OrphanedAt == nil {
		t.Fatalf("resolved=%#v saves=%#v err=%v", resolved, store.saves, err)
	}
	saved := store.saves[0]
	if saved.Fingerprint != state.Fingerprint || !saved.Played || saved.PlaybackPositionTicks != 777 || saved.PlayCount != 3 || !saved.IsFavorite || saved.Likes == nil || !*saved.Likes || saved.PlayedPercentage == nil || *saved.PlayedPercentage != percentage {
		t.Fatalf("mismatch clobbered state: %#v", saved)
	}
}

func TestPersonalPlanSourceResolveIDsSaveFailureFailsClosed(t *testing.T) {
	state := PlaybackState{ItemID: "item", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}
	item := map[string]any{"Id": "item", "Type": "Movie"}
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": []any{item}}), snapshot: personalPlanSourceUpstreamSnapshot()}
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), saveErr: errors.New("write failed")}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	resolved, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanPositive}, personalPlanSourceSnapshot(map[string]PlaybackState{"item": state}), []string{"item"})
	if !errors.Is(err, ErrStoreUnavailable) || resolved != nil || len(store.saves) != 1 {
		t.Fatalf("resolved=%#v saves=%d err=%v", resolved, len(store.saves), err)
	}
}

func TestPersonalPlanSourceResolveIDsRejectsInvalidInputsAndBackendResults(t *testing.T) {
	t.Run("inputs", func(t *testing.T) {
		for _, test := range []struct {
			name string
			plan personalPlan
			ids  []string
		}{
			{"empty list", personalPlan{Kind: personalPlanNegative}, nil},
			{"empty ID", personalPlan{Kind: personalPlanNegative}, []string{""}},
			{"duplicate ID", personalPlan{Kind: personalPlanNegative}, []string{"item", "item"}},
			{"missing required state", personalPlan{Kind: personalPlanPositive}, []string{"item"}},
		} {
			t.Run(test.name, func(t *testing.T) {
				fake := &personalPlanSourceMetadataFake{}
				store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
				source, auth := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
				resolved, err := source.resolveIDs(context.Background(), test.plan, personalPlanSourceSnapshot(nil), test.ids)
				if err == nil || resolved != nil || fake.calls != 0 || auth.ensure != 0 || len(store.saves) != 0 {
					t.Fatalf("resolved=%#v err=%v metadata=%d ensure=%d saves=%d", resolved, err, fake.calls, auth.ensure, len(store.saves))
				}
			})
		}
	})

	for _, test := range []struct {
		name string
		body string
	}{
		{"missing ID", `{"Items":[{"Name":"bad"}]}`},
		{"malformed ID", `{"Items":[{"Id":4}]}`},
		{"duplicate returned ID", `{"Items":[{"Id":"item"},{"Id":"item"}]}`},
		{"unrequested ID", `{"Items":[{"Id":"other"}]}`},
		{"malformed shape", `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: test.body, snapshot: personalPlanSourceUpstreamSnapshot()}
			store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
			source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
			resolved, err := source.resolveIDs(context.Background(), personalPlan{Kind: personalPlanNegative}, personalPlanSourceSnapshot(nil), []string{"item"})
			if err == nil || resolved != nil || len(store.saves) != 0 {
				t.Fatalf("resolved=%#v err=%v saves=%d", resolved, err, len(store.saves))
			}
		})
	}
}

func personalPlanSourceResolvedItems(ids []string) []resolvedPersonalItem {
	items := make([]resolvedPersonalItem, len(ids))
	for i, id := range ids {
		items[i] = resolvedPersonalItem{
			item:  map[string]any{"Id": id, "Name": "original-" + id},
			state: PlaybackState{ItemID: id, GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", PlayCount: i + 1},
		}
	}
	return items
}

func TestPersonalPlanSourceRefineResolvedStructuralOmissionDoesNotOrphan(t *testing.T) {
	item := map[string]any{"Id": "resume", "Type": "Movie", "UserId": "backend-user", "ServerId": "backend-server"}
	upstream := personalPlanSourceUpstreamSnapshot()
	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": []any{item}}), snapshot: upstream},
		{status: http.StatusOK, body: `{"Items":[]}`, snapshot: upstream},
	}}
	state := PlaybackState{ItemID: "resume", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", PlaybackPositionTicks: 1234, Fingerprint: itemFingerprint(item)}
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	plan := personalPlan{
		Kind:       personalPlanResume,
		Refinement: url.Values{"ParentId": {"parent"}, "MediaTypes": {"Video"}, "Filters": {"IsFolder,IsPlayed"}},
		Projection: url.Values{"Fields": {"Name,UserData"}},
		Sort:       []personalSortTerm{{Name: "DateCreated", Source: personalSortMetadata}},
	}
	resolved, err := source.resolveIDs(context.Background(), plan, personalPlanSourceSnapshot(map[string]PlaybackState{"resume": state}), []string{"resume"})
	if err != nil || len(resolved) != 1 || len(store.saves) != 1 || store.saves[0].OrphanedAt != nil {
		t.Fatalf("resolution=%#v saves=%#v err=%v", resolved, store.saves, err)
	}
	refined, err := source.refineResolved(context.Background(), plan, resolved)
	if err != nil || len(refined) != 0 || len(store.saves) != 1 || store.saves[0].OrphanedAt != nil {
		t.Fatalf("refinement=%#v saves=%#v err=%v", refined, store.saves, err)
	}
	neutralQuery := fake.requests[0].URL.Query()
	if neutralQuery.Get("Ids") != "resume" || neutralQuery.Get("Fields") != "Name,DateCreated" || neutralQuery.Get("ParentId") != "" || neutralQuery.Get("MediaTypes") != "" || neutralQuery.Get("Filters") != "" {
		t.Fatalf("neutral resolution query = %v", neutralQuery)
	}
	refinementQuery := fake.requests[1].URL.Query()
	if refinementQuery.Get("Ids") != "resume" || refinementQuery.Get("Fields") != "Name,DateCreated" || refinementQuery.Get("ParentId") != "parent" || refinementQuery.Get("MediaTypes") != "Video" || refinementQuery.Get("Filters") != "IsFolder" {
		t.Fatalf("structural refinement query = %v", refinementQuery)
	}
}

func TestPersonalPlanSourceRefineResolvedNoopCopiesWithoutEgress(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{}
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	source, auth := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	items := personalPlanSourceResolvedItems([]string{"one", "two"})
	refined, err := source.refineResolved(context.Background(), personalPlan{Refinement: url.Values{"Filters": {"IsPlayed,Unknown"}, "IsFavorite": {"true"}}}, items)
	if err != nil || fake.calls != 0 || auth.ensure != 0 || len(store.saves) != 0 || !reflect.DeepEqual(refined, items) {
		t.Fatalf("refined=%#v metadata=%d ensure=%d saves=%d err=%v", refined, fake.calls, auth.ensure, len(store.saves), err)
	}
	refined[0] = resolvedPersonalItem{}
	if items[0].item == nil {
		t.Fatal("no-op refinement returned an aliasing slice")
	}
}

func TestPersonalPlanSourceRefineResolvedBatchesAndPreservesOrder(t *testing.T) {
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf("refine-%03d", i)
	}
	upstream := personalPlanSourceUpstreamSnapshot()
	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": personalPlanSourceItems(ids[:200], true)}), snapshot: upstream},
		{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": personalPlanSourceItems(ids[200:], true)}), snapshot: upstream},
	}}
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	items := personalPlanSourceResolvedItems(ids)
	plan := personalPlan{Refinement: url.Values{"ParentId": {"parent"}, "IncludeItemTypes": {"Movie"}}, Projection: url.Values{"Fields": {"Name"}}}
	refined, err := source.refineResolved(context.Background(), plan, items)
	if err != nil || fake.calls != 2 || len(refined) != 201 || len(store.saves) != 0 || store.findCalls != 0 {
		t.Fatalf("refined=%d metadata=%d saves=%d finds=%d err=%v", len(refined), fake.calls, len(store.saves), store.findCalls, err)
	}
	if got := strings.Split(fake.requests[0].URL.Query().Get("Ids"), ","); len(got) != 200 || !reflect.DeepEqual(got, ids[:200]) {
		t.Fatalf("first refinement batch = %d ordered=%v", len(got), reflect.DeepEqual(got, ids[:200]))
	}
	if got := strings.Split(fake.requests[1].URL.Query().Get("Ids"), ","); !reflect.DeepEqual(got, ids[200:]) {
		t.Fatalf("second refinement batch = %v", got)
	}
	for i, joined := range refined {
		id, ok := personalItemID(joined.item)
		if !ok || id != ids[i] || joined.state.PlayCount != items[i].state.PlayCount {
			t.Fatalf("refined[%d] = %#v state=%#v", i, joined.item, joined.state)
		}
	}
}

func TestPersonalPlanSourceRefineResolvedRejectsBatchFailures(t *testing.T) {
	items := personalPlanSourceResolvedItems([]string{"one", "two"})
	for _, test := range []struct {
		name     string
		response personalPlanSourceMetadataResponse
	}{
		{"transport", personalPlanSourceMetadataResponse{err: errors.New("offline")}},
		{"non-2xx", personalPlanSourceMetadataResponse{status: http.StatusBadGateway, body: `{"Items":[]}`}},
		{"malformed shape", personalPlanSourceMetadataResponse{status: http.StatusOK, body: `[]`}},
		{"malformed ID", personalPlanSourceMetadataResponse{status: http.StatusOK, body: `{"Items":[{"Name":"bad"}]}`}},
		{"duplicate ID", personalPlanSourceMetadataResponse{status: http.StatusOK, body: `{"Items":[{"Id":"one"},{"Id":"one"}]}`}},
		{"unrequested ID", personalPlanSourceMetadataResponse{status: http.StatusOK, body: `{"Items":[{"Id":"other"}]}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.response.snapshot = personalPlanSourceUpstreamSnapshot()
			fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{test.response}}
			store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
			source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
			refined, err := source.refineResolved(context.Background(), personalPlan{Refinement: url.Values{"ParentId": {"parent"}}}, items)
			if err == nil || refined != nil || len(store.saves) != 0 {
				t.Fatalf("refined=%#v saves=%d err=%v", refined, len(store.saves), err)
			}
		})
	}
}

func TestPersonalPlanSourceRefineResolvedSecondBatchFailureReturnsNoPartialItems(t *testing.T) {
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf("refine-%03d", i)
	}
	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": personalPlanSourceItems(ids[:200], false)}), snapshot: personalPlanSourceUpstreamSnapshot()},
		{err: errors.New("offline")},
	}}
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	refined, err := source.refineResolved(context.Background(), personalPlan{Refinement: url.Values{"Recursive": {"true"}}}, personalPlanSourceResolvedItems(ids))
	if err == nil || refined != nil || fake.calls != 2 || len(store.saves) != 0 {
		t.Fatalf("refined=%#v metadata=%d saves=%d err=%v", refined, fake.calls, len(store.saves), err)
	}
}

func TestPersonalPlanSourceLatestQueryOmitsStartIndex(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `[{"Id":"latest","UserId":"latest-user","ServerId":"latest-server"}]`, snapshot: upstreamRequestSnapshot{baseURL: "https://latest.test/emby", userID: "latest-user", serverID: "latest-server", token: "latest-token"}}
	source, _ := newPersonalPlanSourceTestSource(t, fake)
	plan := personalPlan{Route: personalRouteLatest, Shape: personalShapeArray, Path: "/Users/synthetic-user/Items", Neutral: url.Values{"ParentId": {"parent"}}}
	page, err := source.fetchCandidatePage(context.Background(), plan, 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0]["UserId"] != "synthetic-user" || page.Items[0]["ServerId"] != "gateway-server" {
		t.Fatalf("Latest page = %#v", page)
	}
	if fake.request.URL.Query().Get("StartIndex") != "" || fake.request.URL.Query().Get("Limit") != "7" {
		t.Fatalf("emitted query = %v", fake.request.URL.Query())
	}
	if _, err := source.fetchCandidatePage(context.Background(), plan, 1, 7); err == nil {
		t.Fatal("accepted nonzero Latest start")
	}
}

func TestPersonalPlanSourceCandidateProjectionAndSortFields(t *testing.T) {
	for _, test := range []struct {
		name       string
		plan       personalPlan
		body       string
		required   []string
		outputKeys []string
	}{
		{
			name: "negative",
			plan: personalPlan{
				Kind: personalPlanNegative, Shape: personalShapeQueryResult, Path: "/Users/synthetic-user/Items",
				Neutral:    url.Values{"Filters": {"IsFolder,IsPlayed"}, "EnableUserData": {"true"}, "SortBy": {"PlayCount"}},
				Projection: url.Values{"Fields": {"Overview,UserData", "overview"}},
				Sort:       []personalSortTerm{{Name: "DateCreated", Source: personalSortMetadata}},
			},
			body:       `{"Items":[{"Id":"negative","Overview":"summary","DateCreated":"2026-01-01"}]}`,
			required:   []string{"Overview", "DateCreated"},
			outputKeys: []string{"Overview", "DateCreated"},
		},
		{
			name: "ungrouped Latest",
			plan: personalPlan{
				Kind: personalPlanLatest, Route: personalRouteLatest, Shape: personalShapeArray, Path: "/Users/synthetic-user/Items",
				Neutral:    url.Values{"GroupItems": {"false"}},
				Projection: url.Values{"Fields": {"Overview,USERDATA"}},
				Sort:       []personalSortTerm{{Name: "PremiereDate", Source: personalSortMetadata}},
			},
			body:       `[{"Id":"latest","Overview":"summary","PremiereDate":"2026-01-01"}]`,
			required:   []string{"Overview", "PremiereDate"},
			outputKeys: []string{"Overview", "PremiereDate"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: test.body, snapshot: personalPlanSourceUpstreamSnapshot()}
			source, _ := newPersonalPlanSourceTestSource(t, fake)
			page, err := source.fetchCandidatePage(context.Background(), test.plan, 0, 20)
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("page=%#v err=%v", page, err)
			}
			query := fake.request.URL.Query()
			fields := personalPlanSourceFieldSet(splitFilterValues(query["Fields"]))
			for _, field := range test.required {
				if !fields[strings.ToLower(field)] {
					t.Fatalf("Fields %q missing %q", query.Get("Fields"), field)
				}
			}
			if fields["userdata"] || query.Get("EnableUserData") != "" || query.Get("SortBy") != "" || query.Get("SortOrder") != "" || query.Get("IsPlayed") != "" || query.Get("api_key") != "" {
				t.Fatalf("unsafe candidate query = %v", query)
			}
			for _, key := range test.outputKeys {
				if _, exists := page.Items[0][key]; !exists {
					t.Fatalf("candidate output lost %q: %#v", key, page.Items[0])
				}
			}
		})
	}
}

func TestPersonalPlanSourceCandidateRetainsNextUpInternalFields(t *testing.T) {
	required := []string{"Type", "SeriesId", "SeasonId", "ParentIndexNumber", "IndexNumber"}
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"episode","Type":"Episode","SeriesId":"series","SeasonId":"season","ParentIndexNumber":1,"IndexNumber":2,"Overview":"summary","PremiereDate":"2026-01-01"}]}`, snapshot: personalPlanSourceUpstreamSnapshot()}
	source, _ := newPersonalPlanSourceTestSource(t, fake)
	plan := personalPlan{
		Route: personalRouteNextUp, Shape: personalShapeQueryResult, Path: "/Shows/series/Episodes",
		Neutral:    url.Values{"Fields": {"Type,SeriesId,SeasonId,ParentIndexNumber,IndexNumber,UserData"}, "IncludeItemTypes": {"Episode"}},
		Projection: url.Values{"Fields": {"Overview,userdata"}},
		Sort:       []personalSortTerm{{Name: "PremiereDate", Source: personalSortMetadata}},
	}
	page, err := source.fetchCandidatePage(context.Background(), plan, 0, 20)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	query := fake.request.URL.Query()
	fields := personalPlanSourceFieldSet(splitFilterValues(query["Fields"]))
	for _, field := range append(required, "Overview", "PremiereDate") {
		if !fields[strings.ToLower(field)] {
			t.Fatalf("Fields %q missing %q", query.Get("Fields"), field)
		}
		if _, exists := page.Items[0][field]; !exists {
			t.Fatalf("candidate output lost %q: %#v", field, page.Items[0])
		}
	}
	if fields["userdata"] {
		t.Fatalf("Fields retained UserData: %q", query.Get("Fields"))
	}
}

func TestPersonalPlanSourceStrictCandidateShapes(t *testing.T) {
	start, total := 3, 7
	items, returnedStart, returnedTotal, err := strictPersonalPage(map[string]any{
		"Items": []any{map[string]any{"Id": "a"}}, "StartIndex": float64(start), "TotalRecordCount": float64(total),
	}, personalShapeQueryResult, start)
	if err != nil || len(items) != 1 || returnedStart == nil || *returnedStart != start || returnedTotal == nil || *returnedTotal != total {
		t.Fatalf("items=%#v start=%v total=%v err=%v", items, returnedStart, returnedTotal, err)
	}
	items, returnedStart, returnedTotal, err = strictPersonalPage([]any{map[string]any{"Id": "latest"}}, personalShapeArray, 0)
	if err != nil || len(items) != 1 || returnedStart != nil || returnedTotal != nil {
		t.Fatalf("Latest items=%#v start=%v total=%v err=%v", items, returnedStart, returnedTotal, err)
	}
	for _, malformed := range []any{
		[]any{},
		map[string]any{},
		map[string]any{"Items": map[string]any{}},
		map[string]any{"Items": []any{nil}},
		map[string]any{"Items": []any{}, "StartIndex": "0"},
		map[string]any{"Items": []any{}, "TotalRecordCount": 1.5},
	} {
		if _, _, _, err := strictPersonalPage(malformed, personalShapeQueryResult, 0); err == nil {
			t.Fatalf("accepted malformed QueryResult %#v", malformed)
		}
	}
}

func TestPersonalPlanSourceResolutionQueryHasNoPersonalEgress(t *testing.T) {
	plan := personalPlan{
		Refinement: map[string][]string{"ParentId": {"parent"}, "Filters": {"IsFolder,IsPlayed,CustomStructural"}},
		Projection: map[string][]string{"Fields": {"Name,UserData", "DateCreated"}, "EnableUserDatas": {"true"}, "IsFavorite": {"true"}},
		Sort:       []personalSortTerm{{Name: "DateCreated"}, {Name: "PlayCount", Source: personalSortLocal}},
	}
	q := resolutionQueryForPlan(plan)
	if q.Get("ParentId") != "" || q.Get("Filters") != "" || q.Get("EnableUserData") != "" || q.Get("EnableUserDatas") != "" || q.Get("SortBy") != "" {
		t.Fatalf("resolution query = %v", q)
	}
	if got := q.Get("Fields"); got != "Name,DateCreated" {
		t.Fatalf("Fields = %q", got)
	}
}

func TestPersonalPlanSourceResolutionQueryCleansCaseAndRepeatedLists(t *testing.T) {
	q := resolutionQueryForPlan(personalPlan{
		Refinement: url.Values{
			"fIlTeRs":  {"IsNotFolder,LIKES", "Custom,disLIKES"},
			"isPLAYED": {"true"}, "sortBY": {"PlayCount"}, "SORTorder": {"Descending"},
		},
		Projection: url.Values{
			"fIeLdS":         {"Name,userdata", "USERDATA,DateCreated"},
			"eNaBlEuSeRdAtA": {"true"}, "ENABLEUSERDATAS": {"true"},
		},
	})
	if q.Get("Filters") != "" || q.Get("Fields") != "Name,DateCreated" {
		t.Fatalf("resolution query = %v", q)
	}
	for _, key := range []string{"isPLAYED", "sortBY", "SORTorder", "eNaBlEuSeRdAtA", "ENABLEUSERDATAS"} {
		if _, exists := q[key]; exists {
			t.Fatalf("personal key %q survived: %v", key, q)
		}
	}
}

func TestOptionalJSONIntRejectsInvalidPlatformValues(t *testing.T) {
	tooLarge := math.Ldexp(1, strconv.IntSize-1)
	for _, test := range []struct {
		name  string
		value any
		valid bool
	}{
		{"zero", float64(0), true}, {"positive", float64(4), true}, {"negative", float64(-1), false},
		{"fractional", float64(1.5), false}, {"string", "1", false}, {"nan", math.NaN(), false},
		{"inf", math.Inf(1), false}, {"too large", tooLarge, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := optionalJSONInt(map[string]any{"Value": test.value}, "Value")
			if (err == nil) != test.valid || (test.valid && (got == nil || *got != int(test.value.(float64)))) {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
}

func TestPersonalPlanSourceCandidateUsesReturnedSnapshotAndRejectsResponses(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{status: http.StatusOK, body: `{"Items":[{"Id":"item","UserId":"returned-user","ServerId":"returned-server"}],"StartIndex":0,"TotalRecordCount":1}`, snapshot: upstreamRequestSnapshot{baseURL: "https://returned.test/emby", userID: "returned-user", serverID: "returned-server", token: "returned-token"}}
	source, _ := newPersonalPlanSourceTestSource(t, fake)
	plan, err := parsePersonalPlan(personalRouteItems, "/Users/synthetic-user/Items", url.Values{
		"Filters": {"IsPlayed,IsFolder"}, "IsFavorite": {"true"}, "SortBy": {"DateCreated"},
		"Fields": {"Name,UserData"}, "EnableUserData": {"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := source.fetchCandidatePage(context.Background(), plan, 0, 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("page=%#v err=%v", page, err)
	}
	if page.Items[0]["UserId"] != "synthetic-user" || page.Items[0]["ServerId"] != "gateway-server" {
		t.Fatalf("identity rewrite = %#v", page.Items[0])
	}
	query := fake.request.URL.Query()
	if query.Get("Filters") != "IsFolder" || query.Get("IsFavorite") != "" || query.Get("SortBy") != "" || query.Get("Fields") != "Name,DateCreated" || query.Get("EnableUserData") != "" {
		t.Fatalf("personal criteria escaped candidate egress: %v", query)
	}
	for _, test := range []struct {
		name, body string
		status     int
		transport  error
	}{
		{"malformed item", `{"Items":[null]}`, 200, nil},
		{"bad start", `{"Items":[],"StartIndex":1}`, 200, nil},
		{"bad total", `{"Items":[],"TotalRecordCount":1.5}`, 200, nil},
		{"non2xx", `{"Items":[]}`, 503, nil},
		{"transport", ``, 0, errors.New("offline")},
		{"json", `{`, 200, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake.body, fake.status, fake.err = test.body, test.status, test.transport
			if _, err := source.fetchCandidatePage(context.Background(), plan, 0, 10); err == nil {
				t.Fatal("accepted malformed candidate response")
			}
		})
	}
}
