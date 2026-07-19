package gateway

import (
	"errors"
	"net/url"
	"testing"
)

func TestPersonalPlanSelection(t *testing.T) {
	tests := []struct {
		name  string
		route personalRouteKind
		query url.Values
		kind  personalPlanKind
	}{
		{"passthrough", personalRouteItems, nil, personalPlanPassthrough},
		{"positive", personalRouteItems, url.Values{"Filters": {"IsPlayed"}}, personalPlanPositive},
		{"negative", personalRouteItems, url.Values{"Filters": {"IsUnplayed"}}, personalPlanNegative},
		{"compatible positive source", personalRouteItems, url.Values{"Filters": {"IsPlayed"}, "IsFavorite": {"false"}}, personalPlanPositive},
		{"show items", personalRouteShowItems, url.Values{"IsFavorite": {"true"}}, personalPlanPositive},
		{"resume", personalRouteResume, nil, personalPlanResume},
		{"next up", personalRouteNextUp, nil, personalPlanNextUp},
		{"latest", personalRouteLatest, nil, personalPlanLatest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := parsePersonalPlan(tt.route, "/test", tt.query)
			if err != nil {
				t.Fatalf("parsePersonalPlan() error = %v", err)
			}
			if plan.Kind != tt.kind {
				t.Fatalf("kind = %v, want %v", plan.Kind, tt.kind)
			}
		})
	}
}

