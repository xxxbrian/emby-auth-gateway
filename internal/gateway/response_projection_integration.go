package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

func responseProjectionForRoute(method, rel string, decision routeclass.Decision) responseProjection {
	if decision.Ownership == routeclass.LegacyProxy || decision.Operation == routeclass.OperationLegacyProxy {
		return newResponseProjection(responseProjectionLegacyCompatibility)
	}
	if decision.Operation == routeclass.OperationPlaybackInfo {
		return newResponseProjection(responseProjectionPlaybackInfo)
	}
	if decision.Operation == routeclass.OperationLiveStreamOpen {
		return newResponseProjection(responseProjectionLiveStreamResponse)
	}
	if decision.Operation == routeclass.OperationLiveStreamMediaInfo {
		return newResponseProjection(responseProjectionMediaSource)
	}
	parts := responseProjectionPathParts(rel)
	switch {
	case len(parts) == 2 && parts[0] == "system" && parts[1] == "info":
		return newResponseProjection(responseProjectionSystemInfo)
	case isBaseItemEnvelopeArrayRoute(parts):
		return newResponseProjection(responseProjectionBaseItemEnvelopeArray)
	case isAllThemeMediaRoute(parts):
		return newResponseProjection(responseProjectionAllThemeMedia)
	case isDeclaredBaseItemArrayRoute(parts):
		return newResponseProjection(responseProjectionBaseItemArray)
	case isBaseItemEnvelopeRoute(parts):
		return newResponseProjection(responseProjectionBaseItemEnvelope)
	case isDirectBaseItemRoute(parts):
		return newResponseProjection(responseProjectionBaseItem)
	default:
		return newResponseProjection(responseProjectionOpaque)
	}
}

func responseProjectionPathParts(rel string) []string {
	raw := strings.Split(strings.Trim(rel, "/"), "/")
	parts := make([]string, 0, len(raw))
	for _, part := range raw {
		if part != "" {
			parts = append(parts, strings.ToLower(part))
		}
	}
	return parts
}

func isDirectBaseItemRoute(parts []string) bool {
	if len(parts) == 2 && parts[0] == "items" && parts[1] != "" {
		return !isReservedItemsPath(parts[1])
	}
	return (len(parts) == 4 && parts[0] == "users" && parts[1] != "" && parts[2] == "items" && parts[3] != "") ||
		(len(parts) == 2 && (parts[0] == "artists" || parts[0] == "genres" || parts[0] == "persons" || parts[0] == "studios") && parts[1] != "" && !isReservedByNamePath(parts[0], parts[1]))
}

func isBaseItemEnvelopeRoute(parts []string) bool {
	switch {
	case len(parts) == 1 && (parts[0] == "items" || parts[0] == "artists" || parts[0] == "genres" || parts[0] == "persons" || parts[0] == "studios" || parts[0] == "trailers" || parts[0] == "channels"):
		return true
	case len(parts) == 2 && parts[0] == "artists" && (parts[1] == "albumartists" || parts[1] == "instantmix"):
		return true
	case len(parts) == 3 && parts[0] == "items" && parts[1] != "" && (parts[2] == "similar" || parts[2] == "instantmix" || parts[2] == "criticreviews" || parts[2] == "themesongs" || parts[2] == "themevideos"):
		return true
	case len(parts) == 3 && (parts[0] == "artists" || parts[0] == "movies" || parts[0] == "shows" || parts[0] == "trailers") && parts[1] != "" && parts[2] == "similar":
		return true
	case len(parts) == 3 && parts[0] == "users" && parts[1] != "" && (parts[2] == "items" || parts[2] == "views" || parts[2] == "suggestions"):
		return true
	case len(parts) == 5 && parts[0] == "users" && parts[1] != "" && parts[2] == "sections" && parts[3] != "" && parts[4] == "items":
		return true
	case len(parts) == 5 && parts[0] == "users" && parts[1] != "" && parts[2] == "items" && parts[3] != "" && parts[4] == "intros":
		return true
	case len(parts) == 3 && parts[0] == "shows" && parts[1] != "" && (parts[2] == "episodes" || parts[2] == "seasons"):
		return true
	case len(parts) == 2 && parts[0] == "shows" && (parts[1] == "missing" || parts[1] == "upcoming" || parts[1] == "nextup"):
		return true
	case len(parts) == 4 && parts[0] == "users" && parts[1] != "" && parts[2] == "items" && parts[3] == "resume":
		return true
	case len(parts) == 3 && parts[0] == "videos" && parts[1] != "" && parts[2] == "additionalparts":
		return true
	default:
		return false
	}
}

