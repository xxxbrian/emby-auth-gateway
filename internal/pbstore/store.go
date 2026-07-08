package pbstore

import (
	"context"
	"time"

	"emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

type Store struct {
	app    core.App
	cipher *gateway.Cipher
}

func New(app core.App, cipher *gateway.Cipher) *Store {
	return &Store{app: app, cipher: cipher}
}

func (s *Store) AuthenticateGatewayUser(ctx context.Context, username, password string) (*gateway.GatewayUser, error) {
	record, err := s.app.FindFirstRecordByData("gateway_users", "username", username)
	if err != nil {
		return nil, gateway.ErrInvalidCredentials
	}
	if !record.GetBool("enabled") || !record.ValidatePassword(password) {
		return nil, gateway.ErrInvalidCredentials
	}
	return userFromRecord(record), nil
}

func (s *Store) ListPublicUsers(ctx context.Context) ([]gateway.GatewayUser, error) {
	records, err := s.app.FindAllRecords("gateway_users", dbx.HashExp{"enabled": true})
	if err != nil {
		return nil, err
	}
	users := make([]gateway.GatewayUser, 0, len(records))
	for _, record := range records {
		users = append(users, *userFromRecord(record))
	}
	return users, nil
}

func (s *Store) FindUserBySyntheticID(ctx context.Context, syntheticID string) (*gateway.GatewayUser, error) {
	record, err := s.app.FindFirstRecordByData("gateway_users", "synthetic_user_id", syntheticID)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	if !record.GetBool("enabled") {
		return nil, gateway.ErrDisabled
	}
	return userFromRecord(record), nil
}

func (s *Store) FindMappingByGatewayUserID(ctx context.Context, gatewayUserID string) (*gateway.UserMapping, error) {
	mapping, err := s.app.FindFirstRecordByData("user_mappings", "gateway_user", gatewayUserID)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	if !mapping.GetBool("enabled") {
		return nil, gateway.ErrDisabled
	}
	backendID := mapping.GetString("backend_account")
	account, err := s.backendAccountByID(backendID)
	if err != nil {
		return nil, err
	}
	return &gateway.UserMapping{
		ID:               mapping.Id,
		GatewayUserID:    gatewayUserID,
		BackendAccountID: backendID,
		BackendAccount:   *account,
		Enabled:          mapping.GetBool("enabled"),
	}, nil
}

func (s *Store) DefaultBackend(ctx context.Context) (*gateway.BackendAccount, error) {
	records, err := s.app.FindAllRecords("backend_accounts", dbx.HashExp{"enabled": true})
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, gateway.ErrNotFound
	}
	return s.backendAccountFromRecord(records[0])
}

func (s *Store) SaveSession(ctx context.Context, session *gateway.Session) error {
	collection, err := s.app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		return err
	}
	backendToken, err := s.cipher.Encrypt(session.BackendToken)
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	record.Set("gateway_token_hash", session.GatewayTokenHash)
	record.Set("gateway_user", session.GatewayUserID)
	record.Set("gateway_username", session.GatewayUsername)
	record.Set("synthetic_user_id", session.SyntheticUserID)
	record.Set("backend_account", session.BackendAccountID)
	record.Set("backend_server_id", session.BackendServerID)
	record.Set("backend_base_url", session.BackendBaseURL)
	record.Set("backend_user_id", session.BackendUserID)
	record.Set("backend_username", session.BackendUsername)
	record.Set("backend_token_encrypted", backendToken)
	record.Set("client", session.Client)
	record.Set("device", session.Device)
	record.Set("device_id", session.DeviceID)
	record.Set("version", session.Version)
	record.Set("remote_ip", session.RemoteIP)
	record.Set("expires_at", session.ExpiresAt)
	return s.app.Save(record)
}

func (s *Store) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*gateway.Session, error) {
	record, err := s.app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", tokenHash)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	backendToken, err := s.cipher.Decrypt(record.GetString("backend_token_encrypted"))
	if err != nil {
		return nil, err
	}
	createdAt := record.GetDateTime("created").Time()
	expiresAt := record.GetDateTime("expires_at").Time()
	var revokedAt *time.Time
	if !record.GetDateTime("revoked_at").IsZero() {
		t := record.GetDateTime("revoked_at").Time()
		revokedAt = &t
	}
	return &gateway.Session{
		GatewayTokenHash: record.GetString("gateway_token_hash"),
		GatewayUserID:    record.GetString("gateway_user"),
		GatewayUsername:  record.GetString("gateway_username"),
		SyntheticUserID:  record.GetString("synthetic_user_id"),
		BackendAccountID: record.GetString("backend_account"),
		BackendServerID:  record.GetString("backend_server_id"),
		BackendBaseURL:   record.GetString("backend_base_url"),
		BackendUserID:    record.GetString("backend_user_id"),
		BackendUsername:  record.GetString("backend_username"),
		BackendToken:     backendToken,
		Client:           record.GetString("client"),
		Device:           record.GetString("device"),
		DeviceID:         record.GetString("device_id"),
		Version:          record.GetString("version"),
		RemoteIP:         record.GetString("remote_ip"),
		CreatedAt:        createdAt,
		ExpiresAt:        expiresAt,
		RevokedAt:        revokedAt,
	}, nil
}

func (s *Store) RevokeSession(ctx context.Context, tokenHash string) error {
	record, err := s.app.FindFirstRecordByData("gateway_sessions", "gateway_token_hash", tokenHash)
	if err != nil {
		return nil
	}
	record.Set("revoked_at", time.Now().UTC())
	return s.app.Save(record)
}

func (s *Store) backendAccountByID(id string) (*gateway.BackendAccount, error) {
	if id == "" {
		return nil, gateway.ErrNotFound
	}
	record, err := s.app.FindRecordById("backend_accounts", id)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	return s.backendAccountFromRecord(record)
}

func (s *Store) backendAccountFromRecord(record *core.Record) (*gateway.BackendAccount, error) {
	if !record.GetBool("enabled") {
		return nil, gateway.ErrDisabled
	}
	password, err := s.cipher.Decrypt(record.GetString("backend_password_encrypted"))
	if err != nil {
		return nil, err
	}
	serverID := record.GetString("server")
	server, err := s.app.FindRecordById("emby_servers", serverID)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	return &gateway.BackendAccount{
		ID:       record.Id,
		ServerID: serverID,
		BaseURL:  server.GetString("base_url"),
		Username: record.GetString("backend_username"),
		Password: password,
		Enabled:  record.GetBool("enabled") && server.GetBool("enabled"),
	}, nil
}

func userFromRecord(record *core.Record) *gateway.GatewayUser {
	return &gateway.GatewayUser{
		ID:              record.Id,
		Username:        record.GetString("username"),
		SyntheticUserID: record.GetString("synthetic_user_id"),
		Enabled:         record.GetBool("enabled"),
	}
}
