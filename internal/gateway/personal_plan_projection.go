package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
)

var errInvalidPlannedPersonalProjection = errors.New("invalid planned personal projection")

func (s *Server) rewritePlannedPersonalItem(item map[string]any, session *Session, upstream upstreamRequestSnapshot, gatewayToken string, request *http.Request) map[string]any {
	if item == nil {
		return nil
	}
	base := ""
	if s != nil {
		base = s.gatewayBaseForRequest(request)
	}
	serverID := ""
	if s != nil {
		serverID = s.cfg.GatewayServerID
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	projected, err := projectBaseItemDocument(raw, responseProjectionContext{
		session: session, upstream: upstream, gatewayToken: gatewayToken,
		publicGatewayBase: base, gatewayServerID: serverID,
	})
	if err != nil {
		return nil
	}
	if err := validateCredentialSafeJSON(projected, controlledProjectionMaxBytes, upstream.token); err != nil {
		return nil
	}
	var rewritten map[string]any
	if err := decodeJSONUseNumber(projected, &rewritten); err != nil {
		return nil
	}
	return rewritten
}

// projectPlannedPersonalItems only consumes the already joined local states.
// In particular, it deliberately does not discover aggregates or child counts.
func (s *Server) projectPlannedPersonalItems(items []resolvedPersonalItem, session *Session) ([]map[string]any, error) {
	if session == nil {
		return nil, errInvalidPlannedPersonalProjection
	}
	result := make([]map[string]any, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, joined := range items {
		if joined.item == nil {
			return nil, errInvalidPlannedPersonalProjection
		}
		itemID, ok := personalItemID(joined.item)
		if !ok || !safeItemID(itemID) || joined.state.ItemID == "" || joined.state.ItemID != itemID {
			return nil, errInvalidPlannedPersonalProjection
		}
		if _, ok := seen[itemID]; ok {
			return nil, errInvalidPlannedPersonalProjection
		}
		seen[itemID] = struct{}{}
		if (joined.state.GatewayUserID != "" && joined.state.GatewayUserID != session.GatewayUserID) ||
			(joined.state.SyntheticUserID != "" && joined.state.SyntheticUserID != session.SyntheticUserID) {
			return nil, errInvalidPlannedPersonalProjection
		}
		item := clonePlannedPersonalJSONMap(joined.item)
		userData, ok := mapField(item, "UserData")
		if !ok {
			userData = map[string]any{}
			item["UserData"] = userData
		}
		state := joined.state
		if state.GatewayUserID == "" {
			state.GatewayUserID = session.GatewayUserID
		}
		if state.SyntheticUserID == "" {
			state.SyntheticUserID = session.SyntheticUserID
		}
		applyPlaybackStateToUserData(userData, &state, item, nil)
		result = append(result, item)
	}
	return result, nil
}

func clonePlannedPersonalJSONMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = clonePlannedPersonalJSONValue(value)
	}
	return output
}

func clonePlannedPersonalJSONValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return clonePlannedPersonalJSONMap(value)
	case []any:
		output := make([]any, len(value))
		for i, element := range value {
			output[i] = clonePlannedPersonalJSONValue(element)
		}
		return output
	default:
		return value
	}
}
