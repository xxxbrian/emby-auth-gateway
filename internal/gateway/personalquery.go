package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type personalQuery struct {
	rel          string
	raw          url.Values
	backend      url.Values
	positive     map[string]bool
	negative     map[string]bool
	pathSeriesID string
	pathParentID string
	pathItemType string
	start        int
	limit        int
	sortBy       []string
	sortOrder    []string
}

type resolvedPersonalItem struct {
	item  map[string]any
	state PlaybackState
}

func parsePersonalQuery(rel string, q url.Values) personalQuery {
	pq := personalQuery{
		rel:      rel,
		raw:      cloneQuery(q),
		backend:  cloneQuery(q),
		positive: map[string]bool{},
		negative: map[string]bool{},
		start:    intQuery(q, "StartIndex", 0),
		limit:    intQuery(q, "Limit", 0),
		sortBy:   splitFilterValues(q["SortBy"]),
	}
	pq.sortOrder = splitFilterValues(q["SortOrder"])
	filters := splitFilterValues(q["Filters"])
	remaining := make([]string, 0, len(filters))
	for _, filter := range filters {
		switch strings.ToLower(filter) {
		case "isplayed":
			pq.positive["played"] = true
		case "isunplayed":
			pq.negative["played"] = true
		case "isfavorite":
			pq.positive["favorite"] = true
		case "isresumable":
			pq.positive["resumable"] = true
		default:
			remaining = append(remaining, filter)
		}
	}
	pq.backend.Del("Filters")
	if len(remaining) > 0 {
		pq.backend.Set("Filters", strings.Join(remaining, ","))
	}
	pq.applyBoolFilter("IsPlayed", "played")
	pq.applyBoolFilter("IsFavorite", "favorite")
	pq.applyBoolFilter("IsResumable", "resumable")
	pq.applyPathConstraints(rel)
	return pq
}

func (pq *personalQuery) applyBoolFilter(queryName, key string) {
	raw := pq.raw.Get(queryName)
	if raw == "" {
		return
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		pq.backend.Del(queryName)
		return
	}
	if value {
		pq.positive[key] = true
	} else {
		pq.negative[key] = true
	}
	pq.backend.Del(queryName)
}

func (pq *personalQuery) applyPathConstraints(rel string) {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) == 3 && strings.EqualFold(parts[0], "Shows") && strings.EqualFold(parts[2], "Episodes") {
		pq.pathSeriesID = parts[1]
		pq.pathItemType = "Episode"
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "Shows") && strings.EqualFold(parts[2], "Seasons") {
		pq.pathSeriesID = parts[1]
		pq.pathItemType = "Season"
	}
	if seriesID := strings.TrimSpace(pq.raw.Get("SeriesId")); seriesID != "" {
		pq.pathSeriesID = seriesID
	}
	if parentID := strings.TrimSpace(pq.raw.Get("ParentId")); parentID != "" {
		pq.pathParentID = parentID
	}
}

func (pq personalQuery) hasPositive() bool { return len(pq.positive) > 0 }

func (pq personalQuery) hasOnlyNegative() bool { return len(pq.positive) == 0 && len(pq.negative) > 0 }

func (pq personalQuery) matchesState(state PlaybackState) bool {
	if pq.positive["played"] && !state.Played {
		return false
	}
	if pq.positive["favorite"] && !state.IsFavorite {
		return false
	}
	if pq.positive["resumable"] && (state.Played || state.PlaybackPositionTicks <= 0) {
		return false
	}
	if pq.negative["played"] && state.Played {
		return false
	}
	if pq.negative["favorite"] && state.IsFavorite {
		return false
	}
	if pq.negative["resumable"] && !state.Played && state.PlaybackPositionTicks > 0 {
		return false
	}
	return true
}

func (pq personalQuery) matchesKnownStateStructure(state PlaybackState) bool {
	if pq.pathSeriesID != "" && state.SeriesID != "" && state.SeriesID != pq.pathSeriesID {
		return false
	}
	if pq.pathParentID != "" && state.SeasonID != "" && state.SeasonID != pq.pathParentID {
		return false
	}
	if pq.pathItemType != "" && state.ItemType != "" && !strings.EqualFold(state.ItemType, pq.pathItemType) {
		return false
	}
	if includeTypes := lowerSet(splitFilterValues(pq.raw["IncludeItemTypes"])); len(includeTypes) > 0 && state.ItemType != "" && !includeTypes[strings.ToLower(state.ItemType)] {
		return false
	}
	if excludeTypes := lowerSet(splitFilterValues(pq.raw["ExcludeItemTypes"])); len(excludeTypes) > 0 && excludeTypes[strings.ToLower(state.ItemType)] {
		return false
	}
	return true
}

