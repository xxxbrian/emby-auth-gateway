package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func nextUpTestPlan(neutral url.Values) personalPlan {
	return personalPlan{Kind: personalPlanNextUp, Shape: personalShapeQueryResult, Neutral: neutral, Scan: personalScanPolicy{MaxItems: personalPlanScanMaxItems}}
}

func nextUpTestItem(id, series string, season, episode int) map[string]any {
	return map[string]any{"Id": id, "Type": "Episode", "SeriesId": series, "SeasonId": fmt.Sprintf("%s-season-%d", series, season), "ParentIndexNumber": float64(season), "IndexNumber": float64(episode)}
}

func nextUpTestResponse(t *testing.T, items ...map[string]any) personalPlanSourceMetadataResponse {
	t.Helper()
	raw := make([]any, len(items))
	for i := range items {
		raw[i] = items[i]
	}
	return personalPlanSourceMetadataResponse{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": raw})}
}

func nextUpTestPagedResponse(t *testing.T, total int, items ...map[string]any) personalPlanSourceMetadataResponse {
	t.Helper()
	raw := make([]any, len(items))
	for i := range items {
		raw[i] = items[i]
	}
	return personalPlanSourceMetadataResponse{status: http.StatusOK, body: personalPlanSourceJSON(t, map[string]any{"Items": raw, "TotalRecordCount": float64(total)})}
}

func nextUpTestSource(t *testing.T, states []PlaybackState, responses ...personalPlanSourceMetadataResponse) (*personalPlanSource, *personalPlanSourceStore, *personalPlanSourceMetadataFake) {
	t.Helper()
	store := &personalPlanSourceStore{MemoryStore: NewMemoryStore(), states: states}
	fake := &personalPlanSourceMetadataFake{responses: responses}
	source, _ := newPersonalPlanSourceTestSourceWithStore(t, fake, store)
	return source, store, fake
}

func TestParseNextUpControls(t *testing.T) {
	cutoff := "2026-07-19T12:00:00Z"
	controls, err := parseNextUpControls(url.Values{
		"SeriesId":         {"series-1"},
		"ParentId":         {"parent-1"},
		"EnableResumable":  {"false"},
		"NextUpDateCutoff": {cutoff},
	})
	if err != nil {
		t.Fatal(err)
	}
	if controls.seriesID != "series-1" || controls.parentID != "parent-1" || controls.enableResumable {
		t.Fatalf("controls = %#v", controls)
	}
	if controls.cutoff == nil || !controls.cutoff.Equal(time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("cutoff = %#v", controls.cutoff)
	}
	defaults, err := parseNextUpControls(nil)
	if err != nil || !defaults.enableResumable {
		t.Fatalf("defaults = %#v, err=%v", defaults, err)
	}
	for _, query := range []url.Values{
		{"SeriesId": {"series/unsafe"}},
		{"EnableResumable": {"yes"}},
		{"NextUpDateCutoff": {"not-a-date"}},
		{"ParentId": {"a", "b"}},
		{"SeriesId": {"one"}, "seriesid": {"two"}},
	} {
		if _, err := parseNextUpControls(query); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("query %#v error = %v, want ErrBadRequest", query, err)
		}
	}
}

func TestParseNextUpEpisodeRequiresCompleteSafeMetadata(t *testing.T) {
	base := map[string]any{
		"Id": "episode-1", "Type": "Episode", "SeriesId": "series-1", "SeasonId": "season-1",
		"ParentIndexNumber": float64(1), "IndexNumber": float64(2),
	}
	if episode, err := parseNextUpEpisode(base); err != nil || episode.id != "episode-1" || episode.episodeNo != 2 {
		t.Fatalf("episode=%#v err=%v", episode, err)
	}
	for _, field := range []string{"Id", "Type", "SeriesId", "SeasonId", "ParentIndexNumber", "IndexNumber"} {
		item := clonePlannedPersonalJSONMap(base)
		delete(item, field)
		if _, err := parseNextUpEpisode(item); err == nil {
			t.Fatalf("missing %s was accepted", field)
		}
	}
	for _, field := range []string{"ParentIndexNumber", "IndexNumber"} {
		item := clonePlannedPersonalJSONMap(base)
		item[field] = float64(-1)
		if _, err := parseNextUpEpisode(item); err == nil {
			t.Fatalf("negative %s was accepted", field)
		}
	}
}

func TestCompareNextUpOrder(t *testing.T) {
	a := nextUpEpisode{seasonNo: 1, episodeNo: 2}
	b := nextUpEpisode{seasonNo: 2, episodeNo: 1}
	if compareNextUpOrder(a, b) >= 0 || compareNextUpOrder(b, a) <= 0 || compareNextUpOrder(a, a) != 0 {
		t.Fatalf("unexpected order comparisons")
	}
}

