package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (s *Server) ensureBackendSession(ctx context.Context, session *Session) error {
	if session == nil {
		return ErrNotFound
	}
	if strings.TrimSpace(session.BackendToken) != "" && strings.TrimSpace(session.BackendUserID) != "" {
		return nil
	}
	return s.loginBackendAccount(ctx, session)
}

func (s *Server) refreshBackendSession(ctx context.Context, session *Session) error {
	if session == nil {
		return ErrNotFound
	}
	session.BackendToken = ""
	return s.loginBackendAccount(ctx, session)
}

func (s *Server) loginBackendAccount(ctx context.Context, session *Session) error {
	account := session.BackendAccount
	if account.ID == "" {
		account.ID = session.BackendAccountID
	}
	if account.ID == "" {
		return ErrNotFound
	}
	value, err, _ := s.backendAuth.Do(account.ID, func() (any, error) {
		result, loginErr := s.authenticateBackend(ctx, account)
		if loginErr != nil {
			_ = s.store.RecordBackendLoginError(ctx, account.ID, loginErr.Error())
			return nil, loginErr
		}
		now := time.Now().UTC()
		if err := s.store.UpdateBackendToken(ctx, account.ID, result.AccessToken, result.UserID, now); err != nil {
			return nil, err
		}
		if result.ServerID != "" && account.ServerID != "" {
			_ = s.store.UpdateServerInfo(ctx, account.ServerID, result.ServerID, result.ServerName, result.ServerVersion, now)
		}
		return result, nil
	})
	if err != nil {
		return err
	}
	result, ok := value.(*backendAuthResult)
	if !ok || result == nil {
		return fmt.Errorf("backend auth result has unexpected type %T", value)
	}
	session.BackendToken = result.AccessToken
	session.BackendUserID = result.UserID
	session.BackendUsername = result.Username
	if result.ServerID != "" {
		session.BackendServerID = result.ServerID
		session.BackendAccount.Server.BackendServerID = result.ServerID
	}
	if session.BackendBaseURL == "" {
		session.BackendBaseURL = account.BaseURL
	}
	session.BackendIdentity = account.ClientIdentity.WithDefaults()
	session.BackendAccount.BackendToken = result.AccessToken
	session.BackendAccount.BackendUserID = result.UserID
	return nil
}
