package gateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type personalStateSnapshot struct {
	GatewayUserID   string
	SyntheticUserID string
	States          map[string]PlaybackState
}

func (s *Server) personalStateSnapshot(ctx context.Context, session *Session) (personalStateSnapshot, error) {
	if s == nil || s.store == nil || session == nil {
		return personalStateSnapshot{}, fmt.Errorf("%w: missing state snapshot dependency", ErrStoreUnavailable)
	}
	states, err := s.store.ListPlaybackStates(ctx, session.GatewayUserID, PlaybackStateFilter{IncludeOrphaned: true})
	if err != nil {
		return personalStateSnapshot{}, fmt.Errorf("%w: list playback states: %w", ErrStoreUnavailable, err)
	}
	indexed, err := indexPersonalStates(states)
	if err != nil {
		return personalStateSnapshot{}, fmt.Errorf("%w: index playback states: %w", ErrStoreUnavailable, err)
	}
	for id, state := range indexed {
		if state.GatewayUserID != "" && state.GatewayUserID != session.GatewayUserID {
			return personalStateSnapshot{}, fmt.Errorf("%w: playback state %q has conflicting gateway user", ErrStoreUnavailable, id)
		}
		if state.SyntheticUserID != "" && state.SyntheticUserID != session.SyntheticUserID {
			return personalStateSnapshot{}, fmt.Errorf("%w: playback state %q has conflicting synthetic user", ErrStoreUnavailable, id)
		}
		if state.GatewayUserID == "" {
			state.GatewayUserID = session.GatewayUserID
		}
		if state.SyntheticUserID == "" {
			state.SyntheticUserID = session.SyntheticUserID
		}
		indexed[id] = state
	}
	return personalStateSnapshot{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, States: indexed}, nil
}

type personalPlanSource struct {
	server       *Server
	request      *http.Request
	session      *Session
	gatewayToken string
}

func newPersonalPlanSource(server *Server, request *http.Request, session *Session, gatewayToken string) (*personalPlanSource, error) {
	if server == nil || server.store == nil || request == nil || session == nil || server.metadataUpstream == nil {
		return nil, fmt.Errorf("missing personal plan source dependency")
	}
	return &personalPlanSource{server: server, request: request, session: session, gatewayToken: gatewayToken}, nil
}

func (p *personalPlanSource) snapshot(ctx context.Context) (personalStateSnapshot, error) {
	if p == nil || p.server == nil || p.session == nil {
		return personalStateSnapshot{}, fmt.Errorf("%w: missing personal plan source", ErrStoreUnavailable)
	}
	return p.server.personalStateSnapshot(ctx, p.session)
}

func (p *personalPlanSource) fetchCandidatePage(ctx context.Context, plan personalPlan, start, limit int) (personalCandidatePage, error) {
	if p == nil || p.server == nil || p.request == nil || p.session == nil || p.server.metadataUpstream == nil {
		return personalCandidatePage{}, fmt.Errorf("missing personal candidate source dependency")
	}
	if start < 0 || limit <= 0 {
		return personalCandidatePage{}, fmt.Errorf("invalid candidate page start/limit")
	}
	q := cloneQuery(plan.Neutral)
	sanitizeCandidateQuery(q)
	copyResolutionFields(q, plan.Projection)
	appendResolutionSortFields(q, plan.Sort)
	latest := plan.Route == personalRouteLatest || plan.Shape == personalShapeArray
	if latest {
		if start != 0 {
			return personalCandidatePage{}, fmt.Errorf("Latest candidate start must be zero")
		}
	} else {
		q.Set("StartIndex", strconv.Itoa(start))
	}
	q.Set("Limit", strconv.Itoa(limit))
	value, status, upstream, err := p.server.fetchBackendJSON(ctx, p.request, plan.Path, q.Encode(), p.session, p.gatewayToken)
	if err != nil {
		return personalCandidatePage{}, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return personalCandidatePage{}, fmt.Errorf("candidate backend status %d", status)
	}
	items, returnedStart, total, err := strictPersonalPage(value, plan.Shape, start)
	if err != nil {
		return personalCandidatePage{}, err
	}
	rewritten := make([]map[string]any, len(items))
	for i, item := range items {
		rewritten[i] = p.server.rewritePlannedPersonalItem(item, p.session, upstream, p.gatewayToken, p.request)
	}
	return personalCandidatePage{Items: rewritten, RequestedStart: start, ReturnedStart: returnedStart, Total: total, Terminal: len(items) < limit || len(items) == 0}, nil
}

