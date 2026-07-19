package gateway

import (
	"context"
	"fmt"
	"strings"
)

const (
	latestPersonalScanLimit = 10000
	latestSyntheticMarker   = "__personalLatestSynthetic"
	latestSyntheticRank     = "LatestRank"
)

func executeLatestPersonalPlan(ctx context.Context, source *personalPlanSource, plan personalPlan) (personalPlanResult, error) {
	if plan.Kind != personalPlanLatest || plan.Shape != personalShapeArray || plan.Page.Start != 0 {
		return personalPlanResult{}, fmt.Errorf("invalid Latest personal plan")
	}
	limit := 20
	if plan.Page.Limit != nil {
		limit = *plan.Page.Limit
	}
	if limit == 0 {
		return personalPlanResult{Items: []resolvedPersonalItem{}, StartIndex: 0}, nil
	}
	if limit < 0 {
		return personalPlanResult{}, fmt.Errorf("invalid Latest limit")
	}
	if source == nil || source.session == nil {
		return personalPlanResult{}, fmt.Errorf("missing Latest personal plan source")
	}

	snapshot, err := source.snapshot(ctx)
	if err != nil {
		return personalPlanResult{}, err
	}
	var candidates []map[string]any
	if plan.Group.Items {
		page, err := source.fetchCandidatePage(ctx, plan, 0, latestPersonalScanLimit)
		if err != nil {
			return personalPlanResult{}, err
		}
		if len(page.Items) == latestPersonalScanLimit || !page.Terminal {
			return personalPlanResult{}, fmt.Errorf("%w: Latest grouping scan reached %d items", ErrPersonalScanIncomplete, latestPersonalScanLimit)
		}
		if err := validateLatestPrefix(nil, page.Items); err != nil {
			return personalPlanResult{}, err
		}
		candidates = page.Items
	} else if !latestUsesNaturalSort(plan.Sort) {
		page, err := source.fetchCandidatePage(ctx, plan, 0, latestPersonalScanLimit)
		if err != nil {
			return personalPlanResult{}, err
		}
		if len(page.Items) == latestPersonalScanLimit || !page.Terminal {
			return personalPlanResult{}, fmt.Errorf("%w: sorted Latest scan reached %d items", ErrPersonalScanIncomplete, latestPersonalScanLimit)
		}
		if err := validateLatestPrefix(nil, page.Items); err != nil {
			return personalPlanResult{}, err
		}
		candidates = page.Items
	} else {
		candidates, err = fetchLatestUngroupedPrefix(ctx, source, plan, snapshot, limit)
		if err != nil {
			return personalPlanResult{}, err
		}
	}
	candidates, err = attachLatestRanks(candidates)
	if err != nil {
		return personalPlanResult{}, err
	}

	joined, err := joinPersonalCandidates(candidates, snapshot.States, false)
	if err != nil {
		return personalPlanResult{}, err
	}
	filtered := joined[:0]
	for _, item := range joined {
		if personalStateMatches(item.state, plan.Predicates) {
			filtered = append(filtered, item)
		}
	}
	if plan.Group.Items {
		filtered, err = groupLatestPersonalItems(ctx, source, plan, snapshot, filtered)
		if err != nil {
			return personalPlanResult{}, err
		}
	}
	sortPersonalPlanItems(filtered, plan.Sort)
	removeLatestSyntheticSortFields(filtered)
	return personalPlanResult{Items: pagePersonalPlanItems(filtered, plan.Page), StartIndex: 0}, nil
}

func latestUsesNaturalSort(terms []personalSortTerm) bool {
	if len(terms) == 0 {
		return true
	}
	return len(terms) == 1 &&
		strings.EqualFold(terms[0].Name, latestSyntheticRank) &&
		terms[0].Source == personalSortMetadata &&
		terms[0].Direction == personalSortAscending
}

func fetchLatestUngroupedPrefix(ctx context.Context, source *personalPlanSource, plan personalPlan, snapshot personalStateSnapshot, requested int) ([]map[string]any, error) {
	previous := []map[string]any{}
	for fetchLimit := requested; ; fetchLimit = latestNextFetchLimit(fetchLimit) {
		if fetchLimit > latestPersonalScanLimit {
			fetchLimit = latestPersonalScanLimit
		}
		page, err := source.fetchCandidatePage(ctx, plan, 0, fetchLimit)
		if err != nil {
			return nil, err
		}
		if err := validateLatestPrefix(previous, page.Items); err != nil {
			return nil, err
		}
		previous = page.Items
		accepted := 0
		for _, candidate := range previous {
			id, ok := personalItemID(candidate)
			if !ok {
				return nil, fmt.Errorf("Latest candidate has malformed Id")
			}
			state := snapshot.States[id]
			state.ItemID = id
			if personalStateMatches(state, plan.Predicates) {
				accepted++
			}
		}
		if accepted >= requested || page.Terminal {
			return previous, nil
		}
		if fetchLimit == latestPersonalScanLimit {
			return nil, fmt.Errorf("%w: Latest scan reached %d items", ErrPersonalScanIncomplete, latestPersonalScanLimit)
		}
	}
}

func attachLatestRanks(candidates []map[string]any) ([]map[string]any, error) {
	owned := make([]map[string]any, len(candidates))
	for i, candidate := range candidates {
		if _, exists := personalField(candidate, latestSyntheticMarker); exists {
			return nil, fmt.Errorf("Latest candidate %d contains reserved field %q", i, latestSyntheticMarker)
		}
		owned[i] = clonePlannedPersonalJSONMap(candidate)
		deletePersonalField(owned[i], latestSyntheticRank)
		owned[i][latestSyntheticRank] = float64(i)
		owned[i][latestSyntheticMarker] = map[string]any{latestSyntheticRank: true}
	}
	return owned, nil
}

