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
)

func TestLatestPrefixValidation(t *testing.T) {
	previous := []map[string]any{{"Id": "a"}, {"Id": "b"}}
	if err := validateLatestPrefix(previous, []map[string]any{{"Id": "a"}, {"Id": "b"}, {"Id": "c"}}); err != nil {
		t.Fatal(err)
	}
	if err := validateLatestPrefix(previous, []map[string]any{{"Id": "b"}, {"Id": "a"}}); err == nil {
		t.Fatal("accepted prefix drift")
	}
	if err := validateLatestPrefix(nil, []map[string]any{{"Id": "a"}, {"Id": "a"}}); err == nil {
		t.Fatal("accepted duplicate IDs")
	}
}

func TestLatestNextFetchLimit(t *testing.T) {
	if got := latestNextFetchLimit(20); got != 40 {
		t.Fatalf("next limit = %d", got)
	}
	if got := latestNextFetchLimit(latestPersonalScanLimit); got != latestPersonalScanLimit {
		t.Fatalf("at-bound next = %d", got)
	}
	if got := latestNextFetchLimit(latestPersonalScanLimit - 1); got != latestPersonalScanLimit {
		t.Fatalf("near-bound next = %d", got)
	}
	if got := latestNextFetchLimit(math.MaxInt); got != latestPersonalScanLimit {
		t.Fatalf("maxint next = %d", got)
	}
}

func TestClampLatestOutboundLimitBounds(t *testing.T) {
	got, err := clampLatestOutboundLimit(math.MaxInt, nil)
	if err != nil || got != latestPersonalScanLimit {
		t.Fatalf("nil budget clamp = %d err=%v", got, err)
	}
	budget := newLatestGroupedBudget()
	budget.candidates = latestGroupedBudgetCandidates - 7
	got, err = clampLatestOutboundLimit(100, budget)
	if err != nil || got != 7 {
		t.Fatalf("remaining budget clamp = %d err=%v", got, err)
	}
	budget.candidates = latestGroupedBudgetCandidates
	if _, err := clampLatestOutboundLimit(1, budget); !errors.Is(err, ErrPersonalScanIncomplete) {
		t.Fatalf("exhausted budget err=%v", err)
	}
	if _, err := clampLatestOutboundLimit(0, nil); err == nil {
		t.Fatal("accepted zero want")
	}
}

func TestLatestNeededWithinBudget(t *testing.T) {
	if needed, ok := latestNeededWithinBudget(0, 20); !ok || needed != 20 {
		t.Fatalf("ordinary needed=%d ok=%v", needed, ok)
	}
	if _, ok := latestNeededWithinBudget(0, latestPersonalScanLimit+1); ok {
		t.Fatal("accepted limit above scan budget")
	}
	if _, ok := latestNeededWithinBudget(0, math.MaxInt); ok {
		t.Fatal("accepted maxint limit")
	}
	if _, ok := latestNeededWithinBudget(math.MaxInt, 1); ok {
		t.Fatal("accepted start+limit overflow")
	}
	if _, ok := latestNeededWithinBudget(math.MaxInt-1, 2); ok {
		t.Fatal("accepted overflowing start+limit")
	}
}

func latestTestPlan(limit int, grouped bool) personalPlan {
	return personalPlan{
		Kind: personalPlanLatest, Route: personalRouteLatest, Shape: personalShapeArray,
		Path: "/Users/synthetic-user/Items", Neutral: url.Values{"GroupItems": {"false"}},
		Predicates: personalPredicates{Played: personalTruthFalse},
		Group:      personalGroupSpec{Items: grouped}, Page: personalPageSpec{Limit: &limit},
		Sort: []personalSortTerm{{Name: "LatestRank", Source: personalSortMetadata}},
	}
}

func latestTestSource(t *testing.T, store Store, responses ...any) (*personalPlanSource, *personalPlanSourceMetadataFake) {
	t.Helper()
	fakeResponses := make([]personalPlanSourceMetadataResponse, len(responses))
	for i, response := range responses {
		fakeResponses[i] = personalPlanSourceMetadataResponse{
			status: http.StatusOK, body: personalPlanSourceJSON(t, response),
			snapshot: personalPlanSourceUpstreamSnapshot(),
		}
	}
	fake := &personalPlanSourceMetadataFake{responses: fakeResponses}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	return source, fake
}

func TestExecuteLatestDefaultFiltersAndHasNoTotal(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "played", Played: true}}}
	plan, err := parsePersonalPlan(personalRouteLatest, "/Latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	source, fake := latestTestSource(t, store, []any{map[string]any{"Id": "played", "Type": "Movie"}, map[string]any{"Id": "new", "Type": "Movie"}})
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"new"}) || result.Total != nil || result.StartIndex != 0 || fake.calls != 1 || store.listCalls != 1 {
		t.Fatalf("ids=%v total=%v start=%d metadata=%d state=%d", got, result.Total, result.StartIndex, fake.calls, store.listCalls)
	}
}