func strictPersonalPage(value any, shape personalResultShape, requestedStart int) ([]map[string]any, *int, *int, error) {
	var raw any
	var object map[string]any
	if shape == personalShapeArray {
		if _, ok := value.([]any); !ok {
			return nil, nil, nil, fmt.Errorf("Latest response is not an array")
		}
		raw = value
	} else {
		var ok bool
		object, ok = value.(map[string]any)
		if !ok {
			return nil, nil, nil, fmt.Errorf("QueryResult response is not an object")
		}
		var exists bool
		raw, exists = object["Items"]
		if !exists {
			return nil, nil, nil, fmt.Errorf("QueryResult response has no Items")
		}
	}
	array, ok := raw.([]any)
	if !ok {
		return nil, nil, nil, fmt.Errorf("Items is not an array")
	}
	items := make([]map[string]any, len(array))
	for i, item := range array {
		m, ok := item.(map[string]any)
		if !ok || m == nil {
			return nil, nil, nil, fmt.Errorf("Items[%d] is not an object", i)
		}
		items[i] = m
	}
	var returnedStart, total *int
	if object != nil {
		var err error
		returnedStart, err = optionalJSONInt(object, "StartIndex")
		if err != nil {
			return nil, nil, nil, err
		}
		total, err = optionalJSONInt(object, "TotalRecordCount")
		if err != nil {
			return nil, nil, nil, err
		}
		if returnedStart != nil && *returnedStart != requestedStart {
			return nil, nil, nil, fmt.Errorf("returned StartIndex %d does not match requested %d", *returnedStart, requestedStart)
		}
	}
	return items, returnedStart, total, nil
}

func optionalJSONInt(object map[string]any, name string) (*int, error) {
	value, exists := object[name]
	if !exists {
		return nil, nil
	}
	n, ok := value.(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n != math.Trunc(n) || n >= math.Ldexp(1, strconv.IntSize-1) {
		return nil, fmt.Errorf("%s is malformed", name)
	}
	result := int(n)
	return &result, nil
}

func (p *personalPlanSource) resolveIDs(ctx context.Context, plan personalPlan, snapshot personalStateSnapshot, ids []string) ([]resolvedPersonalItem, error) {
	if p == nil || p.server == nil || p.request == nil || p.session == nil {
		return nil, fmt.Errorf("missing personal resolution source dependency")
	}
	if snapshot.GatewayUserID == "" || snapshot.GatewayUserID != p.session.GatewayUserID ||
		snapshot.SyntheticUserID == "" || snapshot.SyntheticUserID != p.session.SyntheticUserID {
		return nil, fmt.Errorf("%w: resolution snapshot identity conflicts with session", ErrStoreUnavailable)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("empty resolution ID list")
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("empty resolution ID")
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("duplicate resolution ID %q", id)
		}
		seen[id] = struct{}{}
	}
	requireState := plan.Kind == personalPlanPositive || plan.Kind == personalPlanResume || plan.Kind == personalPlanNextUp
	for _, id := range ids {
		state, ok := snapshot.States[id]
		if !ok {
			if requireState {
				return nil, fmt.Errorf("resolution ID %q has no scoped state", id)
			}
			continue
		}
		if state.ItemID == "" || state.ItemID != id || state.GatewayUserID == "" || state.GatewayUserID != snapshot.GatewayUserID || state.SyntheticUserID == "" || state.SyntheticUserID != snapshot.SyntheticUserID {
			return nil, fmt.Errorf("%w: resolution state %q conflicts with snapshot", ErrStoreUnavailable, id)
		}
	}
	q := resolutionQueryForPlan(plan)
	result := make(map[string]resolvedPersonalItem, len(ids))
	now := time.Now().UTC()
	for start := 0; start < len(ids); start += personalIDBatchLimit {
		end := start + personalIDBatchLimit
		if end > len(ids) {
			end = len(ids)
		}
		batchIDs := ids[start:end]
		batchSet := make(map[string]struct{}, len(batchIDs))
		for _, id := range batchIDs {
			batchSet[id] = struct{}{}
		}
		batchQuery := cloneQuery(q)
		batchQuery.Set("Ids", strings.Join(batchIDs, ","))
		value, status, upstream, err := p.server.fetchBackendJSON(ctx, p.request, "/Users/"+p.session.SyntheticUserID+"/Items", batchQuery.Encode(), p.session, p.gatewayToken)
		if err != nil {
			return nil, err
		}
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("resolution backend status %d", status)
		}
		items, _, _, err := strictPersonalPage(value, personalShapeQueryResult, 0)
		if err != nil {
			return nil, err
		}
		batchResult := make(map[string]map[string]any, len(items))
		for _, item := range items {
			id, ok := personalItemID(item)
			if !ok {
				return nil, fmt.Errorf("resolution item has malformed Id")
			}
			if _, ok := batchSet[id]; !ok {
				return nil, fmt.Errorf("resolution returned unrequested ID %q", id)
			}
			if _, ok := batchResult[id]; ok {
				return nil, fmt.Errorf("resolution returned duplicate ID %q", id)
			}
			batchResult[id] = item
		}
		for _, id := range batchIDs {
			state, ok := snapshot.States[id]
			if !ok {
				state = PlaybackState{GatewayUserID: p.session.GatewayUserID, SyntheticUserID: p.session.SyntheticUserID, ItemID: id}
			}
			item, present := batchResult[id]
			outcome := reconcileResolvedItem(&state, item, present, now)
			if err := p.server.store.SavePlaybackResolution(ctx, state); err != nil {
				return nil, fmt.Errorf("%w: save playback resolution: %w", ErrStoreUnavailable, err)
			}
			if outcome == resolutionKeep {
				rewritten := p.server.rewritePlannedPersonalItem(item, p.session, upstream, p.gatewayToken, p.request)
				rewrittenID, ok := personalItemID(rewritten)
				if !ok || rewrittenID != id {
					return nil, fmt.Errorf("resolution rewrite produced malformed item for ID %q", id)
				}
				result[id] = resolvedPersonalItem{item: rewritten, state: state}
			}
		}
	}
	out := make([]resolvedPersonalItem, 0, len(result))
	for _, id := range ids {
		if item, ok := result[id]; ok {
			out = append(out, item)
		}
	}
	return out, nil
}

