package gateway

import (
	"bytes"
	"context"
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
	"strings"
	"sync"
	"time"
)

const defaultSessionTTL = 30 * 24 * time.Hour
const gatewayVersion = "0.0.0"

const (
	backendAuthTimeout         = 15 * time.Second
	defaultBackendUserAgent    = "Emby for Android/3.4.20"
	proxyResponseHeaderTimeout = 30 * time.Second
	proxyIdleConnTimeout       = 90 * time.Second
	loginFailureLimit          = 5
	loginFailureBlockDuration  = time.Minute
	proxyJSONLimit             = 20 << 20
	proxyM3U8Limit             = 20 << 20
)

type Server struct {
	cfg         Config
	store       Store
	client      *http.Client
	proxyClient *http.Client
	logins      *loginFailureLimiter
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
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: backendAuthTimeout}
	}
	proxyClient := cfg.HTTPClient
	if proxyClient == nil {
		proxyClient = &http.Client{Transport: defaultProxyTransport()}
	}
	return &Server{cfg: cfg, store: store, client: client, proxyClient: proxyClient, logins: newLoginFailureLimiter()}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.relativePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
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
		s.handleUser(w, r, rel)
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
		http.Error(w, "failed to authenticate", http.StatusUnauthorized)
		return
	}

	password := form.Pw
	if password == "" {
		password = form.Password
	}
	if form.Username == "" || password == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	user, err := s.store.AuthenticateGatewayUser(ctx, form.Username, password)
	if err != nil || user == nil || !user.Enabled {
		s.logins.recordFailure(key, time.Now())
		http.Error(w, "failed to authenticate", http.StatusUnauthorized)
		return
	}

	mapping, err := s.store.FindMappingByGatewayUserID(ctx, user.ID)
	if err != nil || mapping == nil || !mapping.Enabled || !mapping.BackendAccount.Enabled {
		s.logins.recordFailure(key, time.Now())
		http.Error(w, "failed to authenticate", http.StatusUnauthorized)
		return
	}

	auth := firstAuthHeader(r)
	backendResult, err := s.authenticateBackend(ctx, mapping.BackendAccount, auth, r.UserAgent())
	if err != nil {
		http.Error(w, "backend authentication failed", http.StatusBadGateway)
		return
	}

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
		BackendServerID:  backendResult.ServerID,
		BackendBaseURL:   mapping.BackendAccount.BaseURL,
		BackendUserID:    backendResult.UserID,
		BackendUsername:  backendResult.Username,
		BackendToken:     backendResult.AccessToken,
		Client:           auth.Client,
		Device:           auth.Device,
		DeviceID:         auth.DeviceID,
		Version:          auth.Version,
		RemoteIP:         remoteIP(r),
		CreatedAt:        now,
		ExpiresAt:        now.Add(defaultSessionTTL),
	}
	if err := s.store.SaveSession(ctx, session); err != nil {
		http.Error(w, "session save failed", http.StatusInternalServerError)
		return
	}

	rewritten := rewriteJSONValue(backendResult.Raw, session, token, s.gatewayBaseForRequest(r), s.cfg.GatewayServerID)
	if obj, ok := rewritten.(map[string]any); ok {
		obj["AccessToken"] = token
		obj["ServerId"] = s.cfg.GatewayServerID
		if u, ok := obj["User"].(map[string]any); ok {
			u["Id"] = user.SyntheticUserID
			u["Name"] = user.Username
			u["ServerId"] = s.cfg.GatewayServerID
		}
	}

	writeJSON(w, http.StatusOK, rewritten)
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

