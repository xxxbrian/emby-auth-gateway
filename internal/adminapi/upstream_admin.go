package adminapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/controlplane"
	"github.com/xxxbrian/emby-auth-gateway/internal/routepolicy"
)

// --- upstream endpoint admin ---

func (s *Server) handleListEndpoints(e *core.RequestEvent) error {
	items, err := controlplane.ListEndpoints(e.Request.Context(), e.App)
	if err != nil {
		return e.InternalServerError("list endpoints failed", err)
	}
	if items == nil {
		items = []controlplane.EndpointDTO{}
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

type endpointBody struct {
	Key       string `json:"key"`
	BaseURL   string `json:"base_url"`
	Enabled   bool   `json:"enabled"`
	IsDefault bool   `json:"is_default"`
}

func (s *Server) handleCreateEndpoint(e *core.RequestEvent) error {
	var body endpointBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	dto, err := controlplane.UpsertEndpoint(e.Request.Context(), e.App, controlplane.EndpointUpsertInput{
		Key: body.Key, BaseURL: body.BaseURL, Enabled: body.Enabled, IsDefault: body.IsDefault,
	})
	if err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_upstream_endpoint_create", fmt.Sprintf("actor=%s created endpoint key=%s url=%s enabled=%t default=%t", actorSummary(e), dto.Key, dto.BaseURL, dto.Enabled, dto.IsDefault))
	return e.JSON(http.StatusOK, dto)
}

func (s *Server) handleUpdateEndpoint(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	var body endpointBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	dto, err := controlplane.UpsertEndpoint(e.Request.Context(), e.App, controlplane.EndpointUpsertInput{
		ID: id, Key: body.Key, BaseURL: body.BaseURL, Enabled: body.Enabled, IsDefault: body.IsDefault,
	})
	if err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_upstream_endpoint_update", fmt.Sprintf("actor=%s updated endpoint key=%s url=%s enabled=%t default=%t", actorSummary(e), dto.Key, dto.BaseURL, dto.Enabled, dto.IsDefault))
	return e.JSON(http.StatusOK, dto)
}

func (s *Server) handleDeleteEndpoint(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	if err := controlplane.DeleteEndpoint(e.Request.Context(), e.App, id); err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_upstream_endpoint_delete", fmt.Sprintf("actor=%s deleted endpoint id=%s", actorSummary(e), id))
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleProbeEndpointWebSocket(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	endpoint, err := controlplane.EndpointByID(e.Request.Context(), e.App, id)
	if err != nil {
		return e.BadRequestError("endpoint not found", err)
	}
	// Probe as a real Emby client would: present the shared backend token.
	// Anonymous probes can falsely report "not capable" on ingress/CDN layers
	// that require credentials before the WebSocket upgrade succeeds.
	token := ""
	if source, err := controlplane.LoadUpstreamState(e.App); err == nil && source.Source != nil {
		token = source.Source.GetString("backend_token")
	}
	probeErr := controlplane.ProbeEndpointWebSocket(e.Request.Context(), endpoint.BaseURL, "", token)
	capable := probeErr == nil
	if recordErr := controlplane.RecordWebSocketProbeResult(e.Request.Context(), e.App, id, capable, probeErr); recordErr != nil {
		return e.InternalServerError("record probe result failed", recordErr)
	}
	_ = s.auditAdmin(e, "admin_upstream_endpoint_probe_ws", fmt.Sprintf("actor=%s probed websocket endpoint key=%s url=%s capable=%t", actorSummary(e), endpoint.Key, endpoint.BaseURL, capable))
	if !capable {
		return e.JSON(http.StatusOK, map[string]any{
			"websocket_capable": false,
			"error":             probeErr.Error(),
		})
	}
	return e.JSON(http.StatusOK, map[string]any{"websocket_capable": true})
}

// --- route rule admin ---

func (s *Server) handleListRouteRules(e *core.RequestEvent) error {
	items, err := controlplane.ListRouteRules(e.Request.Context(), e.App)
	if err != nil {
		return e.InternalServerError("list route rules failed", err)
	}
	if items == nil {
		items = []controlplane.RouteRuleDTO{}
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handlePreviewRouteRule(e *core.RequestEvent) error {
	q := e.Request.URL.Query()
	req := routepolicy.Request{
		Method:    q.Get("method"),
		Path:      q.Get("path"),
		Transport: q.Get("transport"),
	}
	if req.Path == "" {
		return e.BadRequestError("path is required", nil)
	}
	items, err := controlplane.ListRouteRules(e.Request.Context(), e.App)
	if err != nil {
		return e.InternalServerError("route rule preview failed", err)
	}
	rules := make([]routepolicy.Rule, 0, len(items))
	for _, item := range items {
		rules = append(rules, routepolicy.Rule{
			ID: item.ID, Method: item.Method, Path: item.Path, Transport: item.Transport,
			Target: item.Target, Priority: item.Priority, Enabled: item.Enabled, Reason: item.Reason,
		})
	}
	target := routepolicy.Select(rules, req)
	resp := map[string]any{"target": target}
	if target == "" {
		resp["reason"] = "no rule matches; default endpoint will be used"
	}
	return e.JSON(http.StatusOK, resp)
}

type routeRuleBody struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Transport string `json:"transport"`
	Target    string `json:"target"`
	Priority  int    `json:"priority"`
	Enabled   *bool  `json:"enabled"`
	Reason    string `json:"reason"`
	Updated   string `json:"updated"`
}

func (s *Server) handleCreateRouteRule(e *core.RequestEvent) error {
	var body routeRuleBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	rule, err := controlplane.UpsertRouteRule(e.Request.Context(), e.App, routepolicy.Rule{
		Method: body.Method, Path: body.Path, Transport: body.Transport, Target: body.Target,
		Priority: body.Priority, Enabled: enabled, Reason: body.Reason,
	})
	if err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_route_rule_create", fmt.Sprintf("actor=%s created route rule id=%s method=%s path=%s target=%s transport=%s", actorSummary(e), rule.ID, rule.Method, rule.Path, rule.Target, rule.Transport))
	return e.JSON(http.StatusOK, rule)
}

func (s *Server) handleUpdateRouteRule(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	var body routeRuleBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	var updated time.Time
	if raw := strings.TrimSpace(body.Updated); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			return e.BadRequestError("updated must be RFC3339", err)
		}
		updated = t.UTC()
	}
	rule, err := controlplane.UpsertRouteRule(e.Request.Context(), e.App, routepolicy.Rule{
		ID: id, Method: body.Method, Path: body.Path, Transport: body.Transport, Target: body.Target,
		Priority: body.Priority, Enabled: enabled, Reason: body.Reason, Updated: updated,
	})
	if err != nil {
		if err == controlplane.ErrRuleConflict {
			return e.JSON(http.StatusConflict, map[string]any{"error": "rule_conflict", "message": "route rule was modified; reload and retry"})
		}
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_route_rule_update", fmt.Sprintf("actor=%s updated route rule id=%s path=%s target=%s", actorSummary(e), rule.ID, rule.Path, rule.Target))
	return e.JSON(http.StatusOK, rule)
}

func (s *Server) handleDeleteRouteRule(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	if err := controlplane.DeleteRouteRule(e.Request.Context(), e.App, id); err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_route_rule_delete", fmt.Sprintf("actor=%s deleted route rule id=%s", actorSummary(e), id))
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}