func isDeclaredBaseItemArrayRoute(parts []string) bool {
	return (len(parts) == 3 && parts[0] == "items" && parts[1] != "" && parts[2] == "ancestors") ||
		(len(parts) == 4 && parts[0] == "users" && parts[1] != "" && parts[2] == "items" && parts[3] == "latest") ||
		(len(parts) == 5 && parts[0] == "users" && parts[1] != "" && parts[2] == "items" && parts[3] != "" && (parts[4] == "localtrailers" || parts[4] == "specialfeatures"))
}

func isBaseItemEnvelopeArrayRoute(parts []string) bool {
	return len(parts) == 2 && parts[0] == "movies" && parts[1] == "recommendations"
}

func isAllThemeMediaRoute(parts []string) bool {
	return len(parts) == 3 && parts[0] == "items" && parts[1] != "" && parts[2] == "thememedia"
}

func isReservedItemsPath(part string) bool {
	switch part {
	case "counts", "prefixes", "intros", "filters", "filters2", "remoteimages", "remotesearch", "deleteinfo", "editor", "thumbnail", "subtitles", "images":
		return true
	default:
		return false
	}
}

func isReservedByNamePath(family, name string) bool {
	return family == "artists" && name == "prefixes"
}

func (s *Server) responseProjectionContext(ctx context.Context, r *http.Request, session *Session, upstream upstreamRequestSnapshot, gatewayToken, publicGatewayBase string) responseProjectionContext {
	return s.responseProjectionContextWithStates(ctx, r, session, upstream, gatewayToken, publicGatewayBase, nil, false)
}

func (s *Server) responseProjectionContextForDocument(ctx context.Context, r *http.Request, session *Session, upstream upstreamRequestSnapshot, gatewayToken, publicGatewayBase string, data []byte, projection responseProjection) (responseProjectionContext, error) {
	if session == nil {
		return s.responseProjectionContext(ctx, r, session, upstream, gatewayToken, publicGatewayBase), nil
	}
	documents, err := projectedBaseItemDocuments(data, projection)
	if err != nil {
		return responseProjectionContext{}, err
	}
	if len(documents) == 0 {
		return responseProjectionContext{
			session: session, upstream: upstream, gatewayToken: gatewayToken,
			publicGatewayBase: publicGatewayBase, gatewayServerID: s.cfg.GatewayServerID,
		}, nil
	}
	itemIDs := make([]string, 0, len(documents))
	items := make([]map[string]any, 0, len(documents))
	itemsByID := make(map[string]map[string]any, len(documents))
	seen := make(map[string]bool, len(documents))
	for _, raw := range documents {
		item, err := parseBaseItemDocument(raw)
		if err != nil {
			return responseProjectionContext{}, err
		}
		itemID, ok := item.itemID()
		if !ok {
			continue
		}
		if !safeItemID(itemID) {
			return responseProjectionContext{}, errInvalidBaseItemProjection
		}
		var itemMap map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&itemMap); err != nil {
			return responseProjectionContext{}, err
		}
		items = append(items, itemMap)
		itemsByID[itemID] = itemMap
		if !seen[itemID] {
			seen[itemID] = true
			itemIDs = append(itemIDs, itemID)
		}
	}
	states, err := s.store.ListPlaybackStatesByItemIDs(ctx, session.GatewayUserID, itemIDs)
	if err != nil {
		states = map[string]*PlaybackState{}
	}
	seriesIDs, seasonIDs := aggregateItemIDs(items)
	aggregates, err := s.store.ListPlaybackAggregates(ctx, session.GatewayUserID, seriesIDs, seasonIDs)
	if err != nil {
		aggregates = PlaybackAggregates{Series: map[string]PlaybackAggregate{}, Seasons: map[string]PlaybackAggregate{}}
	}
	s.applyChildCountsToAggregates(ctx, r, session, gatewayToken, items, &aggregates)
	result := responseProjectionContext{
		session: session, upstream: upstream, gatewayToken: gatewayToken,
		publicGatewayBase: publicGatewayBase, gatewayServerID: s.cfg.GatewayServerID,
	}
	result.overlayBaseItem = func(item *baseItemDocument) error {
		itemID, ok := item.itemID()
		if !ok {
			for _, name := range []string{"Item", "Items", "UserData"} {
				if _, exists := item.doc.GetFold(name); exists {
					return errInvalidBaseItemProjection
				}
			}
			return nil
		}
		state := states[itemID]
		if state == nil {
			state = &PlaybackState{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, ItemID: itemID}
		} else {
			copy := *state
			state = &copy
			state.SyntheticUserID = session.SyntheticUserID
		}
		return item.overlayPlaybackState(*state, aggregateForItem(itemsByID[itemID], itemID, aggregates))
	}
	return result, nil
}

