package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/version"

	"golang.org/x/sync/singleflight"
)

const gatewayVersionHeader = "X-Emby-Auth-Gateway-Version"

type Server struct {
	cfg                   Config
	store                 Store
	client                *http.Client
	proxyClient           *http.Client
	logins                *loginFailureLimiter
	backendAuth           singleflight.Group
	backendAuthFailuresMu sync.Mutex
	backendAuthFailures   map[string]backendLoginFailure
	playbackGuards        *playbackGuardTracker
	mediaDeadlineWarning  atomic.Bool
	anonymousImages       anonymousImageNamespaceState
	anonymousImageNow     func() time.Time
}

func NewServer(cfg Config, store Store) *Server {
	if cfg.GatewayBasePath == "" {
		cfg.GatewayBasePath = "/emby"
	}
	if !strings.HasPrefix(cfg.GatewayBasePath, "/") {
		cfg.GatewayBasePath = "/" + cfg.GatewayBasePath
	}
	if cfg.GatewayServerID == "" {
		cfg.GatewayServerID = "emby-auth-gateway"
	}
	if cfg.MinResumePct <= 0 {
		cfg.MinResumePct = defaultMinResumePct
	}
	if cfg.MaxResumePct <= 0 {
		cfg.MaxResumePct = defaultMaxResumePct
	}
	if cfg.MinResumeDurationSeconds <= 0 {
		cfg.MinResumeDurationSeconds = defaultMinResumeDurationSeconds
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: backendAuthTimeout}
	}
	proxyClient := newProxyClient(cfg.HTTPClient)
	return &Server{cfg: cfg, store: store, client: client, proxyClient: proxyClient, logins: newLoginFailureLimiter(), backendAuthFailures: map[string]backendLoginFailure{}, playbackGuards: newPlaybackGuardTracker(), anonymousImageNow: time.Now}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(gatewayVersionHeader, version.Version)
	rel, ok := s.relativePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Credential-accepting routes reject malformed query encoding before any
	// store operation (path policy, session, audit, etc.).
	if acceptsClientCredentials(r.Method, rel) {
		if err := validateRawQuery(r.URL.RawQuery); err != nil {
			writeCredentialQueryError(w, err)
			return
		}
	}
	if !s.pathPolicyAllows(w, r, rel) {
		return
	}

	switch {
	case r.Method == http.MethodPost && equalPath(rel, "/Users/AuthenticateByName"):
		s.handleAuthenticate(w, r)
	case r.Method == http.MethodGet && equalPath(rel, "/System/Info/Public"):
		s.handlePublicSystemInfo(w, r)
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && equalPath(rel, "/System/Ping"):
		s.handlePing(w, r)
	case r.Method == http.MethodPost && equalPath(rel, "/Sessions/Logout"):
		s.handleLogout(w, r, rel)
	case r.Method == http.MethodGet && equalPath(rel, "/Users/Public"):
		s.handlePublicUsers(w, r)
	case r.Method == http.MethodGet && isSingleUserPath(rel):
		s.handleCurrentUser(w, r, rel)
	case r.Method == http.MethodGet && equalPath(rel, "/Branding/Configuration"):
		s.handleBrandingConfiguration(w, r)
	case r.Method == http.MethodGet && equalPath(rel, "/Branding/Css.css"):
		s.handleBrandingCSS(w, r)
	default:
		s.handleProxy(w, r, rel)
	}
}

