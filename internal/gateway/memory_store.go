package gateway

import (
	"context"
	"strings"
	"sync"
	"time"
)

type MemoryStore struct {
	mu                 sync.RWMutex
	Users              map[string]MemoryUser
	Sessions           map[string]*Session
	AuditLogs          []AuditLog
	PathPolicies       []PathPolicy
	PlaybackEvents     []PlaybackEvent
	PlaybackStates     map[string]*PlaybackState
	ItemChildCounts    map[string]ItemChildCount
	DisplayPreferences map[string]*DisplayPreference
	UpstreamSources    map[string]UpstreamSource
	UpstreamEndpoints  map[string]UpstreamEndpoint
}

type MemoryUser struct {
	GatewayUser
	Password string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		Users:              map[string]MemoryUser{},
		Sessions:           map[string]*Session{},
		PlaybackStates:     map[string]*PlaybackState{},
		ItemChildCounts:    map[string]ItemChildCount{},
		DisplayPreferences: map[string]*DisplayPreference{},
		UpstreamSources:    map[string]UpstreamSource{},
		UpstreamEndpoints:  map[string]UpstreamEndpoint{},
	}
}

func (m *MemoryStore) LoadDefaultUpstreamRuntime(ctx context.Context) (*UpstreamRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runtime, _, err := loadMemoryDefaultUpstreamRuntime(m.UpstreamSources, m.UpstreamEndpoints)
	return runtime, err
}

// loadMemoryDefaultUpstreamRuntime requires the caller to hold m.mu. It does
// not acquire locks so CompareAndSwapUpstreamAuth can validate atomically.
func loadMemoryDefaultUpstreamRuntime(sources map[string]UpstreamSource, endpoints map[string]UpstreamEndpoint) (*UpstreamRuntime, string, error) {
	if len(sources) == 0 && len(endpoints) == 0 {
		return nil, "", ErrUpstreamNotFound
	}
	if len(sources) == 0 {
		return nil, "", invalidUpstreamTopology("endpoints without source")
	}
	if len(sources) != 1 {
		return nil, "", invalidUpstreamTopology("expected one source")
	}
	var source UpstreamSource
	var sourceKey string
	for key, candidate := range sources {
		if key != candidate.ID {
			return nil, "", invalidUpstreamTopology("source map identity mismatch")
		}
		source, sourceKey = candidate, key
	}
	var active []UpstreamEndpoint
	for key, endpoint := range endpoints {
		if key != endpoint.ID {
			return nil, "", invalidUpstreamTopology("endpoint map identity mismatch")
		}
		if err := ValidateUpstreamEndpoint(source.ID, endpoint); err != nil {
			return nil, "", err
		}
		if endpoint.Active {
			active = append(active, endpoint)
		}
	}
	if len(active) != 1 {
		return nil, "", invalidUpstreamTopology("expected one active endpoint")
	}
	runtime := &UpstreamRuntime{Source: cloneUpstreamSource(source), Endpoint: active[0]}
	if err := ValidateUpstreamRuntime(*runtime); err != nil {
		return nil, "", err
	}
	return runtime, sourceKey, nil
}

func cloneUpstreamSource(source UpstreamSource) UpstreamSource {
	if source.VersionCheckedAt != nil {
		t := *source.VersionCheckedAt
		source.VersionCheckedAt = &t
	}
	if source.TokenUpdatedAt != nil {
		t := *source.TokenUpdatedAt
		source.TokenUpdatedAt = &t
	}
	if source.LastLoginAt != nil {
		t := *source.LastLoginAt
		source.LastLoginAt = &t
	}
	return source
}

func (m *MemoryStore) CompareAndSwapUpstreamAuth(ctx context.Context, update UpstreamAuthUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateUpstreamAuthUpdate(update); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	source, matched := m.UpstreamSources[update.SourceID]
	matched = matched && source.ID == update.SourceID && source.Key == "default" && source.AuthGenerationID == update.ExpectedGenerationID
	if !matched {
		runtime, _, err := loadMemoryDefaultUpstreamRuntime(m.UpstreamSources, m.UpstreamEndpoints)
		if err != nil {
			return err
		}
		if runtime.Source.ID != update.SourceID {
			return ErrUpstreamNotFound
		}
		return ErrUpstreamAuthConflict
	}
	authenticatedAt := update.AuthenticatedAt.UTC()
	source.AuthGenerationID = update.GenerationID
	source.ClientIdentity.DeviceID = update.DeviceID
	source.BackendUserID = update.BackendUserID
	source.BackendToken = update.BackendToken
	source.TokenUpdatedAt = &authenticatedAt
	source.LastLoginAt = &authenticatedAt
	source.LastLoginError = ""
	m.UpstreamSources[update.SourceID] = source
	return nil
}

