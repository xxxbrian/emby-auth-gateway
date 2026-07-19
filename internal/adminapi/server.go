// Package adminapi implements the /admin/api/v1 control plane HTTP API.
package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminquery"
	"github.com/xxxbrian/emby-auth-gateway/internal/controlplane"
	"github.com/xxxbrian/emby-auth-gateway/internal/pathpolicy"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
	"github.com/xxxbrian/emby-auth-gateway/internal/version"
)

const (
	ctxSessionKey = "adminSession"
	apiPrefix     = "/admin/api/v1"
)

// Config configures the admin API.
type Config struct {
	App core.App
	// Origin is deprecated/unused for CSRF. Writes use same-origin checks against
	// the current request (Origin header vs scheme://Host, or Sec-Fetch-Site).
	Origin              string
	Sessions            *adminauth.Store
	Query               *adminquery.Querier
	Telemetry           *telemetry.Registry // optional
	MediaBufferSnapshot func() telemetry.MediaBufferStatus
	// AcquireReconfigure, when set, is the preferred reconfigure exclusion gate
	// (holds exclusive lock over media copies for the duration of reconfigure).
	// force=false fails immediately if media is active; force=true waits.
	AcquireReconfigure func(force bool) (release func(), err error)
	// ActiveMediaLoad is the fallback reconfigure guard when AcquireReconfigure is nil
	// (sync copies + playbacks). When both are nil, falls back to Telemetry.HasActiveMediaLoad().
	ActiveMediaLoad func() bool
	StartedAt       time.Time
	BootID          string
	LoginLimit      *adminauth.RateLimiter
	APILimit        *adminauth.RateLimiter
}

// Server is the admin API handler set.
type Server struct {
	cfg Config
}

// New creates an admin API server.
func New(cfg Config) (*Server, error) {
	if cfg.App == nil {
		return nil, errors.New("adminapi: app is required")
	}
	if cfg.Sessions == nil {
		cfg.Sessions = adminauth.NewStore(adminauth.DefaultMaxSessions)
	}
	if cfg.Query == nil {
		cfg.Query = adminquery.New(cfg.App, adminquery.DefaultConcurrency)
	}
	if cfg.LoginLimit == nil {
		cfg.LoginLimit = adminauth.NewRateLimiter(20, time.Minute)
	}
	if cfg.APILimit == nil {
		cfg.APILimit = adminauth.NewRateLimiter(120, time.Minute)
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = time.Now().UTC()
	}
	// Origin is ignored for CSRF (same-origin only); keep trimmed if set for diagnostics.
	cfg.Origin = strings.TrimRight(strings.TrimSpace(cfg.Origin), "/")
	return &Server{cfg: cfg}, nil
}

// Mount registers routes under /admin/api/v1 on the PocketBase router.
func (s *Server) Mount(r *router.Router[*core.RequestEvent]) {
	if r == nil || s == nil {
		return
	}
	g := r.Group(apiPrefix)
	g.BindFunc(s.securityHeaders)
	// Unbind global CORS so we never emit Access-Control-Allow-Origin: *.
	g.Unbind(apis.DefaultCorsMiddlewareId)

	// Session create is unauthenticated (exchanges PB JWT).
	g.POST("/session", s.handleSessionCreate)
	g.POST("/session/logout", s.handleSessionLogout)
	g.GET("/session", s.withAuth(s.handleSessionGet))
	g.POST("/session/reauth", s.withAuthWrite(s.handleSessionReauth))

	g.GET("/overview", s.withAuth(s.handleOverview))
	g.GET("/metrics/stream", s.withAuth(s.handleMetricsStream))
	g.GET("/users", s.withAuth(s.handleListUsers))
	g.GET("/users/{id}", s.withAuth(s.handleGetUser))
	g.GET("/sessions", s.withAuth(s.handleListSessions))
	g.GET("/activity/playbacks", s.withAuth(s.handlePlaybacks))
	g.GET("/activity/transfers", s.withAuth(s.handleTransfers))
	g.GET("/audit", s.withAuth(s.handleAudit))
	g.GET("/system", s.withAuth(s.handleSystem))
	g.GET("/path-policies", s.withAuth(s.handleListPolicies))
	g.GET("/path-policies/preview", s.withAuth(s.handlePreviewPolicy))
	g.GET("/upstream", s.withAuth(s.handleGetUpstream))

	g.POST("/users", s.withAuthWrite(s.handleCreateUser))
	g.POST("/users/{id}/enable", s.withAuthWrite(s.handleEnableUser))
	g.POST("/users/{id}/disable", s.withAuthWrite(s.handleDisableUser))
	g.POST("/users/{id}/password", s.withAuthWrite(s.handleResetPassword))
	g.POST("/users/{id}/sessions/revoke-all", s.withAuthWrite(s.handleRevokeUserSessions))
	g.POST("/sessions/{id}/revoke", s.withAuthWrite(s.handleRevokeSession))
	g.POST("/path-policies", s.withAuthWrite(s.handleCreatePolicy))
	g.PUT("/path-policies/{id}", s.withAuthWrite(s.handleUpdatePolicy))
	g.DELETE("/path-policies/{id}", s.withAuthWrite(s.handleDeletePolicy))
	g.POST("/path-policies/install-defaults", s.withAuthWrite(s.handleInstallDefaults))
	g.POST("/upstream/probe", s.withAuthWrite(s.handleUpstreamProbe))
	g.POST("/upstream/reconfigure", s.withAuthWrite(s.handleUpstreamReconfigure))
}