func TestExecuteLatestZeroAndInvalidPlansDoNoWork(t *testing.T) {
	zero := 0
	result, err := executeLatestPersonalPlan(context.Background(), nil, personalPlan{Kind: personalPlanLatest, Shape: personalShapeArray, Page: personalPageSpec{Limit: &zero}})
	if err != nil || result.Items == nil || len(result.Items) != 0 || result.Total != nil {
		t.Fatalf("zero result=%#v err=%v", result, err)
	}
	for _, plan := range []personalPlan{
		{Kind: personalPlanLatest, Shape: personalShapeArray, Page: personalPageSpec{Limit: ptrInt(1)}},
		{Kind: personalPlanNegative, Shape: personalShapeArray, Page: personalPageSpec{Limit: ptrInt(1)}},
		{Kind: personalPlanLatest, Shape: personalShapeQueryResult, Page: personalPageSpec{Limit: ptrInt(1)}},
	} {
		if _, err := executeLatestPersonalPlan(context.Background(), nil, plan); err == nil {
			t.Fatalf("accepted invalid plan %#v", plan)
		}
	}
}

func TestExecuteLatestUngroupedBackfillsAndStops(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "a", Played: true}, {ItemID: "b", Played: true}}}
	plan := latestTestPlan(2, false)
	source, fake := latestTestSource(t, store,
		[]any{map[string]any{"Id": "a"}, map[string]any{"Id": "b"}},
		[]any{map[string]any{"Id": "a"}, map[string]any{"Id": "b"}, map[string]any{"Id": "c"}, map[string]any{"Id": "d"}},
	)
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"c", "d"}) || fake.calls != 2 {
		t.Fatalf("ids=%v calls=%d", got, fake.calls)
	}
}

func TestExecuteLatestUngroupedTerminalAndPrefixDrift(t *testing.T) {
	terminalSource, _ := latestTestSource(t, &personalPlanSourceStore{MemoryStore: NewMemoryStore()},
		[]any{map[string]any{"Id": "only"}},
	)
	result, err := executeLatestPersonalPlan(context.Background(), terminalSource, latestTestPlan(3, false))
	if err != nil || !reflect.DeepEqual(latestResultIDs(result), []string{"only"}) {
		t.Fatalf("terminal ids=%v err=%v", latestResultIDs(result), err)
	}

	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "a", Played: true}, {ItemID: "b", Played: true}}}
	driftSource, _ := latestTestSource(t, store,
		[]any{map[string]any{"Id": "a"}, map[string]any{"Id": "b"}},
		[]any{map[string]any{"Id": "b"}, map[string]any{"Id": "a"}, map[string]any{"Id": "c"}},
	)
	if _, err := executeLatestPersonalPlan(context.Background(), driftSource, latestTestPlan(2, false)); err == nil {
		t.Fatal("accepted drifting Latest prefix")
	}
}

func TestExecuteLatestGroupsAfterFilteringAndResolvesParent(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "old", Played: true}}}
	plan := latestTestPlan(20, true)
	source, fake := latestTestSource(t, store,
		[]any{
			map[string]any{"Id": "old", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "ep-1", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "ep-2", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "movie", "Type": "Movie"},
		},
		map[string]any{"Items": []any{map[string]any{"Id": "show", "Type": "Series", "ChildCount": 99, "Count": 88, "RecursiveItemCount": 77}}},
		// ParentId-scoped exact ChildCount for show (includes played old; local filter drops it).
		[]any{
			map[string]any{"Id": "old", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "ep-1", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "ep-2", "Type": "Episode", "SeriesId": "show"},
		},
	)
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"show", "movie"}) {
		t.Fatalf("ids=%v", got)
	}
	series := result.Items[0].item
	if series["ChildCount"] != 2 || series["Count"] != nil || series["RecursiveItemCount"] != nil || series[latestSyntheticMarker] != nil || series[latestSyntheticRank] != nil {
		t.Fatalf("series=%#v", series)
	}
	if fake.calls != 3 {
		t.Fatalf("metadata calls=%d, want discovery+parent+childcount", fake.calls)
	}
	if got := fake.requests[2].URL.Query().Get("ParentId"); got != "show" {
		t.Fatalf("child-count ParentId=%q", got)
	}
	if fake.requests[2].URL.Query().Get("GroupItems") != "false" {
		t.Fatalf("child-count GroupItems=%q", fake.requests[2].URL.Query().Get("GroupItems"))
	}
}

func TestExecuteLatestRejectsGroupedMalformedAndParentFailures(t *testing.T) {
	for _, candidate := range []map[string]any{{"Id": ""}, {"Id": "bad/id"}, {"Id": "same"}, {"Id": "same"}} {
		store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
		source, _ := latestTestSource(t, store, []any{candidate})
		if candidate["Id"] == "same" {
			source, _ = latestTestSource(t, store, []any{map[string]any{"Id": "same"}, map[string]any{"Id": "same"}})
		}
		if _, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(1, true)); err == nil {
			t.Fatalf("accepted malformed grouped candidate %#v", candidate)
		}
	}
	for _, parent := range []any{map[string]any{"Items": []any{}}, map[string]any{"Items": []any{map[string]any{"Id": "show", "Type": "Movie"}}}} {
		store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
		source, _ := latestTestSource(t, store,
			[]any{map[string]any{"Id": "ep", "Type": "Episode", "SeriesId": "show"}}, parent)
		if _, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(1, true)); err == nil {
			t.Fatalf("accepted bad parent %#v", parent)
		}
	}
}

