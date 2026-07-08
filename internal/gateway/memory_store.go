package gateway

import (
	"context"
	"strings"
	"sync"
	"time"
)

type MemoryStore struct {
	mu             sync.RWMutex
	Users          map[string]MemoryUser
	Mappings       map[string]UserMapping
	Sessions       map[string]*Session
	AuditLogs      []AuditLog
	PathPolicies   []PathPolicy
	PlaybackEvents []PlaybackEvent
	PlaybackStates map[string]*PlaybackState
}

type MemoryUser struct {
	GatewayUser
	Password string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		Users:          map[string]MemoryUser{},
		Mappings:       map[string]UserMapping{},
		Sessions:       map[string]*Session{},
		PlaybackStates: map[string]*PlaybackState{},
	}
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

func (m *MemoryStore) FindMappingByGatewayUserID(ctx context.Context, gatewayUserID string) (*UserMapping, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mapping, ok := m.Mappings[gatewayUserID]; ok {
		return &mapping, nil
	}
	return nil, ErrNotFound
}

func (m *MemoryStore) DefaultBackend(ctx context.Context) (*BackendAccount, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, mapping := range m.Mappings {
		account := mapping.BackendAccount
		return &account, nil
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