func (s *Server) securityHeaders(e *core.RequestEvent) error {
	h := e.Response.Header()
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	// Explicitly avoid CORS *.
	h.Del("Access-Control-Allow-Origin")
	h.Del("Access-Control-Allow-Credentials")
	return e.Next()
}

func (s *Server) withAuth(next func(*core.RequestEvent) error) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		sess, err := s.loadSession(e)
		if err != nil {
			return err
		}
		if !s.cfg.APILimit.Allow("sess:" + sess.ID) {
			return e.TooManyRequestsError("rate limit exceeded", nil)
		}
		if _, err := s.cfg.Sessions.Touch(sess.ID); err != nil {
			return e.UnauthorizedError("session expired", nil)
		}
		e.Set(ctxSessionKey, sess)
		return next(e)
	}
}

func (s *Server) withAuthWrite(next func(*core.RequestEvent) error) func(*core.RequestEvent) error {
	return s.withAuth(func(e *core.RequestEvent) error {
		if err := s.requireOrigin(e); err != nil {
			return err
		}
		sess := sessionFromEvent(e)
		if sess == nil {
			return e.UnauthorizedError("not authenticated", nil)
		}
		csrf := e.Request.Header.Get(adminauth.CSRFHeader)
		if err := s.cfg.Sessions.ValidateCSRF(sess.ID, csrf); err != nil {
			return e.ForbiddenError("csrf validation failed", nil)
		}
		return next(e)
	})
}

func (s *Server) loadSession(e *core.RequestEvent) (*adminauth.Session, error) {
	id := adminauth.ReadSessionID(e.Request)
	if id == "" {
		return nil, e.UnauthorizedError("missing admin session", nil)
	}
	sess, err := s.cfg.Sessions.Get(id)
	if err != nil {
		return nil, e.UnauthorizedError("invalid admin session", nil)
	}
	// Validate PB JWT still belongs to a superuser.
	record, err := e.App.FindAuthRecordByToken(sess.Token, core.TokenTypeAuth)
	if err != nil || record == nil || !record.IsSuperuser() {
		s.cfg.Sessions.Delete(id)
		return nil, e.UnauthorizedError("superuser token invalid", nil)
	}
	// Inject Authorization for any downstream PB middleware that may run.
	if e.Request.Header.Get("Authorization") == "" {
		e.Request.Header.Set("Authorization", "Bearer "+sess.Token)
	}
	if e.Auth == nil {
		e.Auth = record
	}
	return sess, nil
}

func sessionFromEvent(e *core.RequestEvent) *adminauth.Session {
	v := e.Get(ctxSessionKey)
	if v == nil {
		return nil
	}
	sess, _ := v.(*adminauth.Session)
	return sess
}

