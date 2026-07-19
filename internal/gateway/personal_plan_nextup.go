package gateway

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type nextUpControls struct {
	seriesID        string
	parentID        string
	enableResumable bool
	cutoff          *time.Time
}

type nextUpEpisode struct {
	item      map[string]any
	id        string
	series    string
	season    string
	seasonNo  int
	episodeNo int
}

type nextUpSeries struct {
	id            string
	activity      time.Time
	latestPlayed  time.Time
	hasPlayedDate bool
	latestUpdated time.Time
	progress      []resolvedPersonalItem
}

type nextUpSelected struct {
	item     resolvedPersonalItem
	activity time.Time
}

type nextUpScanBudget struct {
	pages int
	items int
	ids   map[string]struct{}
}

func executeNextUpPersonalPlan(ctx context.Context, source *personalPlanSource, plan personalPlan) (personalPlanResult, error) {
	if source == nil || source.session == nil {
		return personalPlanResult{}, fmt.Errorf("NextUp executor requires a source and session")
	}
	if plan.Kind != personalPlanNextUp || plan.Shape != personalShapeQueryResult {
		return personalPlanResult{}, fmt.Errorf("NextUp executor requires a NextUp QueryResult plan")
	}
	controls, err := parseNextUpControls(plan.Neutral)
	if err != nil {
		return personalPlanResult{}, err
	}
	snapshot, err := source.snapshot(ctx)
	if err != nil {
		return personalPlanResult{}, err
	}
	ids := make([]string, 0)
	for id, state := range snapshot.States {
		if state.Played || state.PlaybackPositionTicks > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return nextUpResult(nil, plan), nil
	}
	maxItems := plan.Scan.MaxItems
	if maxItems <= 0 || maxItems > personalPlanScanMaxItems {
		maxItems = personalPlanScanMaxItems
	}
	if len(ids) > maxItems {
		return personalPlanResult{}, fmt.Errorf("%w: %d progress IDs exceed maximum of %d", ErrPersonalScanIncomplete, len(ids), maxItems)
	}
	resolvePlan := plan
	resolvePlan.Projection = cloneQuery(plan.Projection)
	appendNextUpFields(resolvePlan.Projection, "Type", "SeriesId", "SeasonId", "ParentIndexNumber", "IndexNumber")
	resolved, err := source.resolveIDs(ctx, resolvePlan, snapshot, ids)
	if err != nil {
		return personalPlanResult{}, err
	}
	series := make(map[string]*nextUpSeries)
	for _, joined := range resolved {
		typ, ok := nextUpStringField(joined.item, "Type")
		if !ok {
			return personalPlanResult{}, fmt.Errorf("resolved NextUp item has missing or malformed Type")
		}
		if !strings.EqualFold(typ, "Episode") {
			continue
		}
		episode, err := parseNextUpEpisode(joined.item)
		if err != nil {
			return personalPlanResult{}, err
		}
		if controls.seriesID != "" && episode.series != controls.seriesID {
			continue
		}
		state := joined.state
		entry := series[episode.series]
		if entry == nil {
			entry = &nextUpSeries{id: episode.series}
			series[episode.series] = entry
		}
		if state.LastPlayedDate != nil && (!entry.hasPlayedDate || state.LastPlayedDate.After(entry.latestPlayed)) {
			entry.latestPlayed = *state.LastPlayedDate
			entry.hasPlayedDate = true
		}
		if state.UpdatedAt.After(entry.latestUpdated) {
			entry.latestUpdated = state.UpdatedAt
		}
		entry.progress = append(entry.progress, resolvedPersonalItem{item: joined.item, state: state})
	}
	for id, entry := range series {
		if entry.hasPlayedDate {
			entry.activity = entry.latestPlayed
		} else {
			entry.activity = entry.latestUpdated
		}
		if controls.cutoff != nil && entry.activity.Before(*controls.cutoff) {
			delete(series, id)
		}
	}
	if len(series) == 0 {
		return nextUpResult(nil, plan), nil
	}
	if controls.parentID != "" {
		series, err = refineNextUpSeries(ctx, source, plan, controls.parentID, series)
		if err != nil {
			return personalPlanResult{}, err
		}
	}
	candidates := make([]*nextUpSeries, 0, len(series))
	for _, candidate := range series {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].activity.Equal(candidates[j].activity) {
			return candidates[i].activity.After(candidates[j].activity)
		}
		return candidates[i].id < candidates[j].id
	})
	budget := &nextUpScanBudget{ids: make(map[string]struct{})}
	selectedItems := make([]nextUpSelected, 0, len(series))
	for _, candidate := range candidates {
		selected, err := scanNextUpSeries(ctx, source, plan, candidate, controls.enableResumable, budget, snapshot.States)
		if err != nil {
			return personalPlanResult{}, err
		}
		if selected != nil {
			selectedItems = append(selectedItems, nextUpSelected{item: *selected, activity: candidate.activity})
		}
	}
	sort.Slice(selectedItems, func(i, j int) bool {
		ai := selectedItems[i].activity
		aj := selectedItems[j].activity
		if !ai.Equal(aj) {
			return ai.After(aj)
		}
		ei, _ := parseNextUpEpisode(selectedItems[i].item.item)
		ej, _ := parseNextUpEpisode(selectedItems[j].item.item)
		if cmp := compareNextUpOrder(ei, ej); cmp != 0 {
			return cmp < 0
		}
		return personalItemIDForSort(selectedItems[i].item) < personalItemIDForSort(selectedItems[j].item)
	})
	result := make([]resolvedPersonalItem, len(selectedItems))
	for i := range selectedItems {
		result[i] = selectedItems[i].item
	}
	return nextUpResult(result, plan), nil
}