func isAllowedPersonalItemListPath(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return false
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "Users") && strings.EqualFold(parts[2], "Items") {
		return true
	}
	if len(parts) == 1 && strings.EqualFold(parts[0], "Items") {
		return true
	}
	if len(parts) == 3 && strings.EqualFold(parts[0], "Shows") && (strings.EqualFold(parts[2], "Episodes") || strings.EqualFold(parts[2], "Seasons")) {
		return true
	}
	return false
}

func isClearlyNonItemEndpoint(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return true
	}
	if strings.EqualFold(parts[0], "Users") {
		return len(parts) <= 2
	}
	switch strings.ToLower(parts[0]) {
	case "system", "sessions", "displaypreferences", "devices", "plugins", "scheduledtasks", "startup", "auth", "web", "swagger":
		return true
	default:
		return false
	}
}

func (s *Server) writePositivePersonalItems(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string, pq personalQuery) {
	states, err := s.store.ListPlaybackStates(r.Context(), session.GatewayUserID, PlaybackStateFilter{})
	if err != nil {
		http.Error(w, "items unavailable", http.StatusInternalServerError)
		return
	}
	candidates := make([]PlaybackState, 0, len(states))
	for _, state := range states {
		if pq.matchesState(state) && pq.matchesKnownStateStructure(state) {
			candidates = append(candidates, state)
		}
	}
	if len(candidates) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": pq.start})
		return
	}
	sortStatesForPersonalQuery(candidates, pq)
	ids := playbackStateIDs(candidates)
	resolved, err := s.resolvePersonalItemsByID(r.Context(), r, session, gatewayToken, ids, pq)
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	resolved, err = s.filterResolvedPersonalItemsWithBackend(r.Context(), r, session, gatewayToken, resolved, pq)
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	sortResolvedPersonalItems(resolved, pq)
	total := len(resolved)
	paged := pageResolvedPersonalItems(resolved, pq)
	items := make([]any, 0, len(paged))
	for _, item := range paged {
		items = append(items, item.item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"Items": items, "TotalRecordCount": total, "StartIndex": pq.start})
}

func (s *Server) writeNegativePersonalItems(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string, pq personalQuery) {
	states, err := s.store.ListPlaybackStates(r.Context(), session.GatewayUserID, PlaybackStateFilter{})
	if err != nil {
		http.Error(w, "items unavailable", http.StatusInternalServerError)
		return
	}
	exclude := excludeSetForNegativeQuery(states, pq)
	limit := pq.limit
	noLimit := limit <= 0
	batchLimit := limit * 3
	if noLimit {
		limit = personalScanItemLimit
		batchLimit = personalScanBatchLimit
	}
	if batchLimit < personalScanBatchLimit {
		batchLimit = personalScanBatchLimit
	}
	items := []any{}
	skipped := 0
	scanned := 0
	backendStart := 0
	status := http.StatusOK
	upstreamTotal := -1
	for scanned < personalScanItemLimit && len(items) < limit {
		q := cloneQuery(pq.backend)
		q.Set("StartIndex", strconv.Itoa(backendStart))
		q.Set("Limit", strconv.Itoa(batchLimit))
		value, backendStatus, err := s.fetchBackendJSON(r.Context(), r, rel, q.Encode(), session, gatewayToken)
		if err != nil {
			http.Error(w, "backend unavailable", http.StatusBadGateway)
			return
		}
		status = backendStatus
		if total, ok := totalRecordCount(value); ok && upstreamTotal < 0 {
			upstreamTotal = total
		}
		batch := extractItems(value)
		if len(batch) == 0 {
			break
		}
		learnChildCountsFromItems(r.Context(), s.store, session, batch)
		for _, item := range batch {
			scanned++
			id, _ := stringField(item, "Id")
			if id != "" && exclude[id] {
				continue
			}
			if skipped < pq.start {
				skipped++
				continue
			}
			rewritten := s.rewriteProxyJSONValueForRequest(r.Context(), r, item, session, gatewayToken, s.gatewayBaseForRequest(r))
			items = append(items, rewritten)
			if len(items) >= limit || scanned >= personalScanItemLimit {
				break
			}
		}
		backendStart += len(batch)
		if len(batch) < batchLimit {
			break
		}
	}
	total := len(items)
	if upstreamTotal >= 0 && pq.canEstimateNegativeTotal() {
		total = estimateNegativeTotal(upstreamTotal, states, pq)
		if total < len(items)+pq.start {
			total = len(items) + pq.start
		}
	} else if upstreamTotal >= 0 {
		total = upstreamTotal
	}
	writeJSON(w, status, map[string]any{"Items": items, "TotalRecordCount": total, "StartIndex": pq.start})
}