func (s *Server) handleAuthenticate(w http.ResponseWriter, r *http.Request) {
	form, err := parseAuthenticateBody(w, r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	key := loginFailureKey(form.Username, r)
	if s.logins.blocked(key, time.Now()) {
		s.auditLoginFailure(r.Context(), r, form.Username, "login blocked", http.StatusUnauthorized)
		http.Error(w, "failed to authenticate", http.StatusUnauthorized)
		return
	}

	password := form.Pw
	if password == "" {
		password = form.Password
	}
	if form.Username == "" || password == "" {
		s.auditLoginFailure(r.Context(), r, form.Username, "missing credentials", http.StatusBadRequest)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	user, err := s.store.AuthenticateGatewayUser(ctx, form.Username, password)
	if err != nil || user == nil || !user.Enabled {
		s.logins.recordFailure(key, time.Now())
		s.auditLoginFailure(ctx, r, form.Username, "invalid credentials", http.StatusUnauthorized)
		http.Error(w, "failed to authenticate", http.StatusUnauthorized)
		return
	}

	mapping, err := s.store.FindMappingByGatewayUserID(ctx, user.ID)
	if err != nil || mapping == nil || !mapping.Enabled || !mapping.BackendAccount.Enabled {
		s.logins.recordFailure(key, time.Now())
		s.audit(ctx, AuditLog{GatewayUserID: user.ID, SyntheticUserID: user.SyntheticUserID, Event: "mapping_unavailable", Message: "mapping unavailable", RemoteIP: remoteIP(r), Method: r.Method, Path: r.URL.Path, Status: http.StatusUnauthorized})
		http.Error(w, "failed to authenticate", http.StatusUnauthorized)
		return
	}

	clientAuth := firstAuthHeader(r)

	s.logins.clear(key)

	token, tokenHash, err := NewOpaqueToken()
	if err != nil {
		http.Error(w, "token generation failed", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	session := &Session{
		GatewayTokenHash: tokenHash,
		GatewayUserID:    user.ID,
		GatewayUsername:  user.Username,
		SyntheticUserID:  user.SyntheticUserID,
		BackendAccountID: mapping.BackendAccount.ID,
		BackendAccount:   mapping.BackendAccount,
		BackendServerID:  mapping.BackendAccount.Server.BackendServerID,
		BackendBaseURL:   mapping.BackendAccount.BaseURL,
		BackendUserID:    mapping.BackendAccount.BackendUserID,
		BackendUsername:  mapping.BackendAccount.Username,
		BackendToken:     mapping.BackendAccount.BackendToken,
		BackendIdentity:  mapping.BackendAccount.ClientIdentity.WithDefaults(),
		Client:           clientAuth.Client,
		Device:           clientAuth.Device,
		DeviceID:         clientAuth.DeviceID,
		Version:          clientAuth.Version,
		RemoteIP:         remoteIP(r),
		CreatedAt:        now,
		ExpiresAt:        now.Add(defaultSessionTTL),
	}
	if err := s.store.SaveSession(ctx, session); err != nil {
		s.audit(ctx, AuditLog{GatewayUserID: user.ID, SyntheticUserID: user.SyntheticUserID, Event: "session_save_failure", Message: "session save failed", RemoteIP: remoteIP(r), Method: r.Method, Path: r.URL.Path, Status: http.StatusInternalServerError})
		http.Error(w, "session save failed", http.StatusInternalServerError)
		return
	}
	s.audit(ctx, AuditLog{GatewayUserID: user.ID, SyntheticUserID: user.SyntheticUserID, Event: "login_success", Message: "login succeeded", RemoteIP: remoteIP(r), Method: r.Method, Path: r.URL.Path, Status: http.StatusOK})

	w.Header().Set("Cache-Control", "no-store")
	s.setResourceCookie(w, token, session.ExpiresAt)
	writeJSON(w, http.StatusOK, authenticationResultDTO(*user, session, token, s.cfg.GatewayServerID))
}

type authenticateForm struct {
	Username string `json:"Username"`
	Pw       string `json:"Pw"`
	Password string `json:"Password"`
}

func parseAuthenticateBody(w http.ResponseWriter, r *http.Request) (authenticateForm, error) {
	var form authenticateForm
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
	if err != nil {
		return form, err
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/x-www-form-urlencoded" {
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return form, err
		}
		form.Username = values.Get("Username")
		form.Pw = values.Get("Pw")
		form.Password = values.Get("Password")
		return form, nil
	}
	return form, json.Unmarshal(body, &form)
}

func (s *Server) authenticateBackend(ctx context.Context, account BackendAccount) (*backendAuthResult, error) {
	u, err := backendURL(account.BaseURL, "/Users/AuthenticateByName")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]string{
		"Username": account.Username,
		"Pw":       account.Password,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	identity := account.ClientIdentity.WithDefaults()
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("X-Emby-Authorization", backendAuthHeader(identity, "", "").String())

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("backend status %d: %s", resp.StatusCode, string(data))
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	result := &backendAuthResult{Raw: raw}
	if v, _ := raw["AccessToken"].(string); v != "" {
		result.AccessToken = v
	}
	if v, _ := raw["ServerId"].(string); v != "" {
		result.ServerID = v
	}
	if v, _ := raw["ServerName"].(string); v != "" {
		result.ServerName = v
	}
	if v, _ := raw["Version"].(string); v != "" {
		result.ServerVersion = v
	}
	if user, _ := raw["User"].(map[string]any); user != nil {
		result.UserID, _ = user["Id"].(string)
		result.Username, _ = user["Name"].(string)
	}
	if result.AccessToken == "" || result.UserID == "" {
		return nil, errors.New("backend auth response missing token or user id")
	}
	return result, nil
}

type backendAuthResult struct {
	AccessToken   string
	ServerID      string
	ServerName    string
	ServerVersion string
	UserID        string
	Username      string
	Raw           map[string]any
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, rel string) {
	token := ExtractToken(r)
	session, ok := s.activeSession(w, r, token)
	if !ok {
		s.audit(r.Context(), AuditLog{Event: "logout_failure", Message: "session unavailable", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusUnauthorized})
		return
	}
	if err := s.store.RevokeSession(r.Context(), HashToken(token)); err != nil {
		s.audit(r.Context(), AuditLog{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, Event: "logout_failure", Message: "session revoke failed", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusInternalServerError})
		http.Error(w, "session revoke failed", http.StatusInternalServerError)
		return
	}
	if resourceCookieMatches(r, token) {
		s.clearResourceCookie(w)
	}
	s.audit(r.Context(), AuditLog{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, Event: "logout_success", Message: "logout succeeded", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusOK})
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePublicUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListPublicUsers(r.Context())
	if err != nil {
		http.Error(w, "users unavailable", http.StatusInternalServerError)
		return
	}
	items := make([]map[string]any, 0, len(users))
	for _, user := range users {
		if !user.Enabled {
			continue
		}
		items = append(items, userDTO(user, s.cfg.GatewayServerID))
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handlePublicSystemInfo(w http.ResponseWriter, r *http.Request) {
	base := s.gatewayBaseForRequest(r)
	version := s.publicSystemInfoVersion(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":              s.cfg.GatewayServerID,
		"ServerId":        s.cfg.GatewayServerID,
		"ServerName":      "Emby Gateway",
		"Version":         version,
		"LocalAddress":    base,
		"WanAddress":      base,
		"RemoteAddresses": []string{base},
		"LocalAddresses":  []string{base},
	})
}

func (s *Server) publicSystemInfoVersion(ctx context.Context) string {
	servers, err := s.store.ListEnabledServers(ctx)
	if err != nil {
		return defaultBackendServerVersion
	}
	if highest := highestServerVersion(servers); highest != "" {
		return highest
	}
	if err := s.RefreshBackendServerInfo(ctx); err != nil {
		return defaultBackendServerVersion
	}
	servers, err = s.store.ListEnabledServers(ctx)
	if err != nil {
		return defaultBackendServerVersion
	}
	if highest := highestServerVersion(servers); highest != "" {
		return highest
	}
	return defaultBackendServerVersion
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Emby Server is running"))
}

// handleBrandingConfiguration serves the anonymous Emby web branding JSON shim.
// Body is exactly "{}" with no trailing newline.
func (s *Server) handleBrandingConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{}"))
}

// handleBrandingCSS serves the anonymous Emby web branding CSS shim.
func (s *Server) handleBrandingCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleCurrentUser(w http.ResponseWriter, r *http.Request, rel string) {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	token := ExtractToken(r)
	session, ok := s.activeSession(w, r, token)
	if !ok {
		return
	}
	if err := s.guardProxyQueryCredentials(r.Context(), r.URL.RawQuery, token); err != nil {
		writeCredentialQueryError(w, err)
		return
	}
	requestedID := parts[1]
	if !strings.EqualFold(requestedID, "Me") && requestedID != session.SyntheticUserID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, privateUserDTO(session.GatewayUsername, session.SyntheticUserID, s.cfg.GatewayServerID))
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, rel string) {
	resourceKind := resourceRoute(r, rel)
	if resourceKind != resourceRouteNone {
		r = r.WithContext(context.WithValue(r.Context(), resourceCookieContextKey{}, resourceKind))
		applyResourceCachePolicy(w.Header(), resourceKind, http.StatusUnauthorized)
	}
	gatewayToken := ExtractToken(r)
	cookieAuthenticated := false
	if gatewayToken == "" {
		gatewayToken, _, cookieAuthenticated = resourceCookieToken(r, rel)
	}
	session, ok := s.activeSession(w, r, gatewayToken)
	if !ok {
		return
	}
	if err := s.guardProxyQueryCredentials(r.Context(), r.URL.RawQuery, gatewayToken); err != nil {
		writeCredentialQueryError(w, err)
		return
	}
	playbackItemID, isPlaybackInfo := playbackInfoItemID(r.Method, rel)
	guardKey := playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: playbackItemID}
	guardGeneration := uint64(0)
	if isPlaybackInfo {
		guardGeneration = s.playbackGuards.snapshot(guardKey)
	}
	if s.handlePersonalDataRequest(w, r, rel, session, gatewayToken) {
		return
	}
	if err := s.ensureBackendSession(r.Context(), session); err != nil {
		s.audit(r.Context(), AuditLog{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, Event: "backend_auth_failure", Message: "backend authentication failed", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusBadGateway})
		http.Error(w, "backend authentication failed", http.StatusBadGateway)
		return
	}
	if isUpgradeRequest(r) {
		s.prepareBackendUpgrade(r.Context(), r, rel, session)
	}
	proxyURL, err := s.proxyURL(session, rel, r.URL.RawQuery, gatewayToken)
	if err != nil {
		if errors.Is(err, errMalformedQuery) || errors.Is(err, errCredentialConflict) || errors.Is(err, errCredentialStore) {
			writeCredentialQueryError(w, err)
			return
		}
		s.audit(r.Context(), AuditLog{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, Event: "proxy_backend_unavailable", Message: "backend url unavailable", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusBadGateway})
		http.Error(w, "bad backend url", http.StatusBadGateway)
		return
	}
	if isUpgradeRequest(r) {
		s.handleUpgradeProxy(w, r, proxyURL, session, gatewayToken, rel)
		return
	}
	body, rawBody, replayable, err := s.rewriteRequestBody(r, session, gatewayToken)
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(withRedirectCredentialTokens(r.Context(), gatewayToken, session.BackendToken), r.Method, proxyURL.String(), body)
	if err != nil {
		http.Error(w, "bad proxy request", http.StatusInternalServerError)
		return
	}
	if body != nil {
		req.ContentLength = contentLength(body)
	}
	copyRequestHeaders(req.Header, r.Header)
	s.rewriteRequestHeaders(req.Header, session, gatewayToken)
	req.Host = proxyURL.Host

	attemptStarted := time.Now()
	resp, err := s.proxyClient.Do(req)
	if err != nil {
		upstreamStatus := closeResponseOnError(resp)
		s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_backend_unavailable", AuditMessage: "backend unavailable", ClientBody: "backend unavailable", FallbackKind: "upstream_request_error", Duration: time.Since(attemptStarted), UpstreamStatus: upstreamStatus})
		return
	}
	defer resp.Body.Close()
	if isPlaybackInfo && resp.StatusCode == http.StatusUnauthorized && isConcurrentPlaybackDenial(resp) {
		s.writeConcurrentPlaybackDenied(w, r, rel, session, guardKey)
		return
	}
	if resp.StatusCode == http.StatusUnauthorized && replayable {
		failedToken := session.BackendToken
		if err := s.refreshBackendSession(r.Context(), session, failedToken); err == nil {
			s.auditBackendTokenRefresh(r, rel, session, "backend_token_refresh", "backend token refreshed after unauthorized response", http.StatusOK)
			_ = resp.Body.Close()
			retryURL, retryErr := s.proxyURL(session, rel, r.URL.RawQuery, gatewayToken)
			if retryErr != nil {
				s.audit(r.Context(), AuditLog{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, Event: "proxy_backend_unavailable", Message: "backend url unavailable", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusBadGateway})
				http.Error(w, "bad backend url", http.StatusBadGateway)
				return
			}
			var retryBody io.Reader
			if rawBody != nil {
				retryBody = s.rewriteRequestBodyData(rawBody, session, gatewayToken)
			}
			retryReq, retryErr := http.NewRequestWithContext(withRedirectCredentialTokens(r.Context(), gatewayToken, session.BackendToken), r.Method, retryURL.String(), retryBody)
			if retryErr != nil {
				http.Error(w, "bad proxy request", http.StatusInternalServerError)
				return
			}
			if retryBody != nil {
				retryReq.ContentLength = contentLength(retryBody)
			}
			copyRequestHeaders(retryReq.Header, r.Header)
			s.rewriteRequestHeaders(retryReq.Header, session, gatewayToken)
			retryReq.Host = retryURL.Host
			attemptStarted = time.Now()
			resp, err = s.proxyClient.Do(retryReq)
			if err != nil {
				upstreamStatus := closeResponseOnError(resp)
				s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_backend_unavailable", AuditMessage: "backend unavailable", ClientBody: "backend unavailable", FallbackKind: "upstream_request_error", Duration: time.Since(attemptStarted), UpstreamStatus: upstreamStatus})
				return
			}
			defer resp.Body.Close()
			if isPlaybackInfo && resp.StatusCode == http.StatusUnauthorized && isConcurrentPlaybackDenial(resp) {
				s.writeConcurrentPlaybackDenied(w, r, rel, session, guardKey)
				return
			}
		} else if !errors.Is(err, ErrUnauthorized) {
			s.auditBackendTokenRefresh(r, rel, session, "backend_token_refresh_failure", err.Error(), http.StatusUnauthorized)
		}
	}
	if isPlaybackInfo && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		s.playbackGuards.clearIfGeneration(guardKey, guardGeneration)
	}

	rewriteToken := gatewayToken
	if cookieAuthenticated {
		rewriteToken = ""
	}
	s.writeProxyResponse(w, r, rel, resp, session, rewriteToken, s.gatewayBaseForRequest(r))
}

const concurrentPlaybackResponse = `{"error":"playback_access_denied","message":"Playback denied because the concurrent playback limit was exceeded.","reason_code":"max_concurrent_sessions_exceeded"}`