func TestExecuteNextUpNilAndWrongContractDoNotPanic(t *testing.T) {
	for _, test := range []struct {
		name   string
		source *personalPlanSource
		plan   personalPlan
	}{
		{"nil source", nil, nextUpTestPlan(nil)},
		{"nil session", &personalPlanSource{}, nextUpTestPlan(nil)},
		{"wrong kind", &personalPlanSource{session: &Session{}}, personalPlan{Kind: personalPlanPositive, Shape: personalShapeQueryResult}},
		{"wrong shape", &personalPlanSource{session: &Session{}}, personalPlan{Kind: personalPlanNextUp, Shape: personalShapeArray}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := executeNextUpPersonalPlan(context.Background(), test.source, test.plan); err == nil {
				t.Fatal("expected contract error")
			}
		})
	}
}

func TestExecuteNextUpNoHistoryAndExplicitSeriesHaveNoEgress(t *testing.T) {
	source, store, fake := nextUpTestSource(t, nil)
	for _, neutral := range []url.Values{nil, {"SeriesId": {"series-1"}}} {
		result, err := executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(neutral))
		if err != nil || result.Total == nil || *result.Total != 0 || len(result.Items) != 0 {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	}
	if store.listCalls != 2 || fake.calls != 0 {
		t.Fatalf("snapshot calls=%d metadata calls=%d", store.listCalls, fake.calls)
	}
}

func TestExecuteNextUpResumableAndAdvancePreserveCurrentState(t *testing.T) {
	date := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	favorite := true
	likes := false
	anchor := PlaybackState{ItemID: "ep-1", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", PlaybackPositionTicks: 10, LastPlayedDate: &date}
	selected := PlaybackState{ItemID: "ep-2", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", IsFavorite: favorite, Likes: &likes}
	resolution := nextUpTestResponse(t, nextUpTestItem("ep-1", "series-1", 1, 1))
	episodes := nextUpTestResponse(t, nextUpTestItem("ep-1", "series-1", 1, 1), nextUpTestItem("ep-2", "series-1", 1, 2))
	source, _, _ := nextUpTestSource(t, []PlaybackState{anchor, selected}, resolution, episodes)
	result, err := executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(nil))
	if err != nil || len(result.Items) != 1 || result.Items[0].state.ItemID != "ep-1" || result.Items[0].item["Id"] != "ep-1" {
		t.Fatalf("resumable result=%#v err=%v", result, err)
	}
	source, _, _ = nextUpTestSource(t, []PlaybackState{anchor, selected}, resolution, episodes)
	plan := nextUpTestPlan(url.Values{"EnableResumable": {"false"}})
	result, err = executeNextUpPersonalPlan(context.Background(), source, plan)
	if err != nil || len(result.Items) != 1 || result.Items[0].item["Id"] != "ep-2" || !result.Items[0].state.IsFavorite || result.Items[0].state.Likes == nil || *result.Items[0].state.Likes != likes {
		t.Fatalf("advance result=%#v err=%v", result, err)
	}
}

func TestExecuteNextUpIgnoresNonEpisodeButRejectsMalformedEpisode(t *testing.T) {
	movie := map[string]any{"Id": "movie-1", "Type": "Movie"}
	state := PlaybackState{ItemID: "movie-1", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true}
	source, _, fake := nextUpTestSource(t, []PlaybackState{state}, nextUpTestResponse(t, movie))
	result, err := executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(nil))
	if err != nil || result.Total == nil || *result.Total != 0 || fake.calls != 1 {
		t.Fatalf("non-episode result=%#v err=%v calls=%d", result, err, fake.calls)
	}
	malformed := nextUpTestItem("ep-1", "series-1", 1, 1)
	delete(malformed, "SeasonId")
	state.ItemID = "ep-1"
	source, _, _ = nextUpTestSource(t, []PlaybackState{state}, nextUpTestResponse(t, malformed))
	if _, err := executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(nil)); err == nil || !strings.Contains(err.Error(), "SeasonId") {
		t.Fatalf("malformed episode error=%v", err)
	}
}

func TestExecuteNextUpSeriesActivityCutoffAndStablePage(t *testing.T) {
	old := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	equal := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	stateA := PlaybackState{ItemID: "a-progress", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true, SeriesID: "series-a", LastPlayedDate: &old}
	stateB := PlaybackState{ItemID: "b-progress", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true, SeriesID: "series-b", LastPlayedDate: &equal}
	resolution := nextUpTestResponse(t, nextUpTestItem("a-progress", "series-a", 1, 1), nextUpTestItem("b-progress", "series-b", 1, 1))
	source, _, _ := nextUpTestSource(t, []PlaybackState{stateA, stateB}, resolution,
		nextUpTestResponse(t, nextUpTestItem("b-progress", "series-b", 1, 1), nextUpTestItem("b-next", "series-b", 1, 2)),
		nextUpTestResponse(t, nextUpTestItem("a-progress", "series-a", 1, 1), nextUpTestItem("a-next", "series-a", 1, 2)))
	plan := nextUpTestPlan(url.Values{"NextUpDateCutoff": {equal.Format(time.RFC3339)}, "StartIndex": {"0"}})
	result, err := executeNextUpPersonalPlan(context.Background(), source, plan)
	if err != nil || result.Total == nil || *result.Total != 1 || len(result.Items) != 1 || result.Items[0].item["Id"] != "b-next" {
		t.Fatalf("cutoff result=%#v err=%v", result, err)
	}
	if !reflect.DeepEqual(result.Items[0].state.GatewayUserID, "gateway-user") {
		t.Fatal("state identity was not preserved")
	}
}