func TestExecuteLatestRejectsUngroupedUnsafeIDAndBadSeriesID(t *testing.T) {
	unsafeSource, _ := latestTestSource(t, &personalPlanSourceStore{MemoryStore: NewMemoryStore()},
		[]any{map[string]any{"Id": "bad/id", "Type": "Movie"}},
	)
	if _, err := executeLatestPersonalPlan(context.Background(), unsafeSource, latestTestPlan(2, false)); err == nil {
		t.Fatal("accepted unsafe ungrouped ID")
	}
	for _, seriesID := range []any{"", "bad/id", 42} {
		source, _ := latestTestSource(t, &personalPlanSourceStore{MemoryStore: NewMemoryStore()},
			[]any{map[string]any{"Id": "episode", "Type": "Episode", "SeriesId": seriesID}},
		)
		if _, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(2, true)); err == nil {
			t.Fatalf("accepted SeriesId %#v", seriesID)
		}
	}
}

func TestExecuteLatestReturnsParentResolutionFailure(t *testing.T) {
	fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
		{status: http.StatusOK, body: `[{"Id":"episode","Type":"Episode","SeriesId":"show"}]`, snapshot: personalPlanSourceUpstreamSnapshot()},
		{err: errors.New("parent offline")},
	}}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, &personalPlanSourceStore{MemoryStore: NewMemoryStore()})
	if _, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(2, true)); err == nil || err.Error() != "parent offline" {
		t.Fatalf("parent error=%v", err)
	}
}

func TestExecuteLatestGroupedStaleParentOmissionBackfillsLimit(t *testing.T) {
	// Stale first series is omitted by a successful parent response; later valid
	// series backfill the requested Limit after omission and exact ChildCount.
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	plan := latestTestPlan(1, true)
	source, _ := latestTestSource(t, store,
		[]any{
			map[string]any{"Id": "ep-stale", "Type": "Episode", "SeriesId": "stale-show"},
			map[string]any{"Id": "ep-valid", "Type": "Episode", "SeriesId": "valid-show"},
			map[string]any{"Id": "ep-later", "Type": "Episode", "SeriesId": "later-show"},
		},
		map[string]any{"Items": []any{
			map[string]any{"Id": "valid-show", "Type": "Series", "ChildCount": 9},
			map[string]any{"Id": "later-show", "Type": "Series"},
		}},
		[]any{map[string]any{"Id": "ep-valid", "Type": "Episode", "SeriesId": "valid-show"}},
	)
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"valid-show"}) {
		t.Fatalf("ids=%v, want valid-show backfilling Limit=1 after stale omission", got)
	}
	if result.Items[0].item["ChildCount"] != 1 {
		t.Fatalf("ChildCount=%v", result.Items[0].item["ChildCount"])
	}

	planTwo := latestTestPlan(2, true)
	sourceTwo, _ := latestTestSource(t, store,
		[]any{
			map[string]any{"Id": "ep-stale", "Type": "Episode", "SeriesId": "stale-show"},
			map[string]any{"Id": "ep-valid", "Type": "Episode", "SeriesId": "valid-show"},
			map[string]any{"Id": "ep-later", "Type": "Episode", "SeriesId": "later-show"},
		},
		map[string]any{"Items": []any{
			map[string]any{"Id": "valid-show", "Type": "Series"},
			map[string]any{"Id": "later-show", "Type": "Series"},
		}},
		[]any{map[string]any{"Id": "ep-valid", "Type": "Episode", "SeriesId": "valid-show"}},
		[]any{map[string]any{"Id": "ep-later", "Type": "Episode", "SeriesId": "later-show"}},
	)
	resultTwo, err := executeLatestPersonalPlan(context.Background(), sourceTwo, planTwo)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(resultTwo); !reflect.DeepEqual(got, []string{"valid-show", "later-show"}) {
		t.Fatalf("ids=%v", got)
	}
}

func TestExecuteLatestGroupedAllMissingParentsIsFatal(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	source, _ := latestTestSource(t, store,
		[]any{
			map[string]any{"Id": "ep-a", "Type": "Episode", "SeriesId": "show-a"},
			map[string]any{"Id": "ep-b", "Type": "Episode", "SeriesId": "show-b"},
		},
		map[string]any{"Items": []any{}},
	)
	if _, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(2, true)); err == nil {
		t.Fatal("accepted all-missing parent resolution as empty 200")
	}
}