func playbackInfoItemID(method, rel string) (string, bool) {
	if method != http.MethodGet && method != http.MethodPost {
		return "", false
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 3 || !strings.EqualFold(parts[0], "Items") || strings.TrimSpace(parts[1]) == "" || !strings.EqualFold(parts[2], "PlaybackInfo") {
		return "", false
	}
	return parts[1], true
}

type delegatedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r delegatedReadCloser) Close() error { return r.closer.Close() }

type replayReadErrorCloser struct {
	prefix       []byte
	err          error
	errorPending bool
	remainder    io.Reader
	closer       io.Closer
}

func (r *replayReadErrorCloser) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		if len(r.prefix) == 0 && r.errorPending {
			r.errorPending = false
			return n, r.err
		}
		return n, nil
	}
	if r.errorPending {
		r.errorPending = false
		return 0, r.err
	}
	return r.remainder.Read(p)
}

func (r *replayReadErrorCloser) Close() error { return r.closer.Close() }

func isConcurrentPlaybackDenial(resp *http.Response) bool {
	const limit = 48 << 10
	original := resp.Body
	data, err := io.ReadAll(io.LimitReader(original, limit+1))
	restore := func() {
		resp.Body = delegatedReadCloser{Reader: io.MultiReader(bytes.NewReader(data), original), closer: original}
	}
	if err != nil {
		resp.Body = &replayReadErrorCloser{prefix: data, err: err, errorPending: true, remainder: original, closer: original}
		return false
	}
	if len(data) > limit {
		restore()
		return false
	}
	if !hasConcurrentPlaybackReason(data) {
		restore()
		return false
	}
	return true
}

func hasConcurrentPlaybackReason(data []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	count := 0
	var reason string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return false
		}
		name, ok := key.(string)
		if !ok {
			return false
		}
		var value any
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		if strings.EqualFold(name, "reason_code") {
			count++
			if count > 1 {
				return false
			}
			reason, ok = value.(string)
			if !ok {
				return false
			}
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return false
	}
	return count == 1 && reason == "max_concurrent_sessions_exceeded"
}

func (s *Server) writeConcurrentPlaybackDenied(w http.ResponseWriter, r *http.Request, rel string, session *Session, key playbackGuardKey) {
	if s.playbackGuards.deny(key) {
		s.audit(r.Context(), AuditLog{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, Event: "playback_concurrency_denied", Message: "playback denied because the concurrent playback limit was exceeded", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusForbidden})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, concurrentPlaybackResponse)
}

func (s *Server) handleUpgradeProxy(w http.ResponseWriter, r *http.Request, proxyURL *url.URL, session *Session, gatewayToken, rel string) {
	started := time.Now()
	inbound := r
	r = r.WithContext(withRedirectCredentialTokens(r.Context(), gatewayToken, session.BackendToken))
	trackedWriter := &upgradeResponseWriter{ResponseWriter: w}
	proxy := &httputil.ReverseProxy{
		Transport: &upgradeRetryTransport{base: s.proxyClient.Transport, server: s, original: inbound, session: session, gatewayToken: gatewayToken, rel: rel},
		Director: func(req *http.Request) {
			req.URL = proxyURL
			req.Host = proxyURL.Host
			s.rewriteRequestHeaders(req.Header, session, gatewayToken)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if trackedWriter.finalResponse.Load() || trackedWriter.hijacked.Load() {
				return
			}
			s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_backend_unavailable", AuditMessage: "backend unavailable", ClientBody: "backend unavailable", FallbackKind: "upstream_request_error", Duration: time.Since(started)})
		},
	}
	proxy.ServeHTTP(trackedWriter, r)
}

// prepareBackendUpgrade is best-effort: a failed validity probe must not block
// a usable websocket handshake.
func (s *Server) prepareBackendUpgrade(ctx context.Context, r *http.Request, rel string, session *Session) {
	failedToken := session.BackendToken
	stale, err := s.backendTokenIsUnauthorized(ctx, session, failedToken)
	if err != nil || !stale {
		return
	}
	if err := s.refreshBackendSessionKnownUnauthorized(ctx, session, failedToken); err == nil {
		s.auditBackendTokenRefresh(r, rel, session, "backend_token_refresh", "backend token refreshed before upgrade", http.StatusOK)
	} else if !errors.Is(err, ErrUnauthorized) {
		s.auditBackendTokenRefresh(r, rel, session, "backend_token_refresh_failure", "backend token refresh failed before upgrade", http.StatusUnauthorized)
	}
}

func (s *Server) activeSession(w http.ResponseWriter, r *http.Request, token string) (*Session, bool) {
	if token == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	session, err := s.store.FindSessionByTokenHash(r.Context(), HashToken(token))
	if err != nil || !session.Active(time.Now().UTC()) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	return session, true
}

func (s *Server) pathPolicyAllows(w http.ResponseWriter, r *http.Request, rel string) bool {
	decision, err := s.store.CheckPathPolicy(r.Context(), r.Method, rel)
	if err != nil {
		s.audit(r.Context(), s.auditForRequest(r, rel, "path_policy_error", "path policy check failed", http.StatusInternalServerError))
		http.Error(w, "path policy unavailable", http.StatusInternalServerError)
		return false
	}
	if !decision.Allowed {
		s.audit(r.Context(), s.auditForRequest(r, rel, "path_denied", "path policy denied request", http.StatusForbidden))
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) auditForRequest(r *http.Request, rel, event, message string, status int) AuditLog {
	entry := AuditLog{Event: event, Message: message, RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: status}
	if token := ExtractToken(r); token != "" {
		if session, err := s.store.FindSessionByTokenHash(r.Context(), HashToken(token)); err == nil && session != nil {
			entry.GatewayUserID = session.GatewayUserID
			entry.SyntheticUserID = session.SyntheticUserID
		}
	}
	return entry
}

func (s *Server) auditLoginFailure(ctx context.Context, r *http.Request, username, message string, status int) {
	entry := AuditLog{Event: "login_failure", Message: message, RemoteIP: remoteIP(r), Method: r.Method, Path: r.URL.Path, Status: status}
	if strings.TrimSpace(username) != "" {
		if user, err := s.store.FindGatewayUserByUsername(ctx, username); err == nil && user != nil {
			entry.GatewayUserID = user.ID
			entry.SyntheticUserID = user.SyntheticUserID
		}
	}
	s.audit(ctx, entry)
}

func (s *Server) audit(ctx context.Context, entry AuditLog) {
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	_ = s.store.RecordAudit(ctx, entry)
}

func (s *Server) auditBackendTokenRefresh(r *http.Request, rel string, session *Session, event, message string, status int) {
	s.audit(r.Context(), AuditLog{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, Event: event, Message: message, RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: status})
}

func (s *Server) proxyURL(session *Session, rel, rawQuery, gatewayToken string) (*url.URL, error) {
	backend, err := backendURL(session.BackendBaseURL, rel)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}
	q, err := parseRawQuery(rawQuery)
	if err != nil {
		return nil, errMalformedQuery
	}
	rewriteProxyQueryValues(q, gatewayToken, session)
	u.RawQuery = q.Encode()
	u.Path = strings.ReplaceAll(u.Path, session.SyntheticUserID, session.BackendUserID)
	return u, nil
}

func (s *Server) rewriteRequestHeaders(h http.Header, session *Session, gatewayToken string) {
	stripResourceCookie(h)
	// Set replaces all values for a key, collapsing duplicate client headers.
	for _, name := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if len(h.Values(name)) > 0 {
			h.Set(name, session.BackendToken)
		}
	}
	if len(h.Values("X-Emby-Token")) == 0 {
		h.Set("X-Emby-Token", session.BackendToken)
	}
	identity := session.BackendIdentity.WithDefaults()
	h.Set("User-Agent", identity.UserAgent)
	auth := backendAuthHeader(identity, session.BackendUserID, session.BackendToken).String()
	h.Set("X-Emby-Authorization", auth)
	if len(h.Values("Authorization")) > 0 {
		h.Set("Authorization", auth)
	}
}

func backendAuthHeader(identity BackendClientIdentity, userID, token string) AuthHeader {
	identity = identity.WithDefaults()
	return AuthHeader{
		Scheme:   "Emby",
		UserID:   userID,
		Client:   identity.Client,
		Device:   identity.Device,
		DeviceID: identity.DeviceID,
		Version:  identity.Version,
		Token:    token,
		Fields:   map[string]string{},
	}
}

func (s *Server) rewriteRequestBody(r *http.Request, session *Session, gatewayToken string) (io.Reader, []byte, bool, error) {
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil, nil, true, nil
	}
	if !isRewriteableContentType(r.Header.Get("Content-Type")) {
		return r.Body, nil, false, nil
	}
	data, err := io.ReadAll(http.MaxBytesReader(nilResponseWriter{}, r.Body, 10<<20))
	if err != nil {
		return nil, nil, false, err
	}
	return s.rewriteRequestBodyData(data, session, gatewayToken), data, true, nil
}

func (s *Server) rewriteRequestBodyData(data []byte, session *Session, gatewayToken string) io.Reader {
	text := strings.ReplaceAll(string(data), gatewayToken, session.BackendToken)
	text = strings.ReplaceAll(text, session.SyntheticUserID, session.BackendUserID)
	return strings.NewReader(text)
}