// requireOrigin enforces same-origin CSRF for writes and session create:
// Origin must equal the current request origin (scheme://Host), or when Origin
// is absent Sec-Fetch-Site must be same-origin.
func (s *Server) requireOrigin(e *core.RequestEvent) error {
	if e == nil || e.Request == nil {
		return e.ForbiddenError("origin required", nil)
	}
	req := e.Request
	origin := strings.TrimRight(strings.TrimSpace(req.Header.Get("Origin")), "/")
	if origin == "" {
		if site := req.Header.Get("Sec-Fetch-Site"); site == "same-origin" {
			return nil
		}
		return e.ForbiddenError("origin required", nil)
	}
	expected := requestOrigin(req)
	if expected == "" || origin != expected {
		return e.ForbiddenError("origin mismatch", nil)
	}
	return nil
}

// requestOrigin returns scheme://Host for the inbound request.
// Scheme is https when TLS is present, else X-Forwarded-Proto when http/https, else http.
func requestOrigin(r *http.Request) string {
	if r == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if p := firstForwardedProto(r.Header.Get("X-Forwarded-Proto")); p == "http" || p == "https" {
		scheme = p
	}
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func firstForwardedProto(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if i := strings.IndexByte(raw, ','); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

// --- session handlers ---

type sessionCreateBody struct {
	Token string `json:"token"`
}

func (s *Server) handleSessionCreate(e *core.RequestEvent) error {
	if err := s.requireOrigin(e); err != nil {
		return err
	}
	ip := e.RealIP()
	if !s.cfg.LoginLimit.Allow("ip:" + ip) {
		return e.TooManyRequestsError("login rate limit exceeded", nil)
	}
	var body sessionCreateBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		return e.BadRequestError("token is required", nil)
	}
	record, err := e.App.FindAuthRecordByToken(token, core.TokenTypeAuth)
	if err != nil || record == nil || !record.IsSuperuser() {
		return e.UnauthorizedError("invalid superuser token", nil)
	}
	sess, err := s.cfg.Sessions.Create(token, adminauth.Claims{
		SuperuserID: record.Id,
		Email:       record.Email(),
	})
	if err != nil {
		if errors.Is(err, adminauth.ErrSessionFull) {
			return e.TooManyRequestsError("admin session capacity full", nil)
		}
		return e.InternalServerError("failed to create session", err)
	}
	adminauth.SetSessionCookie(e.Response, e.Request, sess.ID, int(adminauth.AbsoluteTTL.Seconds()))
	return e.JSON(http.StatusOK, map[string]any{
		"csrf":         sess.CSRF,
		"superuser_id": sess.SuperuserID,
		"email":        sess.Email,
		"created":      sess.Created,
		"expires_at":   sess.PublicView().ExpiresAt,
	})
}

func (s *Server) handleSessionGet(e *core.RequestEvent) error {
	sess := sessionFromEvent(e)
	if sess == nil {
		return e.UnauthorizedError("not authenticated", nil)
	}
	return e.JSON(http.StatusOK, sess.PublicView())
}

func (s *Server) handleSessionLogout(e *core.RequestEvent) error {
	// Logout is best-effort: clear cookie even without valid session.
	id := adminauth.ReadSessionID(e.Request)
	if id != "" {
		s.cfg.Sessions.Delete(id)
	}
	adminauth.ClearSessionCookie(e.Response, e.Request)
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessionReauth(e *core.RequestEvent) error {
	sess := sessionFromEvent(e)
	var body sessionCreateBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		return e.BadRequestError("token is required", nil)
	}
	record, err := e.App.FindAuthRecordByToken(token, core.TokenTypeAuth)
	if err != nil || record == nil || !record.IsSuperuser() {
		return e.UnauthorizedError("invalid superuser token", nil)
	}
	if record.Id != sess.SuperuserID {
		return e.ForbiddenError("token does not match session superuser", nil)
	}
	// Refresh stored JWT.
	if stored, err := s.cfg.Sessions.Get(sess.ID); err == nil {
		// recreate with new token keeping same session is not supported; update via delete+create would change cookie.
		// Store new token by re-creating is heavy; mutate via Delete+Create and new cookie.
		_ = stored
	}
	// Issue reauth ticket bound to current session; also refresh JWT in store by recreating session.
	// Simpler path: issue ticket only; JWT refresh is optional for reconfigure (uses existing session JWT).
	// But reauth body provides a fresh token — update by delete+create.
	s.cfg.Sessions.Delete(sess.ID)
	newSess, err := s.cfg.Sessions.Create(token, adminauth.Claims{
		SuperuserID: record.Id,
		Email:       record.Email(),
	})
	if err != nil {
		return e.InternalServerError("failed to refresh session", err)
	}
	adminauth.SetSessionCookie(e.Response, e.Request, newSess.ID, int(adminauth.AbsoluteTTL.Seconds()))
	ticket, exp, err := s.cfg.Sessions.IssueReauth(newSess.ID)
	if err != nil {
		return e.InternalServerError("failed to issue reauth ticket", err)
	}
	return e.JSON(http.StatusOK, map[string]any{
		"reauth_ticket": ticket,
		"expires_at":    exp,
		"csrf":          newSess.CSRF,
		"superuser_id":  newSess.SuperuserID,
		"email":         newSess.Email,
	})
}

// --- read handlers ---

func (s *Server) handleOverview(e *core.RequestEvent) error {
	window := telemetry.ParseSeriesWindow(e.Request.URL.Query().Get("window"))
	return e.JSON(http.StatusOK, s.snapshot(window))
}

func (s *Server) snapshot(window telemetry.SeriesWindow) telemetry.Snapshot {
	var snap telemetry.Snapshot
	if s != nil && s.cfg.Telemetry != nil {
		snap = s.cfg.Telemetry.SnapshotWindow(window)
	}
	if s != nil && s.cfg.MediaBufferSnapshot != nil {
		snap.MediaBuffer = s.cfg.MediaBufferSnapshot()
	}
	return snap
}

func (s *Server) handleMetricsStream(e *core.RequestEvent) error {
	w := e.Response
	r := e.Request
	window := telemetry.ParseSeriesWindow(r.URL.Query().Get("window"))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	writeSnap := func() bool {
		b, err := json.Marshal(s.snapshot(window))
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !writeSnap() {
		return nil
	}
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-ticker.C:
			if !writeSnap() {
				return nil
			}
		}
	}
}