func TestExecuteLatestGroupedParentStructuralFailuresRemainFatal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent any
	}{
		{"wrong type", map[string]any{"Items": []any{map[string]any{"Id": "show", "Type": "Movie"}}}},
		{"unrequested", map[string]any{"Items": []any{map[string]any{"Id": "other", "Type": "Series"}}}},
		{"duplicate", map[string]any{"Items": []any{
			map[string]any{"Id": "show", "Type": "Series"},
			map[string]any{"Id": "show", "Type": "Series"},
		}}},
		{"malformed id", map[string]any{"Items": []any{map[string]any{"Id": "", "Type": "Series"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
			source, _ := latestTestSource(t, store,
				[]any{map[string]any{"Id": "ep", "Type": "Episode", "SeriesId": "show"}},
				tc.parent,
			)
			if _, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(1, true)); err == nil {
				t.Fatalf("accepted %s parent result", tc.name)
			}
		})
	}

	t.Run("parent batch non-2xx", func(t *testing.T) {
		fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
			{status: http.StatusOK, body: `[{"Id":"episode","Type":"Episode","SeriesId":"show"}]`, snapshot: personalPlanSourceUpstreamSnapshot()},
			{status: http.StatusInternalServerError, body: `{"error":"upstream"}`, snapshot: personalPlanSourceUpstreamSnapshot()},
		}}
		source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, &personalPlanSourceStore{MemoryStore: NewMemoryStore()})
		if _, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(1, true)); err == nil {
			t.Fatal("accepted parent batch non-2xx")
		}
	})
}

func TestExecuteLatestGroupedCustomSortScanBudgetRemainsIncomplete(t *testing.T) {
	items := make([]any, latestPersonalScanLimit)
	for i := range items {
		items[i] = map[string]any{"Id": fmt.Sprintf("ep-%05d", i), "Type": "Episode", "SeriesId": "show", "Name": "same"}
	}
	plan := latestTestPlan(1, true)
	plan.Sort = []personalSortTerm{{Name: "Name", Source: personalSortMetadata}}
	source, fake := latestTestSource(t, &personalPlanSourceStore{MemoryStore: NewMemoryStore()}, items)
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if !errors.Is(err, ErrPersonalScanIncomplete) || result.Items != nil || fake.calls != 1 {
		t.Fatalf("result=%#v calls=%d err=%v", result, fake.calls, err)
	}
}

