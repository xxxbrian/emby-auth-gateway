package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (s *Server) writePersonalPlanHTTP(w http.ResponseWriter, r *http.Request, rel string, route personalRouteKind, session *Session, gatewayToken string) {
	w.Header().Set("Cache-Control", "no-store")
	plan, err := preparePersonalHTTPPlan(route, rel, r.URL.Query(), session)
	if err != nil {
		s.writePersonalPlanError(w, r, rel, session, err)
		return
	}
	if plan.Kind == personalPlanPassthrough {
		s.writePersonalPlanPassthrough(w, r, rel, session, gatewayToken, plan)
		return
	}

	var source *personalPlanSource
	latestZero := plan.Kind == personalPlanLatest && plan.Page.Limit != nil && *plan.Page.Limit == 0
	if !latestZero {
		source, err = newPersonalPlanSource(s, r, session, gatewayToken)
		if err != nil {
			s.writePersonalPlanInternalError(w, r, rel, session)
			return
		}
	}
	result, err := executePersonalHTTPPlan(r, source, plan)
	if err != nil {
		s.writePersonalPlanError(w, r, rel, session, err)
		return
	}
	items, err := s.projectPlannedPersonalItems(result.Items, session)
	if err != nil {
		s.writePersonalPlanInternalError(w, r, rel, session)
		return
	}
	if plan.Shape == personalShapeArray {
		writeJSON(w, http.StatusOK, items)
		return
	}
	if result.Total == nil || *result.Total < 0 || result.StartIndex < 0 {
		s.writePersonalPlanInternalError(w, r, rel, session)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Items": items, "TotalRecordCount": *result.Total, "StartIndex": result.StartIndex,
	})
}

func preparePersonalHTTPPlan(route personalRouteKind, rel string, query url.Values, session *Session) (personalPlan, error) {
	if session == nil {
		return personalPlan{}, ErrForbidden
	}
	if !relUserMatches(rel, session.SyntheticUserID) {
		return personalPlan{}, ErrForbidden
	}
	plan, err := parsePersonalPlan(route, rel, query)
	if err != nil {
		return personalPlan{}, err
	}
	if !personalQueryUserMatches(plan.Neutral, session.SyntheticUserID) {
		return personalPlan{}, ErrForbidden
	}
	if route != personalRouteShowItems {
		return plan, nil
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 3 || !strings.EqualFold(parts[0], "Shows") || !safeItemID(parts[1]) {
		return personalPlan{}, fmt.Errorf("%w: invalid Show item path", ErrBadRequest)
	}
	itemType := ""
	switch {
	case strings.EqualFold(parts[2], "Episodes"):
		itemType = "Episode"
	case strings.EqualFold(parts[2], "Seasons"):
		itemType = "Season"
	default:
		return personalPlan{}, fmt.Errorf("%w: invalid Show item path", ErrBadRequest)
	}
	if err := validateShowPersonalCriteria(query, parts[1], itemType); err != nil {
		return personalPlan{}, err
	}
	plan.Neutral.Set("SeriesId", parts[1])
	plan.Neutral.Set("IncludeItemTypes", itemType)
	plan.Refinement.Set("SeriesId", parts[1])
	plan.Refinement.Set("IncludeItemTypes", itemType)
	return plan, nil
}

func personalQueryUserMatches(query url.Values, syntheticUserID string) bool {
	values, present := query["UserId"]
	if !present {
		return true
	}
	return len(values) == 1 && values[0] == syntheticUserID
}

func validateShowPersonalCriteria(query url.Values, seriesID, itemType string) error {
	for key, values := range query {
		switch {
		case strings.EqualFold(key, "SeriesId"):
			if len(values) != 1 || values[0] != seriesID {
				return fmt.Errorf("%w: Show SeriesId conflicts with path", ErrBadRequest)
			}
		case strings.EqualFold(key, "IncludeItemTypes"):
			members := splitPersonalList(values)
			if len(members) != 1 || !strings.EqualFold(members[0], itemType) {
				return fmt.Errorf("%w: Show IncludeItemTypes conflicts with path", ErrBadRequest)
			}
		}
	}
	return nil
}

func executePersonalHTTPPlan(r *http.Request, source *personalPlanSource, plan personalPlan) (personalPlanResult, error) {
	switch plan.Kind {
	case personalPlanPositive:
		return executePositivePersonalPlan(r.Context(), source, plan)
	case personalPlanNegative:
		return executeNegativePersonalPlan(r.Context(), source, plan)
	case personalPlanResume:
		return executeResumePersonalPlan(r.Context(), source, plan)
	case personalPlanNextUp:
		return executeNextUpPersonalPlan(r.Context(), source, plan)
	case personalPlanLatest:
		return executeLatestPersonalPlan(r.Context(), source, plan)
	default:
		return personalPlanResult{}, fmt.Errorf("invalid local personal plan kind")
	}
}

func (s *Server) writePersonalPlanPassthrough(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string, plan personalPlan) {
	value, status, upstream, err := s.fetchBackendJSON(r.Context(), r, rel, plan.Neutral.Encode(), session, gatewayToken)
	if err != nil {
		s.writePersonalPlanError(w, r, rel, session, err)
		return
	}
	if err := validatePersonalPassthroughDocument(value); err != nil {
		s.writePersonalPlanUpstreamError(w)
		return
	}
	rewritten := s.rewriteProxyJSONValueForRequestWithSnapshot(r.Context(), r, value, session, upstream, gatewayToken, s.gatewayBaseForRequest(r))
	writeJSON(w, status, rewritten)
}

func validatePersonalPassthroughDocument(value any) error {
	document, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"TotalRecordCount", "StartIndex"} {
		if _, err := optionalJSONInt(document, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) writePersonalPlanError(w http.ResponseWriter, r *http.Request, rel string, session *Session, err error) {
	switch {
	case errors.Is(err, ErrBadRequest):
		http.Error(w, "bad personal query", http.StatusBadRequest)
	case errors.Is(err, ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, ErrPersonalScanIncomplete):
		s.audit(r.Context(), AuditLog{
			GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID,
			Event: "personal_query_incomplete", Message: "personal query incomplete",
			RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusServiceUnavailable,
			ErrorKind: "personal_query_incomplete",
		})
		http.Error(w, "personal query unavailable", http.StatusServiceUnavailable)
	case errors.Is(err, ErrStoreUnavailable):
		s.writePersonalPlanInternalError(w, r, rel, session)
	default:
		s.writePersonalPlanUpstreamError(w)
	}
}

func (s *Server) writePersonalPlanInternalError(w http.ResponseWriter, _ *http.Request, _ string, _ *Session) {
	http.Error(w, "personal query unavailable", http.StatusInternalServerError)
}

func (s *Server) writePersonalPlanUpstreamError(w http.ResponseWriter) {
	http.Error(w, "backend unavailable", http.StatusBadGateway)
}
