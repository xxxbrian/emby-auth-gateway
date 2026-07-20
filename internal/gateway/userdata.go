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
	"strconv"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

type personalDataHandlingOutcome struct {
	Handled              bool
	NoteSuccess          bool
	AllowGenericActivity bool
}

var handledPersonalData = personalDataHandlingOutcome{Handled: true, NoteSuccess: true, AllowGenericActivity: true}

func (s *Server) handlePersonalDataRequest(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string) personalDataHandlingOutcome {
	if outcome := s.handleLocalSessionStateRequest(w, r, rel, session, gatewayToken); outcome.Handled {
		return outcome
	}
	if s.handleDisplayPreferences(w, r, rel, session) {
		return handledPersonalData
	}
	if s.handlePersonalStateWrite(w, r, rel, session, gatewayToken) {
		s.publishSessionsForUser(session.GatewayUserID)
		return handledPersonalData
	}
	if r.Method != http.MethodGet {
		return personalDataHandlingOutcome{}
	}
	switch {
	case isResumePath(rel):
		s.writePersonalPlanHTTP(w, r, rel, personalRouteResume, session, gatewayToken)
		return handledPersonalData
	case isNextUpPath(rel):
		s.writePersonalPlanHTTP(w, r, rel, personalRouteNextUp, session, gatewayToken)
		return handledPersonalData
	case isLatestItemsPath(rel):
		s.writePersonalPlanHTTP(w, r, rel, personalRouteLatest, session, gatewayToken)
		return handledPersonalData
	case isSessionsPath(rel):
		s.writeLocalSessions(w, r, session)
		return handledPersonalData
	case queryHasPersonalFilter(r.URL.Query()) && !isAllowedPersonalItemListPath(rel):
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "unsupported personal filter path", http.StatusBadRequest)
		return handledPersonalData
	case isAllowedPersonalItemListPath(rel):
		route := personalRouteItems
		if isShowPersonalItemListPath(rel) {
			route = personalRouteShowItems
		}
		s.writePersonalPlanHTTP(w, r, rel, route, session, gatewayToken)
		return handledPersonalData
	default:
		return personalDataHandlingOutcome{}
	}
}