func TestExecuteLatestGroupedIsolatesTwoUsersSharingBackend(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	userA := &Session{GatewayUserID: "user-a", SyntheticUserID: "synthetic-a"}
	userB := &Session{GatewayUserID: "user-b", SyntheticUserID: "synthetic-b"}
	for _, state := range []PlaybackState{
		{GatewayUserID: userA.GatewayUserID, SyntheticUserID: userA.SyntheticUserID, ItemID: "ep-1", Played: true},
		{GatewayUserID: userB.GatewayUserID, SyntheticUserID: userB.SyntheticUserID, ItemID: "ep-1", Played: true},
		{GatewayUserID: userB.GatewayUserID, SyntheticUserID: userB.SyntheticUserID, ItemID: "ep-2", Played: true},
	} {
		if err := store.SavePlaybackState(ctx, state); err != nil {
			t.Fatal(err)
		}
	}
	candidates := []any{
		map[string]any{"Id": "ep-1", "Type": "Episode", "SeriesId": "show"},
		map[string]any{"Id": "ep-2", "Type": "Episode", "SeriesId": "show"},
		map[string]any{"Id": "ep-3", "Type": "Episode", "SeriesId": "show"},
	}
	parent := map[string]any{"Items": []any{map[string]any{"Id": "show", "Type": "Series", "ChildCount": 99}}}
	scopedChildren := []any{
		map[string]any{"Id": "ep-1", "Type": "Episode", "SeriesId": "show"},
		map[string]any{"Id": "ep-2", "Type": "Episode", "SeriesId": "show"},
		map[string]any{"Id": "ep-3", "Type": "Episode", "SeriesId": "show"},
	}

	run := func(t *testing.T, session *Session) personalPlanResult {
		t.Helper()
		plan := latestTestPlan(5, true)
		plan.Path = "/Users/" + session.SyntheticUserID + "/Items"
		fake := &personalPlanSourceMetadataFake{responses: []personalPlanSourceMetadataResponse{
			{status: http.StatusOK, body: personalPlanSourceJSON(t, candidates), snapshot: personalPlanSourceUpstreamSnapshot()},
			{status: http.StatusOK, body: personalPlanSourceJSON(t, parent), snapshot: personalPlanSourceUpstreamSnapshot()},
			{status: http.StatusOK, body: personalPlanSourceJSON(t, scopedChildren), snapshot: personalPlanSourceUpstreamSnapshot()},
		}}
		server := NewServer(Config{GatewayServerID: "gateway-server", PublicBaseURL: "https://gateway.test/emby"}, store)
		server.managedAuthUpstream = &phase5AuthSpy{runtime: managedRuntime("old-token")}
		server.metadataUpstream = fake
		request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Users/"+session.SyntheticUserID+"/Items/Latest", nil)
		source, err := newPersonalPlanSource(server, request, session, "gateway-token")
		if err != nil {
			t.Fatal(err)
		}
		result, err := executeLatestPersonalPlan(ctx, source, plan)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	resultA := run(t, userA)
	resultB := run(t, userB)
	if !reflect.DeepEqual(latestResultIDs(resultA), []string{"show"}) || resultA.Items[0].item["ChildCount"] != 2 {
		t.Fatalf("user A ids=%v child=%v", latestResultIDs(resultA), resultA.Items[0].item["ChildCount"])
	}
	if !reflect.DeepEqual(latestResultIDs(resultB), []string{"show"}) || resultB.Items[0].item["ChildCount"] != 1 {
		t.Fatalf("user B ids=%v child=%v", latestResultIDs(resultB), resultB.Items[0].item["ChildCount"])
	}
}

func TestLatestSyntheticMarkerCannotCollide(t *testing.T) {
	if _, err := attachLatestRanks([]map[string]any{{"Id": "x", "__PERSONALLATESTSYNTHETIC": true}}); err == nil {
		t.Fatal("accepted reserved marker")
	}
}

// latestCatalogFake serves Limit-aware ungrouped Latest discovery, Ids parent
// resolution, and ParentId-scoped child counts from one catalog.
type latestCatalogFake struct {
	catalog   []map[string]any
	parents   map[string]map[string]any
	calls     int
	requests  []*http.Request
	snapshot  upstreamRequestSnapshot
	maxReturn int // optional hard cap simulating nonterminal library size
}

func (f *latestCatalogFake) RoundTripMetadata(in metadataUpstreamRequest) (*http.Response, error) {
	f.calls++
	f.requests = append(f.requests, in.Request)
	if in.SnapshotRef != nil {
		*in.SnapshotRef = f.snapshot
		if f.snapshot == (upstreamRequestSnapshot{}) {
			*in.SnapshotRef = personalPlanSourceUpstreamSnapshot()
		}
	}
	q := in.Request.URL.Query()
	limit := 20
	if raw := q.Get("Limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	var body any
	if ids := q.Get("Ids"); ids != "" {
		items := make([]any, 0)
		for _, id := range strings.Split(ids, ",") {
			if parent, ok := f.parents[id]; ok {
				items = append(items, parent)
			}
		}
		body = map[string]any{"Items": items}
	} else if parentID := q.Get("ParentId"); parentID != "" {
		items := make([]any, 0)
		for _, item := range f.catalog {
			if personalStringField(item, "SeriesId") == parentID {
				items = append(items, item)
			}
		}
		if len(items) > limit {
			items = items[:limit]
		}
		body = items
	} else {
		end := limit
		if f.maxReturn > 0 && end > f.maxReturn {
			end = f.maxReturn
		}
		if end > len(f.catalog) {
			end = len(f.catalog)
		}
		items := make([]any, end)
		for i := 0; i < end; i++ {
			items[i] = f.catalog[i]
		}
		body = items
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header), Request: in.Request}, nil
}

func latestTestSourceWithUpstream(t *testing.T, store Store, upstream MetadataUpstream) *personalPlanSource {
	t.Helper()
	if store == nil {
		store = NewMemoryStore()
	}
	server := NewServer(Config{GatewayServerID: "gateway-server", PublicBaseURL: "https://gateway.test/emby"}, store)
	server.managedAuthUpstream = &phase5AuthSpy{runtime: managedRuntime("old-token")}
	server.metadataUpstream = upstream
	request := httptest.NewRequest(http.MethodGet, "https://gateway.test/emby/Users/synthetic-user/Items/Latest", nil)
	source, err := newPersonalPlanSource(server, request, &Session{GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user"}, "gateway-token")
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func TestExecuteLatestGroupedNaturalLargeLibraryReturnsExactPage(t *testing.T) {
	// >10k nonterminal catalog, but enough early distinct series for Limit=3.
	catalog := make([]map[string]any, latestPersonalScanLimit+50)
	parents := make(map[string]map[string]any)
	for i := range catalog {
		series := fmt.Sprintf("show-%05d", i)
		catalog[i] = map[string]any{"Id": fmt.Sprintf("ep-%05d", i), "Type": "Episode", "SeriesId": series}
		if i < 10 {
			parents[series] = map[string]any{"Id": series, "Type": "Series"}
		}
	}
	// Extra unplayed siblings only for early series to prove exact ChildCount.
	for i := 0; i < 3; i++ {
		series := fmt.Sprintf("show-%05d", i)
		catalog = append(catalog, map[string]any{"Id": fmt.Sprintf("ep-%05d-b", i), "Type": "Episode", "SeriesId": series})
	}
	fake := &latestCatalogFake{catalog: catalog, parents: parents, maxReturn: latestPersonalScanLimit + 50}
	source := latestTestSourceWithUpstream(t, &personalPlanSourceStore{MemoryStore: NewMemoryStore()}, fake)

	result, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(3, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"show-00000", "show-00001", "show-00002"}) {
		t.Fatalf("ids=%v", got)
	}
	for i, item := range result.Items {
		if item.item["ChildCount"] != 2 {
			t.Fatalf("item %d ChildCount=%v", i, item.item["ChildCount"])
		}
	}
	// Must not require a full 10k complete scan; discovery uses small adaptive Limits.
	sawLargeDiscovery := false
	for _, req := range fake.requests {
		if req.URL.Query().Get("Ids") != "" || req.URL.Query().Get("ParentId") != "" {
			continue
		}
		if lim, _ := strconv.Atoi(req.URL.Query().Get("Limit")); lim >= latestPersonalScanLimit {
			sawLargeDiscovery = true
		}
	}
	if sawLargeDiscovery {
		t.Fatalf("discovery used full scan bound; requests=%d", fake.calls)
	}
	if fake.calls < 5 {
		t.Fatalf("expected discovery+parents+child counts, calls=%d", fake.calls)
	}
}

func TestExecuteLatestGroupedZeroLocalChildrenBackfills(t *testing.T) {
	// Selected series has zero locally accepted children under ParentId (all played);
	// later valid series backfills Limit.
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{
		{ItemID: "ep-zero-1", Played: true},
		{ItemID: "ep-zero-2", Played: true},
	}}
	// Discovery still sees an "accepted" episode only for later shows; zero-show
	// is forced via ParentId response that only contains played children while
	// discovery prefix includes a different accepted marker path:
	// Use discovery order zero-show (stale-like via zero count) then valid-show.
	// For zero count: discovery needs an accepted ep of zero-show. Make ep-zero-live
	// unplayed in discovery but ParentId scoped returns only played ids (library skew).
	source, fake := latestTestSource(t, store,
		[]any{
			map[string]any{"Id": "ep-zero-live", "Type": "Episode", "SeriesId": "zero-show"},
			map[string]any{"Id": "ep-valid", "Type": "Episode", "SeriesId": "valid-show"},
		},
		map[string]any{"Items": []any{
			map[string]any{"Id": "zero-show", "Type": "Series"},
			map[string]any{"Id": "valid-show", "Type": "Series"},
		}},
		// ParentId=zero-show: only played children => local count 0
		[]any{
			map[string]any{"Id": "ep-zero-1", "Type": "Episode", "SeriesId": "zero-show"},
			map[string]any{"Id": "ep-zero-2", "Type": "Episode", "SeriesId": "zero-show"},
		},
		// ParentId=valid-show
		[]any{map[string]any{"Id": "ep-valid", "Type": "Episode", "SeriesId": "valid-show"}},
	)
	result, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(1, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"valid-show"}) {
		t.Fatalf("ids=%v calls=%d", got, fake.calls)
	}
	if result.Items[0].item["ChildCount"] != 1 {
		t.Fatalf("ChildCount=%v", result.Items[0].item["ChildCount"])
	}
}

func TestExecuteLatestGroupedGlobalBudgetExhaustionIsIncomplete(t *testing.T) {
	// Nonterminal ParentId child-count pages grow until the global candidate budget
	// cannot prove an exact ChildCount; must 503 with no partial result.
	counting := &latestBudgetExhaustFake{
		discovery: []map[string]any{
			{"Id": "ep-00", "Type": "Episode", "SeriesId": "show-00"},
		},
		parents: map[string]map[string]any{"show-00": {"Id": "show-00", "Type": "Series"}},
	}
	source := latestTestSourceWithUpstream(t, &personalPlanSourceStore{MemoryStore: NewMemoryStore()}, counting)
	result, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(1, true))
	if !errors.Is(err, ErrPersonalScanIncomplete) || result.Items != nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

type latestBudgetExhaustFake struct {
	discovery []map[string]any
	parents   map[string]map[string]any
	calls     int
	requests  []*http.Request
}

func (f *latestBudgetExhaustFake) RoundTripMetadata(in metadataUpstreamRequest) (*http.Response, error) {
	f.calls++
	f.requests = append(f.requests, in.Request)
	if in.SnapshotRef != nil {
		*in.SnapshotRef = personalPlanSourceUpstreamSnapshot()
	}
	q := in.Request.URL.Query()
	limit := 20
	if raw := q.Get("Limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	var body any
	switch {
	case q.Get("Ids") != "":
		items := make([]any, 0)
		for _, id := range strings.Split(q.Get("Ids"), ",") {
			if parent, ok := f.parents[id]; ok {
				items = append(items, parent)
			}
		}
		body = map[string]any{"Items": items}
	case q.Get("ParentId") != "":
		// Stable growing nonterminal prefix so adaptive Limit re-fetches remain valid.
		// Always return exactly `limit` items => Terminal=false until budgets fire.
		items := make([]any, limit)
		for i := 0; i < limit; i++ {
			items[i] = map[string]any{"Id": fmt.Sprintf("child-%05d", i), "Type": "Episode", "SeriesId": q.Get("ParentId")}
		}
		body = items
	default:
		end := limit
		if end > len(f.discovery) {
			end = len(f.discovery)
		}
		items := make([]any, end)
		for i := 0; i < end; i++ {
			items[i] = f.discovery[i]
		}
		body = items
	}
	raw, _ := json.Marshal(body)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header), Request: in.Request}, nil
}

