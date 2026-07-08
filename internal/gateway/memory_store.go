package gateway

import (
	"context"
	"sync"
	"time"
)

type MemoryStore struct {
	mu       sync.RWMutex
	Users    map[string]MemoryUser
	Mappings map[string]UserMapping
	Sessions map[string]*Session
}

type MemoryUser struct {
	GatewayUser
	Password string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		Users:    map[string]MemoryUser{},
		Mappings: map[string]UserMapping{},
		Sessions: map[string]*Session{},
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
	}
	return nil
}
