package gateway

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	personalPlanScanPageSize = 100
	personalPlanScanMaxPages = 100
	personalPlanScanMaxItems = 10_000
)

var personalScalarQueryKeys = foldedSet(
	"IsPlayed", "IsFavorite", "IsResumable", "IsLiked", "IsDisliked",
	"StartIndex", "Limit", "GroupItems", "EnableUserData", "UserId", "SeriesId", "ParentId", "IncludeItemTypes",
)

var personalLocalSorts = foldedSet(
	"DatePlayed", "PlayCount", "IsFavorite", "IsFavoriteOrLiked",
	"PlaybackPositionTicks", "PlayedPercentage",
)

// Metadata sorts are limited to stable Emby item fields that local executors
// can resolve and compare. Executor-only synthetic terms are intentionally absent.
var personalMetadataSorts = foldedSet(
	"Name", "SortName", "DateCreated", "PremiereDate", "ProductionYear",
	"CommunityRating", "CriticRating", "OfficialRating",
)

// parsePersonalPlan is the pure route/query boundary used by later endpoint integration.
func parsePersonalPlan(route personalRouteKind, path string, query url.Values) (personalPlan, error) {
	canonical, err := canonicalPersonalQuery(query)
	if err != nil {
		return personalPlan{}, err
	}

	plan := personalPlan{
		Route:      route,
		Path:       path,
		Neutral:    cloneQuery(canonical),
		Refinement: url.Values{},
		Projection: url.Values{},
		Scan: personalScanPolicy{
			PageSize: personalPlanScanPageSize,
			MaxPages: personalPlanScanMaxPages,
			MaxItems: personalPlanScanMaxItems,
		},
	}

	if err := plan.parsePersonalPredicates(canonical); err != nil {
		return personalPlan{}, err
	}
	if err := plan.validateRoutePredicates(); err != nil {
		return personalPlan{}, err
	}
	if err := plan.selectKindAndShape(); err != nil {
		return personalPlan{}, err
	}
	if err := plan.parsePage(canonical); err != nil {
		return personalPlan{}, err
	}
	if err := plan.parseGroup(canonical); err != nil {
		return personalPlan{}, err
	}
	if err := plan.parseSort(canonical); err != nil {
		return personalPlan{}, err
	}
	plan.applyDefaultSort()
	if err := plan.partitionQuery(canonical); err != nil {
		return personalPlan{}, err
	}

	return plan, nil
}

func canonicalPersonalQuery(query url.Values) (url.Values, error) {
	out := make(url.Values, len(query))
	spellings := make(map[string]string, len(query))
	for key, values := range query {
		folded := strings.ToLower(key)
		if previous, exists := spellings[folded]; exists && previous != key {
			return nil, personalPlanBadRequest("query key %q has multiple case variants", key)
		}
		spellings[folded] = key
		if _, scalar := personalScalarQueryKeys[folded]; scalar && len(values) != 1 {
			return nil, personalPlanBadRequest("query key %q must have exactly one value", key)
		}
		canonical := personalCanonicalKey(key)
		out[canonical] = append([]string(nil), values...)
	}
	return out, nil
}

func personalCanonicalKey(key string) string {
	for _, known := range []string{
		"Filters", "Fields", "SortBy", "SortOrder", "IsPlayed", "IsFavorite",
		"IsResumable", "IsLiked", "IsDisliked", "StartIndex", "Limit", "GroupItems", "EnableUserData",
		"UserId", "SeriesId", "ParentId", "IncludeItemTypes",
	} {
		if strings.EqualFold(key, known) {
			return known
		}
	}
	return key
}

func (plan *personalPlan) parsePersonalPredicates(query url.Values) error {
	filters := splitPersonalList(query["Filters"])
	for _, filter := range filters {
		switch strings.ToLower(filter) {
		case "isfolder", "isnotfolder":
		case "isplayed":
			if err := setPersonalTruth(&plan.Predicates.Played, personalTruthTrue, filter); err != nil {
				return err
			}
		case "isunplayed":
			if err := setPersonalTruth(&plan.Predicates.Played, personalTruthFalse, filter); err != nil {
				return err
			}
		case "isfavorite":
			if err := setPersonalTruth(&plan.Predicates.Favorite, personalTruthTrue, filter); err != nil {
				return err
			}
		case "isresumable":
			if err := setPersonalTruth(&plan.Predicates.Resumable, personalTruthTrue, filter); err != nil {
				return err
			}
		case "likes":
			if err := setPersonalRating(&plan.Predicates.Rating, personalRatingLiked, filter); err != nil {
				return err
			}
		case "dislikes":
			if err := setPersonalRating(&plan.Predicates.Rating, personalRatingDisliked, filter); err != nil {
				return err
			}
		default:
			return personalPlanBadRequest("unsupported Filters value %q", filter)
		}
	}

	for _, direct := range []struct {
		name   string
		target *personalTruth
	}{
		{"IsPlayed", &plan.Predicates.Played},
		{"IsFavorite", &plan.Predicates.Favorite},
		{"IsResumable", &plan.Predicates.Resumable},
	} {
		raw, present := personalScalar(query, direct.name)
		if !present {
			continue
		}
		value, err := parsePersonalBool(raw)
		if err != nil {
			return personalPlanBadRequest("%s must be a boolean", direct.name)
		}
		truth := personalTruthFalse
		if value {
			truth = personalTruthTrue
		}
		if err := setPersonalTruth(direct.target, truth, direct.name); err != nil {
			return err
		}
	}
	if _, present := personalScalar(query, "IsLiked"); present {
		return personalPlanBadRequest("IsLiked is unsupported; use Filters=Likes")
	}
	if _, present := personalScalar(query, "IsDisliked"); present {
		return personalPlanBadRequest("IsDisliked is unsupported; use Filters=Dislikes")
	}
	return nil
}