func parseNextUpControls(neutral url.Values) (nextUpControls, error) {
	controls := nextUpControls{enableResumable: true}
	seen := make(map[string]struct{})
	for key, values := range neutral {
		if !strings.EqualFold(key, "SeriesId") && !strings.EqualFold(key, "ParentId") && !strings.EqualFold(key, "EnableResumable") && !strings.EqualFold(key, "NextUpDateCutoff") {
			continue
		}
		folded := strings.ToLower(key)
		if _, exists := seen[folded]; exists {
			return controls, personalPlanBadRequest("NextUp control %q is repeated", key)
		}
		seen[folded] = struct{}{}
		if len(values) != 1 {
			return controls, personalPlanBadRequest("NextUp control %q must be scalar", key)
		}
		value := values[0]
		switch strings.ToLower(key) {
		case "seriesid":
			if !safeItemID(value) {
				return controls, personalPlanBadRequest("SeriesId is unsafe")
			}
			controls.seriesID = value
		case "parentid":
			if !safeItemID(value) {
				return controls, personalPlanBadRequest("ParentId is unsafe")
			}
			controls.parentID = value
		case "enableresumable":
			parsed, err := parsePersonalBool(value)
			if err != nil {
				return controls, personalPlanBadRequest("EnableResumable must be a boolean")
			}
			controls.enableResumable = parsed
		case "nextupdatecutoff":
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return controls, personalPlanBadRequest("NextUpDateCutoff must be RFC3339")
			}
			controls.cutoff = &parsed
		}
	}
	return controls, nil
}

func parseNextUpEpisode(item map[string]any) (nextUpEpisode, error) {
	id, ok := personalItemID(item)
	if !ok || !safeItemID(id) {
		return nextUpEpisode{}, fmt.Errorf("invalid NextUp episode Id")
	}
	typ, ok := nextUpStringField(item, "Type")
	if !ok || !strings.EqualFold(typ, "Episode") {
		return nextUpEpisode{}, fmt.Errorf("NextUp item %q is not an Episode", id)
	}
	series, ok := nextUpStringField(item, "SeriesId")
	if !ok || !safeItemID(series) {
		return nextUpEpisode{}, fmt.Errorf("NextUp episode %q has invalid SeriesId", id)
	}
	season, ok := nextUpStringField(item, "SeasonId")
	if !ok || !safeItemID(season) {
		return nextUpEpisode{}, fmt.Errorf("NextUp episode %q has invalid SeasonId", id)
	}
	seasonNo, ok := nextUpNonnegativeInt(item, "ParentIndexNumber")
	if !ok {
		return nextUpEpisode{}, fmt.Errorf("NextUp episode %q has invalid ParentIndexNumber", id)
	}
	episodeNo, ok := nextUpNonnegativeInt(item, "IndexNumber")
	if !ok {
		return nextUpEpisode{}, fmt.Errorf("NextUp episode %q has invalid IndexNumber", id)
	}
	return nextUpEpisode{item: item, id: id, series: series, season: season, seasonNo: seasonNo, episodeNo: episodeNo}, nil
}