func (m *MemoryStore) UpdateUpstreamServerInfo(ctx context.Context, update UpstreamServerInfoUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateUpstreamServerInfoUpdate(update); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	source, matched := m.UpstreamSources[update.SourceID]
	matched = matched && source.ID == update.SourceID && source.Key == "default" && source.ServerID == update.ServerID
	if !matched {
		runtime, _, err := loadMemoryDefaultUpstreamRuntime(m.UpstreamSources, m.UpstreamEndpoints)
		if err != nil {
			return err
		}
		if runtime.Source.ID != update.SourceID {
			return ErrUpstreamNotFound
		}
		return ErrUpstreamServerInfoConflict
	}
	if update.ServerName != "" {
		source.ServerName = update.ServerName
	}
	if update.ServerVersion != "" {
		source.ServerVersion = update.ServerVersion
	}
	checkedAt := update.CheckedAt.UTC().Truncate(time.Millisecond)
	source.VersionCheckedAt = &checkedAt
	m.UpstreamSources[update.SourceID] = source
	return nil
}

func (m *MemoryStore) AuthenticateGatewayUser(ctx context.Context, username, password string) (*GatewayUser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, user := range m.Users {
		if user.Username == username && user.Password == password && user.Enabled {
			u := user.GatewayUser
			return &u, nil
		}
	}
	return nil, ErrInvalidCredentials
}

func (m *MemoryStore) FindGatewayUserByUsername(ctx context.Context, username string) (*GatewayUser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, user := range m.Users {
		if user.Username == username {
			u := user.GatewayUser
			return &u, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemoryStore) ListPublicUsers(ctx context.Context) ([]GatewayUser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	users := make([]GatewayUser, 0, len(m.Users))
	for _, user := range m.Users {
		users = append(users, user.GatewayUser)
	}
	return users, nil
}

func (m *MemoryStore) FindUserBySyntheticID(ctx context.Context, syntheticID string) (*GatewayUser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, user := range m.Users {
		if user.SyntheticUserID == syntheticID {
			u := user.GatewayUser
			return &u, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemoryStore) RecordAudit(ctx context.Context, entry AuditLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	m.AuditLogs = append(m.AuditLogs, entry)
	return nil
}

func (m *MemoryStore) CheckPathPolicy(ctx context.Context, method, relativePath string) (PathPolicyDecision, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return DecidePathPolicy(m.PathPolicies, method, relativePath), nil
}

func (m *MemoryStore) RecordPlaybackEvent(ctx context.Context, event PlaybackEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	m.PlaybackEvents = append(m.PlaybackEvents, event)
	return nil
}

func (m *MemoryStore) FindPlaybackState(ctx context.Context, gatewayUserID, itemID string) (*PlaybackState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.PlaybackStates == nil {
		return nil, ErrNotFound
	}
	state, ok := m.PlaybackStates[playbackStateKey(gatewayUserID, itemID)]
	if !ok {
		return nil, ErrNotFound
	}
	copyState := *state
	return &copyState, nil
}

func (m *MemoryStore) ListPlaybackStatesByItemIDs(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*PlaybackState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	states := make(map[string]*PlaybackState, len(itemIDs))
	for _, itemID := range itemIDs {
		if itemID == "" {
			continue
		}
		state, ok := m.PlaybackStates[playbackStateKey(gatewayUserID, itemID)]
		if !ok {
			continue
		}
		if state.OrphanedAt != nil {
			continue
		}
		copyState := *state
		states[itemID] = &copyState
	}
	return states, nil
}

func (m *MemoryStore) ListPlaybackAggregates(ctx context.Context, gatewayUserID string, seriesIDs, seasonIDs []string) (PlaybackAggregates, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	aggregates := PlaybackAggregates{Series: map[string]PlaybackAggregate{}, Seasons: map[string]PlaybackAggregate{}}
	seriesSet := playbackIDSet(seriesIDs)
	seasonSet := playbackIDSet(seasonIDs)
	if len(seriesSet) == 0 && len(seasonSet) == 0 {
		return aggregates, nil
	}
	for _, state := range m.PlaybackStates {
		if state.GatewayUserID != gatewayUserID || state.OrphanedAt != nil {
			continue
		}
		if seriesSet[state.SeriesID] {
			aggregates.Series[state.SeriesID] = addMemoryPlaybackAggregate(aggregates.Series[state.SeriesID], *state)
		}
		if seasonSet[state.SeasonID] {
			aggregates.Seasons[state.SeasonID] = addMemoryPlaybackAggregate(aggregates.Seasons[state.SeasonID], *state)
		}
	}
	return aggregates, nil
}

func (m *MemoryStore) ListItemChildCounts(ctx context.Context, itemIDs []string) (map[string]ItemChildCount, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	counts := map[string]ItemChildCount{}
	for _, itemID := range itemIDs {
		if count, ok := m.ItemChildCounts[itemChildCountKey(itemID)]; ok {
			counts[itemID] = count
		}
	}
	return counts, nil
}

func (m *MemoryStore) SaveItemChildCount(ctx context.Context, count ItemChildCount) error {
	if count.ItemID == "" || count.ChildCount <= 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ItemChildCounts == nil {
		m.ItemChildCounts = map[string]ItemChildCount{}
	}
	if count.UpdatedAt.IsZero() {
		count.UpdatedAt = time.Now().UTC()
	}
	m.ItemChildCounts[itemChildCountKey(count.ItemID)] = count
	return nil
}

func playbackIDSet(ids []string) map[string]bool {
	set := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			set[id] = true
		}
	}
	return set
}

func addMemoryPlaybackAggregate(aggregate PlaybackAggregate, state PlaybackState) PlaybackAggregate {
	aggregate.KnownItemCount++
	if state.Played {
		aggregate.PlayedCount++
	}
	if state.LastPlayedDate != nil && (aggregate.LastPlayedDate == nil || state.LastPlayedDate.After(*aggregate.LastPlayedDate)) {
		t := *state.LastPlayedDate
		aggregate.LastPlayedDate = &t
	}
	activity := state.UpdatedAt
	if state.LastPlayedDate != nil && state.LastPlayedDate.After(activity) {
		activity = *state.LastPlayedDate
	}
	if !activity.IsZero() && (aggregate.LastActivityDate == nil || activity.After(*aggregate.LastActivityDate)) {
		t := activity
		aggregate.LastActivityDate = &t
	}
	return aggregate
}

func (m *MemoryStore) SavePlaybackState(ctx context.Context, state PlaybackState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.PlaybackStates == nil {
		m.PlaybackStates = map[string]*PlaybackState{}
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = time.Now().UTC()
	}
	copyState := state
	m.PlaybackStates[playbackStateKey(state.GatewayUserID, state.ItemID)] = &copyState
	return nil
}

func (m *MemoryStore) ListPlaybackStates(ctx context.Context, gatewayUserID string, filter PlaybackStateFilter) ([]PlaybackState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	states := make([]PlaybackState, 0, len(m.PlaybackStates))
	for _, state := range m.PlaybackStates {
		if state.GatewayUserID != gatewayUserID {
			continue
		}
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
		copyState := *state
		states = append(states, copyState)
	}
	return states, nil
}

func (m *MemoryStore) FindDisplayPreference(ctx context.Context, gatewayUserID, preferenceID, client string) (*DisplayPreference, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	preference, ok := m.DisplayPreferences[displayPreferenceKey(gatewayUserID, preferenceID, client)]
	if !ok {
		return nil, ErrNotFound
	}
	copyPreference := *preference
	return &copyPreference, nil
}

func (m *MemoryStore) SaveDisplayPreference(ctx context.Context, preference DisplayPreference) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisplayPreferences == nil {
		m.DisplayPreferences = map[string]*DisplayPreference{}
	}
	if preference.UpdatedAt.IsZero() {
		preference.UpdatedAt = time.Now().UTC()
	}
	copyPreference := preference
	m.DisplayPreferences[displayPreferenceKey(preference.GatewayUserID, preference.PreferenceID, preference.Client)] = &copyPreference
	return nil
}

func (m *MemoryStore) SaveSession(ctx context.Context, session *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copySession := *session
	m.Sessions[session.GatewayTokenHash] = &copySession
	return nil
}

func (m *MemoryStore) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.Sessions[tokenHash]
	if !ok {
		return nil, ErrNotFound
	}
	copySession := *session
	return &copySession, nil
}