func (s *Server) resolvePersonalItemsByID(ctx context.Context, r *http.Request, session *Session, gatewayToken string, ids []string, pq personalQuery) ([]resolvedPersonalItem, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := queryForIDResolution(pq.raw)
	appendResolutionFields(q, pq.sortBy)
	out := make([]resolvedPersonalItem, 0, len(ids))
	now := time.Now().UTC()
	for start := 0; start < len(ids); start += personalIDBatchLimit {
		end := start + personalIDBatchLimit
		if end > len(ids) {
			end = len(ids)
		}
		batchIDs := ids[start:end]
		q.Set("Ids", strings.Join(batchIDs, ","))
		value, status, err := s.fetchBackendJSON(ctx, r, "/Users/"+session.SyntheticUserID+"/Items", q.Encode(), session, gatewayToken)
		if err != nil || status < 200 || status >= 300 {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("backend status %d", status)
		}
		items := extractItems(value)
		learnChildCountsFromItems(ctx, s.store, session, items)
		byID := map[string]map[string]any{}
		for _, item := range items {
			if id, _ := stringField(item, "Id"); id != "" {
				byID[id] = item
			}
		}
		for _, id := range batchIDs {
			item, ok := byID[id]
			state := s.stateForItem(ctx, session, id)
			if !ok {
				state.OrphanedAt = &now
				_ = s.store.SavePlaybackState(ctx, *state)
				continue
			}
			if !itemMatchesResolutionQuery(item, pq.raw) || !pq.matchesResolvedPath(item) {
				continue
			}
			fingerprint := itemFingerprint(item)
			if state.Fingerprint != "" && fingerprint != "" && !fingerprintsCompatible(state.Fingerprint, fingerprint) {
				state.OrphanedAt = &now
				_ = s.store.SavePlaybackState(ctx, *state)
				continue
			}
			state.OrphanedAt = nil
			state.LastSeenAt = &now
			mergeItemMetadata(state, item)
			_ = s.store.SavePlaybackState(ctx, *state)
			rewritten := s.rewriteProxyJSONValueForRequest(ctx, r, item, session, gatewayToken, s.gatewayBaseForRequest(r))
			if m, ok := rewritten.(map[string]any); ok {
				out = append(out, resolvedPersonalItem{item: m, state: *state})
			}
		}
	}
	return out, nil
}

func (s *Server) filterResolvedPersonalItemsWithBackend(ctx context.Context, r *http.Request, session *Session, gatewayToken string, items []resolvedPersonalItem, pq personalQuery) ([]resolvedPersonalItem, error) {
	if len(items) == 0 || !pq.hasBackendOnlyFilters() {
		return items, nil
	}
	allowed := map[string]bool{}
	ids := make([]string, 0, len(items))
	byID := map[string]resolvedPersonalItem{}
	for _, item := range items {
		if item.state.ItemID == "" {
			continue
		}
		ids = append(ids, item.state.ItemID)
		byID[item.state.ItemID] = item
	}
	for start := 0; start < len(ids); start += personalIDBatchLimit {
		end := start + personalIDBatchLimit
		if end > len(ids) {
			end = len(ids)
		}
		q := queryForBackendIDFilter(pq.backend)
		q.Set("Ids", strings.Join(ids[start:end], ","))
		value, status, err := s.fetchBackendJSON(ctx, r, "/Users/"+session.SyntheticUserID+"/Items", q.Encode(), session, gatewayToken)
		if err != nil || status < 200 || status >= 300 {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("backend status %d", status)
		}
		for _, item := range extractItems(value) {
			if id, _ := stringField(item, "Id"); id != "" {
				allowed[id] = true
				if existing, ok := byID[id]; ok {
					rewritten := s.rewriteProxyJSONValueForRequest(ctx, r, item, session, gatewayToken, s.gatewayBaseForRequest(r))
					if m, ok := rewritten.(map[string]any); ok {
						existing.item = m
						byID[id] = existing
					}
				}
			}
		}
	}
	filtered := make([]resolvedPersonalItem, 0, len(items))
	for _, item := range items {
		if allowed[item.state.ItemID] {
			filtered = append(filtered, byID[item.state.ItemID])
		}
	}
	return filtered, nil
}

