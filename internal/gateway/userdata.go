package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handlePersonalDataRequest(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string) bool {
	if s.handleLocalSessionStateRequest(w, r, rel, session, gatewayToken) {
		return true
	}
	if s.handleDisplayPreferences(w, r, rel, session) {
		return true
	}
	if s.handlePersonalStateWrite(w, r, rel, session, gatewayToken) {
		return true
	}
	if r.Method != http.MethodGet {
		return false
	}
	switch {
	case isResumePath(rel):
		s.writeResumeItems(w, r, session, gatewayToken)
		return true
	case isNextUpPath(rel):
		s.writeNextUpItems(w, r, session, gatewayToken)
		return true
	case isLatestItemsPath(rel):
		s.writeLatestItems(w, r, rel, session, gatewayToken)
		return true
	case isSessionsPath(rel):
		s.writeFilteredSessions(w, r, rel, session, gatewayToken)
		return true
	case queryHasPersonalFilter(r.URL.Query()) && !isAllowedPersonalItemListPath(rel) && !isClearlyNonItemEndpoint(rel):
		http.Error(w, "unsupported personal filter path", http.StatusBadRequest)
		return true
	case shouldLocalizePersonalFilter(rel, r.URL.Query()):
		s.writePersonalFilteredItems(w, r, rel, session, gatewayToken)
		return true
	default:
		return false
	}
}

func (s *Server) handleLocalSessionStateRequest(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string) bool {
	switch {
	case isPlaybackReportRequest(r.Method, rel):
		if err := s.recordPlaybackRequest(r, rel, session, gatewayToken); err != nil {
			if errors.Is(err, ErrBadRequest) {
				http.Error(w, "bad request body", http.StatusBadRequest)
				return true
			}
			http.Error(w, "playback state unavailable", http.StatusInternalServerError)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
		return true
	case isPlaybackKeepaliveRequest(r.Method, rel), isSessionCapabilitiesRequest(r.Method, rel):
		w.WriteHeader(http.StatusNoContent)
		return true
	default:
		return false
	}
}

func (s *Server) handlePersonalStateWrite(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) < 4 || !strings.EqualFold(parts[0], "Users") {
		return false
	}
	if parts[1] != session.SyntheticUserID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return true
	}
	now := time.Now().UTC()
	writeState := func(itemID string, update func(*PlaybackState)) {
		state, err := s.stateForItem(r.Context(), session, itemID)
		if err != nil {
			http.Error(w, "user data unavailable", http.StatusInternalServerError)
			return
		}
		update(state)
		s.enrichPlaybackStateMetadata(r.Context(), r, session, gatewayToken, state)
		state.UpdatedAt = now
		if err := s.store.SavePlaybackState(r.Context(), *state); err != nil {
			http.Error(w, "user data unavailable", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, userDataDTO(state))
	}

	if len(parts) == 4 && strings.EqualFold(parts[2], "PlayedItems") {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			return false
		}
		writeState(parts[3], func(state *PlaybackState) {
			state.Played = r.Method == http.MethodPost
			if state.Played {
				state.PlayCount++
				state.PlaybackPositionTicks = 0
				state.PlayedPercentage = floatPtr(100)
				state.LastPlayedDate = &now
			} else {
				state.PlaybackPositionTicks = 0
				state.PlayedPercentage = nil
			}
		})
		return true
	}

	if len(parts) == 4 && strings.EqualFold(parts[2], "FavoriteItems") {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			return false
		}
		writeState(parts[3], func(state *PlaybackState) {
			state.IsFavorite = r.Method == http.MethodPost
		})
		return true
	}

	if len(parts) == 5 && strings.EqualFold(parts[2], "Items") && strings.EqualFold(parts[4], "Rating") {
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			return false
		}
		likes, hasLikes := requestLikes(r)
		writeState(parts[3], func(state *PlaybackState) {
			if r.Method == http.MethodDelete || !hasLikes {
				state.Likes = nil
			} else {
				state.Likes = &likes
			}
		})
		return true
	}

	if len(parts) == 5 && strings.EqualFold(parts[2], "Items") && strings.EqualFold(parts[4], "UserData") && r.Method == http.MethodPost {
		body, err := readJSONBody(r, 2<<20)
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return true
		}
		writeState(parts[3], func(state *PlaybackState) {
			applyUserDataBodyToState(body, state, now)
		})
		return true
	}

	return false
}

