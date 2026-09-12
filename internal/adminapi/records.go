package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminmedia"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminquery"
)

func (s *Server) handleAuditDetail(e *core.RequestEvent) error {
	item, err := s.cfg.Query.GetAudit(e.Request.Context(), e.Request.PathValue("id"))
	if errors.Is(err, adminquery.ErrRecordNotFound) {
		return e.NotFoundError("audit record not found", nil)
	}
	if err != nil {
		return e.BadRequestError("could not read audit record", nil)
	}
	return e.JSON(http.StatusOK, item)
}

func (s *Server) handleUserMedia(e *core.RequestEvent) error {
	ctx, cancel := context.WithTimeout(e.Request.Context(), 5*time.Second)
	defer cancel()
	q := e.Request.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return e.BadRequestError("limit must be a positive integer", nil)
		}
		limit = n
	}
	// The configured Emby server is pinned by controlplane. Resolve the access
	// scope before and after the local read so concurrent reconfiguration never
	// associates a page with an unverified account scope.
	var sourceRef string
	if s.cfg.Media != nil {
		sourceRef, _ = s.cfg.Media.SourceRef(ctx)
	}
	page, err := s.cfg.Query.ListUserMedia(ctx, e.Request.PathValue("id"), q.Get("view"), q.Get("cursor"), limit)
	if errors.Is(err, adminquery.ErrRecordNotFound) {
		return e.NotFoundError("user not found", nil)
	}
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	if sourceRef != "" && s.cfg.Media != nil {
		// Resolve bounded batches without repairing or saving personal state.
		// A same-server item ID can be reused: verify the existing fingerprint
		// before attaching a shared catalogue reference to saved progress.
		for start := 0; start < len(page.Items); start += adminmedia.MaxBatch {
			end := min(start+adminmedia.MaxBatch, len(page.Items))
			ids := make([]string, 0, end-start)
			for i := start; i < end; i++ {
				if !page.Items[i].Orphaned && adminmedia.ValidID(page.Items[i].ItemID) {
					ids = append(ids, page.Items[i].ItemID)
				}
			}
			if len(ids) == 0 {
				continue
			}
			metadata, readErr := s.cfg.Media.Items(ctx, sourceRef, ids, false)
			byID := make(map[string]adminmedia.Item, len(metadata))
			for _, item := range metadata {
				byID[item.ID] = item
			}
			for i := start; i < end; i++ {
				row := &page.Items[i]
				if row.Orphaned {
					continue
				}
				item, present := byID[row.ItemID]
				row.MetadataStatus = "unavailable"
				if readErr != nil || !present {
					continue
				}
				row.MetadataStatus = item.Status
				if item.Status != "available" {
					continue
				}
				matches := row.Fingerprint == ""
				if s.cfg.MatchMediaIdentity != nil {
					matches = s.cfg.MatchMediaIdentity(row.Fingerprint, item.Type, item.Name, item.SeriesID)
				}
				if !matches {
					row.MetadataStatus = "identity_mismatch"
					continue
				}
				row.SourceRef = sourceRef
			}
		}
		current, sourceErr := s.cfg.Media.SourceRef(ctx)
		if sourceErr != nil || current != sourceRef {
			for i := range page.Items {
				page.Items[i].SourceRef = ""
				page.Items[i].MetadataStatus = "source_changed"
			}
		}
	}
	return e.JSON(http.StatusOK, page)
}