func nextUpStringField(item map[string]any, name string) (string, bool) {
	for key, value := range item {
		if strings.EqualFold(key, name) {
			v, ok := value.(string)
			return v, ok && v != ""
		}
	}
	return "", false
}

func nextUpNonnegativeInt(item map[string]any, name string) (int, bool) {
	for key, value := range item {
		if !strings.EqualFold(key, name) {
			continue
		}
		var n float64
		switch v := value.(type) {
		case float64:
			n = v
		case int:
			return v, v >= 0
		case int64:
			if v < 0 || (strconv.IntSize == 32 && v > math.MaxInt32) {
				return 0, false
			}
			return int(v), true
		default:
			return 0, false
		}
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n != math.Trunc(n) || n >= math.Ldexp(1, strconv.IntSize-1) {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

func refineNextUpSeries(ctx context.Context, source *personalPlanSource, plan personalPlan, parentID string, series map[string]*nextUpSeries) (map[string]*nextUpSeries, error) {
	items := make([]resolvedPersonalItem, 0, len(series))
	for id := range series {
		items = append(items, resolvedPersonalItem{item: map[string]any{"Id": id, "Type": "Series"}, state: PlaybackState{ItemID: id, GatewayUserID: source.session.GatewayUserID, SyntheticUserID: source.session.SyntheticUserID}})
	}
	sort.Slice(items, func(i, j int) bool { return personalItemIDForSort(items[i]) < personalItemIDForSort(items[j]) })
	refine := plan
	refine.Refinement = url.Values{"ParentId": {parentID}, "IncludeItemTypes": {"Series"}}
	refined, err := source.refineResolved(ctx, refine, items)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]struct{}, len(refined))
	for _, item := range refined {
		id, ok := personalItemID(item.item)
		if !ok {
			return nil, fmt.Errorf("refined NextUp series has invalid Id")
		}
		allowed[id] = struct{}{}
	}
	out := make(map[string]*nextUpSeries)
	for id, value := range series {
		if _, ok := allowed[id]; ok {
			out[id] = value
		}
	}
	return out, nil
}

func scanNextUpSeries(ctx context.Context, source *personalPlanSource, plan personalPlan, candidate *nextUpSeries, enableResumable bool, budget *nextUpScanBudget, states map[string]PlaybackState) (*resolvedPersonalItem, error) {
	scanPlan := plan
	scanPlan.Path = "/Shows/" + candidate.id + "/Episodes"
	scanPlan.Neutral = cloneQuery(plan.Neutral)
	deleteNextUpControls(scanPlan.Neutral)
	scanPlan.Neutral.Set("UserId", source.session.SyntheticUserID)
	scanPlan.Neutral.Set("IncludeItemTypes", "Episode")
	copyResolutionFields(scanPlan.Neutral, plan.Projection)
	appendNextUpFields(scanPlan.Neutral, "Type", "SeriesId", "SeasonId", "ParentIndexNumber", "IndexNumber")
	scanPlan.Shape = personalShapeQueryResult
	scan := newPersonalPlanScan(personalScanPolicy{PageSize: personalPlanScanPageSize, MaxPages: personalPlanScanMaxPages, MaxItems: personalPlanScanMaxItems})
	for !scan.Complete() {
		if budget.pages >= personalPlanScanMaxPages || budget.items >= personalPlanScanMaxItems {
			return nil, fmt.Errorf("%w: global NextUp episode budget exhausted", ErrPersonalScanIncomplete)
		}
		page, err := source.fetchCandidatePage(ctx, scanPlan, scan.NextStart(), personalPlanScanPageSize)
		if err != nil {
			return nil, err
		}
		budget.pages++
		budget.items += len(page.Items)
		if budget.items > personalPlanScanMaxItems {
			return nil, fmt.Errorf("%w: global NextUp episode item budget exceeded", ErrPersonalScanIncomplete)
		}
		if err = scan.Add(page); err != nil {
			return nil, err
		}
		if !scan.Complete() && (budget.pages >= personalPlanScanMaxPages || budget.items >= personalPlanScanMaxItems) {
			return nil, fmt.Errorf("%w: global NextUp episode budget requires another page", ErrPersonalScanIncomplete)
		}
	}
	all := make(map[string]nextUpEpisode, scan.CandidateCount())
	for _, item := range scan.Candidates() {
		episode, err := parseNextUpEpisode(item)
		if err != nil || episode.series != candidate.id {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("NextUp episode has wrong SeriesId")
		}
		if _, exists := budget.ids[episode.id]; exists {
			return nil, fmt.Errorf("duplicate global NextUp episode %q", episode.id)
		}
		budget.ids[episode.id] = struct{}{}
		all[episode.id] = episode
	}
	progress := make(map[string]resolvedPersonalItem, len(candidate.progress))
	for _, item := range candidate.progress {
		id, _ := personalItemID(item.item)
		if _, ok := all[id]; !ok {
			return nil, fmt.Errorf("NextUp progress anchor %q absent from episode universe", id)
		}
		progress[id] = item
	}
	var anchor *nextUpEpisode
	for id := range progress {
		episode := all[id]
		if anchor == nil || compareNextUpOrder(episode, *anchor) > 0 || (compareNextUpOrder(episode, *anchor) == 0 && episode.id > anchor.id) {
			copy := episode
			anchor = &copy
		}
	}
	if anchor == nil {
		return nil, nil
	}
	if joined, ok := progress[anchor.id]; ok && !joined.state.Played && joined.state.PlaybackPositionTicks > 0 && enableResumable {
		return &resolvedPersonalItem{item: all[anchor.id].item, state: joined.state}, nil
	}
	var selected *nextUpEpisode
	for _, episode := range all {
		if compareNextUpOrder(episode, *anchor) <= 0 || (progress[episode.id].state.Played) {
			continue
		}
		if selected == nil || compareNextUpOrder(episode, *selected) < 0 || (compareNextUpOrder(episode, *selected) == 0 && episode.id < selected.id) {
			copy := episode
			selected = &copy
		}
	}
	if selected == nil {
		return nil, nil
	}
	state := PlaybackState{ItemID: selected.id, GatewayUserID: source.session.GatewayUserID, SyntheticUserID: source.session.SyntheticUserID}
	if joined, ok := states[selected.id]; ok {
		if joined.ItemID != selected.id || (joined.GatewayUserID != "" && joined.GatewayUserID != source.session.GatewayUserID) || (joined.SyntheticUserID != "" && joined.SyntheticUserID != source.session.SyntheticUserID) {
			return nil, fmt.Errorf("selected NextUp state %q conflicts with snapshot", selected.id)
		}
		state = joined
	}
	return &resolvedPersonalItem{item: selected.item, state: state}, nil
}

func deleteNextUpControls(query url.Values) {
	for key := range query {
		switch strings.ToLower(key) {
		case "seriesid", "parentid", "enableresumable", "nextupdatecutoff":
			delete(query, key)
		}
	}
}

func appendNextUpFields(query url.Values, required ...string) {
	fields := splitFilterValues(query["Fields"])
	seen := make(map[string]struct{}, len(fields)+len(required))
	for _, field := range fields {
		seen[strings.ToLower(field)] = struct{}{}
	}
	for _, field := range required {
		if _, exists := seen[strings.ToLower(field)]; !exists {
			fields = append(fields, field)
			seen[strings.ToLower(field)] = struct{}{}
		}
	}
	query.Set("Fields", strings.Join(fields, ","))
}

func compareNextUpOrder(a, b nextUpEpisode) int {
	if a.seasonNo < b.seasonNo {
		return -1
	}
	if a.seasonNo > b.seasonNo {
		return 1
	}
	if a.episodeNo < b.episodeNo {
		return -1
	}
	if a.episodeNo > b.episodeNo {
		return 1
	}
	return 0
}
func nextUpResult(items []resolvedPersonalItem, plan personalPlan) personalPlanResult {
	total := len(items)
	page := pagePersonalPlanItems(items, plan.Page)
	return personalPlanResult{Items: page, Total: &total, StartIndex: plan.Page.Start}
}