func TestPersonalPlanParserValidAliasesAndPaging(t *testing.T) {
	plan, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{
		"fIlTeRs":    {"isplayed", "isnotfolder"},
		"iSfAvOrItE": {"false"},
		"StArTiNdEx": {"3"},
		"Fields":     {"Name,UserData,Overview"},
		"SortBy":     {"Name", "DatePlayed"},
		"SortOrder":  {"Descending"},
		"ParentId":   {"parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Predicates.Played != personalTruthTrue || plan.Predicates.Favorite != personalTruthFalse {
		t.Fatalf("predicates = %#v", plan.Predicates)
	}
	if plan.Page.Start != 3 || plan.Page.Limit != nil {
		t.Fatalf("page = %#v, want start 3 and nil limit", plan.Page)
	}
	if plan.Projection.Get("Fields") != "Name,Overview" {
		t.Fatalf("projection = %#v", plan.Projection)
	}
	if plan.Neutral.Get("ParentId") != "parent" || plan.Neutral.Get("Filters") != "isnotfolder" {
		t.Fatalf("neutral = %#v", plan.Neutral)
	}
	if len(plan.Sort) != 2 || plan.Sort[0].Direction != personalSortDescending || plan.Sort[1].Source != personalSortLocal {
		t.Fatalf("sort = %#v", plan.Sort)
	}
}

func TestPersonalPlanParserRejectsInvalidQueries(t *testing.T) {
	tests := []struct {
		name  string
		route personalRouteKind
		query url.Values
	}{
		{"case variants", personalRouteItems, url.Values{"Limit": {"1"}, "lImIt": {"1"}}},
		{"duplicate scalar", personalRouteItems, url.Values{"Limit": {"1", "2"}}},
		{"duplicate UserId", personalRouteItems, url.Values{"UserId": {"user", "user"}}},
		{"case variant UserId", personalRouteItems, url.Values{"UserId": {"user"}, "userid": {"user"}}},
		{"malformed bool", personalRouteItems, url.Values{"IsPlayed": {"maybe"}}},
		{"malformed int", personalRouteItems, url.Values{"StartIndex": {"x"}}},
		{"negative page", personalRouteItems, url.Values{"Limit": {"-1"}}},
		{"unknown filter", personalRouteItems, url.Values{"Filters": {"IsPlayed,NoSuchFilter"}}},
		{"unsupported rating alias", personalRouteItems, url.Values{"IsLiked": {"true"}}},
		{"contradictory truth", personalRouteItems, url.Values{"Filters": {"IsPlayed,IsUnplayed"}, "IsPlayed": {"false"}}},
		{"likes and dislikes", personalRouteItems, url.Values{"Filters": {"Likes,Dislikes"}}},
		{"sort directions too many", personalRouteItems, url.Values{"SortBy": {"Name,DateCreated"}, "SortOrder": {"Ascending,Descending,Ascending"}}},
		{"sort direction invalid", personalRouteItems, url.Values{"SortBy": {"Name"}, "SortOrder": {"Random"}}},
		{"random local sort", personalRouteItems, url.Values{"Filters": {"IsPlayed"}, "SortBy": {"Random"}}},
		{"latest start", personalRouteLatest, url.Values{"StartIndex": {"1"}}},
		{"generic grouping", personalRouteItems, url.Values{"Filters": {"IsPlayed"}, "GroupBy": {"SeriesId"}}},
		{"non-latest group items", personalRouteResume, url.Values{"GroupItems": {"true"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePersonalPlan(tt.route, "/test", tt.query)
			if err == nil || !errors.Is(err, ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
		})
	}
}

func TestPersonalPlanParserRejectsNextUpPersonalPredicates(t *testing.T) {
	tests := []struct {
		name  string
		query url.Values
	}{
		{"filter played", url.Values{"Filters": {"IsPlayed"}}},
		{"filter unplayed", url.Values{"Filters": {"IsUnplayed"}}},
		{"filter favorite", url.Values{"Filters": {"IsFavorite"}}},
		{"filter resumable", url.Values{"Filters": {"IsResumable"}}},
		{"filter likes", url.Values{"Filters": {"Likes"}}},
		{"filter dislikes", url.Values{"Filters": {"Dislikes"}}},
		{"direct played true", url.Values{"IsPlayed": {"true"}}},
		{"direct played false", url.Values{"IsPlayed": {"false"}}},
		{"direct favorite true", url.Values{"IsFavorite": {"true"}}},
		{"direct favorite false", url.Values{"IsFavorite": {"false"}}},
		{"direct resumable true", url.Values{"IsResumable": {"true"}}},
		{"direct resumable false", url.Values{"IsResumable": {"false"}}},
		{"unsupported direct liked", url.Values{"IsLiked": {"true"}}},
		{"unsupported direct disliked", url.Values{"IsDisliked": {"true"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePersonalPlan(personalRouteNextUp, "/Shows/NextUp", tt.query)
			if err == nil || !errors.Is(err, ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
		})
	}
}

func TestPersonalPlanParserNextUpSortContract(t *testing.T) {
	for _, sortBy := range []string{"DatePlayed", "Name"} {
		t.Run(sortBy, func(t *testing.T) {
			_, err := parsePersonalPlan(personalRouteNextUp, "/Shows/NextUp", url.Values{"SortBy": {sortBy}})
			if err == nil || !errors.Is(err, ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
		})
	}

	_, err := parsePersonalPlan(personalRouteNextUp, "/Shows/NextUp", url.Values{"SortOrder": {"Descending"}})
	if err == nil || !errors.Is(err, ErrBadRequest) {
		t.Fatalf("SortOrder without SortBy error = %v, want ErrBadRequest", err)
	}
}

func TestPersonalPlanParserNextUpControlsAndFieldsRemainValid(t *testing.T) {
	plan, err := parsePersonalPlan(personalRouteNextUp, "/Shows/NextUp", url.Values{
		"SeriesId":         {"series-1"},
		"ParentId":         {"parent-1"},
		"EnableResumable":  {"false"},
		"NextUpDateCutoff": {"2026-07-19T12:00:00Z"},
		"StartIndex":       {"2"},
		"Limit":            {"5"},
		"Fields":           {"Name,Overview,UserData"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Page.Start != 2 || plan.Page.Limit == nil || *plan.Page.Limit != 5 {
		t.Fatalf("plan controls = %#v", plan)
	}
	if plan.Projection.Get("Fields") != "Name,Overview" || len(plan.Sort) != 2 || plan.Sort[0].Name != "SeriesActivity" || plan.Sort[1].Name != "EpisodeOrder" {
		t.Fatalf("plan projection/sort = %#v", plan)
	}
}

func TestPersonalPlanParserLocalSortAllowlist(t *testing.T) {
	t.Run("unknown local sort", func(t *testing.T) {
		_, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{
			"Filters": {"IsPlayed"},
			"SortBy":  {"MadeUpMetadataField"},
		})
		if err == nil || !errors.Is(err, ErrBadRequest) {
			t.Fatalf("error = %v, want ErrBadRequest", err)
		}
	})

	t.Run("unknown passthrough sort", func(t *testing.T) {
		plan, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{"SortBy": {"MadeUpMetadataField"}})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Kind != personalPlanPassthrough || len(plan.Sort) != 1 || plan.Sort[0].Name != "MadeUpMetadataField" || plan.Neutral.Get("SortBy") != "MadeUpMetadataField" {
			t.Fatalf("plan = %#v", plan)
		}
	})

	for _, sortBy := range []string{"Name", "DateCreated", "ProductionYear", "SortName"} {
		t.Run("established "+sortBy, func(t *testing.T) {
			plan, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{
				"Filters": {"IsPlayed"},
				"SortBy":  {sortBy},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Sort) != 1 || plan.Sort[0].Source != personalSortMetadata {
				t.Fatalf("sort = %#v", plan.Sort)
			}
		})
	}

	for _, sortBy := range []string{"Name", "SortName", "DateCreated", "PremiereDate", "ProductionYear", "CommunityRating", "CriticRating", "OfficialRating"} {
		t.Run("metadata "+sortBy, func(t *testing.T) {
			plan, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{
				"Filters": {"IsPlayed"},
				"SortBy":  {sortBy},
			})
			if err != nil || len(plan.Sort) != 1 || plan.Sort[0].Source != personalSortMetadata {
				t.Fatalf("allowlisted metadata sort %q: plan=%#v err=%v", sortBy, plan, err)
			}
		})
	}

	for _, sortBy := range []string{"Runtime", "DateLastContentAdded", "Album", "AlbumArtist", "Artist", "SeriesSortName", "Studio", "VideoBitRate", "Budget", "Revenue", "IsFolder", "IsUnplayed"} {
		t.Run("removed local "+sortBy, func(t *testing.T) {
			_, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{
				"Filters": {"IsPlayed"},
				"SortBy":  {sortBy},
			})
			if err == nil || !errors.Is(err, ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}

			plan, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{"SortBy": {sortBy}})
			if err != nil || plan.Kind != personalPlanPassthrough || plan.Neutral.Get("SortBy") != sortBy || len(plan.Sort) != 1 || plan.Sort[0].Name != sortBy {
				t.Fatalf("passthrough plan=%#v err=%v", plan, err)
			}
		})
	}

	for _, sortBy := range []string{"LatestRank", "SeriesActivity", "EpisodeOrder"} {
		t.Run("synthetic "+sortBy, func(t *testing.T) {
			_, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{
				"Filters": {"IsPlayed"},
				"SortBy":  {sortBy},
			})
			if err == nil || !errors.Is(err, ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
		})
	}
}

func TestPersonalPlanParserDefaultsAndShapes(t *testing.T) {
	items, err := parsePersonalPlan(personalRouteItems, "/Items", url.Values{"Limit": {"0"}})
	if err != nil || items.Page.Limit == nil || *items.Page.Limit != 0 {
		t.Fatalf("Items zero page = %#v, err = %v", items.Page, err)
	}
	resume, err := parsePersonalPlan(personalRouteResume, "/Resume", nil)
	if err != nil || resume.Page.Limit != nil {
		t.Fatalf("Resume defaults = %#v, err = %v", resume, err)
	}
	nextUp, err := parsePersonalPlan(personalRouteNextUp, "/NextUp", nil)
	if err != nil || nextUp.Page.Limit == nil || *nextUp.Page.Limit != 20 {
		t.Fatalf("NextUp page = %#v, err = %v", nextUp.Page, err)
	}
	latest, err := parsePersonalPlan(personalRouteLatest, "/Latest", nil)
	if err != nil || latest.Page.Limit == nil || *latest.Page.Limit != 20 || latest.Predicates.Played != personalTruthFalse || !latest.Group.Items {
		t.Fatalf("Latest defaults = %#v, err = %v", latest, err)
	}
	ungrouped, err := parsePersonalPlan(personalRouteLatest, "/Latest", url.Values{"GroupItems": {"false"}})
	if err != nil || ungrouped.Group.Items {
		t.Fatalf("ungrouped Latest = %#v, err = %v", ungrouped, err)
	}
}
