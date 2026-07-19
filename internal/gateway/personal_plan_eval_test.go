package gateway

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestPersonalStateMatches(t *testing.T) {
	for _, tt := range []struct {
		want  personalTruth
		value bool
		match bool
	}{
		{personalTruthAny, false, true}, {personalTruthAny, true, true},
		{personalTruthTrue, false, false}, {personalTruthTrue, true, true},
		{personalTruthFalse, false, true}, {personalTruthFalse, true, false},
	} {
		if got := personalTruthMatches(tt.want, tt.value); got != tt.match {
			t.Errorf("truth %v against %v = %v, want %v", tt.want, tt.value, got, tt.match)
		}
	}
	liked, disliked := true, false
	for _, tt := range []struct {
		want  personalRating
		likes *bool
		match bool
	}{
		{personalRatingAny, nil, true}, {personalRatingAny, &liked, true}, {personalRatingAny, &disliked, true},
		{personalRatingLiked, nil, false}, {personalRatingLiked, &liked, true}, {personalRatingLiked, &disliked, false},
		{personalRatingDisliked, nil, false}, {personalRatingDisliked, &liked, false}, {personalRatingDisliked, &disliked, true},
	} {
		if got := personalRatingMatches(tt.want, tt.likes); got != tt.match {
			t.Errorf("rating %v against %v = %v, want %v", tt.want, tt.likes, got, tt.match)
		}
	}
	states := []PlaybackState{{}, {Played: true, IsFavorite: true, PlaybackPositionTicks: 20}, {PlaybackPositionTicks: 20}}
	for _, tt := range []struct {
		name  string
		state PlaybackState
		p     personalPredicates
		want  bool
	}{
		{"all predicates", states[1], personalPredicates{Played: personalTruthTrue, Favorite: personalTruthTrue, Resumable: personalTruthFalse}, true},
		{"resumable excludes played", states[1], personalPredicates{Resumable: personalTruthTrue}, false},
		{"resumable true", states[2], personalPredicates{Resumable: personalTruthTrue}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := personalStateMatches(tt.state, tt.p); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPersonalStateJoin(t *testing.T) {
	states, err := indexPersonalStates([]PlaybackState{{ItemID: "a", Played: true}})
	if err != nil {
		t.Fatal(err)
	}
	candidates := []map[string]any{{"Id": "a"}, {"Id": "b"}}
	got, err := joinPersonalCandidates(candidates, states, true)
	if err != nil || len(got) != 1 {
		t.Fatalf("require state got %d items", len(got))
	}
	got, err = joinPersonalCandidates(candidates, states, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].state.ItemID != "b" {
		t.Fatalf("optional join: %#v", got)
	}
	if _, err := indexPersonalStates([]PlaybackState{{ItemID: "a"}, {ItemID: "a"}}); err == nil {
		t.Fatal("duplicate state accepted")
	}
	if _, err := indexPersonalStates([]PlaybackState{{ID: "state-without-item"}}); !errors.Is(err, ErrPersonalEmptyStateID) {
		t.Fatalf("empty state ID error = %v", err)
	}
	if _, err := joinPersonalCandidates([]map[string]any{{"Name": "missing-id"}}, states, false); !errors.Is(err, ErrPersonalMalformedCandidate) {
		t.Fatalf("malformed candidate error = %v", err)
	}
	if _, err := joinPersonalCandidates([]map[string]any{{"Id": "a"}, {"Id": "a"}}, states, false); !errors.Is(err, ErrPersonalDuplicateCandidate) {
		t.Fatalf("duplicate candidate error = %v", err)
	}
}

func TestSortPersonalPlanItems(t *testing.T) {
	d1 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	d2 := d1.AddDate(1, 0, 0)
	items := []resolvedPersonalItem{{item: map[string]any{"Id": "b", "Name": "z", "Number": 2, "When": d2.Format(time.RFC3339)}}, {item: map[string]any{"Id": "a", "Name": "A", "Number": 10, "When": d1.Format(time.RFC3339)}}, {item: map[string]any{"Id": "c"}}}
	sortPersonalPlanItems(items, []personalSortTerm{{Name: "Number"}})
	if !reflect.DeepEqual([]string{"b", "a", "c"}, sortedIDs(items)) {
		t.Fatalf("numeric order: %v", sortedIDs(items))
	}
	sortPersonalPlanItems(items, []personalSortTerm{{Name: "Number", Direction: personalSortDescending}})
	if !reflect.DeepEqual([]string{"a", "b", "c"}, sortedIDs(items)) {
		t.Fatalf("numeric descending/missing order: %v", sortedIDs(items))
	}
	sortPersonalPlanItems(items, []personalSortTerm{{Name: "When", Direction: personalSortDescending}})
	if !reflect.DeepEqual([]string{"b", "a", "c"}, sortedIDs(items)) {
		t.Fatalf("date descending/missing order: %v", sortedIDs(items))
	}
	sortPersonalPlanItems(items, []personalSortTerm{{Name: "Name"}})
	if !reflect.DeepEqual([]string{"a", "b", "c"}, sortedIDs(items)) {
		t.Fatalf("normalized string order: %v", sortedIDs(items))
	}
}

func TestSortPersonalPlanRetainedMetadataFieldsByValue(t *testing.T) {
	tests := []struct {
		name string
		low  any
		high any
	}{
		{"Name", "Alpha", "zeta"},
		{"SortName", "alpha", "Zulu"},
		{"DateCreated", "2025-01-01T00:00:00Z", "2026-01-01T00:00:00Z"},
		{"PremiereDate", "2025-02-01T00:00:00Z", "2026-02-01T00:00:00Z"},
		{"ProductionYear", float64(2020), float64(2025)},
		{"CommunityRating", float64(6.5), float64(8.5)},
		{"CriticRating", float64(60), float64(90)},
		{"OfficialRating", "PG", "TV-14"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := []resolvedPersonalItem{
				{item: map[string]any{"Id": "a", tt.name: tt.high}},
				{item: map[string]any{"Id": "b", tt.name: tt.low}},
			}
			sortPersonalPlanItems(items, []personalSortTerm{{Name: tt.name, Source: personalSortMetadata}})
			if got := sortedIDs(items); !reflect.DeepEqual(got, []string{"b", "a"}) {
				t.Fatalf("ascending order=%v, want [b a]", got)
			}
			sortPersonalPlanItems(items, []personalSortTerm{{Name: tt.name, Source: personalSortMetadata, Direction: personalSortDescending}})
			if got := sortedIDs(items); !reflect.DeepEqual(got, []string{"a", "b"}) {
				t.Fatalf("descending order=%v, want [a b]", got)
			}
		})
	}
}

func TestResolutionFieldsForRetainedMetadataSorts(t *testing.T) {
	for _, tt := range []struct {
		input string
		field string
	}{
		{"sortname", "SortName"},
		{"datecreated", "DateCreated"},
		{"premieredate", "PremiereDate"},
		{"productionyear", "ProductionYear"},
		{"communityrating", "CommunityRating"},
		{"criticrating", "CriticRating"},
		{"officialrating", "OfficialRating"},
	} {
		if got := resolutionFieldsForSort(tt.input); !reflect.DeepEqual(got, []string{tt.field}) {
			t.Errorf("resolutionFieldsForSort(%q)=%v, want [%s]", tt.input, got, tt.field)
		}
	}
	if got := resolutionFieldsForSort("Name"); got != nil {
		t.Fatalf("Name resolution fields=%v, want nil", got)
	}
}

func TestSortPersonalPlanItemsDeterministicTies(t *testing.T) {
	items := []resolvedPersonalItem{{item: map[string]any{"Id": "c"}}, {item: map[string]any{"Id": "a", "Name": "same"}}, {item: map[string]any{"Id": "b", "Name": "same"}}}
	sortPersonalPlanItems(items, []personalSortTerm{{Name: "Missing"}})
	if got := sortedIDs(items); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("fully missing order = %v", got)
	}
	sortPersonalPlanItems(items, []personalSortTerm{{Name: "Name", Direction: personalSortDescending}})
	if got := sortedIDs(items); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("equal-value ID tie/missing order = %v", got)
	}
}