func isPlaybackReportRequest(method, rel string) bool {
	if method != http.MethodPost {
		return false
	}
	switch {
	case equalPath(rel, "/Sessions/Playing"):
		return true
	case equalPath(rel, "/Sessions/Playing/Progress"):
		return true
	case equalPath(rel, "/Sessions/Playing/Stopped"):
		return true
	default:
		return false
	}
}

func isPlaybackKeepaliveRequest(method, rel string) bool {
	return method == http.MethodPost && equalPath(rel, "/Sessions/Playing/Ping")
}

func isSessionCapabilitiesRequest(method, rel string) bool {
	if method != http.MethodPost {
		return false
	}
	switch {
	case equalPath(rel, "/Sessions/Capabilities"):
		return true
	case equalPath(rel, "/Sessions/Capabilities/Full"):
		return true
	default:
		return false
	}
}

func (s *Server) recordPlaybackRequest(r *http.Request, rel string, session *Session, gatewayToken string) error {
	var data []byte
	if r.Body != nil {
		var err error
		data, err = io.ReadAll(http.MaxBytesReader(nilResponseWriter{}, r.Body, 10<<20))
		r.Body = io.NopCloser(bytes.NewReader(data))
		if err != nil {
			return err
		}
	}
	details, ok := playbackDetailsFromRequest(r, data)
	if !ok || details.ItemID == "" {
		return nil
	}
	key := playbackGuardKey{GatewayTokenHash: session.GatewayTokenHash, ItemID: details.ItemID}
	if active, auditEligible := s.playbackGuards.suppress(key); active {
		if auditEligible {
			s.audit(r.Context(), AuditLog{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, Event: "playback_report_suppressed", Message: "playback report suppressed after concurrent playback denial", RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusNoContent})
		}
		return nil
	}
	now := time.Now().UTC()
	eventName := playbackEventName(rel)
	_ = s.store.RecordPlaybackEvent(r.Context(), PlaybackEvent{
		GatewayUserID:    session.GatewayUserID,
		SyntheticUserID:  session.SyntheticUserID,
		ItemID:           details.ItemID,
		Event:            eventName,
		PositionTicks:    details.PositionTicks,
		Played:           details.Played,
		PlayedPercentage: details.PlayedPercentage,
		RemoteIP:         remoteIP(r),
		CreatedAt:        now,
	})
	state, err := s.store.FindPlaybackState(r.Context(), session.GatewayUserID, details.ItemID)
	if err != nil || state == nil {
		state = &PlaybackState{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, ItemID: details.ItemID}
	}
	state.SyntheticUserID = session.SyntheticUserID
	if details.ItemName != "" {
		state.ItemName = details.ItemName
	}
	if details.ItemType != "" {
		state.ItemType = details.ItemType
	}
	if details.SeriesID != "" {
		state.SeriesID = details.SeriesID
	}
	if details.SeriesName != "" {
		state.SeriesName = details.SeriesName
	}
	if details.SeasonID != "" {
		state.SeasonID = details.SeasonID
	}
	if details.HasIndexNumber {
		state.IndexNumber = details.IndexNumber
	}
	if details.HasParentIndexNumber {
		state.ParentIndexNumber = details.ParentIndexNumber
	}
	if details.HasRunTimeTicks {
		state.RunTimeTicks = details.RunTimeTicks
	}
	if details.Fingerprint != "" {
		state.Fingerprint = details.Fingerprint
	}
	if details.HasPositionTicks {
		state.PlaybackPositionTicks = details.PositionTicks
	}
	if details.PlayedPercentage != nil {
		percentage := *details.PlayedPercentage
		state.PlayedPercentage = &percentage
	}
	wasPlayed := state.Played
	if details.Played != nil {
		state.Played = *details.Played
	}
	if eventName == "stopped" {
		if state.RunTimeTicks <= 0 {
			s.enrichPlaybackStateMetadata(r.Context(), r, session, gatewayToken, state)
		}
		applyStoppedPlaybackState(state, now, wasPlayed, s.resumePolicyForState(state))
	}
	state.UpdatedAt = now
	_ = s.store.SavePlaybackState(r.Context(), *state)
	return nil
}

func playbackDetailsFromRequest(r *http.Request, data []byte) (playbackDetails, bool) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if len(bytes.TrimSpace(data)) > 0 {
		if ct == "application/x-www-form-urlencoded" {
			values, err := url.ParseQuery(string(data))
			if err == nil {
				merged := cloneQuery(r.URL.Query())
				for key, vals := range values {
					merged[key] = append([]string(nil), vals...)
				}
				return playbackDetailsFromValues(merged)
			}
		}
		if ct == "" || isJSONContentType(ct) || looksLikeJSON(data) {
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.UseNumber()
			var body any
			if err := decoder.Decode(&body); err == nil {
				if details, ok := playbackDetailsFromJSON(body); ok {
					return mergePlaybackDetails(details, r.URL.Query()), true
				}
				if details, ok := playbackDetailsFromValues(r.URL.Query()); ok {
					if bodyDetails, bodyOK := playbackDetailsFromJSON(body); bodyOK || bodyDetails.HasPositionTicks || bodyDetails.HasRunTimeTicks || bodyDetails.Played != nil || bodyDetails.PlayedPercentage != nil {
						return mergePlaybackDetails(bodyDetails, r.URL.Query()), true
					}
					return details, true
				}
			}
		}
	}
	return playbackDetailsFromValues(r.URL.Query())
}

func mergePlaybackDetails(details playbackDetails, values url.Values) playbackDetails {
	queryDetails, ok := playbackDetailsFromValues(values)
	if !ok {
		return details
	}
	if details.ItemID == "" {
		details.ItemID = queryDetails.ItemID
	}
	if !details.HasPositionTicks && queryDetails.HasPositionTicks {
		details.PositionTicks = queryDetails.PositionTicks
		details.HasPositionTicks = true
	}
	if !details.HasRunTimeTicks && queryDetails.HasRunTimeTicks {
		details.RunTimeTicks = queryDetails.RunTimeTicks
		details.HasRunTimeTicks = true
	}
	if details.Played == nil && queryDetails.Played != nil {
		details.Played = queryDetails.Played
	}
	if details.PlayedPercentage == nil && queryDetails.PlayedPercentage != nil {
		details.PlayedPercentage = queryDetails.PlayedPercentage
	}
	return details
}

type playbackDetails struct {
	ItemID               string
	PositionTicks        int64
	HasPositionTicks     bool
	Played               *bool
	PlayedPercentage     *float64
	ItemName             string
	ItemType             string
	SeriesID             string
	SeriesName           string
	SeasonID             string
	IndexNumber          int
	ParentIndexNumber    int
	RunTimeTicks         int64
	HasIndexNumber       bool
	HasParentIndexNumber bool
	HasRunTimeTicks      bool
	Fingerprint          string
}

func playbackDetailsFromJSON(v any) (playbackDetails, bool) {
	obj, ok := v.(map[string]any)
	if !ok {
		return playbackDetails{}, false
	}
	details := playbackDetails{}
	if itemID, ok := stringField(obj, "ItemId"); ok {
		details.ItemID = itemID
	} else if item, ok := mapField(obj, "Item"); ok {
		details.ItemID, _ = stringField(item, "Id")
	}
	if item, ok := mapField(obj, "Item"); ok {
		details.ItemName, _ = stringField(item, "Name")
		details.ItemType, _ = stringField(item, "Type")
		details.SeriesID, _ = stringField(item, "SeriesId")
		details.SeriesName, _ = stringField(item, "SeriesName")
		details.SeasonID, _ = stringField(item, "SeasonId")
		if v, ok := int64Field(item, "IndexNumber"); ok {
			details.IndexNumber = int(v)
			details.HasIndexNumber = true
		}
		if v, ok := int64Field(item, "ParentIndexNumber"); ok {
			details.ParentIndexNumber = int(v)
			details.HasParentIndexNumber = true
		}
		if v, ok := int64Field(item, "RunTimeTicks"); ok {
			details.RunTimeTicks = v
			details.HasRunTimeTicks = true
		}
		details.Fingerprint = itemFingerprint(item)
	}
	if ticks, ok := int64Field(obj, "PositionTicks"); ok {
		details.PositionTicks = ticks
		details.HasPositionTicks = true
	} else if ticks, ok := int64Field(obj, "PlaybackPositionTicks"); ok {
		details.PositionTicks = ticks
		details.HasPositionTicks = true
	}
	if ticks, ok := int64Field(obj, "RunTimeTicks"); ok && !details.HasRunTimeTicks {
		details.RunTimeTicks = ticks
		details.HasRunTimeTicks = true
	}
	if played, ok := boolField(obj, "Played"); ok {
		details.Played = &played
	}
	if percentage, ok := float64Field(obj, "PlayedPercentage"); ok {
		details.PlayedPercentage = &percentage
	}
	return details, details.ItemID != ""
}