func (s *Server) responseProjectionContextWithStates(ctx context.Context, r *http.Request, session *Session, upstream upstreamRequestSnapshot, gatewayToken, publicGatewayBase string, states map[string]*PlaybackState, statesLoaded bool) responseProjectionContext {
	projection := responseProjectionContext{
		session: session, upstream: upstream, gatewayToken: gatewayToken,
		publicGatewayBase: publicGatewayBase, gatewayServerID: s.cfg.GatewayServerID,
	}
	if session == nil {
		return projection
	}
	projection.overlayBaseItem = func(item *baseItemDocument) error {
		itemID, ok := item.itemID()
		if !ok {
			for _, name := range []string{"Item", "Items", "UserData"} {
				if _, exists := item.doc.GetFold(name); exists {
					return errInvalidBaseItemProjection
				}
			}
			return nil
		}
		if !safeItemID(itemID) {
			return errInvalidBaseItemProjection
		}
		var state *PlaybackState
		if statesLoaded {
			state = states[itemID]
		} else {
			state, _ = s.store.FindPlaybackState(ctx, session.GatewayUserID, itemID)
		}
		if state == nil {
			state = &PlaybackState{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, ItemID: itemID}
		} else {
			copy := *state
			state = &copy
			state.SyntheticUserID = session.SyntheticUserID
		}

		raw, err := item.marshal()
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var itemMap map[string]any
		if err := decoder.Decode(&itemMap); err != nil {
			return err
		}
		seriesIDs, seasonIDs := aggregateItemIDs([]map[string]any{itemMap})
		aggregates, err := s.store.ListPlaybackAggregates(ctx, session.GatewayUserID, seriesIDs, seasonIDs)
		if err != nil {
			aggregates = PlaybackAggregates{Series: map[string]PlaybackAggregate{}, Seasons: map[string]PlaybackAggregate{}}
		}
		s.applyChildCountsToAggregates(ctx, r, session, gatewayToken, []map[string]any{itemMap}, &aggregates)
		return item.overlayPlaybackState(*state, aggregateForItem(itemMap, itemID, aggregates))
	}
	return projection
}

func isSuccessfulResponse(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices
}

func isTextContentType(contentType string) bool {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return strings.HasPrefix(mediaType, "text/") || strings.HasSuffix(mediaType, "+xml") || mediaType == "application/xml"
}

func validateCredentialSafeResponse(data []byte, jsonDocument bool, upstream upstreamRequestSnapshot) error {
	if jsonDocument {
		return validateCredentialSafeJSON(data, proxyJSONLimit, upstream.token)
	}
	return validateCredentialSafeText(data, proxyJSONLimit, upstream.token)
}

func isResponseCredentialHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "proxy-authenticate", "www-authenticate", "authentication-info", "set-cookie", "x-emby-token", "x-mediabrowser-token", "x-emby-authorization", "x-mediabrowser-authorization":
		return true
	default:
		return false
	}
}

func clearProjectedEntityHeaders(header http.Header) {
	for _, name := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Content-Encoding", "ETag", "Last-Modified", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest"} {
		header.Del(name)
	}
}

func clearProjectionFailureHeaders(header http.Header) {
	clearProjectedEntityHeaders(header)
	for _, name := range []string{"Content-Type", "Content-Encoding", "Location", "Content-Location", "Set-Cookie", "Authorization", "Proxy-Authorization", "Proxy-Authenticate", "WWW-Authenticate", "Authentication-Info", "X-Emby-Token", "X-MediaBrowser-Token", "X-Emby-Authorization", "X-MediaBrowser-Authorization"} {
		header.Del(name)
	}
}

func (s *Server) writeResponseProjectionFailure(w http.ResponseWriter, r *http.Request, rel string, session *Session) {
	resetProjectionFailureHeaders(w.Header())
	applyResourceCachePolicy(w.Header(), resourceRouteFromContext(r), http.StatusBadGateway)
	s.audit(r.Context(), AuditLog{GatewayUserID: sessionGatewayUserID(session), SyntheticUserID: sessionSyntheticUserID(session), Event: "proxy_projection_failed", Message: "backend response projection failed", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusBadGateway})
	http.Error(w, "backend unavailable", http.StatusBadGateway)
}

func appendJSONNewline(data []byte) []byte {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return append(data, '\n')
	}
	return data
}