func (m *MemoryStore) SessionTokenExists(ctx context.Context, tokenHash string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.Sessions[tokenHash]
	return ok, nil
}

func (m *MemoryStore) RevokeSession(ctx context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.Sessions[tokenHash]; ok {
		now := time.Now().UTC()
		session.RevokedAt = &now
		return nil
	}
	return ErrNotFound
}

func playbackStateKey(gatewayUserID, itemID string) string {
	return gatewayUserID + "\x00" + itemID
}

func itemChildCountKey(itemID string) string {
	return itemID
}

func displayPreferenceKey(gatewayUserID, preferenceID, client string) string {
	return gatewayUserID + "\x00" + preferenceID + "\x00" + client
}

func methodMatches(policyMethod, requestMethod string) bool {
	policyMethod = strings.TrimSpace(policyMethod)
	return policyMethod == "" || policyMethod == "*" || strings.EqualFold(policyMethod, requestMethod)
}

func pathMatches(policyPath, requestPath string) bool {
	policyPath = strings.TrimSpace(policyPath)
	if policyPath == "" || policyPath == "*" {
		return true
	}
	if strings.HasSuffix(policyPath, "*") {
		return strings.HasPrefix(strings.ToLower(requestPath), strings.ToLower(strings.TrimSuffix(policyPath, "*")))
	}
	return equalPath(policyPath, requestPath)
}