func playbackDetailsFromValues(values url.Values) (playbackDetails, bool) {
	details := playbackDetails{}
	details.ItemID = firstValue(values, "ItemId", "ItemID", "Item.Id", "Id")
	if ticks, ok := int64Value(values, "PositionTicks", "PlaybackPositionTicks"); ok {
		details.PositionTicks = ticks
		details.HasPositionTicks = true
	}
	if ticks, ok := int64Value(values, "RunTimeTicks"); ok {
		details.RunTimeTicks = ticks
		details.HasRunTimeTicks = true
	}
	if played, ok := boolValue(values, "Played"); ok {
		details.Played = &played
	}
	if percentage, ok := float64Value(values, "PlayedPercentage"); ok {
		details.PlayedPercentage = &percentage
	}
	return details, details.ItemID != ""
}

func firstValue(values url.Values, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(values.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func int64Value(values url.Values, names ...string) (int64, bool) {
	for _, name := range names {
		if raw := strings.TrimSpace(values.Get(name)); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			return v, err == nil
		}
	}
	return 0, false
}

func float64Value(values url.Values, names ...string) (float64, bool) {
	for _, name := range names {
		if raw := strings.TrimSpace(values.Get(name)); raw != "" {
			v, err := strconv.ParseFloat(raw, 64)
			return v, err == nil
		}
	}
	return 0, false
}

func boolValue(values url.Values, names ...string) (bool, bool) {
	for _, name := range names {
		if raw := strings.TrimSpace(values.Get(name)); raw != "" {
			v, err := strconv.ParseBool(raw)
			return v, err == nil
		}
	}
	return false, false
}

type resumePolicy struct {
	MinPct             float64
	MaxPct             float64
	MinDurationSeconds float64
}

func (s *Server) resumePolicyForState(state *PlaybackState) resumePolicy {
	policy := resumePolicy{MinPct: s.cfg.MinResumePct, MaxPct: s.cfg.MaxResumePct, MinDurationSeconds: s.cfg.MinResumeDurationSeconds}
	if state != nil && (strings.EqualFold(state.ItemType, "AudioBook") || strings.EqualFold(state.ItemType, "Book")) {
		policy.MinDurationSeconds = 0
	}
	return policy
}

func applyStoppedPlaybackState(state *PlaybackState, now time.Time, wasPlayed bool, policy resumePolicy) {
	completed := state.Played
	position := state.PlaybackPositionTicks
	if position < 0 {
		position = 0
	}
	runtime := state.RunTimeTicks
	if !completed && runtime > 0 && position > 0 {
		percentage := (float64(position) / float64(runtime)) * 100
		durationSeconds := float64(runtime) / float64(embyTicksPerSecond)
		switch {
		case percentage < policy.MinPct:
			position = 0
			state.PlayedPercentage = nil
		case percentage > policy.MaxPct || position >= runtime-embyTicksPerSecond:
			completed = true
		case policy.MinDurationSeconds > 0 && durationSeconds < policy.MinDurationSeconds:
			completed = true
		}
	}
	if !completed && state.PlayedPercentage != nil && *state.PlayedPercentage >= policy.MaxPct {
		completed = true
	}
	if completed {
		lastPlayed := now
		state.LastPlayedDate = &lastPlayed
		state.Played = true
		state.PlaybackPositionTicks = 0
		state.PlayedPercentage = floatPtr(100)
		if !wasPlayed {
			state.PlayCount++
		}
		return
	}
	state.PlaybackPositionTicks = position
}

func playbackEventName(rel string) string {
	switch {
	case equalPath(rel, "/Sessions/Playing/Progress"):
		return "progress"
	case equalPath(rel, "/Sessions/Playing/Stopped"):
		return "stopped"
	default:
		return "playing"
	}
}

func (s *Server) writeProxyResponse(w http.ResponseWriter, r *http.Request, rel string, resp *http.Response, session *Session, gatewayToken, publicGatewayBase string) {
	ct := resp.Header.Get("Content-Type")
	cookieRoute := resourceRouteFromContext(r)
	if !responseAllowsBody(r.Method, resp.StatusCode) {
		if cookieRoute != resourceRouteNone {
			w.Header().Del("Cache-Control")
		}
		copyResponseHeaders(w.Header(), resp.Header, rel, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID)
		applyResourceCachePolicy(w.Header(), cookieRoute, resp.StatusCode)
		setContentLength(w.Header(), resp.ContentLength)
		w.WriteHeader(resp.StatusCode)
		return
	}
	s.clearMediaWriteDeadline(w, r, rel, resp, session)
	if isImageContentType(ct) && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.ContentLength == 0 {
		s.rejectInvalidImageResponse(w, r, rel, session, "backend returned an empty image")
		return
	}

	if cookieRoute != resourceRouteNone {
		w.Header().Del("Cache-Control")
	}
	copyResponseHeaders(w.Header(), resp.Header, rel, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID)
	applyResourceCachePolicy(w.Header(), cookieRoute, resp.StatusCode)
	if isM3U8ContentType(ct) || strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".m3u8") {
		readStarted := time.Now()
		data, err := readLimited(resp.Body, proxyM3U8Limit)
		if err != nil {
			s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_read_failed", AuditMessage: "backend response read failed", ClientBody: "response read failed", FallbackKind: "upstream_read_error", Duration: time.Since(readStarted), UpstreamStatus: resp.StatusCode})
			return
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(rewriteM3U8(data, rel, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID))
		return
	}

	if isMediaStreamResponse(r, rel, resp) {
		setContentLength(w.Header(), resp.ContentLength)
		w.WriteHeader(resp.StatusCode)
		s.copyMediaBodyOrAbort(w, r, rel, resp, session)
		return
	}

	if isStreamingContentType(ct) {
		if isImageContentType(ct) {
			s.writeImageProxyResponse(w, r, rel, resp, session)
			return
		}
		setContentLength(w.Header(), resp.ContentLength)
		w.WriteHeader(resp.StatusCode)
		s.copyMediaBodyOrAbort(w, r, rel, resp, session)
		return
	}
	if resp.StatusCode == http.StatusOK && strings.TrimSpace(ct) == "" {
		s.writeMissingContentTypeResponse(w, r, rel, resp, session, gatewayToken, publicGatewayBase)
		return
	}

	if isJSONContentType(ct) || strings.TrimSpace(ct) == "" {
		readStarted := time.Now()
		data, err := readLimited(resp.Body, proxyJSONLimit)
		if err != nil {
			s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_read_failed", AuditMessage: "backend response read failed", ClientBody: "response read failed", FallbackKind: "upstream_read_error", Duration: time.Since(readStarted), UpstreamStatus: resp.StatusCode})
			return
		}
		var value any
		if looksLikeJSON(data) && json.Unmarshal(data, &value) == nil {
			w.Header().Del("Content-Length")
			writeJSON(w, resp.StatusCode, s.rewriteProxyJSONValueForRequest(r.Context(), r, value, session, gatewayToken, publicGatewayBase))
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(rewriteBytes(data, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID))
		return
	}

	setContentLength(w.Header(), resp.ContentLength)
	w.WriteHeader(resp.StatusCode)
	s.copyProxyBodyOrAbort(w, r, rel, resp.Body, session)
}

func (s *Server) writeImageProxyResponse(w http.ResponseWriter, r *http.Request, rel string, resp *http.Response, session *Session) {
	var first [imageValidationTailSize]byte
	readStarted := time.Now()
	n, err := resp.Body.Read(first[:])
	if err != nil && err != io.EOF {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Del("ETag")
		w.Header().Del("Last-Modified")
		s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_invalid_image", AuditMessage: "backend image response read failed", ClientBody: "invalid image response", FallbackKind: "upstream_read_error", Duration: time.Since(readStarted), UpstreamStatus: resp.StatusCode})
		return
	}
	if n == 0 {
		if err == io.EOF {
			s.rejectInvalidImageResponse(w, r, rel, session, "backend returned an empty image")
			return
		}
		s.rejectInvalidImageResponse(w, r, rel, session, "backend returned an invalid image response")
		return
	}

	setContentLength(w.Header(), resp.ContentLength)
	w.WriteHeader(resp.StatusCode)
	fullImage := resp.StatusCode == http.StatusOK && strings.TrimSpace(r.Header.Get("Range")) == "" && strings.TrimSpace(resp.Header.Get("Content-Range")) == ""
	if !fullImage {
		if _, writeErr := w.Write(first[:n]); writeErr != nil {
			return
		}
		if err != nil && err != io.EOF {
			s.abortProxyBody(r, rel, session, err)
		}
		if err != io.EOF {
			s.copyProxyBodyOrAbort(w, r, rel, resp.Body, session)
		}
		return
	}

	validator := newImageStreamValidator(w, resp.Header.Get("Content-Type"))
	if _, writeErr := validator.Write(first[:n]); writeErr != nil {
		return
	}
	if err != nil && err != io.EOF {
		s.abortProxyBody(r, rel, session, err)
	}
	if err != io.EOF {
		if _, copyErr := io.Copy(validator, resp.Body); copyErr != nil {
			s.abortProxyBody(r, rel, session, copyErr)
		}
	}
	if finishErr := validator.Finish(); finishErr != nil {
		s.abortProxyBody(r, rel, session, finishErr)
	}
}

const imageValidationTailSize = 12

type imageStreamValidator struct {
	dst         io.Writer
	mediaType   string
	prefix      []byte
	tail        []byte
	total       int64
	passthrough bool
}

func newImageStreamValidator(dst io.Writer, contentType string) *imageStreamValidator {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return &imageStreamValidator{dst: dst, mediaType: mediaType, passthrough: mediaType != "image/jpeg" && mediaType != "image/png" && mediaType != "image/webp"}
}

func (v *imageStreamValidator) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	v.total += int64(len(p))
	if len(v.prefix) < imageValidationTailSize {
		need := imageValidationTailSize - len(v.prefix)
		if need > len(p) {
			need = len(p)
		}
		v.prefix = append(v.prefix, p[:need]...)
	}
	if v.passthrough {
		return v.dst.Write(p)
	}

	combined := make([]byte, 0, len(v.tail)+len(p))
	combined = append(combined, v.tail...)
	combined = append(combined, p...)
	if len(combined) <= imageValidationTailSize {
		v.tail = combined
		return len(p), nil
	}
	flushLen := len(combined) - imageValidationTailSize
	if _, err := v.dst.Write(combined[:flushLen]); err != nil {
		return 0, err
	}
	v.tail = append(v.tail[:0], combined[flushLen:]...)
	return len(p), nil
}

func (v *imageStreamValidator) Finish() error {
	if v.passthrough {
		return nil
	}
	if err := v.validate(); err != nil {
		return err
	}
	_, err := v.dst.Write(v.tail)
	return err
}

func (v *imageStreamValidator) validate() error {
	switch v.mediaType {
	case "image/jpeg":
		if len(v.prefix) < 2 || len(v.tail) < 2 || v.prefix[0] != 0xff || v.prefix[1] != 0xd8 || v.tail[len(v.tail)-2] != 0xff || v.tail[len(v.tail)-1] != 0xd9 {
			return errors.New("backend returned an incomplete JPEG image")
		}
	case "image/png":
		pngSignature := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
		pngIEND := []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82}
		if len(v.prefix) < len(pngSignature) || !bytes.Equal(v.prefix[:len(pngSignature)], pngSignature) || len(v.tail) != len(pngIEND) || !bytes.Equal(v.tail, pngIEND) {
			return errors.New("backend returned an incomplete PNG image")
		}
	case "image/webp":
		if len(v.prefix) < 12 || string(v.prefix[:4]) != "RIFF" || string(v.prefix[8:12]) != "WEBP" || int64(binary.LittleEndian.Uint32(v.prefix[4:8]))+8 != v.total {
			return errors.New("backend returned an incomplete WebP image")
		}
	}
	return nil
}