func (plan *personalPlan) validateRoutePredicates() error {
	if plan.Route != personalRouteNextUp {
		return nil
	}
	if plan.Predicates.Played != personalTruthAny ||
		plan.Predicates.Favorite != personalTruthAny ||
		plan.Predicates.Resumable != personalTruthAny ||
		plan.Predicates.Rating != personalRatingAny {
		return personalPlanBadRequest("personal predicates are unsupported for NextUp")
	}
	return nil
}

func (plan *personalPlan) parsePage(query url.Values) error {
	if raw, present := personalScalar(query, "StartIndex"); present {
		if plan.Route == personalRouteLatest {
			return personalPlanBadRequest("StartIndex is unsupported for Latest")
		}
		start, err := parsePersonalNonnegativeInt("StartIndex", raw)
		if err != nil {
			return err
		}
		plan.Page.Start = start
	}
	if raw, present := personalScalar(query, "Limit"); present {
		limit, err := parsePersonalNonnegativeInt("Limit", raw)
		if err != nil {
			return err
		}
		plan.Page.Limit = &limit
	} else if plan.Route == personalRouteNextUp || plan.Route == personalRouteLatest {
		limit := 20
		plan.Page.Limit = &limit
	}
	return nil
}

func (plan *personalPlan) parseGroup(query url.Values) error {
	for key := range query {
		folded := strings.ToLower(key)
		if strings.HasPrefix(folded, "group") && !strings.EqualFold(key, "GroupItems") {
			if plan.Kind == personalPlanPassthrough {
				continue
			}
			return personalPlanBadRequest("generic grouping parameter %q is unsupported", key)
		}
	}
	raw, present := personalScalar(query, "GroupItems")
	if plan.Route != personalRouteLatest {
		if present {
			if plan.Kind == personalPlanPassthrough {
				return nil
			}
			return personalPlanBadRequest("GroupItems is only supported for Latest")
		}
		return nil
	}
	plan.Group.Items = true
	if !present {
		return nil
	}
	value, err := parsePersonalBool(raw)
	if err != nil {
		return personalPlanBadRequest("GroupItems must be a boolean")
	}
	plan.Group.Items = value
	plan.Group.Explicit = true
	return nil
}

func (plan *personalPlan) parseSort(query url.Values) error {
	names := splitPersonalList(query["SortBy"])
	directions := splitPersonalList(query["SortOrder"])
	if len(names) == 0 && len(directions) != 0 {
		return personalPlanBadRequest("SortOrder requires SortBy")
	}
	if _, present := query["SortBy"]; plan.Route == personalRouteNextUp && present {
		return personalPlanBadRequest("SortBy is unsupported for NextUp")
	}
	if len(directions) != 0 && len(directions) != 1 && len(directions) != len(names) {
		return personalPlanBadRequest("SortOrder count must be one or match SortBy count")
	}
	for i, name := range names {
		direction := personalSortAscending
		if len(directions) != 0 {
			raw := directions[0]
			if len(directions) > 1 {
				raw = directions[i]
			}
			switch strings.ToLower(raw) {
			case "ascending":
			case "descending":
				direction = personalSortDescending
			default:
				return personalPlanBadRequest("invalid SortOrder direction %q", raw)
			}
		}
		foldedName := strings.ToLower(name)
		source := personalSortMetadata
		if _, local := personalLocalSorts[foldedName]; local {
			source = personalSortLocal
		} else if _, metadata := personalMetadataSorts[foldedName]; !metadata && plan.Kind != personalPlanPassthrough {
			if strings.EqualFold(name, "Random") {
				return personalPlanBadRequest("SortBy Random is unsupported for local plans")
			}
			return personalPlanBadRequest("unsupported SortBy value %q for local plan", name)
		}
		plan.Sort = append(plan.Sort, personalSortTerm{Name: name, Source: source, Direction: direction})
	}
	return nil
}

func (plan *personalPlan) applyDefaultSort() {
	if len(plan.Sort) != 0 {
		return
	}
	switch plan.Route {
	case personalRouteResume:
		plan.Sort = []personalSortTerm{
			{Name: "LastPlayedDate", Source: personalSortLocal, Direction: personalSortDescending},
			{Name: "UpdatedAt", Source: personalSortLocal, Direction: personalSortDescending},
		}
	case personalRouteNextUp:
		plan.Sort = []personalSortTerm{
			{Name: "SeriesActivity", Source: personalSortLocal, Direction: personalSortDescending},
			{Name: "EpisodeOrder", Source: personalSortMetadata, Direction: personalSortAscending},
		}
	case personalRouteLatest:
		plan.Sort = []personalSortTerm{{Name: "LatestRank", Source: personalSortMetadata, Direction: personalSortAscending}}
	}
}