func (s *Server) handleLocalSessionStateRequest(w http.ResponseWriter, r *http.Request, rel string, session *Session, gatewayToken string) personalDataHandlingOutcome {
	switch {
	case isPlaybackReportRequest(r.Method, rel), isPlaybackKeepaliveRequest(r.Method, rel):
		success := s.handlePlaybackReport(w, r, rel, session, gatewayToken)
		return personalDataHandlingOutcome{Handled: true, NoteSuccess: success}
	case isSessionCapabilitiesRequest(r.Method, rel):
		if equalPath(rel, "/Sessions/Capabilities/Full") {
			s.handleSessionCapabilitiesFull(w, r, session)
			s.publishSessionsForUser(session.GatewayUserID)
			return personalDataHandlingOutcome{Handled: true, NoteSuccess: true}
		}
		s.handleSessionCapabilitiesSlim(w, r, session)
		s.publishSessionsForUser(session.GatewayUserID)
		return personalDataHandlingOutcome{Handled: true, NoteSuccess: true}
	default:
		return personalDataHandlingOutcome{}
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
		body, err := readUserDataWriteBody(r, 2<<20)
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return true
		}
		writeState(parts[3], func(state *PlaybackState) {
			applyUserDataWriteToState(body, state, now)
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
	key := displayPreferenceRouteKey(parts[1], r.URL.Query(), session.Client)
	if r.Method == http.MethodGet {
		preference, err := s.store.FindDisplayPreference(r.Context(), session.GatewayUserID, key.PreferenceID, key.Client)
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
	if err := s.store.SaveDisplayPreference(r.Context(), DisplayPreference{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, PreferenceID: key.PreferenceID, Client: key.Client, PayloadJSON: string(data), UpdatedAt: time.Now().UTC()}); err != nil {
		http.Error(w, "display preferences unavailable", http.StatusInternalServerError)
		return true
	}
	writeRawJSON(w, http.StatusOK, data)
	return true
}

// writeLocalSessions serves GET /Sessions from gateway-owned session state only.
// Zero upstream Ensure/dial (including no /Sessions proxy). Filter first, then
// one batched ListCurrentPlaybacks and optional batched local UserData load.
// Returns a raw SessionInfo array with no-store.
func (s *Server) writeLocalSessions(w http.ResponseWriter, r *http.Request, session *Session) {
	items, err := s.projectLocalSessions(r.Context(), session, r.URL.Query())
	if err != nil {
		s.audit(r.Context(), AuditLog{
			GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID,
			Event: "session_projection_failed", Message: "session projection failed",
			RemoteIP: remoteIP(r), Method: r.Method, Path: r.URL.Path, Status: http.StatusInternalServerError,
			ErrorKind: "session_projection",
		})
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "sessions unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, items)
}

// projectLocalSessions is the transport-neutral local SessionInfo projector.
// It performs one active-session list, one current-playback batch, and one
// gateway-local UserData batch for the viewer's gateway user.
func (s *Server) projectLocalSessions(ctx context.Context, viewer *Session, filters url.Values) ([]any, error) {
	if viewer == nil {
		return nil, ErrUnauthorized
	}
	now := time.Now().UTC()
	sessions, err := s.sessions.ListActiveSessions(ctx, viewer.GatewayUserID, now)
	if err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	filtered := filterLocalSessions(sessions, viewer, filters)

	// Collect token hashes after filter; one batch current-playback load (no N+1).
	hashes := make([]string, 0, len(filtered))
	for i := range filtered {
		if h := filtered[i].GatewayTokenHash; h != "" {
			hashes = append(hashes, h)
		}
	}

	currents := map[string]CurrentPlayback{}
	if len(hashes) > 0 {
		repo, repoErr := s.playbackRepo()
		if repoErr != nil {
			return nil, repoErr
		}
		listed, listErr := repo.ListCurrentPlaybacks(ctx, hashes)
		if listErr != nil {
			return nil, fmt.Errorf("list current playbacks: %w", listErr)
		}
		if listed != nil {
			currents = listed
		}
	}

	// Validate currents (fail closed on integrity) and collect item IDs for UserData.
	itemIDs := make([]string, 0, len(currents))
	seenItems := make(map[string]struct{}, len(currents))
	validated := make(map[string]CurrentPlayback, len(currents))
	for i := range filtered {
		hash := filtered[i].GatewayTokenHash
		cp, ok := currents[hash]
		if !ok {
			continue
		}
		if err := ValidateCurrentPlayback(&cp, hash); err != nil {
			return nil, fmt.Errorf("validate current playback: %w", err)
		}
		validated[hash] = cp
		if _, seen := seenItems[cp.ItemID]; !seen && cp.ItemID != "" {
			seenItems[cp.ItemID] = struct{}{}
			itemIDs = append(itemIDs, cp.ItemID)
		}
	}

	// Batch gateway-local UserData for the authenticated gateway user only.
	states := map[string]*PlaybackState{}
	if len(itemIDs) > 0 {
		loaded, loadErr := s.store.ListPlaybackStatesByItemIDs(ctx, viewer.GatewayUserID, itemIDs)
		if loadErr != nil {
			return nil, fmt.Errorf("list playback states: %w", loadErr)
		}
		if loaded != nil {
			states = loaded
		}
	}

	items := make([]any, 0, len(filtered))
	for i := range filtered {
		hash := filtered[i].GatewayTokenHash
		var currentPtr *CurrentPlayback
		var userData *embyUserItemData
		if cp, ok := validated[hash]; ok {
			// Copy into local so each session keeps its own row (no shared pointer).
			cpCopy := cp
			currentPtr = &cpCopy
			state := states[cp.ItemID]
			if state == nil {
				state = &PlaybackState{
					GatewayUserID:   viewer.GatewayUserID,
					SyntheticUserID: viewer.SyntheticUserID,
					ItemID:          cp.ItemID,
				}
			}
			data := userDataWireDTO(state)
			userData = &data
		}
		var live func(SessionConnectionIdentity) bool
		if s.sessionHub != nil {
			live = s.sessionHub.Present
		}
		supportsRemoteControl := sessionSupportsRemoteControl(&filtered[i], live)
		items = append(items, sessionInfoWireDTO(&filtered[i], s.cfg.GatewayServerID, currentPtr, userData, supportsRemoteControl))
	}
	return items, nil
}

func (s *Server) fetchBackendJSON(ctx context.Context, r *http.Request, rel, rawQuery string, session *Session, gatewayToken string) (any, int, upstreamRequestSnapshot, error) {
	if s.metadataUpstream == nil {
		return nil, 0, upstreamRequestSnapshot{}, errors.New("metadata upstream unavailable")
	}
	query, err := parseRawQuery(rawQuery)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	if !relUserMatches(rel, session.SyntheticUserID) {
		return nil, 0, upstreamRequestSnapshot{}, ErrForbidden
	}
	for key, values := range query {
		if !strings.EqualFold(key, "UserId") {
			continue
		}
		for _, value := range values {
			if value != session.SyntheticUserID {
				return nil, 0, upstreamRequestSnapshot{}, ErrForbidden
			}
		}
	}
	runtime, err := s.managedAuthUpstream.Ensure(ctx)
	if err != nil {
		// Confirmed Ensure failure only; cancellation/deadline must not flip auth state.
		if ctx.Err() == nil {
			s.emitAuthUnavailable(session)
		}
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	upstream, err := upstreamRequestSnapshotFromRuntime(runtime)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	requestURL := (&url.URL{Path: rel, RawQuery: query.Encode()}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	request := metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Session: session, Snapshot: upstream, refreshResult: s.upstreamRefreshReporter(ctx, r, rel, session)}, Ownership: routeclass.MetadataProxy, Internal: true, SnapshotRef: &upstream}
	resp, err := s.metadataUpstream.RoundTripMetadata(request)
	if err != nil {
		return nil, 0, upstreamRequestSnapshot{}, err
	}
	wrapResponseBodyOnce(resp)
	defer resp.Body.Close()
	data, err := readLimited(resp.Body, proxyJSONLimit)
	if err != nil {
		return nil, resp.StatusCode, upstream, err
	}
	var value any
	if err := decodeJSONUseNumber(data, &value); err != nil {
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

type userDataWriteBody struct {
	Played                json.RawMessage `json:"Played"`
	PlaybackPositionTicks json.RawMessage `json:"PlaybackPositionTicks"`
	PositionTicks         json.RawMessage `json:"PositionTicks"`
	PlayedPercentage      json.RawMessage `json:"PlayedPercentage"`
	PlayCount             json.RawMessage `json:"PlayCount"`
	IsFavorite            json.RawMessage `json:"IsFavorite"`
	Favorite              json.RawMessage `json:"Favorite"`
	Likes                 json.RawMessage `json:"Likes"`
}

func readUserDataWriteBody(r *http.Request, limit int64) (userDataWriteBody, error) {
	data, err := io.ReadAll(http.MaxBytesReader(nilResponseWriter{}, r.Body, limit))
	if err != nil {
		return userDataWriteBody{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte(`{}`)
	}
	var body userDataWriteBody
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&body); err != nil {
		return userDataWriteBody{}, err
	}
	return body, nil
}

func applyUserDataWriteToState(body userDataWriteBody, state *PlaybackState, now time.Time) {
	if played, ok := rawBool(body.Played); ok {
		if played && !state.Played {
			state.PlayCount++
		}
		state.Played = played
		if played {
			state.LastPlayedDate = &now
		}
	}
	if ticks, ok := rawInt64(body.PlaybackPositionTicks); ok {
		state.PlaybackPositionTicks = ticks
	}
	if ticks, ok := rawInt64(body.PositionTicks); ok {
		state.PlaybackPositionTicks = ticks
	}
	if percentage, ok := rawFloat64(body.PlayedPercentage); ok {
		state.PlayedPercentage = &percentage
	}
	if count, ok := rawInt64(body.PlayCount); ok {
		state.PlayCount = int(count)
	}
	if favorite, ok := rawBool(body.IsFavorite); ok {
		state.IsFavorite = favorite
	}
	if favorite, ok := rawBool(body.Favorite); ok {
		state.IsFavorite = favorite
	}
	if likes, ok := rawBool(body.Likes); ok {
		state.Likes = &likes
	}
}

func rawBool(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, false
	}
	var value bool
	return value, json.Unmarshal(raw, &value) == nil
}

func rawInt64(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var value int64
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	return value, err == nil
}

func rawFloat64(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var value float64
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return 0, false
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	return value, err == nil
}

func readJSONBody(r *http.Request, limit int64) (map[string]any, error) {
	data, err := io.ReadAll(http.MaxBytesReader(nilResponseWriter{}, r.Body, limit))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var body map[string]any
	if err := decodeJSONUseNumber(data, &body); err != nil {
		return nil, err
	}
	return body, nil
}

func userDataDTO(state *PlaybackState) map[string]any {
	typed := userDataWireDTO(state)
	data, _ := json.Marshal(typed)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func userDataWireDTO(state *PlaybackState) embyUserItemData {
	if state == nil {
		return embyUserItemData{}
	}
	played := state.Played
	position := state.PlaybackPositionTicks
	playCount := state.PlayCount
	favorite := state.IsFavorite
	out := embyUserItemData{
		PlaybackPositionTicks: &position,
		PlayCount:             &playCount,
		IsFavorite:            &favorite,
		Played:                &played,
		Key:                   state.ItemID,
		ItemID:                state.ItemID,
	}
	if state.Played {
		percentage := 100.0
		unplayed := 0
		out.PlayedPercentage = &percentage
		out.UnplayedItemCount = &unplayed
	} else if percentage, ok := playedPercentageForItem(state, nil); ok {
		out.PlayedPercentage = &percentage
	} else if state.PlayedPercentage != nil {
		percentage := *state.PlayedPercentage
		out.PlayedPercentage = &percentage
	}
	if state.LastPlayedDate != nil {
		date := state.LastPlayedDate.UTC().Format(time.RFC3339)
		out.LastPlayedDate = &date
	}
	if state.Likes != nil {
		out.Likes = json.RawMessage(strconv.FormatBool(*state.Likes))
	}
	return out
}

func userDataMapToWireDTO(data map[string]any) *embyUserItemData {
	if data == nil {
		return nil
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var out embyUserItemData
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil
	}
	return &out
}

type displayPreferenceRoute struct {
	PreferenceID string
	Client       string
}

func displayPreferenceRouteKey(preferenceID string, query url.Values, fallbackClient string) displayPreferenceRoute {
	client := query.Get("Client")
	if client == "" {
		client = fallbackClient
	}
	return displayPreferenceRoute{PreferenceID: preferenceID, Client: client}
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
	// Stable overlapping facets only. Name is intentionally excluded so renames
	// and partial metadata cannot create false fingerprint mismatches.
	parts := []string{}
	for _, key := range []string{"Type", "SeriesId"} {
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
	for key := range q {
		switch strings.ToLower(key) {
		case "isplayed", "isfavorite", "isresumable", "isliked", "isdisliked":
			return true
		}
	}
	for key, values := range q {
		if !strings.EqualFold(key, "Filters") {
			continue
		}
		for _, filter := range splitFilterValues(values) {
			switch strings.ToLower(filter) {
			case "isplayed", "isunplayed", "isresumable", "isfavorite", "likes", "dislikes":
				return true
			}
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

func isShowPersonalItemListPath(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	return len(parts) == 3 && strings.EqualFold(parts[0], "Shows") &&
		(strings.EqualFold(parts[2], "Episodes") || strings.EqualFold(parts[2], "Seasons"))
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