func TestExecuteNextUpParentFilterAndMissingAnchor(t *testing.T) {
	state := PlaybackState{ItemID: "progress", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true}
	resolution := nextUpTestResponse(t, nextUpTestItem("progress", "series-1", 1, 1))
	source, _, fake := nextUpTestSource(t, []PlaybackState{state}, resolution, nextUpTestResponse(t))
	result, err := executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(url.Values{"ParentId": {"parent-1"}}))
	if err != nil || result.Total == nil || *result.Total != 0 || fake.calls != 2 {
		t.Fatalf("parent result=%#v err=%v calls=%d", result, err, fake.calls)
	}
	if got := fake.requests[1].URL.Query().Get("ParentId"); got != "parent-1" {
		t.Fatalf("ParentId query=%q", got)
	}

	source, _, _ = nextUpTestSource(t, []PlaybackState{state}, resolution, nextUpTestResponse(t, nextUpTestItem("other", "series-1", 1, 2)))
	if _, err := executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(nil)); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("missing anchor error=%v", err)
	}
}

func TestExecuteNextUpCompleteTwoPageScanAndPerSeriesFailure(t *testing.T) {
	state := PlaybackState{ItemID: "ep-1", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true}
	pageOne := make([]map[string]any, 0, personalPlanScanPageSize)
	pageOne = append(pageOne, nextUpTestItem("ep-1", "series-1", 1, 1))
	for i := 2; i <= personalPlanScanPageSize; i++ {
		pageOne = append(pageOne, nextUpTestItem(fmt.Sprintf("ep-%d", i), "series-1", 1, i))
	}
	pageTwo := nextUpTestItem("ep-101", "series-1", 1, 101)
	resolution := nextUpTestResponse(t, nextUpTestItem("ep-1", "series-1", 1, 1))
	source, _, _ := nextUpTestSource(t, []PlaybackState{state}, resolution, nextUpTestPagedResponse(t, 101, pageOne...), nextUpTestPagedResponse(t, 101, pageTwo))
	result, err := executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(nil))
	if err != nil || result.Total == nil || *result.Total != 1 || len(result.Items) != 1 || result.Items[0].item["Id"] != "ep-2" {
		t.Fatalf("two-page result=%#v err=%v", result, err)
	}

	source, _, _ = nextUpTestSource(t, []PlaybackState{state}, resolution, personalPlanSourceMetadataResponse{err: errors.New("series backend failed")})
	if _, err := executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(nil)); err == nil || !strings.Contains(err.Error(), "series backend failed") {
		t.Fatalf("series failure error=%v", err)
	}
}

func TestExecuteNextUpExactTotalPageZeroAndProgressBound(t *testing.T) {
	date := time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)
	states := []PlaybackState{
		{ItemID: "ep-a", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true, LastPlayedDate: &date},
		{ItemID: "ep-b", GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true, LastPlayedDate: &date},
	}
	resolution := nextUpTestResponse(t, nextUpTestItem("ep-a", "series-a", 1, 1), nextUpTestItem("ep-b", "series-b", 1, 1))
	source, store, fake := nextUpTestSource(t, states, resolution,
		nextUpTestResponse(t, nextUpTestItem("ep-a", "series-a", 1, 1), nextUpTestItem("next-a", "series-a", 1, 2)),
		nextUpTestResponse(t, nextUpTestItem("ep-b", "series-b", 1, 1), nextUpTestItem("next-b", "series-b", 1, 2)))
	zero := 0
	plan := nextUpTestPlan(nil)
	plan.Page.Limit = &zero
	result, err := executeNextUpPersonalPlan(context.Background(), source, plan)
	if err != nil || result.Total == nil || *result.Total != 2 || len(result.Items) != 0 || result.StartIndex != 0 {
		t.Fatalf("zero page result=%#v err=%v", result, err)
	}
	if store.listCalls != 1 || fake.calls != 3 {
		t.Fatalf("calls list=%d metadata=%d", store.listCalls, fake.calls)
	}

	bound := make([]PlaybackState, personalPlanScanMaxItems+1)
	for i := range bound {
		bound[i] = PlaybackState{ItemID: fmt.Sprintf("progress-%d", i), GatewayUserID: "gateway-user", SyntheticUserID: "synthetic-user", Played: true}
	}
	source, store, fake = nextUpTestSource(t, bound)
	_, err = executeNextUpPersonalPlan(context.Background(), source, nextUpTestPlan(nil))
	if !errors.Is(err, ErrPersonalScanIncomplete) || store.listCalls != 1 || fake.calls != 0 {
		t.Fatalf("bound err=%v list=%d metadata=%d", err, store.listCalls, fake.calls)
	}
}