func (s *Server) copyProxyBodyOrAbort(w http.ResponseWriter, r *http.Request, rel string, body io.Reader, session *Session) {
	if _, err := io.Copy(w, body); err != nil {
		s.abortProxyBody(r, rel, session, err)
	}
}

func (s *Server) copyMediaBodyOrAbort(w http.ResponseWriter, r *http.Request, rel string, resp *http.Response, session *Session) {
	s.copyMediaReaderOrAbort(w, r, rel, resp.Body, resp.ContentLength, resp.StatusCode, session)
}

func (s *Server) copyMediaReaderOrAbort(w http.ResponseWriter, r *http.Request, rel string, src io.Reader, expectedLength int64, upstreamStatus int, session *Session) {
	result := copyMediaBody(w, src, expectedLength)
	if result.Err == nil {
		return
	}
	if r.Context().Err() == nil {
		s.auditMediaCopyFailure(r, rel, upstreamStatus, session, result)
	}
	panic(http.ErrAbortHandler)
}

func (s *Server) abortProxyBody(r *http.Request, rel string, session *Session, err error) {
	s.audit(r.Context(), AuditLog{GatewayUserID: sessionGatewayUserID(session), SyntheticUserID: sessionSyntheticUserID(session), Event: "proxy_read_failed", Message: err.Error(), RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusBadGateway})
	panic(http.ErrAbortHandler)
}

