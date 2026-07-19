package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
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
}

func latestTestPlan(limit int, grouped bool) personalPlan {
	return personalPlan{
		Kind: personalPlanLatest, Route: personalRouteLatest, Shape: personalShapeArray,
		Path: "/Users/synthetic-user/Items", Neutral: url.Values{},
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
	source, _ := latestTestSource(t, store,
		[]any{
			map[string]any{"Id": "old", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "ep-1", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "ep-2", "Type": "Episode", "SeriesId": "show"},
			map[string]any{"Id": "movie", "Type": "Movie"},
		},
		map[string]any{"Items": []any{map[string]any{"Id": "show", "Type": "Series", "ChildCount": 99, "Count": 88, "RecursiveItemCount": 77}}},
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

func TestLatestSyntheticMarkerCannotCollide(t *testing.T) {
	if _, err := attachLatestRanks([]map[string]any{{"Id": "x", "__PERSONALLATESTSYNTHETIC": true}}); err == nil {
		t.Fatal("accepted reserved marker")
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