func (s *Server) handleDisplayPreferences(w http.ResponseWriter, r *http.Request, rel string, session *Session) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "DisplayPreferences") {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		return false
	}
	client := r.URL.Query().Get("Client")
	if client == "" {
		client = session.Client
	}
	if r.Method == http.MethodGet {
		preference, err := s.store.FindDisplayPreference(r.Context(), session.GatewayUserID, parts[1], client)
		if err != nil || preference == nil || strings.TrimSpace(preference.PayloadJSON) == "" {
			writeJSON(w, http.StatusOK, map[string]any{})
			return true
		}
		writeRawJSON(w, http.StatusOK, []byte(preference.PayloadJSON))
		return true
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return true
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte(`{}`)
	}
	if !json.Valid(data) {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return true
	}
	if err := s.store.SaveDisplayPreference(r.Context(), DisplayPreference{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, PreferenceID: parts[1], Client: client, PayloadJSON: string(data), UpdatedAt: time.Now().UTC()}); err != nil {
		http.Error(w, "display preferences unavailable", http.StatusInternalServerError)
		return true
	}
	writeRawJSON(w, http.StatusOK, data)
	return true
}

func (s *Server) writeResumeItems(w http.ResponseWriter, r *http.Request, session *Session, gatewayToken string) {
	if !pathUserMatches(r.URL.Path, s.cfg.GatewayBasePath, session.SyntheticUserID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	resumable := true
	states, err := s.store.ListPlaybackStates(r.Context(), session.GatewayUserID, PlaybackStateFilter{Resumable: &resumable, IncludeOrphaned: true})
	if err != nil {
		http.Error(w, "resume unavailable", http.StatusInternalServerError)
		return
	}
	sort.SliceStable(states, func(i, j int) bool {
		return stateRecency(states[i]).After(stateRecency(states[j]))
	})
	ids := playbackStateIDs(states)
	items, err := s.resolveItemsByID(r.Context(), r, session, gatewayToken, ids)
	if err != nil {
		http.Error(w, "resume unavailable", http.StatusInternalServerError)
		return
	}
	items = groupResumeItems(items)
	total := len(items)
	items = pageItems(items, r.URL.Query())
	writeJSON(w, http.StatusOK, map[string]any{"Items": items, "TotalRecordCount": total, "StartIndex": intQuery(r.URL.Query(), "StartIndex", 0)})
}

func (s *Server) writePersonalFilteredItems(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string) {
	if !relUserMatches(rel, session.SyntheticUserID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	pq := parsePersonalQuery(rel, r.URL.Query())
	if pq.hasPositive() {
		s.writePositivePersonalItems(w, r, rel, session, gatewayToken, pq)
		return
	}
	if pq.hasOnlyNegative() {
		s.writeNegativePersonalItems(w, r, rel, session, gatewayToken, pq)
		return
	}
	value, status, upstream, err := s.fetchBackendJSON(r.Context(), r, rel, pq.backend.Encode(), session, gatewayToken)
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, status, s.rewriteProxyJSONValueForRequestWithSnapshot(r.Context(), r, value, session, upstream, gatewayToken, s.gatewayBaseForRequest(r)))
}

func (s *Server) writeLatestItems(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string) {
	if !relUserMatches(rel, session.SyntheticUserID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	q := cloneQuery(r.URL.Query())
	limit := intQuery(q, "Limit", 0)
	if limit <= 0 {
		limit = 20
	}
	if limit > latestBackfillLimit {
		limit = latestBackfillLimit
	}
	played := true
	states, _ := s.store.ListPlaybackStates(r.Context(), session.GatewayUserID, PlaybackStateFilter{Played: &played})
	playedSet := playbackStateSet(states)
	requestLimit := limit
	status := http.StatusOK
	items := []any{}
	for requestLimit <= latestBackfillLimit {
		q.Set("Limit", strconv.Itoa(requestLimit))
		value, backendStatus, upstream, err := s.fetchBackendJSON(r.Context(), r, rel, q.Encode(), session, gatewayToken)
		if err != nil {
			http.Error(w, "backend unavailable", http.StatusBadGateway)
			return
		}
		status = backendStatus
		extracted := extractItems(value)
		learnChildCountsFromItems(r.Context(), s.store, session, extracted)
		kept := make([]any, 0, limit)
		for _, item := range extracted {
			id, _ := stringField(item, "Id")
			if id != "" && playedSet[id] {
				continue
			}
			rewritten := s.rewriteProxyJSONValueForRequestWithSnapshot(r.Context(), r, item, session, upstream, gatewayToken, s.gatewayBaseForRequest(r))
			kept = append(kept, rewritten)
			if len(kept) >= limit {
				break
			}
		}
		items = kept
		if len(items) >= limit || len(extracted) < requestLimit {
			break
		}
		requestLimit *= 3
	}
	writeJSON(w, status, items)
}

func (s *Server) writeFilteredSessions(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string) {
	value, status, upstream, err := s.fetchBackendJSON(r.Context(), r, rel, r.URL.RawQuery, session, gatewayToken)
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	rewritten := s.rewriteProxyJSONValueForRequestWithSnapshot(r.Context(), r, value, session, upstream, gatewayToken, s.gatewayBaseForRequest(r))
	deviceID := upstream.identity.WithDefaults().DeviceID
	if deviceID == "" {
		writeJSON(w, status, []any{})
		return
	}
	writeJSON(w, status, filterItemsValue(rewritten, func(item map[string]any) bool {
		id, _ := stringField(item, "DeviceId")
		return id == deviceID
	}))
}

func (s *Server) writeNextUpItems(w http.ResponseWriter, r *http.Request, session *Session, gatewayToken string) {
	states, err := s.store.ListPlaybackStates(r.Context(), session.GatewayUserID, PlaybackStateFilter{})
	if err != nil {
		http.Error(w, "next up unavailable", http.StatusInternalServerError)
		return
	}
	series := recentlyActiveSeries(states)
	if seriesID := strings.TrimSpace(r.URL.Query().Get("SeriesId")); seriesID != "" {
		series = []string{seriesID}
	}
	maxSeries := intQuery(r.URL.Query(), "Limit", 20) + 20
	if maxSeries > 0 && len(series) > maxSeries {
		series = series[:maxSeries]
	}
	playedByID := playbackStateSet(filterStates(states, func(state PlaybackState) bool { return state.Played }))
	items := make([]any, 0, len(series))
	episodeQuery := queryForIDResolution(r.URL.Query())
	for _, seriesID := range series {
		episodeValue, status, upstream, err := s.fetchBackendJSON(r.Context(), r, "/Shows/"+seriesID+"/Episodes", episodeQuery.Encode(), session, gatewayToken)
		if err != nil || status < 200 || status >= 300 {
			continue
		}
		episodes := extractItems(episodeValue)
		// Only cache when Emby reports a trusted total; len(episodes) may be a partial page.
		if total, ok := totalRecordCount(episodeValue); ok && total > 0 {
			_ = s.store.SaveItemChildCount(r.Context(), ItemChildCount{ItemID: seriesID, ChildCount: total})
		}
		sort.SliceStable(episodes, func(i, j int) bool {
			return episodeOrderLess(episodes[i], episodes[j])
		})
		last := lastWatchedEpisodeIndex(states, seriesID)
		for _, episode := range episodes {
			id, _ := stringField(episode, "Id")
			if id == "" || playedByID[id] || !episodeAfter(episode, last) {
				continue
			}
			rewritten := s.rewriteProxyJSONValueForRequestWithSnapshot(r.Context(), r, episode, session, upstream, gatewayToken, s.gatewayBaseForRequest(r))
			if item, ok := rewritten.(map[string]any); ok {
				items = append(items, item)
			}
			break
		}
	}
	total := len(items)
	items = pageItems(items, r.URL.Query())
	writeJSON(w, http.StatusOK, map[string]any{"Items": items, "TotalRecordCount": total, "StartIndex": intQuery(r.URL.Query(), "StartIndex", 0)})
}

func (s *Server) personalFilterIDs(ctx context.Context, gatewayUserID string, q url.Values) ([]string, bool, []string, error) {
	filters := splitFilterValues(q["Filters"])
	remaining := make([]string, 0, len(filters))
	type personalFilterOp struct {
		positive bool
		filter   PlaybackStateFilter
	}
	ops := make([]personalFilterOp, 0, 6)
	for _, filter := range filters {
		switch strings.ToLower(filter) {
		case "isplayed":
			v := true
			ops = append(ops, personalFilterOp{positive: true, filter: PlaybackStateFilter{Played: &v}})
		case "isfavorite":
			v := true
			ops = append(ops, personalFilterOp{positive: true, filter: PlaybackStateFilter{Favorite: &v}})
		case "isresumable":
			v := true
			ops = append(ops, personalFilterOp{positive: true, filter: PlaybackStateFilter{Resumable: &v}})
		case "isunplayed":
			v := true
			ops = append(ops, personalFilterOp{positive: false, filter: PlaybackStateFilter{Played: &v}})
		default:
			remaining = append(remaining, filter)
		}
	}
	for _, name := range []string{"IsPlayed", "IsFavorite", "IsResumable"} {
		raw := q.Get(name)
		if raw == "" {
			continue
		}
		value, err := strconv.ParseBool(raw)
		if err != nil {
			q.Del(name)
			continue
		}
		var filter PlaybackStateFilter
		switch name {
		case "IsPlayed":
			v := true
			filter = PlaybackStateFilter{Played: &v}
		case "IsFavorite":
			v := true
			filter = PlaybackStateFilter{Favorite: &v}
		case "IsResumable":
			v := true
			filter = PlaybackStateFilter{Resumable: &v}
		}
		ops = append(ops, personalFilterOp{positive: value, filter: filter})
		q.Del(name)
	}
	q.Del("Filters")
	if len(remaining) > 0 {
		q.Set("Filters", strings.Join(remaining, ","))
	}
	if len(ops) == 0 {
		return nil, false, nil, nil
	}

	allStates, err := s.store.ListPlaybackStates(ctx, gatewayUserID, PlaybackStateFilter{})
	if err != nil {
		return nil, false, nil, err
	}

	var positive map[string]bool
	hasPositive := false
	exclude := map[string]bool{}
	intersectPositive := func(states []PlaybackState) {
		ids := playbackStateSet(states)
		if !hasPositive {
			positive = ids
			hasPositive = true
			return
		}
		for id := range positive {
			if !ids[id] {
				delete(positive, id)
			}
		}
	}
	for _, op := range ops {
		matched := statesMatchingFilter(allStates, op.filter)
		if op.positive {
			intersectPositive(matched)
		} else {
			for _, state := range matched {
				exclude[state.ItemID] = true
			}
		}
	}
	return sortedSetKeys(positive), hasPositive, sortedSetKeys(exclude), nil
}

// statesMatchingFilter applies the same field checks as MemoryStore.ListPlaybackStates.
func statesMatchingFilter(states []PlaybackState, filter PlaybackStateFilter) []PlaybackState {
	out := make([]PlaybackState, 0, len(states))
	for _, state := range states {
		if !filter.IncludeOrphaned && state.OrphanedAt != nil {
			continue
		}
		if filter.Played != nil && state.Played != *filter.Played {
			continue
		}
		if filter.Favorite != nil && state.IsFavorite != *filter.Favorite {
			continue
		}
		if filter.Resumable != nil {
			resumable := state.PlaybackPositionTicks > 0 && !state.Played
			if resumable != *filter.Resumable {
				continue
			}
		}
		if filter.SeriesID != "" && state.SeriesID != filter.SeriesID {
			continue
		}
		if filter.SeasonID != "" && state.SeasonID != filter.SeasonID {
			continue
		}
		out = append(out, state)
	}
	return out
}

func (s *Server) resolveItemsByID(ctx context.Context, r *http.Request, session *Session, gatewayToken string, ids []string) ([]any, error) {
	if len(ids) == 0 {
		return []any{}, nil
	}
	requestQuery := cloneQuery(r.URL.Query())
	q := queryForIDResolution(requestQuery)
	out := make([]any, 0, len(ids))
	now := time.Now().UTC()
	for start := 0; start < len(ids); start += personalIDBatchLimit {
		end := start + personalIDBatchLimit
		if end > len(ids) {
			end = len(ids)
		}
		batchIDs := ids[start:end]
		q.Set("Ids", strings.Join(batchIDs, ","))
		value, status, upstream, err := s.fetchBackendJSON(ctx, r, "/Users/"+session.SyntheticUserID+"/Items", q.Encode(), session, gatewayToken)
		if err != nil || status < 200 || status >= 300 {
			continue
		}
		items := extractItems(value)
		byID := map[string]map[string]any{}
		for _, item := range items {
			if id, _ := stringField(item, "Id"); id != "" {
				byID[id] = item
			}
		}
		for _, id := range batchIDs {
			item, ok := byID[id]
			state, err := s.stateForItem(ctx, session, id)
			if err != nil {
				return nil, err
			}
			if !ok {
				_ = reconcileResolvedItem(state, nil, false, now)
				if err := s.store.SavePlaybackResolution(ctx, *state); err != nil {
					return nil, fmt.Errorf("%w: save playback resolution: %w", ErrStoreUnavailable, err)
				}
				continue
			}
			if !itemMatchesResolutionQuery(item, requestQuery) {
				continue
			}
			outcome := reconcileResolvedItem(state, item, true, now)
			if err := s.store.SavePlaybackResolution(ctx, *state); err != nil {
				return nil, fmt.Errorf("%w: save playback resolution: %w", ErrStoreUnavailable, err)
			}
			if outcome != resolutionKeep {
				continue
			}
			rewritten := s.rewriteProxyJSONValueForRequestWithSnapshot(ctx, r, item, session, upstream, gatewayToken, s.gatewayBaseForRequest(r))
			if m, ok := rewritten.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

func queryForIDResolution(q url.Values) url.Values {
	copy := cloneQuery(q)
	for name := range copy {
		if !isIDResolutionProjectionParam(name) {
			copy.Del(name)
		}
	}
	return copy
}

func isIDResolutionProjectionParam(name string) bool {
	switch strings.ToLower(name) {
	case "fields", "enableimagetypes", "imagetypelimit", "enableimages", "enableuserdata", "enableuserdatas", "enabletotalrecordcount":
		return true
	default:
		return false
	}
}

func itemMatchesResolutionQuery(item map[string]any, q url.Values) bool {
	if mediaTypes := lowerSet(splitFilterValues(q["MediaTypes"])); len(mediaTypes) > 0 {
		mediaType, _ := stringField(item, "MediaType")
		if !mediaTypes[strings.ToLower(mediaType)] {
			return false
		}
	}
	if includeTypes := lowerSet(splitFilterValues(q["IncludeItemTypes"])); len(includeTypes) > 0 {
		itemType, _ := stringField(item, "Type")
		if !includeTypes[strings.ToLower(itemType)] {
			return false
		}
	}
	if excludeTypes := lowerSet(splitFilterValues(q["ExcludeItemTypes"])); len(excludeTypes) > 0 {
		itemType, _ := stringField(item, "Type")
		if excludeTypes[strings.ToLower(itemType)] {
			return false
		}
	}
	if parentID := strings.TrimSpace(q.Get("ParentId")); parentID != "" {
		itemParentID, _ := stringField(item, "ParentId")
		if itemParentID != parentID {
			return false
		}
	}
	if seriesID := strings.TrimSpace(q.Get("SeriesId")); seriesID != "" {
		itemSeriesID, _ := stringField(item, "SeriesId")
		if itemSeriesID != seriesID {
			return false
		}
	}
	return true
}

func lowerSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[strings.ToLower(value)] = true
		}
	}
	return set
}

func (s *Server) fetchBackendJSON(ctx context.Context, r *http.Request, rel, rawQuery string, session *Session, gatewayToken string) (any, int, upstreamRequestSnapshot, error) {
	runtime, err := s.upstreamAuth.Ensure(ctx)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	upstream, err := upstreamRequestSnapshotFromRuntime(runtime)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	u, err := s.proxyURL(upstream, session, rel, rawQuery, gatewayToken)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	copyRequestHeaders(req.Header, r.Header)
	s.rewriteRequestHeaders(req.Header, upstream)
	req.Host = u.Host
	resp, err := s.proxyClient.Do(req)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		if refreshed, confirmed, refreshErr := s.refreshAfterUnauthorized(ctx, upstream); confirmed && refreshErr == nil {
			upstream = refreshed
			s.auditBackendTokenRefresh(r, rel, session, "backend_token_refresh", "backend token refreshed after unauthorized response", http.StatusOK)
			_ = resp.Body.Close()
			u, err = s.proxyURL(upstream, session, rel, rawQuery, gatewayToken)
			if err != nil {
				return nil, 0, upstreamRequestSnapshot{}, err
			}
			req, err = http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
			if err != nil {
				return nil, 0, upstreamRequestSnapshot{}, err
			}
			copyRequestHeaders(req.Header, r.Header)
			s.rewriteRequestHeaders(req.Header, upstream)
			req.Host = u.Host
			resp, err = s.proxyClient.Do(req)
			if err != nil {
				return nil, 0, upstreamRequestSnapshot{}, err
			}
			defer resp.Body.Close()
		}
	}
	data, err := readLimited(resp.Body, proxyJSONLimit)
	if err != nil {
		return nil, resp.StatusCode, upstream, err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, resp.StatusCode, upstream, err
	}
	return value, resp.StatusCode, upstream, nil
}

func (s *Server) stateForItem(ctx context.Context, session *Session, itemID string) (*PlaybackState, error) {
	state, err := s.store.FindPlaybackState(ctx, session.GatewayUserID, itemID)
	if err == nil && state != nil {
		state.SyntheticUserID = session.SyntheticUserID
		return state, nil
	}
	if errors.Is(err, ErrNotFound) {
		return &PlaybackState{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, ItemID: itemID}, nil
	}
	if err == nil {
		return nil, fmt.Errorf("%w: find playback state returned nil", ErrStoreUnavailable)
	}
	return nil, err
}

func (s *Server) enrichPlaybackStateMetadata(ctx context.Context, r *http.Request, session *Session, gatewayToken string, state *PlaybackState) {
	if state == nil || state.ItemID == "" {
		return
	}
	value, status, _, err := s.fetchBackendJSON(ctx, r, "/Users/"+session.SyntheticUserID+"/Items/"+state.ItemID, "", session, gatewayToken)
	if err != nil || status < 200 || status >= 300 {
		return
	}
	item, ok := value.(map[string]any)
	if !ok {
		return
	}
	mergeItemMetadata(state, item)
	now := time.Now().UTC()
	state.OrphanedAt = nil
	state.LastSeenAt = &now
}

func applyUserDataBodyToState(body map[string]any, state *PlaybackState, now time.Time) {
	if played, ok := boolField(body, "Played"); ok {
		if played && !state.Played {
			state.PlayCount++
		}
		state.Played = played
		if played {
			state.LastPlayedDate = &now
		}
	}
	if ticks, ok := int64Field(body, "PlaybackPositionTicks"); ok {
		state.PlaybackPositionTicks = ticks
	}
	if ticks, ok := int64Field(body, "PositionTicks"); ok {
		state.PlaybackPositionTicks = ticks
	}
	if percentage, ok := float64Field(body, "PlayedPercentage"); ok {
		state.PlayedPercentage = &percentage
	}
	if count, ok := int64Field(body, "PlayCount"); ok {
		state.PlayCount = int(count)
	}
	if favorite, ok := boolField(body, "IsFavorite"); ok {
		state.IsFavorite = favorite
	}
	if favorite, ok := boolField(body, "Favorite"); ok {
		state.IsFavorite = favorite
	}
	if likes, ok := boolField(body, "Likes"); ok {
		state.Likes = &likes
	}
}

func readJSONBody(r *http.Request, limit int64) (map[string]any, error) {
	data, err := io.ReadAll(http.MaxBytesReader(nilResponseWriter{}, r.Body, limit))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

func userDataDTO(state *PlaybackState) map[string]any {
	data := map[string]any{}
	applyPlaybackStateToUserData(data, state, nil, nil)
	return data
}

func requestLikes(r *http.Request) (bool, bool) {
	if raw := r.URL.Query().Get("Likes"); raw != "" {
		v, err := strconv.ParseBool(raw)
		return v, err == nil
	}
	body, err := readJSONBody(r, 2<<20)
	if err != nil {
		return false, false
	}
	return boolField(body, "Likes")
}

func extractItems(value any) []map[string]any {
	if list, ok := value.([]any); ok {
		return mapsFromAnyList(list)
	}
	if obj, ok := value.(map[string]any); ok {
		if items, ok := obj["Items"].([]any); ok {
			return mapsFromAnyList(items)
		}
	}
	return nil
}

func mapsFromAnyList(list []any) []map[string]any {
	items := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return items
}

func filterItemsValue(value any, keep func(map[string]any) bool) any {
	if list, ok := value.([]any); ok {
		filtered := make([]any, 0, len(list))
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok || keep(m) {
				filtered = append(filtered, item)
			}
		}
		return filtered
	}
	if obj, ok := value.(map[string]any); ok {
		items, ok := obj["Items"].([]any)
		if !ok {
			return value
		}
		filtered := make([]any, 0, len(items))
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok || keep(m) {
				filtered = append(filtered, item)
			}
		}
		obj["Items"] = filtered
		obj["TotalRecordCount"] = len(filtered)
	}
	return value
}