func (p *personalPlanSource) refineResolved(ctx context.Context, plan personalPlan, items []resolvedPersonalItem) ([]resolvedPersonalItem, error) {
	if p == nil || p.server == nil || p.request == nil || p.session == nil || p.server.metadataUpstream == nil {
		return nil, fmt.Errorf("missing personal refinement source dependency")
	}
	q, effective := refinementQueryForPlan(plan)
	if !effective || len(items) == 0 {
		return append([]resolvedPersonalItem(nil), items...), nil
	}
	ids := make([]string, len(items))
	seen := make(map[string]struct{}, len(items))
	for i, joined := range items {
		id, ok := personalItemID(joined.item)
		if !ok {
			return nil, fmt.Errorf("refinement item %d has malformed Id", i)
		}
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate refinement ID %q", id)
		}
		seen[id] = struct{}{}
		ids[i] = id
	}
	result := make(map[string]map[string]any, len(items))
	for start := 0; start < len(ids); start += personalIDBatchLimit {
		end := start + personalIDBatchLimit
		if end > len(ids) {
			end = len(ids)
		}
		batchIDs := ids[start:end]
		batchSet := make(map[string]struct{}, len(batchIDs))
		for _, id := range batchIDs {
			batchSet[id] = struct{}{}
		}
		batchQuery := cloneQuery(q)
		batchQuery.Set("Ids", strings.Join(batchIDs, ","))
		value, status, upstream, err := p.server.fetchBackendJSON(ctx, p.request, "/Users/"+p.session.SyntheticUserID+"/Items", batchQuery.Encode(), p.session, p.gatewayToken)
		if err != nil {
			return nil, err
		}
		if status < http.StatusOK || status >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("refinement backend status %d", status)
		}
		returned, _, _, err := strictPersonalPage(value, personalShapeQueryResult, 0)
		if err != nil {
			return nil, err
		}
		batchResult := make(map[string]map[string]any, len(returned))
		for _, item := range returned {
			id, ok := personalItemID(item)
			if !ok {
				return nil, fmt.Errorf("refinement item has malformed Id")
			}
			if _, requested := batchSet[id]; !requested {
				return nil, fmt.Errorf("refinement returned unrequested ID %q", id)
			}
			if _, duplicate := batchResult[id]; duplicate {
				return nil, fmt.Errorf("refinement returned duplicate ID %q", id)
			}
			rewritten := p.server.rewritePlannedPersonalItem(item, p.session, upstream, p.gatewayToken, p.request)
			rewrittenID, ok := personalItemID(rewritten)
			if !ok || rewrittenID != id {
				return nil, fmt.Errorf("refinement rewrite produced malformed item for ID %q", id)
			}
			batchResult[id] = rewritten
		}
		for id, item := range batchResult {
			result[id] = item
		}
	}
	out := make([]resolvedPersonalItem, 0, len(result))
	for i, id := range ids {
		if item, matched := result[id]; matched {
			out = append(out, resolvedPersonalItem{item: item, state: items[i].state})
		}
	}
	return out, nil
}