func TestExecuteLatestGroupedHugeLimitIsIncompleteWithoutUpstream(t *testing.T) {
	// Client Limit above scan/candidate budget must 503 with no partial body and
	// without amplifying the first outbound Latest Limit (or any request).
	for _, limit := range []int{latestPersonalScanLimit + 1, latestGroupedBudgetCandidates + 1, math.MaxInt} {
		fake := &personalPlanSourceMetadataFake{}
		source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, &personalPlanSourceStore{MemoryStore: NewMemoryStore()})
		result, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(limit, true))
		if !errors.Is(err, ErrPersonalScanIncomplete) || result.Items != nil || fake.calls != 0 {
			t.Fatalf("limit=%d result=%#v calls=%d err=%v", limit, result, fake.calls, err)
		}
	}
}

func TestExecuteLatestUngroupedHugeLimitClampsOutbound(t *testing.T) {
	// Ungrouped MaxInt Limit must never egress Limit above the scan bound.
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore()}
	source, fake := latestTestSource(t, store, []any{
		map[string]any{"Id": "only", "Type": "Movie"},
	})
	plan := latestTestPlan(math.MaxInt, false)
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"only"}) {
		t.Fatalf("ids=%v", got)
	}
	if fake.calls != 1 {
		t.Fatalf("calls=%d", fake.calls)
	}
	outbound, err := strconv.Atoi(fake.request.URL.Query().Get("Limit"))
	if err != nil || outbound <= 0 || outbound > latestPersonalScanLimit {
		t.Fatalf("outbound Limit=%q", fake.request.URL.Query().Get("Limit"))
	}
}