func (pq personalQuery) hasBackendOnlyFilters() bool {
	for name := range pq.backend {
		if isBackendOnlyPersonalQueryParam(name) {
			return true
		}
	}
	return false
}

func (pq personalQuery) canEstimateNegativeTotal() bool {
	return !pq.hasBackendOnlyFilters()
}

func isBackendOnlyPersonalQueryParam(name string) bool {
	switch strings.ToLower(name) {
	case "api_key", "userid", "ids", "startindex", "limit", "sortby", "sortorder", "fields", "enableimagetypes", "imagetypelimit", "enableimages", "enableuserdata", "enableuserdatas", "enabletotalrecordcount", "mediatypes", "includeitemtypes", "excludeitemtypes", "parentid", "seriesid":
		return false
	default:
		return true
	}
}

func queryForBackendIDFilter(q url.Values) url.Values {
	copy := cloneQuery(q)
	copy.Del("StartIndex")
	copy.Del("Limit")
	return copy
}

func (pq personalQuery) matchesResolvedPath(item map[string]any) bool {
	if pq.pathSeriesID != "" {
		seriesID, _ := stringField(item, "SeriesId")
		if seriesID != pq.pathSeriesID {
			return false
		}
	}
	if pq.pathParentID != "" {
		parentID, _ := stringField(item, "ParentId")
		seasonID, _ := stringField(item, "SeasonId")
		if parentID != pq.pathParentID && seasonID != pq.pathParentID {
			return false
		}
	}
	if pq.pathItemType != "" {
		itemType, _ := stringField(item, "Type")
		if !strings.EqualFold(itemType, pq.pathItemType) {
			return false
		}
	}
	return true
}

func excludeSetForNegativeQuery(states []PlaybackState, pq personalQuery) map[string]bool {
	exclude := map[string]bool{}
	for _, state := range states {
		if state.OrphanedAt != nil || !pq.matchesKnownStateStructure(state) {
			continue
		}
		if pq.negative["played"] && state.Played {
			exclude[state.ItemID] = true
		}
		if pq.negative["favorite"] && state.IsFavorite {
			exclude[state.ItemID] = true
		}
		if pq.negative["resumable"] && !state.Played && state.PlaybackPositionTicks > 0 {
			exclude[state.ItemID] = true
		}
	}
	return exclude
}

func estimateNegativeTotal(upstreamTotal int, states []PlaybackState, pq personalQuery) int {
	excluded := 0
	for _, state := range states {
		if state.OrphanedAt != nil || !pq.matchesCompleteKnownStateStructure(state) {
			continue
		}
		if (pq.negative["played"] && state.Played) || (pq.negative["favorite"] && state.IsFavorite) || (pq.negative["resumable"] && !state.Played && state.PlaybackPositionTicks > 0) {
			excluded++
		}
	}
	total := upstreamTotal - excluded
	if total < 0 {
		return 0
	}
	return total
}

func (pq personalQuery) matchesCompleteKnownStateStructure(state PlaybackState) bool {
	if pq.pathSeriesID != "" && state.SeriesID != pq.pathSeriesID {
		return false
	}
	if pq.pathParentID != "" && state.SeasonID != pq.pathParentID {
		return false
	}
	if pq.pathItemType != "" && !strings.EqualFold(state.ItemType, pq.pathItemType) {
		return false
	}
	if includeTypes := lowerSet(splitFilterValues(pq.raw["IncludeItemTypes"])); len(includeTypes) > 0 && !includeTypes[strings.ToLower(state.ItemType)] {
		return false
	}
	if excludeTypes := lowerSet(splitFilterValues(pq.raw["ExcludeItemTypes"])); len(excludeTypes) > 0 && excludeTypes[strings.ToLower(state.ItemType)] {
		return false
	}
	return true
}