func resolutionQueryForPlan(plan personalPlan) url.Values {
	q := url.Values{}
	copyResolutionFields(q, plan.Projection)
	appendResolutionSortFields(q, plan.Sort)
	return q
}

func sanitizeCandidateQuery(q url.Values) {
	for key := range q {
		if strings.EqualFold(key, "EnableUserData") || strings.EqualFold(key, "EnableUserDatas") || strings.EqualFold(key, "UserData") || strings.EqualFold(key, "SortBy") || strings.EqualFold(key, "SortOrder") || isPersonalDirectResolutionKey(key) {
			delete(q, key)
		}
	}
	canonicalizeResolutionList(q, "Filters")
	if filters := splitFilterValues(q["Filters"]); len(filters) != 0 {
		kept := filters[:0]
		for _, filter := range filters {
			if strings.EqualFold(filter, "IsFolder") || strings.EqualFold(filter, "IsNotFolder") {
				kept = append(kept, filter)
			}
		}
		if len(kept) == 0 {
			delete(q, "Filters")
		} else {
			q.Set("Filters", strings.Join(kept, ","))
		}
	}
}

func refinementQueryForPlan(plan personalPlan) (url.Values, bool) {
	q := cloneQuery(plan.Refinement)
	for key := range q {
		if isEgressCredentialQueryKey(strings.ToLower(key)) || strings.EqualFold(key, "Fields") || strings.EqualFold(key, "EnableUserData") || strings.EqualFold(key, "EnableUserDatas") || strings.EqualFold(key, "UserData") || strings.EqualFold(key, "UserId") || strings.EqualFold(key, "SortBy") || strings.EqualFold(key, "SortOrder") || strings.EqualFold(key, "StartIndex") || strings.EqualFold(key, "Limit") || strings.EqualFold(key, "Ids") || isPersonalDirectResolutionKey(key) {
			delete(q, key)
		}
	}
	canonicalizeResolutionList(q, "Filters")
	if filters := splitFilterValues(q["Filters"]); len(filters) != 0 {
		kept := filters[:0]
		for _, filter := range filters {
			if strings.EqualFold(filter, "IsFolder") || strings.EqualFold(filter, "IsNotFolder") {
				kept = append(kept, filter)
			}
		}
		if len(kept) == 0 {
			delete(q, "Filters")
		} else {
			q.Set("Filters", strings.Join(kept, ","))
		}
	}
	effective := len(q) != 0
	copyResolutionFields(q, plan.Projection)
	appendResolutionSortFields(q, plan.Sort)
	return q, effective
}

func copyResolutionFields(q url.Values, projection url.Values) {
	var fields []string
	canonicalizeResolutionList(q, "Fields")
	fields = append(fields, q["Fields"]...)
	for key, values := range projection {
		if strings.EqualFold(key, "Fields") {
			fields = append(fields, values...)
		}
	}
	kept := make([]string, 0, len(fields))
	seen := make(map[string]struct{}, len(fields))
	for _, field := range splitFilterValues(fields) {
		folded := strings.ToLower(field)
		if folded == "userdata" {
			continue
		}
		if _, exists := seen[folded]; exists {
			continue
		}
		seen[folded] = struct{}{}
		kept = append(kept, field)
	}
	delete(q, "Fields")
	if len(kept) != 0 {
		q.Set("Fields", strings.Join(kept, ","))
	}
}

func appendResolutionSortFields(q url.Values, sortTerms []personalSortTerm) {
	sortNames := make([]string, 0, len(sortTerms))
	for _, term := range sortTerms {
		if term.Source == personalSortMetadata {
			sortNames = append(sortNames, term.Name)
		}
	}
	appendResolutionFields(q, sortNames)
}

func canonicalizeResolutionList(q url.Values, canonical string) {
	var values []string
	for key, entries := range q {
		if strings.EqualFold(key, canonical) {
			values = append(values, entries...)
			delete(q, key)
		}
	}
	if len(values) != 0 {
		q[canonical] = values
	}
}

func isPersonalDirectResolutionKey(key string) bool {
	switch strings.ToLower(key) {
	case "isplayed", "isfavorite", "isresumable", "isliked", "isdisliked":
		return true
	default:
		return false
	}
}