func (plan *personalPlan) partitionQuery(query url.Values) error {
	if plan.Kind == personalPlanPassthrough {
		plan.Neutral = cloneQuery(query)
		plan.Refinement = url.Values{}
		plan.Projection = url.Values{}
		return nil
	}

	plan.Neutral = cloneQuery(query)
	for _, key := range []string{
		"IsPlayed", "IsFavorite", "IsResumable", "IsLiked", "IsDisliked",
		"SortBy", "SortOrder", "StartIndex", "Limit", "GroupItems", "Fields", "EnableUserData",
	} {
		plan.Neutral.Del(key)
	}

	structuralFilters := make([]string, 0)
	for _, filter := range splitPersonalList(query["Filters"]) {
		if strings.EqualFold(filter, "IsFolder") || strings.EqualFold(filter, "IsNotFolder") {
			structuralFilters = append(structuralFilters, filter)
		}
	}
	plan.Neutral.Del("Filters")
	if len(structuralFilters) != 0 {
		plan.Neutral.Set("Filters", strings.Join(structuralFilters, ","))
	}
	plan.Refinement = cloneQuery(plan.Neutral)
	if plan.Route == personalRouteLatest {
		plan.Neutral.Set("GroupItems", "false")
		plan.Refinement.Set("GroupItems", "false")
	}

	fields := splitPersonalList(query["Fields"])
	projected := make([]string, 0, len(fields))
	for _, field := range fields {
		if strings.EqualFold(field, "UserData") {
			continue
		}
		projected = append(projected, field)
	}
	if len(projected) != 0 {
		plan.Projection.Set("Fields", strings.Join(projected, ","))
	}
	if raw, present := personalScalar(query, "EnableUserData"); present {
		value, err := parsePersonalBool(raw)
		if err != nil {
			return personalPlanBadRequest("EnableUserData must be a boolean")
		}
		plan.Projection.Set("EnableUserData", strconv.FormatBool(value))
	}
	return nil
}

func (plan *personalPlan) selectKindAndShape() error {
	switch plan.Route {
	case personalRouteResume:
		if plan.Predicates.Resumable == personalTruthFalse {
			return personalPlanBadRequest("Resume conflicts with IsResumable=false")
		}
		plan.Kind = personalPlanResume
		plan.Shape = personalShapeQueryResult
		plan.Predicates.Resumable = personalTruthTrue
	case personalRouteNextUp:
		plan.Kind = personalPlanNextUp
		plan.Shape = personalShapeQueryResult
	case personalRouteLatest:
		plan.Kind = personalPlanLatest
		plan.Shape = personalShapeArray
		if plan.Predicates.Played == personalTruthAny {
			plan.Predicates.Played = personalTruthFalse
		}
	default:
		plan.Shape = personalShapeQueryResult
		if plan.Predicates.hasPositive() {
			plan.Kind = personalPlanPositive
		} else if plan.Predicates.hasNegative() {
			plan.Kind = personalPlanNegative
		} else {
			plan.Kind = personalPlanPassthrough
			plan.Shape = personalShapePassthrough
		}
	}
	return nil
}

func (predicates personalPredicates) hasPositive() bool {
	return predicates.Played == personalTruthTrue || predicates.Favorite == personalTruthTrue ||
		predicates.Resumable == personalTruthTrue || predicates.Rating != personalRatingAny
}

func (predicates personalPredicates) hasNegative() bool {
	return predicates.Played == personalTruthFalse || predicates.Favorite == personalTruthFalse ||
		predicates.Resumable == personalTruthFalse
}

func setPersonalTruth(target *personalTruth, value personalTruth, alias string) error {
	if *target != personalTruthAny && *target != value {
		return personalPlanBadRequest("conflicting personal predicate %q", alias)
	}
	*target = value
	return nil
}

func setPersonalRating(target *personalRating, value personalRating, alias string) error {
	if *target != personalRatingAny && *target != value {
		return personalPlanBadRequest("conflicting rating predicate %q", alias)
	}
	*target = value
	return nil
}

func personalScalar(query url.Values, key string) (string, bool) {
	values, present := query[key]
	if !present {
		return "", false
	}
	return values[0], true
}

func splitPersonalList(values []string) []string {
	var out []string
	for _, value := range values {
		for _, member := range strings.Split(value, ",") {
			if member = strings.TrimSpace(member); member != "" {
				out = append(out, member)
			}
		}
	}
	return out
}

func parsePersonalNonnegativeInt(name, raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, personalPlanBadRequest("%s must be a nonnegative integer", name)
	}
	return value, nil
}

func parsePersonalBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, strconv.ErrSyntax
	}
}

func personalPlanBadRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadRequest, fmt.Sprintf(format, args...))
}
