package gateway

import (
	"errors"
	"net/url"
	"reflect"
	"strings"
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

func TestSanitizeMetadataRawQueryCompatibilityRoutesToGlobalBaseItem(t *testing.T) {
	raw := "UserId=synthetic&Filters=IsPlayed,Genre&EnableUserData=true"
	compat, err := sanitizeMetadataRawQuery(raw, "synthetic", "backend", "gw-token")
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend", "gw-token", metadataQueryPolicyGlobalBaseItem)
	if err != nil {
		t.Fatal(err)
	}
	if compat != explicit {
		t.Fatalf("compatibility wrapper diverged:\n compat=%q\nexplicit=%q", compat, explicit)
	}
	if !strings.Contains(compat, "EnableUserData=false") || !strings.Contains(compat, "UserId=backend") {
		t.Fatalf("global BaseItem output missing required appends: %q", compat)
	}
	if strings.Contains(compat, "IsPlayed") || strings.Contains(compat, "EnableUserData=true") {
		t.Fatalf("personal semantics leaked: %q", compat)
	}
}

func TestMetadataQueryPolicySystemInfoEmitsEmptyAfterValidation(t *testing.T) {
	// Observed Emby Web System/Info shape: client/device/language neutrals plus credentials.
	raw := strings.Join([]string{
		"api_key=selected-gateway-token",
		"X-Emby-Client=Emby+Web",
		"X-Emby-Device-Name=Chrome",
		"X-Emby-Device-Id=device-abc",
		"X-Emby-Client-Version=4.9.5.0",
		"X-Emby-Language=en-us",
		"UserId=synthetic",
		"EnableUserData=true",
		"IsFavorite=true",
		"signature=selected-gateway-token",
	}, "&")
	got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "", "selected-gateway-token", metadataQueryPolicySystemInfo)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("SystemInfo query = %q, want empty", got)
	}
	// Backend ID is not required after validation for SystemInfo.
	got, err = sanitizeMetadataRawQueryWithPolicy("X-Emby-Client=Emby+Web", "synthetic", "", "", metadataQueryPolicySystemInfo)
	if err != nil || got != "" {
		t.Fatalf("SystemInfo without backend = %q, %v", got, err)
	}
	// Empty synthetic is allowed when no UserId is present.
	got, err = sanitizeMetadataRawQueryWithPolicy("X-Emby-Language=zh-cn", "", "", "", metadataQueryPolicySystemInfo)
	if err != nil || got != "" {
		t.Fatalf("SystemInfo without synthetic = %q, %v", got, err)
	}
}