func TestExecuteLatestGroupedBoundedDiscoveryRespectsOutboundClamp(t *testing.T) {
	// Even when Limit equals the scan bound, outbound discovery Limit stays <= bound.
	catalog := make([]map[string]any, 5)
	parents := make(map[string]map[string]any, 5)
	for i := range catalog {
		series := fmt.Sprintf("show-%d", i)
		catalog[i] = map[string]any{"Id": fmt.Sprintf("ep-%d", i), "Type": "Episode", "SeriesId": series}
		parents[series] = map[string]any{"Id": series, "Type": "Series"}
	}
	fake := &latestCatalogFake{catalog: catalog, parents: parents}
	source := latestTestSourceWithUpstream(t, &personalPlanSourceStore{MemoryStore: NewMemoryStore()}, fake)
	result, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(3, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(latestResultIDs(result)) != 3 {
		t.Fatalf("ids=%v", latestResultIDs(result))
	}
	for _, req := range fake.requests {
		if lim := req.URL.Query().Get("Limit"); lim != "" {
			n, err := strconv.Atoi(lim)
			if err != nil || n <= 0 || n > latestPersonalScanLimit {
				t.Fatalf("outbound Limit=%q path=%s query=%v", lim, req.URL.Path, req.URL.Query())
			}
		}
	}
}

func TestParsePersonalPlanLatestStartIndexRejected(t *testing.T) {
	// StartIndex remains a parser 400 for Latest (not an amplification vector).
	if _, err := parsePersonalPlan(personalRouteLatest, "/Users/u/Items/Latest", url.Values{"StartIndex": {"1"}, "Limit": {"20"}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err=%v", err)
	}
	if _, err := parsePersonalPlan(personalRouteLatest, "/Users/u/Items/Latest", url.Values{"StartIndex": {strconv.Itoa(math.MaxInt)}, "Limit": {strconv.Itoa(math.MaxInt)}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("maxint start err=%v", err)
	}
}

func TestExecuteLatestGroupedNaturalQueryHasNoPersonalEgress(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "played", Played: true}}}
	source, fake := latestTestSource(t, store,
		[]any{
			map[string]any{"Id": "played", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "fresh", "Type": "Episode", "SeriesId": "show"},
		},
		map[string]any{"Items": []any{map[string]any{"Id": "show", "Type": "Series"}}},
		[]any{
			map[string]any{"Id": "played", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "fresh", "Type": "Episode", "SeriesId": "show"},
		},
	)
	plan := latestTestPlan(1, true)
	plan.Neutral.Set("ParentId", "") // ensure clone path still sanitizes
	if _, err := executeLatestPersonalPlan(context.Background(), source, plan); err != nil {
		t.Fatal(err)
	}
	for i, req := range fake.requests {
		q := req.URL.Query()
		for _, key := range []string{"IsPlayed", "IsFavorite", "IsResumable", "IsLiked", "IsDisliked", "api_key", "X-Emby-Token", "UserId"} {
			if q.Get(key) != "" {
				t.Fatalf("request %d leaked %s: %v", i, key, q)
			}
		}
		if strings.Contains(strings.ToLower(q.Get("Filters")), "isplayed") {
			t.Fatalf("request %d Filters leaked personal predicate: %v", i, q)
		}
		if q.Get("EnableUserData") != "" || strings.Contains(strings.ToLower(q.Get("Fields")), "userdata") {
			t.Fatalf("request %d leaked UserData: %v", i, q)
		}
		// Gateway credential must not appear in raw query.
		if strings.Contains(req.URL.RawQuery, "gateway-token") {
			t.Fatalf("request %d leaked gateway token: %s", i, req.URL.RawQuery)
		}
	}
}

