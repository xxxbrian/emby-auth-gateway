package adminapi

import (
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminmedia"
)

// Metadata and image bursts have independent budgets, while sharing the exact
// same session validation, revocation and expiry as monitoring JSON routes.
func (s *Server) withMediaAuth(next func(*core.RequestEvent) error, image bool) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		sess, err := s.loadSession(e)
		if err != nil {
			return err
		}
		limiter := s.cfg.MediaLimit
		if image {
			limiter = s.cfg.ImageLimit
		}
		if !limiter.Allow("sess:" + sess.ID) {
			return e.TooManyRequestsError("media rate limit exceeded", nil)
		}
		if _, err := s.cfg.Sessions.Touch(sess.ID); err != nil {
			return e.UnauthorizedError("session expired", nil)
		}
		e.Set(ctxSessionKey, sess)
		return next(e)
	}
}

func (s *Server) handleMediaItems(e *core.RequestEvent) error {
	ids, err := adminmedia.ValidateIDs(strings.Split(e.Request.URL.Query().Get("ids"), ","))
	if err != nil {
		return e.BadRequestError("provide 1 to 50 valid media IDs", nil)
	}
	if s.cfg.Media == nil {
		return e.JSON(http.StatusServiceUnavailable, map[string]string{"message": "Media catalogue is unavailable"})
	}
	items, err := s.cfg.Media.Items(e.Request.Context(), e.Request.URL.Query().Get("source_ref"), ids, false)
	if err != nil {
		return e.BadRequestError("invalid media request", nil)
	}
	return e.JSON(http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleMediaItem(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	if !adminmedia.ValidID(id) {
		return e.BadRequestError("invalid media ID", nil)
	}
	if s.cfg.Media == nil {
		return e.JSON(http.StatusServiceUnavailable, map[string]string{"message": "Media catalogue is unavailable"})
	}
	items, err := s.cfg.Media.Items(e.Request.Context(), e.Request.URL.Query().Get("source_ref"), []string{id}, true)
	if err != nil {
		return e.BadRequestError("invalid media request", nil)
	}
	return e.JSON(http.StatusOK, items[0])
}

func (s *Server) handleMediaImage(e *core.RequestEvent) error {
	if s.cfg.Media == nil {
		return e.JSON(http.StatusServiceUnavailable, map[string]string{"message": "Media image is unavailable"})
	}
	q := e.Request.URL.Query()
	size := q.Get("size")
	if size == "" {
		size = "small"
	}
	data, contentType, status := s.cfg.Media.Image(e.Request.Context(), q.Get("source_ref"), e.Request.PathValue("id"), e.Request.PathValue("type"), size)
	if status != http.StatusOK {
		if status != 400 && status != 404 && status != 409 {
			status = http.StatusServiceUnavailable
		}
		return e.JSON(status, map[string]string{"message": "Media image is unavailable"})
	}
	e.Response.Header().Set("Content-Type", contentType)
	e.Response.Header().Set("Cache-Control", "private, no-store")
	e.Response.WriteHeader(http.StatusOK)
	_, err := e.Response.Write(data)
	return err
}
