package gateway

import (
	"context"
	"fmt"
	"math"
	"strings"
)

const (
	latestPersonalScanLimit       = 10000
	latestGroupedBudgetCandidates = 10000
	latestGroupedBudgetRequests   = 256
	latestSyntheticMarker         = "__personalLatestSynthetic"
	latestSyntheticRank           = "LatestRank"
)

// latestGroupedBudget bounds decoded candidates and upstream requests across
// adaptive discovery, parent resolution, and per-series exact-count fetches.
type latestGroupedBudget struct {
	candidates    int
	requests      int
	maxCandidates int
	maxRequests   int
}

func newLatestGroupedBudget() *latestGroupedBudget {
	return &latestGroupedBudget{
		maxCandidates: latestGroupedBudgetCandidates,
		maxRequests:   latestGroupedBudgetRequests,
	}
}

func (b *latestGroupedBudget) reserveRequest() error {
	if b == nil {
		return nil
	}
	if b.requests+1 > b.maxRequests {
		return fmt.Errorf("%w: Latest grouped request budget exhausted (%d)", ErrPersonalScanIncomplete, b.maxRequests)
	}
	b.requests++
	return nil
}

func (b *latestGroupedBudget) addCandidates(n int) error {
	if b == nil {
		return nil
	}
	if n < 0 {
		n = 0
	}
	if b.candidates+n > b.maxCandidates {
		return fmt.Errorf("%w: Latest grouped candidate budget exhausted (%d)", ErrPersonalScanIncomplete, b.maxCandidates)
	}
	b.candidates += n
	return nil
}

func (b *latestGroupedBudget) remainingCandidates() int {
	if b == nil {
		return latestPersonalScanLimit
	}
	rem := b.maxCandidates - b.candidates
	if rem < 0 {
		return 0
	}
	return rem
}

// clampLatestOutboundLimit bounds every Latest upstream Limit to the scan cap and
// remaining candidate budget. Overflow-safe: want is never used as an unbounded
// outbound Limit. A zero remaining budget yields ErrPersonalScanIncomplete.
func clampLatestOutboundLimit(want int, budget *latestGroupedBudget) (int, error) {
	if want <= 0 {
		return 0, fmt.Errorf("invalid Latest outbound limit")
	}
	maxAllowed := latestPersonalScanLimit
	if budget != nil {
		rem := budget.remainingCandidates()
		if rem <= 0 {
			return 0, fmt.Errorf("%w: Latest candidate budget exhausted (%d)", ErrPersonalScanIncomplete, budget.maxCandidates)
		}
		if rem < maxAllowed {
			maxAllowed = rem
		}
	}
	if want > maxAllowed {
		return maxAllowed, nil
	}
	return want, nil
}