func (s *Server) handleListUsers(e *core.RequestEvent) error {
	users, err := s.cfg.Query.ListUsers(e.Request.Context())
	if err != nil {
		return e.InternalServerError("list users failed", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"items": users})
}

func (s *Server) handleGetUser(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	user, err := s.cfg.Query.GetUser(e.Request.Context(), id)
	if err != nil {
		return e.NotFoundError("user not found", err)
	}
	return e.JSON(http.StatusOK, user)
}

func (s *Server) handleListSessions(e *core.RequestEvent) error {
	userID := e.Request.URL.Query().Get("user_id")
	items, err := s.cfg.Query.ListSessions(e.Request.Context(), userID)
	if err != nil {
		return e.InternalServerError("list sessions failed", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handlePlaybacks(e *core.RequestEvent) error {
	var items []telemetry.Playback
	if s.cfg.Telemetry != nil {
		items = s.cfg.Telemetry.ActivePlaybacks()
	}
	if items == nil {
		items = []telemetry.Playback{}
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleTransfers(e *core.RequestEvent) error {
	var items []telemetry.Transfer
	if s.cfg.Telemetry != nil {
		items = s.cfg.Telemetry.ActiveTransfers()
	}
	if items == nil {
		items = []telemetry.Transfer{}
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleAudit(e *core.RequestEvent) error {
	q := e.Request.URL.Query()
	now := time.Now().UTC()
	from, to := now.Add(-time.Hour), now
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return e.BadRequestError("invalid from", err)
		}
		from = t.UTC()
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return e.BadRequestError("invalid to", err)
		}
		to = t.UTC()
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return e.BadRequestError("invalid limit", err)
		}
		limit = n
	}
	items, err := s.cfg.Query.ListAudit(e.Request.Context(), from, to, limit, q.Get("cursor"))
	if err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleSystem(e *core.RequestEvent) error {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	uptime := int64(time.Since(s.cfg.StartedAt).Seconds())
	if uptime < 0 {
		uptime = 0
	}
	return e.JSON(http.StatusOK, map[string]any{
		"version":    version.Version,
		"boot_id":    s.cfg.BootID,
		"started_at": s.cfg.StartedAt.UTC(),
		"uptime_sec": uptime,
		"goroutines": runtime.NumGoroutine(),
		"heap_bytes": ms.HeapAlloc,
		"go_version": runtime.Version(),
	})
}

func (s *Server) handleListPolicies(e *core.RequestEvent) error {
	items, err := s.cfg.Query.ListPolicies(e.Request.Context())
	if err != nil {
		return e.InternalServerError("list policies failed", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handlePreviewPolicy(e *core.RequestEvent) error {
	method := e.Request.URL.Query().Get("method")
	path := e.Request.URL.Query().Get("path")
	if method == "" || path == "" {
		return e.BadRequestError("method and path are required", nil)
	}
	dec, err := controlplane.PreviewPolicy(e.Request.Context(), e.App, method, path)
	if err != nil {
		return e.InternalServerError("preview failed", err)
	}
	return e.JSON(http.StatusOK, dec)
}

func (s *Server) handleGetUpstream(e *core.RequestEvent) error {
	up, err := s.cfg.Query.GetUpstream(e.Request.Context())
	if err != nil {
		return e.InternalServerError("get upstream failed", err)
	}
	return e.JSON(http.StatusOK, up)
}

// --- audit ---

// auditAdmin records a successful admin mutation into audit_logs.
// Message must be a short non-secret summary (no passwords/tokens).
// Returns an error when the audit row cannot be written; callers may still
// succeed the mutation but should surface the failure (log / audit_warning).
func (s *Server) auditAdmin(e *core.RequestEvent, event, message string) error {
	if e == nil || e.App == nil || e.Request == nil {
		return fmt.Errorf("audit context unavailable")
	}
	col, err := e.App.FindCollectionByNameOrId("audit_logs")
	if err != nil {
		logAuditFailure(e, event, err)
		return err
	}
	rec := core.NewRecord(col)
	rec.Set("event", event)
	rec.Set("message", message)
	rec.Set("remote_ip", e.RealIP())
	rec.Set("method", e.Request.Method)
	rec.Set("path", e.Request.URL.Path)
	rec.Set("status", http.StatusOK)
	if err := e.App.Save(rec); err != nil {
		logAuditFailure(e, event, err)
		return err
	}
	return nil
}

func logAuditFailure(e *core.RequestEvent, event string, err error) {
	if e == nil || e.App == nil || err == nil {
		return
	}
	e.App.Logger().Error("admin audit log save failed", "event", event, "error", err)
}

func actorSummary(e *core.RequestEvent) string {
	sess := sessionFromEvent(e)
	if sess == nil {
		return "unknown"
	}
	email := strings.TrimSpace(sess.Email)
	id := strings.TrimSpace(sess.SuperuserID)
	switch {
	case email != "" && id != "":
		return email + " (" + id + ")"
	case email != "":
		return email
	case id != "":
		return id
	default:
		return "unknown"
	}
}

// --- write handlers ---

type createUserBody struct {
	Username        string `json:"username"`
	Password        string `json:"password"`
	SyntheticUserID string `json:"synthetic_user_id"`
}

func (s *Server) handleCreateUser(e *core.RequestEvent) error {
	var body createUserBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	if err := controlplane.CreateUser(e.Request.Context(), e.App, controlplane.UpsertUserInput{
		Username:        body.Username,
		Password:        body.Password,
		SyntheticUserID: body.SyntheticUserID,
	}); err != nil {
		if errors.Is(err, controlplane.ErrUserExists) {
			return e.JSON(http.StatusConflict, map[string]any{
				"error":   "user_exists",
				"message": "user already exists",
			})
		}
		return e.BadRequestError(err.Error(), err)
	}
	// Always return UserDTO for the created user (never bare {ok:true}).
	rec, err := e.App.FindFirstRecordByData("users", "username", strings.TrimSpace(body.Username))
	if err != nil {
		return e.InternalServerError("created user lookup failed", err)
	}
	user, err := s.cfg.Query.GetUser(e.Request.Context(), rec.Id)
	if err != nil {
		return e.InternalServerError("created user lookup failed", err)
	}
	_ = s.auditAdmin(e, "admin_user_create", fmt.Sprintf("actor=%s created user username=%s id=%s", actorSummary(e), user.Username, user.ID))
	return e.JSON(http.StatusOK, user)
}

func (s *Server) handleEnableUser(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	if err := controlplane.SetUserEnabled(e.Request.Context(), e.App, id, true); err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_user_enable", fmt.Sprintf("actor=%s enabled user id=%s", actorSummary(e), id))
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDisableUser(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	if err := controlplane.SetUserEnabled(e.Request.Context(), e.App, id, false); err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_user_disable", fmt.Sprintf("actor=%s disabled user id=%s", actorSummary(e), id))
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

type passwordBody struct {
	Password string `json:"password"`
}

func (s *Server) handleResetPassword(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	var body passwordBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	if err := controlplane.ResetUserPassword(e.Request.Context(), e.App, id, body.Password); err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_user_password", fmt.Sprintf("actor=%s reset password for user id=%s", actorSummary(e), id))
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRevokeUserSessions(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	n, err := controlplane.RevokeUserSessions(e.Request.Context(), e.App, id)
	if err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_session_revoke", fmt.Sprintf("actor=%s revoked all sessions for user id=%s count=%d", actorSummary(e), id, n))
	return e.JSON(http.StatusOK, map[string]any{"revoked": n})
}

func (s *Server) handleRevokeSession(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	if err := controlplane.RevokeSessionByID(e.Request.Context(), e.App, id); err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_session_revoke", fmt.Sprintf("actor=%s revoked session id=%s", actorSummary(e), id))
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

type policyBody struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
	Priority int    `json:"priority"`
	Enabled  *bool  `json:"enabled"`
	// Updated is an optional optimistic concurrency token (RFC3339 from list).
	Updated string `json:"updated"`
}

func (s *Server) handleCreatePolicy(e *core.RequestEvent) error {
	var body policyBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	p, err := controlplane.UpsertPolicy(e.Request.Context(), e.App, pathpolicy.Policy{
		Method:   body.Method,
		Path:     body.Path,
		Action:   body.Action,
		Reason:   body.Reason,
		Priority: body.Priority,
		Enabled:  enabled,
	})
	if err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_policy_create", fmt.Sprintf("actor=%s created policy id=%s method=%s path=%s action=%s", actorSummary(e), p.ID, p.Method, p.Path, p.Action))
	return e.JSON(http.StatusOK, p)
}

func (s *Server) handleUpdatePolicy(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	var body policyBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	var expectedUpdated time.Time
	if raw := strings.TrimSpace(body.Updated); raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			return e.BadRequestError("updated must be RFC3339", err)
		}
		expectedUpdated = t.UTC()
	}
	p, err := controlplane.UpsertPolicy(e.Request.Context(), e.App, pathpolicy.Policy{
		ID:       id,
		Method:   body.Method,
		Path:     body.Path,
		Action:   body.Action,
		Reason:   body.Reason,
		Priority: body.Priority,
		Enabled:  enabled,
		Updated:  expectedUpdated,
	})
	if err != nil {
		if errors.Is(err, controlplane.ErrPolicyConflict) {
			return e.JSON(http.StatusConflict, map[string]any{
				"error":   "policy_conflict",
				"message": "policy was modified; reload and retry",
			})
		}
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_policy_update", fmt.Sprintf("actor=%s updated policy id=%s method=%s path=%s action=%s", actorSummary(e), p.ID, p.Method, p.Path, p.Action))
	return e.JSON(http.StatusOK, p)
}

func (s *Server) handleDeletePolicy(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	if err := controlplane.DeletePolicy(e.Request.Context(), e.App, id); err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_policy_delete", fmt.Sprintf("actor=%s deleted policy id=%s", actorSummary(e), id))
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleInstallDefaults(e *core.RequestEvent) error {
	created, preserved, err := controlplane.InstallDefaultPolicies(e.App)
	if err != nil {
		return e.InternalServerError("install defaults failed", err)
	}
	_ = s.auditAdmin(e, "admin_policy_install_defaults", fmt.Sprintf("actor=%s installed default policies created=%d preserved=%d", actorSummary(e), created, preserved))
	return e.JSON(http.StatusOK, map[string]any{"created": created, "preserved": preserved})
}

type upstreamBody struct {
	EmbyBaseURL                 string `json:"emby_base_url"`
	BackendUsername             string `json:"backend_username"`
	BackendPassword             string `json:"backend_password"`
	BackendUserAgent            string `json:"backend_user_agent"`
	BackendAuthorizationClient  string `json:"backend_authorization_client"`
	BackendAuthorizationDevice  string `json:"backend_authorization_device"`
	BackendAuthorizationVersion string `json:"backend_authorization_version"`
	Force                       bool   `json:"force"`
}

func (s *Server) handleUpstreamProbe(e *core.RequestEvent) error {
	var body upstreamBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	serverID, name, ver, backendUserID, latency, err := controlplane.ProbeUpstream(e.Request.Context(), e.App, controlplane.UpstreamReconfigureInput{
		EmbyBaseURL:                 body.EmbyBaseURL,
		BackendUsername:             body.BackendUsername,
		BackendPassword:             body.BackendPassword,
		BackendUserAgent:            body.BackendUserAgent,
		BackendAuthorizationClient:  body.BackendAuthorizationClient,
		BackendAuthorizationDevice:  body.BackendAuthorizationDevice,
		BackendAuthorizationVersion: body.BackendAuthorizationVersion,
	})
	if err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	_ = s.auditAdmin(e, "admin_upstream_probe", fmt.Sprintf("actor=%s probed upstream server_id=%s latency_ms=%d", actorSummary(e), serverID, latency))
	return e.JSON(http.StatusOK, map[string]any{
		"server_id":       serverID,
		"server_name":     name,
		"server_version":  ver,
		"backend_user_id": backendUserID,
		"latency_ms":      latency,
	})
}

// hasActiveMediaLoad prefers the sync ActiveMediaLoad hook (copies + playbacks).
// Falls back to telemetry when the hook is not wired.
func (s *Server) hasActiveMediaLoad() bool {
	if s == nil {
		return false
	}
	if s.cfg.ActiveMediaLoad != nil {
		return s.cfg.ActiveMediaLoad()
	}
	return s.cfg.Telemetry != nil && s.cfg.Telemetry.HasActiveMediaLoad()
}

func (s *Server) handleUpstreamReconfigure(e *core.RequestEvent) error {
	sess := sessionFromEvent(e)
	ticket := e.Request.Header.Get(adminauth.ReauthHeader)
	if err := s.cfg.Sessions.ConsumeReauth(sess.ID, ticket); err != nil {
		return e.ForbiddenError("valid reauth ticket required", err)
	}
	var body upstreamBody
	if err := e.BindBody(&body); err != nil {
		return e.BadRequestError("invalid body", err)
	}
	if s.cfg.AcquireReconfigure != nil {
		release, err := s.cfg.AcquireReconfigure(body.Force)
		if err != nil {
			return e.JSON(http.StatusConflict, map[string]any{
				"error":   "active_media_load",
				"message": "active playbacks or transfers present; pass force=true to proceed",
			})
		}
		defer release()
	} else if !body.Force && s.hasActiveMediaLoad() {
		return e.JSON(http.StatusConflict, map[string]any{
			"error":   "active_media_load",
			"message": "active playbacks or transfers present; pass force=true to proceed",
		})
	}
	result, err := controlplane.ReconfigureUpstream(e.Request.Context(), e.App, controlplane.UpstreamReconfigureInput{
		EmbyBaseURL:                 body.EmbyBaseURL,
		BackendUsername:             body.BackendUsername,
		BackendPassword:             body.BackendPassword,
		BackendUserAgent:            body.BackendUserAgent,
		BackendAuthorizationClient:  body.BackendAuthorizationClient,
		BackendAuthorizationDevice:  body.BackendAuthorizationDevice,
		BackendAuthorizationVersion: body.BackendAuthorizationVersion,
		AllowCreate:                 false,
		Force:                       body.Force,
	})
	if err != nil {
		return e.BadRequestError(err.Error(), err)
	}
	resp := map[string]any{
		"cleanup_warning": result.CleanupWarning,
	}
	if auditErr := s.auditAdmin(e, "admin_upstream_reconfigure", fmt.Sprintf("actor=%s reconfigured upstream force=%t", actorSummary(e), body.Force)); auditErr != nil {
		// Mutation succeeded; do not fail solely on audit write, but make it visible.
		resp["audit_warning"] = "audit log write failed: " + auditErr.Error()
	}
	up, _ := s.cfg.Query.GetUpstream(e.Request.Context())
	resp["upstream"] = up
	return e.JSON(http.StatusOK, resp)
}