func (s *Server) rejectInvalidImageResponse(w http.ResponseWriter, r *http.Request, rel string, session *Session, message string) {
	if resourceRouteFromContext(r) == resourceRouteImage {
		w.Header().Set("Cache-Control", "private, no-store")
		mergeVaryCookie(w.Header())
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Del("ETag")
	w.Header().Del("Last-Modified")
	s.audit(r.Context(), AuditLog{GatewayUserID: sessionGatewayUserID(session), SyntheticUserID: sessionSyntheticUserID(session), Event: "proxy_invalid_image", Message: message, RemoteIP: remoteIP(r), Method: r.Method, Path: rel, Status: http.StatusBadGateway})
	http.Error(w, "invalid image response", http.StatusBadGateway)
}

func setContentLength(header http.Header, length int64) {
	if length >= 0 {
		header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
}

func responseAllowsBody(method string, status int) bool {
	if method == http.MethodHead {
		return false
	}
	return status >= 200 && status != http.StatusNoContent && status != http.StatusResetContent && status != http.StatusNotModified
}

func sessionGatewayUserID(session *Session) string {
	if session == nil {
		return ""
	}
	return session.GatewayUserID
}

func sessionSyntheticUserID(session *Session) string {
	if session == nil {
		return ""
	}
	return session.SyntheticUserID
}

func (s *Server) rewriteProxyJSONValue(ctx context.Context, v any, session *Session, gatewayToken, publicGatewayBase string) any {
	return s.rewriteProxyJSONValueForRequest(ctx, nil, v, session, gatewayToken, publicGatewayBase)
}

func (s *Server) rewriteProxyJSONValueForRequest(ctx context.Context, r *http.Request, v any, session *Session, gatewayToken, publicGatewayBase string) any {
	rewritten := rewriteJSONValue(v, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID)
	if session == nil {
		return rewritten
	}
	items := selectBaseItems(rewritten)
	itemIDs := itemIDsFromBaseItems(items)
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
	s.overlayUserData(items, session, states, aggregates)
	return rewritten
}

func (s *Server) applyChildCountsToAggregates(ctx context.Context, r *http.Request, session *Session, gatewayToken string, items []map[string]any, aggregates *PlaybackAggregates) {
	seriesIDs, seasonIDs := aggregateItemIDs(items)
	ids := append(seriesIDs, seasonIDs...)
	counts, err := s.store.ListItemChildCounts(ctx, session.BackendAccountID, ids)
	if err != nil {
		counts = map[string]ItemChildCount{}
	}
	missing := []string{}
	for _, id := range ids {
		if count, ok := counts[id]; ok && count.ChildCount > 0 {
			applyAggregateTotal(aggregates, id, count.ChildCount)
			continue
		}
		if r != nil && len(missing) < aggregateChildCountLookups {
			missing = append(missing, id)
		}
	}
	for _, id := range missing {
		count := s.fetchItemChildCount(ctx, r, session, gatewayToken, id)
		if count <= 0 {
			continue
		}
		applyAggregateTotal(aggregates, id, count)
		_ = s.store.SaveItemChildCount(ctx, ItemChildCount{BackendAccountID: session.BackendAccountID, ItemID: id, ChildCount: count})
	}
}

func (s *Server) fetchItemChildCount(ctx context.Context, r *http.Request, session *Session, gatewayToken, itemID string) int {
	q := url.Values{}
	q.Set("ParentId", itemID)
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Episode")
	q.Set("Limit", "0")
	value, status, err := s.fetchBackendJSON(ctx, r, "/Users/"+session.SyntheticUserID+"/Items", q.Encode(), session, gatewayToken)
	if err != nil || status < 200 || status >= 300 {
		return 0
	}
	if total, ok := totalRecordCount(value); ok {
		return total
	}
	return len(extractItems(value))
}

func applyAggregateTotal(aggregates *PlaybackAggregates, itemID string, count int) {
	if aggregate, ok := aggregates.Series[itemID]; ok {
		aggregate.TotalItemCount = count
		aggregates.Series[itemID] = aggregate
	}
	if aggregate, ok := aggregates.Seasons[itemID]; ok {
		aggregate.TotalItemCount = count
		aggregates.Seasons[itemID] = aggregate
	}
}

func selectBaseItems(v any) []map[string]any {
	if item, ok := v.(map[string]any); ok {
		if isBaseItemJSON(item) {
			return []map[string]any{item}
		}
		items := make([]map[string]any, 0)
		if wrapped, ok := mapField(item, "Item"); ok && isBaseItemJSON(wrapped) {
			items = append(items, wrapped)
		}
		if values, ok := field(item, "Items"); ok {
			if array, ok := values.([]any); ok {
				for _, value := range array {
					if item, ok := value.(map[string]any); ok && isBaseItemJSON(item) {
						items = append(items, item)
					}
				}
			}
		}
		return items
	}
	array, ok := v.([]any)
	if !ok {
		return nil
	}
	items := make([]map[string]any, 0, len(array))
	for _, value := range array {
		if item, ok := value.(map[string]any); ok && isBaseItemJSON(item) {
			items = append(items, item)
		}
	}
	return items
}

func itemIDsFromBaseItems(items []map[string]any) []string {
	seen := map[string]struct{}{}
	itemIDs := make([]string, 0, len(items))
	for _, item := range items {
		itemID, _ := stringField(item, "Id")
		if _, exists := seen[itemID]; !exists {
			seen[itemID] = struct{}{}
			itemIDs = append(itemIDs, itemID)
		}
	}
	return itemIDs
}

func aggregateItemIDs(items []map[string]any) ([]string, []string) {
	seriesSeen := map[string]struct{}{}
	seasonSeen := map[string]struct{}{}
	var seriesIDs []string
	var seasonIDs []string
	for _, item := range items {
		itemID, _ := stringField(item, "Id")
		itemType, _ := stringField(item, "Type")
		if strings.EqualFold(itemType, "Series") {
			if _, exists := seriesSeen[itemID]; !exists {
				seriesSeen[itemID] = struct{}{}
				seriesIDs = append(seriesIDs, itemID)
			}
		}
		if strings.EqualFold(itemType, "Season") {
			if _, exists := seasonSeen[itemID]; !exists {
				seasonSeen[itemID] = struct{}{}
				seasonIDs = append(seasonIDs, itemID)
			}
		}
	}
	return seriesIDs, seasonIDs
}

func (s *Server) overlayUserData(items []map[string]any, session *Session, states map[string]*PlaybackState, aggregates PlaybackAggregates) {
	for _, item := range items {
		itemID, _ := stringField(item, "Id")
		userData, ok := mapField(item, "UserData")
		if !ok {
			userData = map[string]any{}
			item["UserData"] = userData
		}
		state := states[itemID]
		if state == nil {
			state = &PlaybackState{GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID, ItemID: itemID}
		}
		applyPlaybackStateToUserData(userData, state, item, aggregateForItem(item, itemID, aggregates))
	}
}

func isBaseItemJSON(obj map[string]any) bool {
	if itemID, ok := stringField(obj, "Id"); !ok || itemID == "" {
		return false
	}
	if _, ok := field(obj, "UserData"); ok {
		return true
	}
	if itemType, ok := stringField(obj, "Type"); ok && isBaseItemType(itemType) {
		return true
	}
	for _, name := range []string{"MediaType", "RunTimeTicks", "SeriesId", "SeasonId", "ProviderIds", "MediaSources"} {
		if _, ok := field(obj, name); ok {
			return true
		}
	}
	return false
}

func isBaseItemType(itemType string) bool {
	switch strings.ToLower(itemType) {
	case "adultvideo", "aggregatefolder", "audio", "audiobook", "basepluginfolder", "book", "boxset", "channel", "channelfolderitem", "collectionfolder", "episode", "folder", "game", "gamesystem", "genre", "livetvchannel", "livetvprogram", "manualplaylistsfolder", "movie", "musicalbum", "musicartist", "musicgenre", "musicvideo", "person", "photo", "photoalbum", "playlist", "program", "recording", "season", "series", "studio", "trailer", "tvchannel", "tvprogram", "userrootfolder", "userview", "video", "year":
		return true
	default:
		return false
	}
}

func applyPlaybackStateToUserData(userData map[string]any, state *PlaybackState, item map[string]any, aggregate *PlaybackAggregate) {
	userData["Played"] = state.Played
	userData["PlaybackPositionTicks"] = state.PlaybackPositionTicks
	if state.Played {
		userData["PlayedPercentage"] = 100.0
	} else if percentage, ok := playedPercentageForItem(state, item); ok {
		userData["PlayedPercentage"] = percentage
	} else if state.PlayedPercentage != nil {
		userData["PlayedPercentage"] = *state.PlayedPercentage
	} else {
		delete(userData, "PlayedPercentage")
	}
	if state.LastPlayedDate != nil {
		userData["LastPlayedDate"] = state.LastPlayedDate.UTC().Format(time.RFC3339)
	} else {
		delete(userData, "LastPlayedDate")
	}
	userData["PlayCount"] = state.PlayCount
	userData["IsFavorite"] = state.IsFavorite
	userData["ItemId"] = state.ItemID
	userData["Key"] = state.ItemID
	delete(userData, "Rating")
	if state.Played {
		userData["UnplayedItemCount"] = 0
	} else {
		delete(userData, "UnplayedItemCount")
	}
	applyAggregateUserData(userData, item, aggregate)
	if state.Likes != nil {
		userData["Likes"] = *state.Likes
	} else {
		delete(userData, "Likes")
	}
}

func aggregateForItem(item map[string]any, itemID string, aggregates PlaybackAggregates) *PlaybackAggregate {
	itemType, _ := stringField(item, "Type")
	if strings.EqualFold(itemType, "Series") {
		if aggregate, ok := aggregates.Series[itemID]; ok {
			return &aggregate
		}
	}
	if strings.EqualFold(itemType, "Season") {
		if aggregate, ok := aggregates.Seasons[itemID]; ok {
			return &aggregate
		}
	}
	return nil
}

func applyAggregateUserData(userData map[string]any, item map[string]any, aggregate *PlaybackAggregate) {
	if aggregate == nil {
		return
	}
	total := itemChildCount(item)
	if total <= 0 {
		total = aggregate.TotalItemCount
	}
	if total <= 0 {
		return
	}
	played := aggregate.PlayedCount
	if played > total {
		played = total
	}
	unplayed := total - played
	userData["UnplayedItemCount"] = unplayed
	userData["Played"] = played >= total
	if played > 0 {
		userData["PlayedPercentage"] = (float64(played) / float64(total)) * 100
	} else {
		delete(userData, "PlayedPercentage")
	}
	if aggregate.LastPlayedDate != nil {
		userData["LastPlayedDate"] = aggregate.LastPlayedDate.UTC().Format(time.RFC3339)
	}
}

func playedPercentageForItem(state *PlaybackState, item map[string]any) (float64, bool) {
	runtime := state.RunTimeTicks
	if ticks, ok := int64Field(item, "RunTimeTicks"); ok && ticks > 0 {
		runtime = ticks
	}
	if runtime <= 0 || state.PlaybackPositionTicks <= 0 {
		return 0, false
	}
	percentage := (float64(state.PlaybackPositionTicks) / float64(runtime)) * 100
	if percentage < 0 {
		percentage = 0
	}
	if percentage > 100 {
		percentage = 100
	}
	return percentage, true
}

func itemChildCount(item map[string]any) int {
	for _, name := range []string{"RecursiveItemCount", "ChildCount", "Count"} {
		if value, ok := int64Field(item, name); ok && value > 0 {
			return int(value)
		}
	}
	return 0
}

func rewriteJSONValue(v any, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			if raw, ok := v.(string); ok && session != nil && (strings.EqualFold(k, "DirectStreamUrl") || strings.EqualFold(k, "TranscodingUrl")) {
				out[k] = rewriteMediaReference(raw, session, gatewayToken, publicGatewayBase, gatewayServerID, false)
			} else {
				out[k] = rewriteJSONValue(v, session, gatewayToken, publicGatewayBase, gatewayServerID)
			}
			if s, ok := out[k].(string); ok && session != nil {
				switch strings.ToLower(k) {
				case "accesstoken":
					if session.BackendToken != "" && s == session.BackendToken {
						out[k] = gatewayToken
					}
				case "serverid":
					if s == session.BackendServerID || s == "" {
						out[k] = gatewayServerID
					}
				case "userid":
					if session.BackendUserID != "" && s == session.BackendUserID {
						out[k] = session.SyntheticUserID
					}
				case "id":
					if session.BackendUserID != "" && s == session.BackendUserID {
						out[k] = session.SyntheticUserID
					} else if s == session.BackendServerID {
						out[k] = gatewayServerID
					}
				}
			}
		}
		if publicGatewayBase != "" {
			if _, ok := out["LocalAddress"]; ok {
				out["LocalAddress"] = publicGatewayBase
			}
			if _, ok := out["WanAddress"]; ok {
				out["WanAddress"] = publicGatewayBase
			}
			if _, ok := out["RemoteAddresses"]; ok {
				out["RemoteAddresses"] = []string{publicGatewayBase}
			}
			if _, ok := out["LocalAddresses"]; ok {
				out["LocalAddresses"] = []string{publicGatewayBase}
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = rewriteJSONValue(item, session, gatewayToken, publicGatewayBase, gatewayServerID)
		}
		return out
	case string:
		if session == nil {
			return x
		}
		return rewriteString(x, session, gatewayToken, publicGatewayBase, gatewayServerID)
	default:
		return v
	}
}

func rewriteBytes(data []byte, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) []byte {
	if session == nil {
		return data
	}
	return []byte(rewriteString(string(data), session, gatewayToken, publicGatewayBase, gatewayServerID))
}

func rewriteM3U8(data []byte, rel string, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) []byte {
	if session == nil {
		return data
	}
	return rewriteM3U8MediaReferences(data, rel, session, gatewayToken, publicGatewayBase, gatewayServerID)
}

func rewriteString(s string, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) string {
	if session.BackendToken != "" {
		s = strings.ReplaceAll(s, session.BackendToken, gatewayToken)
	}
	if session.BackendUserID != "" {
		s = strings.ReplaceAll(s, session.BackendUserID, session.SyntheticUserID)
	}
	if session.BackendServerID != "" {
		s = strings.ReplaceAll(s, session.BackendServerID, gatewayServerID)
	}
	return s
}

func copyResponseHeaders(dst, src http.Header, rel string, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) {
	for k, vals := range src {
		if isHopHeader(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, val := range vals {
			if strings.EqualFold(k, "Set-Cookie") && strings.HasPrefix(strings.TrimSpace(val), resourceCookieName+"=") {
				continue
			}
			if strings.EqualFold(k, "Location") || strings.EqualFold(k, "Content-Location") {
				val = rewriteResponseLocation(val, rel, session, gatewayToken, publicGatewayBase, gatewayServerID)
			} else {
				val = rewriteString(val, session, gatewayToken, publicGatewayBase, gatewayServerID)
			}
			dst.Add(k, val)
		}
	}
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vals := range src {
		if isHopHeader(k) || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		for _, val := range vals {
			dst.Add(k, val)
		}
	}
	stripResourceCookie(dst)
}

func removeHopHeaders(h http.Header) {
	for _, key := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(key)
	}
}

func isHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func isJSONContentType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func looksLikeJSON(data []byte) bool {
	data = bytes.TrimSpace(data)
	return len(data) > 0 && (data[0] == '{' || data[0] == '[')
}

func isM3U8ContentType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "application/vnd.apple.mpegurl" || mt == "application/x-mpegurl" || mt == "audio/mpegurl"
}