// latestNeededWithinBudget reports whether StartIndex+Limit group pagination can
// possibly be proven under the global discovery/candidate bounds. Overflow in
// start+limit is treated as unsatisfiable.
func latestNeededWithinBudget(start, limit int) (needed int, ok bool) {
	if start < 0 || limit < 0 {
		return 0, false
	}
	if limit == 0 {
		return 0, true
	}
	if start > 0 && limit > math.MaxInt-start {
		return 0, false
	}
	needed = start + limit
	if needed < start || needed < limit {
		return 0, false
	}
	if needed > latestPersonalScanLimit || needed > latestGroupedBudgetCandidates {
		return needed, false
	}
	return needed, true
}

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

	// Default natural LatestRank + GroupItems uses adaptive prefix discovery and
	// ParentId-scoped exact ChildCount under a global request/candidate budget.
	if plan.Group.Items && latestUsesNaturalSort(plan.Sort) {
		needed, ok := latestNeededWithinBudget(plan.Page.Start, limit)
		if !ok {
			return personalPlanResult{}, fmt.Errorf("%w: Latest grouped Limit/StartIndex exceeds scan budget", ErrPersonalScanIncomplete)
		}
		return executeLatestGroupedNaturalPlan(ctx, source, plan, snapshot, needed)
	}

	var candidates []map[string]any
	if plan.Group.Items {
		// Custom-sort grouped: keep complete one-shot scan; 503 at existing bound.
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

func executeLatestGroupedNaturalPlan(ctx context.Context, source *personalPlanSource, plan personalPlan, snapshot personalStateSnapshot, needed int) (personalPlanResult, error) {
	budget := newLatestGroupedBudget()
	prevBudget := source.latestBudget
	source.latestBudget = budget
	defer func() { source.latestBudget = prevBudget }()

	previous := []map[string]any{}
	fetchLimit, err := clampLatestOutboundLimit(needed, budget)
	if err != nil {
		return personalPlanResult{}, err
	}
	if fetchLimit < 1 {
		fetchLimit = 1
	}
	terminal := false
	parents := make(map[string]resolvedPersonalItem)
	missingParents := make(map[string]struct{})
	childCounts := make(map[string]int)
	childCountKnown := make(map[string]struct{})
	zeroChild := make(map[string]struct{})
	outputCap := needed
	if outputCap > latestPersonalScanLimit {
		outputCap = latestPersonalScanLimit
	}

	for {
		for {
			page, err := source.fetchCandidatePage(ctx, plan, 0, fetchLimit)
			if err != nil {
				return personalPlanResult{}, err
			}
			if err := validateLatestPrefix(previous, page.Items); err != nil {
				return personalPlanResult{}, err
			}
			previous = page.Items
			terminal = page.Terminal

			filtered, err := latestFilterCandidates(previous, snapshot, plan.Predicates)
			if err != nil {
				return personalPlanResult{}, err
			}
			groups, err := latestProvisionalGroups(filtered)
			if err != nil {
				return personalPlanResult{}, err
			}
			if terminal || latestUsableGroupCount(groups, missingParents, zeroChild) >= needed {
				break
			}
			if fetchLimit >= latestPersonalScanLimit {
				return personalPlanResult{}, fmt.Errorf("%w: Latest grouped discovery reached %d items", ErrPersonalScanIncomplete, latestPersonalScanLimit)
			}
			next, err := clampLatestOutboundLimit(latestNextFetchLimit(fetchLimit), budget)
			if err != nil {
				return personalPlanResult{}, err
			}
			if next <= fetchLimit {
				return personalPlanResult{}, fmt.Errorf("%w: Latest grouped discovery cannot grow past %d", ErrPersonalScanIncomplete, fetchLimit)
			}
			fetchLimit = next
		}

		filtered, err := latestFilterCandidates(previous, snapshot, plan.Predicates)
		if err != nil {
			return personalPlanResult{}, err
		}
		groups, err := latestProvisionalGroups(filtered)
		if err != nil {
			return personalPlanResult{}, err
		}

		// Resolve parents for series not yet classified.
		unresolved := make([]string, 0)
		seenUnresolved := make(map[string]struct{})
		for _, group := range groups {
			if group.seriesID == "" {
				continue
			}
			if _, ok := parents[group.seriesID]; ok {
				continue
			}
			if _, ok := missingParents[group.seriesID]; ok {
				continue
			}
			if _, ok := seenUnresolved[group.seriesID]; ok {
				continue
			}
			seenUnresolved[group.seriesID] = struct{}{}
			unresolved = append(unresolved, group.seriesID)
		}
		if len(unresolved) > 0 {
			resolved, err := source.resolveIDs(ctx, personalPlan{Kind: personalPlanLatest, Projection: plan.Projection, Sort: plan.Sort}, snapshot, unresolved)
			if err != nil {
				return personalPlanResult{}, err
			}
			if err := ingestLatestResolvedParents(parents, missingParents, unresolved, resolved); err != nil {
				return personalPlanResult{}, err
			}
		}

		outputs := make([]resolvedPersonalItem, 0, outputCap)
		seriesRequested := 0
		resolvedParentCount := 0
		hadIndividual := false
		for _, group := range groups {
			if len(outputs) >= needed {
				break
			}
			if group.seriesID == "" {
				hadIndividual = true
				outputs = append(outputs, group.first)
				continue
			}
			seriesRequested++
			if _, missing := missingParents[group.seriesID]; missing {
				continue
			}
			if _, zero := zeroChild[group.seriesID]; zero {
				continue
			}
			parent, ok := parents[group.seriesID]
			if !ok {
				continue
			}
			resolvedParentCount++
			if _, known := childCountKnown[group.seriesID]; !known {
				count, err := countLatestSeriesChildren(ctx, source, plan, snapshot, group.seriesID)
				if err != nil {
					return personalPlanResult{}, err
				}
				childCounts[group.seriesID] = count
				childCountKnown[group.seriesID] = struct{}{}
				if count == 0 {
					zeroChild[group.seriesID] = struct{}{}
					continue
				}
			}
			count := childCounts[group.seriesID]
			if count == 0 {
				continue
			}
			built, err := buildLatestGroupedSeriesItem(parent, group.first, count, plan)
			if err != nil {
				return personalPlanResult{}, err
			}
			outputs = append(outputs, built)
		}

		if len(outputs) >= needed || terminal {
			if len(outputs) == 0 && seriesRequested > 0 && resolvedParentCount == 0 && !hadIndividual {
				return personalPlanResult{}, fmt.Errorf("Latest parent resolution returned 0 of %d series", seriesRequested)
			}
			sortPersonalPlanItems(outputs, plan.Sort)
			removeLatestSyntheticSortFields(outputs)
			return personalPlanResult{Items: pagePersonalPlanItems(outputs, plan.Page), StartIndex: 0}, nil
		}

		// Stale/zero-child drops require more discovery when the library is nonterminal.
		if fetchLimit >= latestPersonalScanLimit {
			return personalPlanResult{}, fmt.Errorf("%w: Latest grouped discovery cannot satisfy Limit within %d items", ErrPersonalScanIncomplete, latestPersonalScanLimit)
		}
		next, err := clampLatestOutboundLimit(latestNextFetchLimit(fetchLimit), budget)
		if err != nil {
			return personalPlanResult{}, err
		}
		if next <= fetchLimit {
			return personalPlanResult{}, fmt.Errorf("%w: Latest grouped discovery cannot grow past %d", ErrPersonalScanIncomplete, fetchLimit)
		}
		fetchLimit = next
	}
}

type latestProvisionalGroup struct {
	seriesID string // empty for non-episode individual
	first    resolvedPersonalItem
}

func latestFilterCandidates(candidates []map[string]any, snapshot personalStateSnapshot, predicates personalPredicates) ([]resolvedPersonalItem, error) {
	ranked, err := attachLatestRanks(candidates)
	if err != nil {
		return nil, err
	}
	joined, err := joinPersonalCandidates(ranked, snapshot.States, false)
	if err != nil {
		return nil, err
	}
	filtered := make([]resolvedPersonalItem, 0, len(joined))
	for _, item := range joined {
		if personalStateMatches(item.state, predicates) {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

func latestProvisionalGroups(filtered []resolvedPersonalItem) ([]latestProvisionalGroup, error) {
	groups := make([]latestProvisionalGroup, 0)
	seenSeries := make(map[string]struct{})
	for _, item := range filtered {
		if !strings.EqualFold(personalStringField(item.item, "Type"), "Episode") {
			groups = append(groups, latestProvisionalGroup{first: item})
			continue
		}
		seriesID := personalStringField(item.item, "SeriesId")
		if seriesID == "" || !safeItemID(seriesID) {
			return nil, fmt.Errorf("Latest Episode %q has missing or unsafe SeriesId", personalItemIDForSort(item))
		}
		if _, exists := seenSeries[seriesID]; exists {
			continue
		}
		seenSeries[seriesID] = struct{}{}
		groups = append(groups, latestProvisionalGroup{seriesID: seriesID, first: item})
	}
	return groups, nil
}

func latestUsableGroupCount(groups []latestProvisionalGroup, missingParents, zeroChild map[string]struct{}) int {
	count := 0
	for _, group := range groups {
		if group.seriesID == "" {
			count++
			continue
		}
		if _, ok := missingParents[group.seriesID]; ok {
			continue
		}
		if _, ok := zeroChild[group.seriesID]; ok {
			continue
		}
		count++
	}
	return count
}

func ingestLatestResolvedParents(parents map[string]resolvedPersonalItem, missing map[string]struct{}, requested []string, resolved []resolvedPersonalItem) error {
	requestedSet := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		requestedSet[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(resolved))
	for _, parent := range resolved {
		id, ok := personalItemID(parent.item)
		if !ok || !strings.EqualFold(personalStringField(parent.item, "Type"), "Series") {
			return fmt.Errorf("Latest parent resolution returned incompatible series item")
		}
		if _, ok := requestedSet[id]; !ok {
			return fmt.Errorf("Latest parent resolution returned unrequested ID %q", id)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("Latest parent resolution returned duplicate ID %q", id)
		}
		if _, exists := personalField(parent.item, latestSyntheticMarker); exists {
			return fmt.Errorf("Latest parent %q contains reserved field %q", id, latestSyntheticMarker)
		}
		seen[id] = struct{}{}
		parents[id] = parent
	}
	for _, id := range requested {
		if _, ok := parents[id]; !ok {
			missing[id] = struct{}{}
		}
	}
	return nil
}

func countLatestSeriesChildren(ctx context.Context, source *personalPlanSource, plan personalPlan, snapshot personalStateSnapshot, seriesID string) (int, error) {
	scoped := plan
	scoped.Neutral = cloneQuery(plan.Neutral)
	scoped.Neutral.Set("ParentId", seriesID)
	scoped.Neutral.Set("GroupItems", "false")

	previous := []map[string]any{}
	fetchLimit, err := clampLatestOutboundLimit(20, source.latestBudget)
	if err != nil {
		return 0, err
	}
	for {
		page, err := source.fetchCandidatePage(ctx, scoped, 0, fetchLimit)
		if err != nil {
			return 0, err
		}
		if err := validateLatestPrefix(previous, page.Items); err != nil {
			return 0, err
		}
		previous = page.Items
		accepted := 0
		for _, candidate := range previous {
			id, ok := personalItemID(candidate)
			if !ok || !safeItemID(id) {
				return 0, fmt.Errorf("Latest scoped child has malformed Id")
			}
			// Episodes under a series parent must not claim a different SeriesId.
			if strings.EqualFold(personalStringField(candidate, "Type"), "Episode") {
				got := personalStringField(candidate, "SeriesId")
				if got != "" && got != seriesID {
					return 0, fmt.Errorf("Latest scoped child SeriesId mismatch")
				}
			}
			state := snapshot.States[id]
			state.ItemID = id
			if personalStateMatches(state, plan.Predicates) {
				accepted++
			}
		}
		if page.Terminal {
			return accepted, nil
		}
		if fetchLimit >= latestPersonalScanLimit {
			return 0, fmt.Errorf("%w: Latest series child count reached %d items", ErrPersonalScanIncomplete, latestPersonalScanLimit)
		}
		next, err := clampLatestOutboundLimit(latestNextFetchLimit(fetchLimit), source.latestBudget)
		if err != nil {
			return 0, err
		}
		if next <= fetchLimit {
			return 0, fmt.Errorf("%w: Latest series child count cannot grow past %d", ErrPersonalScanIncomplete, fetchLimit)
		}
		fetchLimit = next
	}
}

func buildLatestGroupedSeriesItem(parent, firstChild resolvedPersonalItem, childCount int, plan personalPlan) (resolvedPersonalItem, error) {
	parent.item = clonePlannedPersonalJSONMap(parent.item)
	deletePersonalField(parent.item, latestSyntheticRank)
	for key := range parent.item {
		switch strings.ToLower(key) {
		case "childcount", "recursiveitemcount", "recursivechildcount", "count", "itemcount", "unplayeditemcount":
			delete(parent.item, key)
		}
	}
	if rank, exists := personalField(firstChild.item, latestSyntheticRank); exists {
		parent.item[latestSyntheticRank] = rank
	} else {
		return resolvedPersonalItem{}, fmt.Errorf("Latest group has no internal rank")
	}
	parent.item["ChildCount"] = childCount
	synthetic := map[string]any{latestSyntheticRank: true}
	for _, term := range plan.Sort {
		if term.Source != personalSortMetadata {
			continue
		}
		if _, exists := personalField(parent.item, term.Name); exists {
			continue
		}
		if value, exists := personalField(firstChild.item, term.Name); exists {
			parent.item[term.Name] = value
			synthetic[term.Name] = true
		}
	}
	parent.item[latestSyntheticMarker] = synthetic
	return parent, nil
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
	fetchLimit, err := clampLatestOutboundLimit(requested, source.latestBudget)
	if err != nil {
		return nil, err
	}
	for {
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
		if fetchLimit >= latestPersonalScanLimit {
			return nil, fmt.Errorf("%w: Latest scan reached %d items", ErrPersonalScanIncomplete, latestPersonalScanLimit)
		}
		next, err := clampLatestOutboundLimit(latestNextFetchLimit(fetchLimit), source.latestBudget)
		if err != nil {
			return nil, err
		}
		if next <= fetchLimit {
			return nil, fmt.Errorf("%w: Latest scan cannot grow past %d", ErrPersonalScanIncomplete, fetchLimit)
		}
		fetchLimit = next
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
	if current >= latestPersonalScanLimit {
		return latestPersonalScanLimit
	}
	// Overflow-safe doubling: stop at the scan bound rather than wrapping.
	if current > latestPersonalScanLimit/2 {
		return latestPersonalScanLimit
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

// groupLatestPersonalItems groups a complete filtered candidate set for custom-sort
// Latest. Natural-order grouping uses executeLatestGroupedNaturalPlan instead.
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
	// Successful, structurally valid parent responses may omit stale/deleted
	// series. Drop those groups and let later valid groups backfill Limit.
	result := append([]resolvedPersonalItem(nil), individuals...)
	resolvedParents := 0
	for _, seriesID := range order {
		parent, ok := byID[seriesID]
		if !ok {
			continue
		}
		resolvedParents++
		children := groups[seriesID]
		built, err := buildLatestGroupedSeriesItem(parent, children[0], len(children), plan)
		if err != nil {
			return nil, fmt.Errorf("Latest group %q: %w", seriesID, err)
		}
		result = append(result, built)
	}
	if resolvedParents == 0 && len(result) == 0 {
		return nil, fmt.Errorf("Latest parent resolution returned 0 of %d series", len(order))
	}
	// Audit/telemetry: no established success-path degradation hook on the
	// Latest personal execution path (requested/resolved/omitted counts only).
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