func TestMetadataQueryPolicyPathBoundNeutralViewsPreservesNeutralsAppendsNothing(t *testing.T) {
	// Observed Emby Web Views/HomeSections shape.
	raw := strings.Join([]string{
		"UserId=synthetic",
		"api_key=selected-gateway-token",
		"X-Emby-Client=Emby+Web",
		"X-Emby-Device-Id=device-abc",
		"X-Emby-Language=en-us",
		"IncludeExternalContent=false",
		"EnableUserData=true",
		"IsPlayed=true",
		"Filters=IsFavorite,IsFolder",
	}, "&")
	got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend", "selected-gateway-token", metadataQueryPolicyPathBoundNeutral)
	if err != nil {
		t.Fatal(err)
	}
	want := "X-Emby-Client=Emby+Web&X-Emby-Device-Id=device-abc&X-Emby-Language=en-us&IncludeExternalContent=false&Filters=IsFolder"
	if got != want {
		t.Fatalf("path-bound neutral:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "UserId=") || strings.Contains(got, "EnableUserData") || strings.Contains(got, "api_key") {
		t.Fatalf("Views must not append or keep user/credential fields: %q", got)
	}
}

func TestMetadataQueryPolicyPathBoundBaseItemAppendsEnableUserDataOnly(t *testing.T) {
	raw := "UserId=synthetic&ParentId=lib-1&Recursive=true&IncludeItemTypes=Movie&Fields=Path%2CUserData&EnableUserData=true&api_key=tok"
	got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend", "tok", metadataQueryPolicyPathBoundBaseItem)
	if err != nil {
		t.Fatal(err)
	}
	want := "ParentId=lib-1&Recursive=true&IncludeItemTypes=Movie&Fields=Path&EnableUserData=false"
	if got != want {
		t.Fatalf("path-bound BaseItem:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "UserId=") {
		t.Fatalf("path-bound BaseItem must not append query UserId: %q", got)
	}
}

func TestMetadataQueryPolicyGlobalBaseItemAppendsBackendUserAndDisableUserData(t *testing.T) {
	raw := "StartIndex=0&Limit=50&UserId=synthetic&SortBy=DatePlayed%2CSortName&Filters=IsPlayed%2CGenre"
	got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend-user", "", metadataQueryPolicyGlobalBaseItem)
	if err != nil {
		t.Fatal(err)
	}
	want := "StartIndex=0&Limit=50&SortBy=SortName&Filters=Genre&EnableUserData=false&UserId=backend-user"
	if got != want {
		t.Fatalf("global BaseItem:\n got %q\nwant %q", got, want)
	}
}

func TestMetadataQueryPolicyNonBaseItemImagePreservesNeutralsNoUserFields(t *testing.T) {
	// Image / non-BaseItem metadata: strip credentials and user fields, keep neutrals.
	raw := "maxWidth=400&maxHeight=400&quality=90&tag=abc%2Bdef&UserId=synthetic&api_key=selected&EnableUserData=true&IsFavorite=true"
	got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend", "selected", metadataQueryPolicyNonBaseItem)
	if err != nil {
		t.Fatal(err)
	}
	want := "maxWidth=400&maxHeight=400&quality=90&tag=abc%2Bdef"
	if got != want {
		t.Fatalf("non-BaseItem/image:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "UserId") || strings.Contains(got, "EnableUserData") || strings.Contains(got, "api_key") {
		t.Fatalf("non-BaseItem must not keep/append user or credential fields: %q", got)
	}
}

func TestMetadataQueryPolicyPreservesRawOrderingDuplicatesAndEscaping(t *testing.T) {
	raw := "sig=a%2Bb&dup=one&dup=two+words&opaque=synthetic&keep=z&keep=a&UserId=synthetic"
	got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend", "", metadataQueryPolicyPathBoundNeutral)
	if err != nil {
		t.Fatal(err)
	}
	want := "sig=a%2Bb&dup=one&dup=two+words&opaque=synthetic&keep=z&keep=a"
	if got != want {
		t.Fatalf("raw preservation:\n got %q\nwant %q", got, want)
	}
}

func TestMetadataQueryPolicyRemovesExactSelectedCredentialRegardlessOfKey(t *testing.T) {
	selected := "selected-gateway-credential"
	raw := "before=one&signature=" + selected + "&signature=ordinary-signed-value&API_KEY=other&Token=generic&after=two&UserId=synthetic"
	for _, policy := range []metadataQueryPolicy{
		metadataQueryPolicyPathBoundNeutral,
		metadataQueryPolicyPathBoundBaseItem,
		metadataQueryPolicyGlobalBaseItem,
		metadataQueryPolicyNonBaseItem,
	} {
		got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend", selected, policy)
		if err != nil {
			t.Fatalf("policy %v: %v", policy, err)
		}
		if strings.Contains(got, selected) {
			t.Fatalf("policy %v leaked selected credential: %q", policy, got)
		}
		if strings.Contains(got, "API_KEY=") || strings.Contains(strings.ToLower(got), "token=generic") {
			t.Fatalf("policy %v leaked egress credential alias: %q", policy, got)
		}
		if !strings.Contains(got, "signature=ordinary-signed-value") || !strings.Contains(got, "before=one") || !strings.Contains(got, "after=two") {
			t.Fatalf("policy %v dropped unrelated pairs: %q", policy, got)
		}
	}
	// SystemInfo still validates credentials then emits empty.
	got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "", selected, metadataQueryPolicySystemInfo)
	if err != nil || got != "" {
		t.Fatalf("SystemInfo credential strip = %q, %v", got, err)
	}
}

func TestMetadataQueryPolicyRejectsForeignUserIdForEveryPolicy(t *testing.T) {
	cases := []string{
		"UserId=foreign",
		"USERID=synthetic&userid=other",
		"UserId=",
		"UserId",
		"UserId=synthetic&UserId=foreign",
	}
	policies := []metadataQueryPolicy{
		metadataQueryPolicySystemInfo,
		metadataQueryPolicyPathBoundNeutral,
		metadataQueryPolicyPathBoundBaseItem,
		metadataQueryPolicyGlobalBaseItem,
		metadataQueryPolicyNonBaseItem,
	}
	for _, policy := range policies {
		for _, raw := range cases {
			got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend", "", policy)
			if got != "" || !errors.Is(err, ErrForbidden) {
				t.Fatalf("policy %v raw %q = %q, %v", policy, raw, got, err)
			}
		}
	}
}

func TestMetadataQueryPolicyIdentityRequirements(t *testing.T) {
	// Global BaseItem still requires both IDs up front.
	if got, err := sanitizeMetadataRawQueryWithPolicy("Recursive=true", "", "backend", "", metadataQueryPolicyGlobalBaseItem); got != "" || !errors.Is(err, ErrForbidden) {
		t.Fatalf("global missing synthetic = %q, %v", got, err)
	}
	if got, err := sanitizeMetadataRawQueryWithPolicy("Recursive=true", "synthetic", "", "", metadataQueryPolicyGlobalBaseItem); got != "" || !errors.Is(err, ErrForbidden) {
		t.Fatalf("global missing backend = %q, %v", got, err)
	}

	// Path-bound / non-BaseItem do not require backend when no UserId is present.
	for _, policy := range []metadataQueryPolicy{
		metadataQueryPolicyPathBoundNeutral,
		metadataQueryPolicyPathBoundBaseItem,
		metadataQueryPolicyNonBaseItem,
	} {
		got, err := sanitizeMetadataRawQueryWithPolicy("Recursive=true", "synthetic", "", "", policy)
		if err != nil {
			t.Fatalf("policy %v without backend: %v", policy, err)
		}
		if !strings.Contains(got, "Recursive=true") {
			t.Fatalf("policy %v dropped neutral: %q", policy, got)
		}
	}

	// Missing synthetic fails only when a UserId pair must be validated.
	if got, err := sanitizeMetadataRawQueryWithPolicy("UserId=synthetic", "", "backend", "", metadataQueryPolicyPathBoundNeutral); got != "" || !errors.Is(err, ErrForbidden) {
		t.Fatalf("path-bound with UserId and empty synthetic = %q, %v", got, err)
	}
	got, err := sanitizeMetadataRawQueryWithPolicy("Recursive=true", "", "", "", metadataQueryPolicyPathBoundNeutral)
	if err != nil || got != "Recursive=true" {
		t.Fatalf("path-bound without identity = %q, %v", got, err)
	}
}

func TestMetadataQueryPolicyStripsPersonalSemanticsAcrossPolicies(t *testing.T) {
	raw := "Filters=IsPlayed,Movies,IsFavorite&SortBy=DatePlayed,SortName&Fields=Path,UserData&IsResumable=true&IsLiked=true&EnableUserDatas=true&UserId=synthetic"
	for _, policy := range []metadataQueryPolicy{
		metadataQueryPolicyPathBoundNeutral,
		metadataQueryPolicyPathBoundBaseItem,
		metadataQueryPolicyGlobalBaseItem,
		metadataQueryPolicyNonBaseItem,
	} {
		got, err := sanitizeMetadataRawQueryWithPolicy(raw, "synthetic", "backend", "", policy)
		if err != nil {
			t.Fatalf("policy %v: %v", policy, err)
		}
		for _, leak := range []string{"IsPlayed", "IsFavorite", "DatePlayed", "IsResumable", "IsLiked", "EnableUserDatas=true", "UserId=synthetic"} {
			if strings.Contains(got, leak) {
				t.Fatalf("policy %v leaked personal %q in %q", policy, leak, got)
			}
		}
		if strings.Contains(got, "Fields=Path,UserData") || strings.Contains(got, "UserData,") || strings.Contains(got, ",UserData") {
			t.Fatalf("policy %v leaked Fields UserData membership in %q", policy, got)
		}
		if !strings.Contains(got, "Filters=Movies") || !strings.Contains(got, "SortBy=SortName") || !strings.Contains(got, "Fields=Path") {
			t.Fatalf("policy %v lost neutral list members: %q", policy, got)
		}
	}
}

func TestMetadataQueryPolicyMalformedQuery(t *testing.T) {
	got, err := sanitizeMetadataRawQueryWithPolicy("a=%zz", "synthetic", "backend", "", metadataQueryPolicyGlobalBaseItem)
	if got != "" || !errors.Is(err, ErrBadRequest) {
		t.Fatalf("malformed = %q, %v", got, err)
	}
}

func cloneURLValues(input url.Values) url.Values {
	copy := make(url.Values, len(input))
	for key, values := range input {
		copy[key] = append([]string(nil), values...)
	}
	return copy
}