func (s *Server) authenticateBackend(ctx context.Context, account BackendAccount, auth AuthHeader, userAgent string) (*backendAuthResult, error) {
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
	if strings.TrimSpace(userAgent) == "" {
		userAgent = defaultBackendUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	auth.UserID = ""
	auth.Token = ""
	if auth.Scheme == "" {
		auth.Scheme = "Emby"
	}
	req.Header.Set("X-Emby-Authorization", auth.String())

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
	AccessToken string
	ServerID    string
	UserID      string
	Username    string
	Raw         map[string]any
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, rel string) {
	token := ExtractToken(r)
	session, ok := s.activeSession(w, r, token)
	if !ok {
		return
	}
	_ = s.forwardLogout(r.Context(), r, rel, session, token)
	if err := s.store.RevokeSession(r.Context(), HashToken(token)); err != nil {
		http.Error(w, "session revoke failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) forwardLogout(ctx context.Context, r *http.Request, rel string, session *Session, gatewayToken string) error {
	u, err := s.proxyURL(session, rel, r.URL.RawQuery, gatewayToken)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	copyRequestHeaders(req.Header, r.Header)
	s.rewriteRequestHeaders(req.Header, session, gatewayToken)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
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
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":              s.cfg.GatewayServerID,
		"ServerId":        s.cfg.GatewayServerID,
		"ServerName":      "Emby Gateway",
		"Version":         gatewayVersion,
		"LocalAddress":    base,
		"WanAddress":      base,
		"RemoteAddresses": []string{base},
		"LocalAddresses":  []string{base},
	})
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Emby Server is running"))
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request, rel string) {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	user, err := s.store.FindUserBySyntheticID(r.Context(), parts[1])
	if err != nil || user == nil || !user.Enabled {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, userDTO(*user, s.cfg.GatewayServerID))
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request, rel string) {
	gatewayToken := ExtractToken(r)
	session, ok := s.activeSession(w, r, gatewayToken)
	if !ok {
		return
	}

	proxyURL, err := s.proxyURL(session, rel, r.URL.RawQuery, gatewayToken)
	if err != nil {
		http.Error(w, "bad backend url", http.StatusBadGateway)
		return
	}
	if isUpgradeRequest(r) {
		s.handleUpgradeProxy(w, r, proxyURL, session, gatewayToken)
		return
	}

	body, err := s.rewriteRequestBody(r, session, gatewayToken)
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, proxyURL.String(), body)
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

	resp, err := s.proxyClient.Do(req)
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	s.writeProxyResponse(w, resp, session, gatewayToken, s.gatewayBaseForRequest(r))
}