func mergeItemMetadata(state *PlaybackState, item map[string]any) {
	if v, ok := stringField(item, "Name"); ok {
		state.ItemName = v
	}
	if v, ok := stringField(item, "Type"); ok {
		state.ItemType = v
	}
	if v, ok := stringField(item, "SeriesId"); ok {
		state.SeriesID = v
	}
	if v, ok := stringField(item, "SeriesName"); ok {
		state.SeriesName = v
	}
	if v, ok := stringField(item, "SeasonId"); ok {
		state.SeasonID = v
	}
	if v, ok := int64Field(item, "IndexNumber"); ok {
		state.IndexNumber = int(v)
	}
	if v, ok := int64Field(item, "ParentIndexNumber"); ok {
		state.ParentIndexNumber = int(v)
	}
	if v, ok := int64Field(item, "RunTimeTicks"); ok {
		state.RunTimeTicks = v
	}
	state.Fingerprint = itemFingerprint(item)
}

func itemFingerprint(item map[string]any) string {
	parts := []string{}
	for _, key := range []string{"Type", "Name", "SeriesId"} {
		if v, ok := stringField(item, key); ok {
			parts = append(parts, strings.ToLower(key)+"="+v)
		}
	}
	return strings.Join(parts, "|")
}

func fingerprintsCompatible(a, b string) bool {
	aParts := fingerprintParts(a)
	bParts := fingerprintParts(b)
	if len(aParts) == 0 || len(bParts) == 0 {
		return true
	}
	for key, aValue := range aParts {
		if bValue, ok := bParts[key]; ok && bValue != aValue {
			return false
		}
	}
	return true
}

