package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	return s.refreshBackendSessionWithKnownUnauthorized(ctx, session, failedToken, false)
}

func (s *Server) refreshBackendSessionKnownUnauthorized(ctx context.Context, session *Session, failedToken string) error {
	return s.refreshBackendSessionWithKnownUnauthorized(ctx, session, failedToken, true)
}

func (s *Server) refreshBackendSessionWithKnownUnauthorized(ctx context.Context, session *Session, failedToken string, knownUnauthorized bool) error {
	if session == nil {
		return ErrNotFound
	}
	var account *BackendAccount
	if fetched, err := s.store.FindBackendAccountByID(ctx, session.BackendAccountID); err == nil && fetched != nil {
		account = fetched
		if !account.Enabled {
			return ErrDisabled
		}
		if strings.TrimSpace(account.BackendToken) != "" && account.BackendToken != failedToken && strings.TrimSpace(account.BackendUserID) != "" {
			applyBackendAccountToSession(session, *account)
			return nil
		}
		session.BackendAccount = *account
	} else {
		account = &session.BackendAccount
	}
	if !knownUnauthorized {
		if stale, err := s.backendTokenIsUnauthorized(ctx, session, failedToken); err != nil || !stale {
			return ErrUnauthorized
		}
	}
	if account != nil && tokenRefreshTooSoon(*account, failedToken, time.Now().UTC()) {
		return ErrUnauthorized
	}
	session.BackendToken = ""
	if err := s.loginBackendAccount(ctx, session); err != nil {
		return err
	}
	if session.BackendToken == "" || session.BackendToken == failedToken {
		return ErrUnauthorized
	}
	_ = s.logoutBackendToken(ctx, session, failedToken)
	return nil
}

func (s *Server) backendTokenIsUnauthorized(ctx context.Context, session *Session, token string) (bool, error) {
	if strings.TrimSpace(token) == "" {
		return true, nil
	}
	u, err := backendURL(session.BackendBaseURL, "/System/Info")
	if err != nil {
		return false, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, backendAuthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	identity := session.BackendIdentity.WithDefaults()
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("X-Emby-Authorization", backendAuthHeader(identity, session.BackendUserID, token).String())
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusUnauthorized, nil
}

func (s *Server) logoutBackendToken(ctx context.Context, session *Session, token string) error {
	if strings.TrimSpace(token) == "" || session == nil {
		return nil
	}
	u, err := backendURL(session.BackendBaseURL, "/Sessions/Logout")
	if err != nil {
		return err
	}
	logoutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backendAuthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(logoutCtx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	identity := session.BackendIdentity.WithDefaults()
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("X-Emby-Authorization", backendAuthHeader(identity, session.BackendUserID, token).String())
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("backend logout status %d", resp.StatusCode)
	}
	return nil
}

func tokenRefreshTooSoon(account BackendAccount, failedToken string, now time.Time) bool {
	if account.TokenUpdatedAt == nil || strings.TrimSpace(failedToken) == "" {
		return false
	}
	if account.BackendToken != "" && account.BackendToken != failedToken {
		return false
	}
	return now.Sub(account.TokenUpdatedAt.UTC()) < backendTokenRefreshMinInterval
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
