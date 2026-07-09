package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type backendLoginFailure struct {
	until   time.Time
	message string
}

func (s *Server) ensureBackendSession(ctx context.Context, session *Session) error {
	if session == nil {
		return ErrNotFound
	}
	if strings.TrimSpace(session.BackendToken) != "" && strings.TrimSpace(session.BackendUserID) != "" {
		return nil
	}
	return s.loginBackendAccount(ctx, session)
}

func (s *Server) refreshBackendSession(ctx context.Context, session *Session, failedToken string) error {
	if session == nil {
		return ErrNotFound
	}
	if account, err := s.store.FindBackendAccountByID(ctx, session.BackendAccountID); err == nil && account != nil {
		if !account.Enabled {
			return ErrDisabled
		}
		if strings.TrimSpace(account.BackendToken) != "" && account.BackendToken != failedToken && strings.TrimSpace(account.BackendUserID) != "" {
			applyBackendAccountToSession(session, *account)
			return nil
		}
		session.BackendAccount = *account
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
	if !account.Enabled {
		return ErrDisabled
	}
	if err := s.backendLoginCooldownError(account.ID, time.Now().UTC()); err != nil {
		return err
	}
	value, err, _ := s.backendAuth.Do(account.ID, func() (any, error) {
		loginCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backendAuthTimeout)
		defer cancel()
		if err := s.backendLoginCooldownError(account.ID, time.Now().UTC()); err != nil {
			return nil, err
		}
		result, loginErr := s.authenticateBackend(loginCtx, account)
		if loginErr != nil {
			_ = s.store.RecordBackendLoginError(loginCtx, account.ID, loginErr.Error())
			s.recordBackendLoginFailure(account.ID, loginErr.Error(), time.Now().UTC())
			return nil, loginErr
		}
		now := time.Now().UTC()
		if err := s.store.UpdateBackendToken(loginCtx, account.ID, result.AccessToken, result.UserID, now); err != nil {
			return nil, err
		}
		s.clearBackendLoginFailure(account.ID)
		if result.ServerID != "" && account.ServerID != "" {
			_ = s.store.UpdateServerInfo(loginCtx, account.ServerID, result.ServerID, result.ServerName, result.ServerVersion, now)
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

func applyBackendAccountToSession(session *Session, account BackendAccount) {
	session.BackendAccount = account
	session.BackendAccountID = account.ID
	session.BackendServerID = account.Server.BackendServerID
	session.BackendBaseURL = account.BaseURL
	session.BackendUserID = account.BackendUserID
	session.BackendUsername = account.Username
	session.BackendToken = account.BackendToken
	session.BackendIdentity = account.ClientIdentity.WithDefaults()
}

func (s *Server) backendLoginCooldownError(accountID string, now time.Time) error {
	s.backendAuthFailuresMu.Lock()
	defer s.backendAuthFailuresMu.Unlock()
	failure, ok := s.backendAuthFailures[accountID]
	if !ok {
		return nil
	}
	if !now.Before(failure.until) {
		delete(s.backendAuthFailures, accountID)
		return nil
	}
	if strings.TrimSpace(failure.message) == "" {
		return errors.New("backend authentication is cooling down")
	}
	return fmt.Errorf("backend authentication is cooling down: %s", failure.message)
}

func (s *Server) recordBackendLoginFailure(accountID, message string, now time.Time) {
	s.backendAuthFailuresMu.Lock()
	defer s.backendAuthFailuresMu.Unlock()
	s.backendAuthFailures[accountID] = backendLoginFailure{until: now.Add(backendLoginFailureCooldown), message: message}
}

func (s *Server) clearBackendLoginFailure(accountID string) {
	s.backendAuthFailuresMu.Lock()
	defer s.backendAuthFailuresMu.Unlock()
	delete(s.backendAuthFailures, accountID)
}