func fingerprintParts(fingerprint string) map[string]string {
	parts := map[string]string{}
	for _, part := range strings.Split(fingerprint, "|") {
		name, value, ok := strings.Cut(part, "=")
		if !ok || name == "" {
			continue
		}
		parts[name] = value
	}
	return parts
}

func splitFilterValues(values []string) []string {
	filters := []string{}
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				filters = append(filters, part)
			}
		}
	}
	return filters
}

func queryHasPersonalFilter(q url.Values) bool {
	if q.Get("IsPlayed") != "" || q.Get("IsFavorite") != "" || q.Get("IsResumable") != "" {
		return true
	}
	for _, filter := range splitFilterValues(q["Filters"]) {
		switch strings.ToLower(filter) {
		case "isplayed", "isunplayed", "isresumable", "isfavorite":
			return true
		}
	}
	return false
}

func isResumePath(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	return len(parts) == 4 && strings.EqualFold(parts[0], "Users") && strings.EqualFold(parts[2], "Items") && strings.EqualFold(parts[3], "Resume")
}

func isLatestItemsPath(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	return len(parts) == 4 && strings.EqualFold(parts[0], "Users") && strings.EqualFold(parts[2], "Items") && strings.EqualFold(parts[3], "Latest")
}

func isItemsPath(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	return len(parts) == 3 && strings.EqualFold(parts[0], "Users") && strings.EqualFold(parts[2], "Items")
}