func (s *Server) handleUpgradeProxy(w http.ResponseWriter, r *http.Request, proxyURL *url.URL, session *Session, gatewayToken string) {
	proxy := &httputil.ReverseProxy{
		Transport: s.proxyClient.Transport,
		Director: func(req *http.Request) {
			req.URL = proxyURL
			req.Host = proxyURL.Host
			s.rewriteRequestHeaders(req.Header, session, gatewayToken)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "backend unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
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

func (s *Server) proxyURL(session *Session, rel, rawQuery, gatewayToken string) (*url.URL, error) {
	backend, err := backendURL(session.BackendBaseURL, rel)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, err
	}
	for key, vals := range q {
		for i, val := range vals {
			if val == gatewayToken {
				vals[i] = session.BackendToken
			}
			if val == session.SyntheticUserID {
				vals[i] = session.BackendUserID
			}
		}
		q[key] = vals
	}
	u.RawQuery = q.Encode()
	u.Path = strings.ReplaceAll(u.Path, session.SyntheticUserID, session.BackendUserID)
	return u, nil
}

func (s *Server) rewriteRequestHeaders(h http.Header, session *Session, gatewayToken string) {
	for _, name := range []string{"X-Emby-Token", "X-MediaBrowser-Token"} {
		if h.Get(name) != "" {
			h.Set(name, session.BackendToken)
		}
	}
	if h.Get("X-Emby-Token") == "" {
		h.Set("X-Emby-Token", session.BackendToken)
	}
	for _, name := range []string{"X-Emby-Authorization", "Authorization"} {
		if v := h.Get(name); v != "" {
			auth := ParseEmbyAuthHeader(v)
			if auth.Token == gatewayToken || auth.Token == "" {
				auth.Token = session.BackendToken
			}
			if auth.UserID == session.SyntheticUserID || auth.UserID == "" {
				auth.UserID = session.BackendUserID
			}
			h.Set(name, auth.String())
		}
	}
}

func (s *Server) rewriteRequestBody(r *http.Request, session *Session, gatewayToken string) (io.Reader, error) {
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil, nil
	}
	if !isRewriteableContentType(r.Header.Get("Content-Type")) {
		return r.Body, nil
	}
	data, err := io.ReadAll(http.MaxBytesReader(nilResponseWriter{}, r.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), gatewayToken, session.BackendToken)
	text = strings.ReplaceAll(text, session.SyntheticUserID, session.BackendUserID)
	return strings.NewReader(text), nil
}

func (s *Server) writeProxyResponse(w http.ResponseWriter, resp *http.Response, session *Session, gatewayToken, publicGatewayBase string) {
	copyResponseHeaders(w.Header(), resp.Header, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID)
	ct := resp.Header.Get("Content-Type")
	if isM3U8ContentType(ct) || strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".m3u8") {
		data, err := readLimited(resp.Body, proxyM3U8Limit)
		if err != nil {
			http.Error(w, "response read failed", http.StatusBadGateway)
			return
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(rewriteM3U8(data, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID))
		return
	}

	if isStreamingContentType(ct) {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	if isJSONContentType(ct) {
		data, err := readLimited(resp.Body, proxyJSONLimit)
		if err != nil {
			http.Error(w, "response read failed", http.StatusBadGateway)
			return
		}
		var value any
		if err := json.Unmarshal(data, &value); err == nil {
			w.Header().Del("Content-Length")
			writeJSON(w, resp.StatusCode, rewriteJSONValue(value, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID))
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(rewriteBytes(data, session, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID))
		return
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func rewriteJSONValue(v any, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = rewriteJSONValue(v, session, gatewayToken, publicGatewayBase, gatewayServerID)
			if s, ok := out[k].(string); ok && session != nil {
				switch strings.ToLower(k) {
				case "accesstoken":
					if s == session.BackendToken {
						out[k] = gatewayToken
					}
				case "serverid":
					if s == session.BackendServerID || s == "" {
						out[k] = gatewayServerID
					}
				case "userid":
					if s == session.BackendUserID {
						out[k] = session.SyntheticUserID
					}
				case "id":
					if s == session.BackendUserID {
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

func rewriteM3U8(data []byte, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) []byte {
	return rewriteBytes(data, session, gatewayToken, publicGatewayBase, gatewayServerID)
}

func rewriteString(s string, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) string {
	s = strings.ReplaceAll(s, session.BackendToken, gatewayToken)
	s = strings.ReplaceAll(s, session.BackendUserID, session.SyntheticUserID)
	if session.BackendServerID != "" {
		s = strings.ReplaceAll(s, session.BackendServerID, gatewayServerID)
	}
	if session.BackendBaseURL != "" && publicGatewayBase != "" {
		s = strings.ReplaceAll(s, strings.TrimRight(session.BackendBaseURL, "/"), strings.TrimRight(publicGatewayBase, "/"))
	}
	return s
}

func copyResponseHeaders(dst, src http.Header, session *Session, gatewayToken, publicGatewayBase, gatewayServerID string) {
	for k, vals := range src {
		if isHopHeader(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, val := range vals {
			dst.Add(k, rewriteString(val, session, gatewayToken, publicGatewayBase, gatewayServerID))
		}
	}
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vals := range src {
		if isHopHeader(k) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, val := range vals {
			dst.Add(k, val)
		}
	}
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

func isM3U8ContentType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return mt == "application/vnd.apple.mpegurl" || mt == "application/x-mpegurl" || mt == "audio/mpegurl"
}

func isStreamingContentType(ct string) bool {
	mt, _, _ := mime.ParseMediaType(ct)
	return strings.HasPrefix(mt, "video/") || strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "image/") || mt == "application/octet-stream"
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func equalPath(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
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