func TestPersonalLocalSortFields(t *testing.T) {
	pct := 42.5
	liked := true
	played := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	updated := played.Add(time.Hour)
	item := resolvedPersonalItem{state: PlaybackState{
		ItemID: "local", LastPlayedDate: &played, UpdatedAt: updated, PlayCount: 3,
		IsFavorite: true, Likes: &liked, PlaybackPositionTicks: 99, PlayedPercentage: &pct,
		IndexNumber: 7,
	}}
	for _, name := range []string{"DatePlayed", "LastPlayedDate", "UpdatedAt", "PlayCount", "IsFavorite", "IsFavoriteOrLiked", "PlaybackPositionTicks", "PlayedPercentage", "SeriesActivity"} {
		if _, ok := personalSortValue(item, personalSortTerm{Name: name, Source: personalSortLocal}); !ok {
			t.Errorf("local sort field %q was missing", name)
		}
	}
	for _, name := range []string{"EpisodeOrder", "LatestRank"} {
		if _, ok := personalSortValue(item, personalSortTerm{Name: name, Source: personalSortLocal}); ok {
			t.Errorf("local sort field %q fabricated a value", name)
		}
	}
}

func sortedIDs(items []resolvedPersonalItem) []string {
	out := make([]string, len(items))
	for i := range items {
		out[i] = personalItemIDForSort(items[i])
	}
	return out
}

func TestPagePersonalPlanItems(t *testing.T) {
	items := []resolvedPersonalItem{{state: PlaybackState{ItemID: "a"}}, {state: PlaybackState{ItemID: "b"}}, {state: PlaybackState{ItemID: "c"}}}
	zero, two := 0, 2
	for _, tt := range []struct {
		name string
		spec personalPageSpec
		want []string
	}{{"remainder", personalPageSpec{Start: 1}, []string{"b", "c"}}, {"nil limit", personalPageSpec{Start: 99}, []string{}}, {"zero", personalPageSpec{Limit: &zero}, []string{}}, {"bounded", personalPageSpec{Start: 1, Limit: &two}, []string{"b", "c"}}, {"negative start", personalPageSpec{Start: -1, Limit: &two}, []string{"a", "b"}}} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sortedIDs(pagePersonalPlanItems(items, tt.spec)); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	page := pagePersonalPlanItems(items, personalPageSpec{Limit: &two})
	page[0] = resolvedPersonalItem{}
	if items[0].state.ItemID != "a" {
		t.Fatal("page aliases caller slice")
	}
}