func shouldLocalizePersonalFilter(rel string, q url.Values) bool {
	return queryHasPersonalFilter(q) && isAllowedPersonalItemListPath(rel)
}

func isNextUpPath(rel string) bool {
	return equalPath(rel, "/Shows/NextUp")
}

func isSessionsPath(rel string) bool {
	return equalPath(rel, "/Sessions")
}

func relUserMatches(rel, syntheticUserID string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], "Users") {
		return true
	}
	return parts[1] == syntheticUserID
}

func pathUserMatches(requestPath, gatewayBasePath, syntheticUserID string) bool {
	base := strings.TrimRight(gatewayBasePath, "/")
	rel := strings.TrimPrefix(requestPath, base)
	return relUserMatches(rel, syntheticUserID)
}

func groupResumeItems(items []any) []any {
	grouped := make([]any, 0, len(items))
	seenSeries := map[string]bool{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if ok {
			if seriesID, _ := stringField(m, "SeriesId"); seriesID != "" {
				if seenSeries[seriesID] {
					continue
				}
				seenSeries[seriesID] = true
			} else if seriesID, _ := stringField(m, "SeriesID"); seriesID != "" {
				if seenSeries[seriesID] {
					continue
				}
				seenSeries[seriesID] = true
			}
		} else if state, ok := item.(PlaybackState); ok && state.SeriesID != "" {
			if seenSeries[state.SeriesID] {
				continue
			}
			seenSeries[state.SeriesID] = true
		}
		grouped = append(grouped, item)
	}
	return grouped
}