func TestExecuteLatestSortsMetadataLocalAndIDTies(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{
		{ItemID: "b", PlayCount: 2}, {ItemID: "a", PlayCount: 2}, {ItemID: "c", PlayCount: 1},
	}}
	plan := latestTestPlan(3, false)
	plan.Predicates.Played = personalTruthAny
	plan.Sort = []personalSortTerm{
		{Name: "PlayCount", Source: personalSortLocal, Direction: personalSortDescending},
		{Name: "Name", Source: personalSortMetadata},
	}
	source, _ := latestTestSource(t, store, []any{
		map[string]any{"Id": "b", "Name": "same"}, map[string]any{"Id": "c", "Name": "first"}, map[string]any{"Id": "a", "Name": "same"},
	})
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("sorted ids=%v", got)
	}
	for _, item := range result.Items {
		if _, exists := personalField(item.item, latestSyntheticRank); exists {
			t.Fatalf("synthetic rank leaked in %#v", item.item)
		}
		if _, exists := personalField(item.item, latestSyntheticMarker); exists {
			t.Fatalf("synthetic marker leaked in %#v", item.item)
		}
	}
}

func TestExecuteLatestCustomSortScansPastNaturalPrefix(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{
		{ItemID: "early", PlayCount: 1}, {ItemID: "winner", PlayCount: 9},
	}}
	plan := latestTestPlan(1, false)
	plan.Predicates.Played = personalTruthAny
	plan.Sort = []personalSortTerm{{Name: "PlayCount", Source: personalSortLocal, Direction: personalSortDescending}}
	source, fake := latestTestSource(t, store, []any{
		map[string]any{"Id": "early", "Name": "Early"},
		map[string]any{"Id": "winner", "Name": "Later"},
	})
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := latestResultIDs(result); !reflect.DeepEqual(got, []string{"winner"}) {
		t.Fatalf("custom-sort ids=%v", got)
	}
	if fake.calls != 1 || fake.request.URL.Query().Get("Limit") != "10000" {
		t.Fatalf("calls=%d query=%v", fake.calls, fake.request.URL.Query())
	}
}

func TestExecuteLatestCustomSortFullBoundIsIncomplete(t *testing.T) {
	items := make([]any, latestPersonalScanLimit)
	for i := range items {
		items[i] = map[string]any{"Id": fmt.Sprintf("item-%05d", i), "Name": "same"}
	}
	plan := latestTestPlan(1, false)
	plan.Sort = []personalSortTerm{{Name: "Name", Source: personalSortMetadata}}
	source, fake := latestTestSource(t, &personalPlanSourceStore{MemoryStore: NewMemoryStore()}, items)
	result, err := executeLatestPersonalPlan(context.Background(), source, plan)
	if !errors.Is(err, ErrPersonalScanIncomplete) || result.Items != nil || fake.calls != 1 {
		t.Fatalf("result=%#v calls=%d err=%v", result, fake.calls, err)
	}
}

func TestLatestNaturalSortEligibility(t *testing.T) {
	if !latestUsesNaturalSort(nil) || !latestUsesNaturalSort([]personalSortTerm{{Name: "latestrank", Source: personalSortMetadata}}) {
		t.Fatal("natural Latest sort was not eligible for prefix scan")
	}
	for _, terms := range [][]personalSortTerm{
		{{Name: "LatestRank", Source: personalSortMetadata, Direction: personalSortDescending}},
		{{Name: "LatestRank", Source: personalSortLocal}},
		{{Name: "Name", Source: personalSortMetadata}},
		{{Name: "LatestRank", Source: personalSortMetadata}, {Name: "Name", Source: personalSortMetadata}},
	} {
		if latestUsesNaturalSort(terms) {
			t.Fatalf("custom sort eligible for prefix scan: %#v", terms)
		}
	}
}

func TestExecuteLatestUsesAuthenticatedGatewayUserSnapshot(t *testing.T) {
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: []PlaybackState{{ItemID: "foreign-played", Played: true}}}
	source, _ := latestTestSource(t, store, []any{map[string]any{"Id": "foreign-played"}, map[string]any{"Id": "fresh"}})
	result, err := executeLatestPersonalPlan(context.Background(), source, latestTestPlan(3, false))
	if err != nil {
		t.Fatal(err)
	}
	if store.lastUserID != source.session.GatewayUserID || !reflect.DeepEqual(latestResultIDs(result), []string{"fresh"}) || result.Total != nil {
		t.Fatalf("user=%q ids=%v total=%v", store.lastUserID, latestResultIDs(result), result.Total)
	}
}

func latestResultIDs(result personalPlanResult) []string {
	ids := make([]string, len(result.Items))
	for i, item := range result.Items {
		ids[i] = personalItemIDForSort(item)
	}
	return ids
}

func ptrInt(value int) *int { return &value }