func sortStatesForPersonalQuery(states []PlaybackState, pq personalQuery) {
	if len(pq.sortBy) == 0 {
		sort.SliceStable(states, func(i, j int) bool { return stateRecency(states[i]).After(stateRecency(states[j])) })
	}
}

func sortResolvedPersonalItems(items []resolvedPersonalItem, pq personalQuery) {
	if len(pq.sortBy) == 0 {
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		for idx, sortName := range pq.sortBy {
			desc := sortOrderDesc(pq.sortOrder, idx)
			cmp := compareResolvedPersonalItem(items[i], items[j], sortName)
			if cmp == 0 {
				continue
			}
			if desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return stateRecency(items[i].state).After(stateRecency(items[j].state))
	})
}

func compareResolvedPersonalItem(a, b resolvedPersonalItem, sortName string) int {
	switch strings.ToLower(sortName) {
	case "dateplayed", "lastplayeddate":
		return compareTime(stateRecency(a.state), stateRecency(b.state))
	case "playcount":
		return compareInt(a.state.PlayCount, b.state.PlayCount)
	}
	if av, ok := float64Field(a.item, sortName); ok {
		if bv, ok := float64Field(b.item, sortName); ok {
			return compareFloat(av, bv)
		}
	}
	av := sortStringValue(a.item, sortName)
	bv := sortStringValue(b.item, sortName)
	if av == bv {
		return 0
	}
	if av < bv {
		return -1
	}
	return 1
}

func sortStringValue(item map[string]any, sortName string) string {
	for _, name := range []string{sortName, "SortName", "Name"} {
		if v, ok := stringField(item, name); ok {
			return strings.ToLower(v)
		}
	}
	if v, ok := float64Field(item, sortName); ok {
		return strconv.FormatFloat(v, 'f', 8, 64)
	}
	return ""
}

func compareTime(a, b time.Time) int {
	if a.Equal(b) {
		return 0
	}
	if a.Before(b) {
		return -1
	}
	return 1
}

func compareInt(a, b int) int {
	if a == b {
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

func compareFloat(a, b float64) int {
	if a == b {
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

func sortOrderDesc(orders []string, idx int) bool {
	if idx < len(orders) {
		return strings.EqualFold(orders[idx], "Descending") || strings.EqualFold(orders[idx], "Desc")
	}
	if len(orders) > 0 {
		last := orders[len(orders)-1]
		return strings.EqualFold(last, "Descending") || strings.EqualFold(last, "Desc")
	}
	return false
}

func pageResolvedPersonalItems(items []resolvedPersonalItem, pq personalQuery) []resolvedPersonalItem {
	start := pq.start
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		return nil
	}
	limit := pq.limit
	if limit <= 0 || start+limit > len(items) {
		return items[start:]
	}
	return items[start : start+limit]
}

func appendResolutionFields(q url.Values, sortBy []string) {
	fields := splitFilterValues(q["Fields"])
	seen := lowerSet(fields)
	for _, sortName := range sortBy {
		for _, field := range resolutionFieldsForSort(sortName) {
			if !seen[strings.ToLower(field)] {
				fields = append(fields, field)
				seen[strings.ToLower(field)] = true
			}
		}
	}
	if len(fields) > 0 {
		q.Set("Fields", strings.Join(fields, ","))
	}
}

func resolutionFieldsForSort(sortName string) []string {
	switch strings.ToLower(sortName) {
	case "datecreated":
		return []string{"DateCreated"}
	case "premieredate":
		return []string{"PremiereDate"}
	case "communityrating", "criticrating", "officialrating", "productionyear":
		return []string{sortName}
	default:
		return nil
	}
}

func totalRecordCount(value any) (int, bool) {
	obj, ok := value.(map[string]any)
	if !ok {
		return 0, false
	}
	if v, ok := int64Field(obj, "TotalRecordCount"); ok {
		return int(v), true
	}
	return 0, false
}

func learnChildCountsFromItems(ctx context.Context, store Store, session *Session, items []map[string]any) {
	if session == nil {
		return
	}
	for _, item := range items {
		itemID, _ := stringField(item, "Id")
		count := itemChildCount(item)
		if itemID == "" || count <= 0 {
			continue
		}
		_ = store.SaveItemChildCount(ctx, ItemChildCount{BackendAccountID: session.BackendAccountID, ItemID: itemID, ChildCount: count})
	}
}