func playbackStateIDs(states []PlaybackState) []string {
	ids := make([]string, 0, len(states))
	for _, state := range states {
		if state.ItemID != "" {
			ids = append(ids, state.ItemID)
		}
	}
	return ids
}

func playbackStateSet(states []PlaybackState) map[string]bool {
	set := map[string]bool{}
	for _, state := range states {
		if state.ItemID != "" {
			set[state.ItemID] = true
		}
	}
	return set
}

func sortedSetKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func filterStates(states []PlaybackState, keep func(PlaybackState) bool) []PlaybackState {
	filtered := make([]PlaybackState, 0, len(states))
	for _, state := range states {
		if keep(state) {
			filtered = append(filtered, state)
		}
	}
	return filtered
}

func recentlyActiveSeries(states []PlaybackState) []string {
	latest := map[string]time.Time{}
	for _, state := range states {
		if state.SeriesID == "" || state.OrphanedAt != nil {
			continue
		}
		recency := stateRecency(state)
		if recency.After(latest[state.SeriesID]) {
			latest[state.SeriesID] = recency
		}
	}
	type pair struct {
		seriesID string
		time     time.Time
	}
	pairs := make([]pair, 0, len(latest))
	for seriesID, t := range latest {
		pairs = append(pairs, pair{seriesID: seriesID, time: t})
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].time.After(pairs[j].time) })
	ids := make([]string, 0, len(pairs))
	for _, p := range pairs {
		ids = append(ids, p.seriesID)
	}
	return ids
}

