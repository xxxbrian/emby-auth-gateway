package gateway

import (
	"errors"
	"net/url"
	"reflect"
	"testing"
)

func TestSanitizeMetadataQueryCanonicalizesPersonalParameters(t *testing.T) {
	input := url.Values{
		"uSeRiD":           {"synthetic"},
		"USERID":           {"synthetic"},
		"isplayed":         {"true"},
		"ISFAVORITE":       {"true"},
		"IsResumable":      {"true"},
		"isliked":          {"true"},
		"ISDISLIKED":       {"true"},
		"filters":          {"IsPlayed,Movies,likes", "Series,IsUnplayed"},
		"FILTERS":          {"Genres,DISLIKES,IsFavorite,Years"},
		"sortby":           {"DatePlayed,SortName,PLAYCOUNT"},
		"SORTBY":           {"ProductionYear,IsFavoriteOrLiked,PlaybackPositionTicks,PlayedPercentage"},
		"fields":           {"Path,UserData,MediaSources"},
		"FIELDS":           {"ProviderIds,userdata"},
		"enableuserdata":   {"true", "false"},
		"ENABLEUSERDATAS":  {"true"},
		"StartIndex":       {"20"},
		"Limit":            {"50"},
		"IncludeItemTypes": {"Movie,Episode"},
		"Recursive":        {"true"},
		"signature":        {"cdn-signature"},
		"Policy":           {"catalog"},
	}
	original := cloneURLValues(input)

	encoded, err := SanitizeMetadataQuery(input, "synthetic", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, original) {
		t.Fatalf("input mutated:\n got: %#v\nwant: %#v", input, original)
	}
	got, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatal(err)
	}

	want := url.Values{
		"UserId":           {"backend"},
		"EnableUserData":   {"false"},
		"Filters":          {"Genres,Years", "Movies", "Series"},
		"SortBy":           {"ProductionYear", "SortName"},
		"Fields":           {"ProviderIds", "Path,MediaSources"},
		"StartIndex":       {"20"},
		"Limit":            {"50"},
		"IncludeItemTypes": {"Movie,Episode"},
		"Recursive":        {"true"},
		"signature":        {"cdn-signature"},
		"Policy":           {"catalog"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized query:\n got: %#v\nwant: %#v\nencoded: %s", got, want, encoded)
	}
	if encoded2, err := SanitizeMetadataQuery(input, "synthetic", "backend"); err != nil || encoded2 != encoded {
		t.Fatalf("result is not deterministic: %q, %v", encoded2, err)
	}
}

func TestSanitizeMetadataQueryRejectsEveryForeignUserAliasWithoutOutput(t *testing.T) {
	tests := []url.Values{
		{"UserId": {"foreign"}},
		{"USERID": {"synthetic", "foreign"}},
		{"UserId": {"synthetic"}, "userid": {"other"}},
		{"UserId": {""}},
		{"UserId": nil},
	}
	for _, input := range tests {
		original := cloneURLValues(input)
		got, err := SanitizeMetadataQuery(input, "synthetic", "backend")
		if got != "" || !errors.Is(err, ErrForbidden) {
			t.Fatalf("SanitizeMetadataQuery(%v) = %q, %v", input, got, err)
		}
		if !reflect.DeepEqual(input, original) {
			t.Fatalf("rejected input mutated: %#v", input)
		}
	}
}

func TestSanitizeMetadataQueryRequiresReadyIdentity(t *testing.T) {
	for _, ids := range [][2]string{{"", "backend"}, {"synthetic", ""}, {"", ""}} {
		got, err := SanitizeMetadataQuery(url.Values{"UserId": {ids[0]}}, ids[0], ids[1])
		if got != "" || !errors.Is(err, ErrForbidden) {
			t.Fatalf("ids %q = %q, %v", ids, got, err)
		}
	}
}

func TestSanitizeMetadataQueryPreservesUnrelatedListOrderAndSpacing(t *testing.T) {
	input := url.Values{"FiLtErS": {" Genre ,IsPlayed,,OfficialRating "}, "Fields": {"Path,, UserData ,Overview"}}
	encoded, err := SanitizeMetadataQuery(input, "synthetic", "backend")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := url.ParseQuery(encoded)
	if got.Get("Filters") != " Genre ,,OfficialRating " || got.Get("Fields") != "Path,,Overview" {
		t.Fatalf("unrelated membership/order changed: %#v", got)
	}
}

func TestSanitizeMetadataQueryStripsCredentialAliases(t *testing.T) {
	input := url.Values{
		"API_KEY":              {"selected", "unselected"},
		"Access_Token":         {"unselected"},
		"ToKeN":                {"generic"},
		"x-EMBY-token":         {"unselected"},
		"X-MediaBrowser-Token": {"selected", "unselected"},
		"UserId":               {"synthetic"},
		"signature":            {"a+b"},
		"Policy":               {"catalog"},
		"Key-Pair-Id":          {"key-id"},
	}
	original := cloneURLValues(input)

	encoded, err := SanitizeMetadataQuery(input, "synthetic", "backend")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, original) {
		t.Fatalf("input mutated:\n got: %#v\nwant: %#v", input, original)
	}
	got, err := url.ParseQuery(encoded)
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{
		"UserId":         {"backend"},
		"EnableUserData": {"false"},
		"signature":      {"a+b"},
		"Policy":         {"catalog"},
		"Key-Pair-Id":    {"key-id"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sanitized query:\n got: %#v\nwant: %#v", got, want)
	}
}

func cloneURLValues(input url.Values) url.Values {
	copy := make(url.Values, len(input))
	for key, values := range input {
		copy[key] = append([]string(nil), values...)
	}
	return copy
}