func isStreamingContentType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return strings.HasPrefix(mt, "video/") || strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "image/") || mt == "application/octet-stream"
}

func isImageContentType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return strings.HasPrefix(mt, "image/")
}

func isUpgradeRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && headerHasToken(r.Header, "Connection", "upgrade")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, value := range h.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func isRewriteableContentType(ct string) bool {
	if ct == "" {
		return true
	}
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "application/json" || strings.HasSuffix(mt, "+json") || strings.HasPrefix(mt, "text/") || mt == "application/x-www-form-urlencoded"
}

func backendURL(base, rel string) (string, error) {
	if strings.TrimSpace(base) == "" {
		return "", errors.New("empty backend base url")
	}
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(baseURL.Path, "/")
	baseURL.Path = path.Join(basePath, "/"+strings.TrimLeft(rel, "/"))
	return baseURL.String(), nil
}

func (s *Server) relativePath(requestPath string) (string, bool) {
	base := strings.TrimRight(s.cfg.GatewayBasePath, "/")
	if requestPath == base {
		return "/", true
	}
	if !strings.HasPrefix(requestPath, base+"/") {
		return "", false
	}
	return "/" + strings.TrimPrefix(requestPath, base+"/"), true
}

func (s *Server) publicGatewayBase() string {
	base := strings.TrimRight(s.cfg.PublicBaseURL, "/")
	if base == "" {
		return strings.TrimRight(s.cfg.GatewayBasePath, "/")
	}
	pathPart := strings.TrimRight(s.cfg.GatewayBasePath, "/")
	if strings.HasSuffix(base, pathPart) {
		return base
	}
	return base + pathPart
}

func (s *Server) gatewayBaseForRequest(r *http.Request) string {
	if strings.TrimSpace(s.cfg.PublicBaseURL) != "" {
		return s.publicGatewayBase()
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return s.publicGatewayBase()
	}
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return strings.TrimRight(scheme+"://"+host, "/") + strings.TrimRight(s.cfg.GatewayBasePath, "/")
}

func firstAuthHeader(r *http.Request) AuthHeader {
	for _, name := range []string{"X-Emby-Authorization", "Authorization"} {
		if value := r.Header.Get(name); value != "" {
			return ParseEmbyAuthHeader(value)
		}
	}
	return AuthHeader{Scheme: "Emby", Fields: map[string]string{}}
}

func userDTO(user GatewayUser, serverID string) map[string]any {
	return map[string]any{
		"Name":                  user.Username,
		"ServerId":              serverID,
		"ServerName":            "Emby Gateway",
		"Id":                    user.SyntheticUserID,
		"HasPassword":           true,
		"HasConfiguredPassword": true,
		"EnableAutoLogin":       false,
	}
}

func authenticationResultDTO(user GatewayUser, session *Session, token, serverID string) map[string]any {
	userObj := privateUserDTO(user.Username, user.SyntheticUserID, serverID)
	sessionInfo := map[string]any{
		"ServerId":           serverID,
		"UserId":             user.SyntheticUserID,
		"UserName":           user.Username,
		"Client":             session.Client,
		"DeviceName":         session.Device,
		"DeviceId":           session.DeviceID,
		"ApplicationVersion": session.Version,
		"SupportedCommands":  []any{},
		"PlayableMediaTypes": []any{},
		"AdditionalUsers":    []any{},
	}
	return map[string]any{
		"AccessToken": token,
		"ServerId":    serverID,
		"User":        userObj,
		"SessionInfo": sessionInfo,
	}
}

func privateUserDTO(username, syntheticID, serverID string) map[string]any {
	userObj := userDTO(GatewayUser{Username: username, SyntheticUserID: syntheticID}, serverID)
	userObj["Configuration"] = map[string]any{
		"PlayDefaultAudioTrack":      true,
		"SubtitleMode":               "Smart",
		"RememberAudioSelections":    true,
		"RememberSubtitleSelections": true,
		"EnableNextEpisodeAutoPlay":  true,
		"HidePlayedInLatest":         true,
		"HidePlayedInMoreLikeThis":   false,
		"HidePlayedInSuggestions":    false,
		"EnableLocalPassword":        false,
		"DisplayMissingEpisodes":     false,
		"ResumeRewindSeconds":        0,
		"OrderedViews":               []any{},
		"LatestItemsExcludes":        []any{},
		"MyMediaExcludes":            []any{},
	}
	userObj["Policy"] = map[string]any{
		"IsAdministrator":                  false,
		"IsHidden":                         false,
		"IsDisabled":                       false,
		"EnableUserPreferenceAccess":       true,
		"EnableRemoteControlOfOtherUsers":  false,
		"EnableSharedDeviceControl":        false,
		"EnableRemoteAccess":               true,
		"EnableMediaPlayback":              true,
		"EnableAudioPlaybackTranscoding":   true,
		"EnableVideoPlaybackTranscoding":   true,
		"EnablePlaybackRemuxing":           true,
		"EnableContentDownloading":         true,
		"EnableLiveTvAccess":               false,
		"EnableLiveTvManagement":           false,
		"EnableUserCreatedContent":         false,
		"EnableCollectionManagement":       false,
		"EnableSubtitleManagement":         false,
		"EnableContentDeletion":            false,
		"EnablePublicSharing":              false,
		"EnableContentDeletionFromFolders": []any{},
		"RestrictedFeatures":               []any{},
		"EnableMediaConversion":            false,
		"EnableAllChannels":                true,
		"EnableAllFolders":                 true,
		"EnableAllDevices":                 true,
		"BlockedTags":                      []any{},
		"AccessSchedules":                  []any{},
		"BlockUnratedItems":                []any{},
		"EnabledChannels":                  []any{},
		"EnabledFolders":                   []any{},
		"EnabledDevices":                   []any{},
	}
	return userObj
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func equalPath(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

func stringField(obj map[string]any, name string) (string, bool) {
	value, ok := field(obj, name)
	if !ok {
		return "", false
	}
	s, ok := value.(string)
	return s, ok && s != ""
}

func mapField(obj map[string]any, name string) (map[string]any, bool) {
	value, ok := field(obj, name)
	if !ok {
		return nil, false
	}
	m, ok := value.(map[string]any)
	return m, ok
}

func boolField(obj map[string]any, name string) (bool, bool) {
	value, ok := field(obj, name)
	if !ok {
		return false, false
	}
	b, ok := value.(bool)
	return b, ok
}

func int64Field(obj map[string]any, name string) (int64, bool) {
	value, ok := field(obj, name)
	if !ok {
		return 0, false
	}
	switch x := value.(type) {
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func float64Field(obj map[string]any, name string) (float64, bool) {
	value, ok := field(obj, name)
	if !ok {
		return 0, false
	}
	switch x := value.(type) {
	case json.Number:
		n, err := x.Float64()
		return n, err == nil
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func field(obj map[string]any, name string) (any, bool) {
	for key, value := range obj {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return nil, false
}

func isSingleUserPath(rel string) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	return len(parts) == 2 && strings.EqualFold(parts[0], "Users") && !strings.EqualFold(parts[1], "Public")
}

func remoteIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func contentLength(r io.Reader) int64 {
	if sr, ok := r.(*strings.Reader); ok {
		return int64(sr.Len())
	}
	if br, ok := r.(*bytes.Reader); ok {
		return int64(br.Len())
	}
	return -1
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return data, nil
}

func defaultProxyTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       proxyIdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: proxyResponseHeaderTimeout,
	}
}

type loginFailureLimiter struct {
	mu      sync.Mutex
	entries map[string]loginFailureEntry
}

type loginFailureEntry struct {
	count       int
	blockedTill time.Time
}

func newLoginFailureLimiter() *loginFailureLimiter {
	return &loginFailureLimiter{entries: map[string]loginFailureEntry{}}
}

func (l *loginFailureLimiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[key]
	if !ok || entry.blockedTill.IsZero() {
		return false
	}
	if now.Before(entry.blockedTill) {
		return true
	}
	delete(l.entries, key)
	return false
}

func (l *loginFailureLimiter) recordFailure(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	entry.count++
	if entry.count >= loginFailureLimit {
		entry.blockedTill = now.Add(loginFailureBlockDuration)
	}
	l.entries[key] = entry
}

func (l *loginFailureLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

func loginFailureKey(username string, r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(username)) + "\x00" + remoteIP(r)
}

type nilResponseWriter struct{}

func (nilResponseWriter) Header() http.Header       { return http.Header{} }
func (nilResponseWriter) Write([]byte) (int, error) { return 0, nil }
func (nilResponseWriter) WriteHeader(int)           {}