func latestNextFetchLimit(current int) int {
	if current < 1 {
		return 1
	}
	next := current * 2
	if next <= current {
		return latestPersonalScanLimit
	}
	return next
}

func validateLatestPrefix(previous, current []map[string]any) error {
	if len(current) < len(previous) {
		return fmt.Errorf("Latest upstream response shortened its prior prefix")
	}
	seen := make(map[string]struct{}, len(current))
	for i, item := range current {
		id, ok := personalItemID(item)
		if !ok || !safeItemID(id) {
			return fmt.Errorf("Latest candidate %d has malformed Id", i)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("Latest upstream response contains duplicate Id %q", id)
		}
		seen[id] = struct{}{}
		if i < len(previous) {
			priorID, priorOK := personalItemID(previous[i])
			if !priorOK || priorID != id {
				return fmt.Errorf("Latest upstream response changed its ID prefix at %d", i)
			}
		}
	}
	return nil
}

func groupLatestPersonalItems(ctx context.Context, source *personalPlanSource, plan personalPlan, snapshot personalStateSnapshot, items []resolvedPersonalItem) ([]resolvedPersonalItem, error) {
	groups := make(map[string][]resolvedPersonalItem)
	order := make([]string, 0, len(items))
	individuals := make([]resolvedPersonalItem, 0, len(items))
	for _, item := range items {
		if !strings.EqualFold(personalStringField(item.item, "Type"), "Episode") {
			individuals = append(individuals, item)
			continue
		}
		seriesID := personalStringField(item.item, "SeriesId")
		if seriesID == "" || !safeItemID(seriesID) {
			return nil, fmt.Errorf("Latest Episode %q has missing or unsafe SeriesId", personalItemIDForSort(item))
		}
		if _, exists := groups[seriesID]; !exists {
			order = append(order, seriesID)
		}
		groups[seriesID] = append(groups[seriesID], item)
	}
	if len(order) == 0 {
		return individuals, nil
	}
	parents, err := source.resolveIDs(ctx, personalPlan{Kind: personalPlanLatest, Projection: plan.Projection, Sort: plan.Sort}, snapshot, order)
	if err != nil {
		return nil, err
	}
	if len(parents) != len(order) {
		return nil, fmt.Errorf("Latest parent resolution returned %d of %d series", len(parents), len(order))
	}
	byID := make(map[string]resolvedPersonalItem, len(parents))
	requested := make(map[string]struct{}, len(order))
	for _, id := range order {
		requested[id] = struct{}{}
	}
	for _, parent := range parents {
		id, ok := personalItemID(parent.item)
		if !ok || !strings.EqualFold(personalStringField(parent.item, "Type"), "Series") {
			return nil, fmt.Errorf("Latest parent resolution returned incompatible series item")
		}
		if _, ok := requested[id]; !ok {
			return nil, fmt.Errorf("Latest parent resolution returned unrequested ID %q", id)
		}
		if _, exists := byID[id]; exists {
			return nil, fmt.Errorf("Latest parent resolution returned duplicate ID %q", id)
		}
		if _, exists := personalField(parent.item, latestSyntheticMarker); exists {
			return nil, fmt.Errorf("Latest parent %q contains reserved field %q", id, latestSyntheticMarker)
		}
		byID[id] = parent
	}
	result := append([]resolvedPersonalItem(nil), individuals...)
	for _, seriesID := range order {
		parent, ok := byID[seriesID]
		if !ok {
			return nil, fmt.Errorf("Latest parent %q was not resolved", seriesID)
		}
		children := groups[seriesID]
		parent.item = clonePlannedPersonalJSONMap(parent.item)
		deletePersonalField(parent.item, latestSyntheticRank)
		for key := range parent.item {
			switch strings.ToLower(key) {
			case "childcount", "recursiveitemcount", "recursivechildcount", "count", "itemcount", "unplayeditemcount":
				delete(parent.item, key)
			}
		}
		if rank, exists := personalField(children[0].item, latestSyntheticRank); exists {
			parent.item[latestSyntheticRank] = rank
		} else {
			return nil, fmt.Errorf("Latest group %q has no internal rank", seriesID)
		}
		parent.item["ChildCount"] = len(children)
		synthetic := map[string]any{latestSyntheticRank: true}
		// The first child supplies the upstream latest rank and sort metadata.
		for _, term := range plan.Sort {
			if term.Source != personalSortMetadata {
				continue
			}
			if _, exists := personalField(parent.item, term.Name); exists {
				continue
			}
			if value, exists := personalField(children[0].item, term.Name); exists {
				parent.item[term.Name] = value
				synthetic[term.Name] = true
			}
		}
		parent.item[latestSyntheticMarker] = synthetic
		result = append(result, parent)
	}
	return result, nil
}

func removeLatestSyntheticSortFields(items []resolvedPersonalItem) {
	for _, item := range items {
		markerValue, exists := personalField(item.item, latestSyntheticMarker)
		marker, ok := markerValue.(map[string]any)
		if !ok {
			if exists {
				deletePersonalField(item.item, latestSyntheticMarker)
			}
			continue
		}
		for key := range marker {
			deletePersonalField(item.item, key)
		}
		deletePersonalField(item.item, latestSyntheticMarker)
	}
}

func personalStringField(item map[string]any, name string) string {
	value, ok := personalField(item, name)
	if !ok {
		return ""
	}
	result, _ := value.(string)
	return result
}

func personalField(item map[string]any, name string) (any, bool) {
	for key, value := range item {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return nil, false
}

func deletePersonalField(item map[string]any, name string) {
	for key := range item {
		if strings.EqualFold(key, name) {
			delete(item, key)
		}
	}
}