func stateRecency(state PlaybackState) time.Time {
	if state.LastPlayedDate != nil {
		return *state.LastPlayedDate
	}
	return state.UpdatedAt
}

type episodeIndex struct {
	season  int
	episode int
	valid   bool
}

func lastWatchedEpisodeIndex(states []PlaybackState, seriesID string) episodeIndex {
	last := episodeIndex{}
	for _, state := range states {
		if state.SeriesID != seriesID || (!state.Played && state.PlaybackPositionTicks == 0) {
			continue
		}
		idx := episodeIndex{season: state.ParentIndexNumber, episode: state.IndexNumber, valid: true}
		if !last.valid || indexAfter(idx, last) {
			last = idx
		}
	}
	return last
}

func episodeAfter(item map[string]any, last episodeIndex) bool {
	if !last.valid {
		return true
	}
	idx := itemEpisodeIndex(item)
	return idx.valid && indexAfter(idx, last)
}

func episodeOrderLess(a, b map[string]any) bool {
	ai := itemEpisodeIndex(a)
	bi := itemEpisodeIndex(b)
	return indexAfter(bi, ai)
}

func itemEpisodeIndex(item map[string]any) episodeIndex {
	season, _ := int64Field(item, "ParentIndexNumber")
	episode, _ := int64Field(item, "IndexNumber")
	return episodeIndex{season: int(season), episode: int(episode), valid: true}
}

func indexAfter(a, b episodeIndex) bool {
	if a.season != b.season {
		return a.season > b.season
	}
	return a.episode > b.episode
}

func pageItems(items []any, q url.Values) []any {
	start := intQuery(q, "StartIndex", 0)
	if start < 0 {
		start = 0
	}
	if start >= len(items) {
		return []any{}
	}
	limit := intQuery(q, "Limit", 0)
	if limit <= 0 || start+limit > len(items) {
		return items[start:]
	}
	return items[start : start+limit]
}

func intQuery(q url.Values, name string, fallback int) int {
	v, err := strconv.Atoi(q.Get(name))
	if err != nil {
		return fallback
	}
	return v
}

func cloneQuery(q url.Values) url.Values {
	copy := url.Values{}
	for k, vals := range q {
		copy[k] = append([]string(nil), vals...)
	}
	return copy
}

func floatPtr(v float64) *float64 {
	return &v
}

func writeRawJSON(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
