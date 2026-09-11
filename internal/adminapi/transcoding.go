package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

type transcodingCursor struct {
	BootID    string    `json:"boot"`
	CreatedAt time.Time `json:"created"`
	ID        string    `json:"id"`
}
type transcodingPage struct {
	BootID     string              `json:"boot_id"`
	At         time.Time           `json:"at"`
	Aggregate  transcode.Aggregate `json:"aggregate"`
	Items      []transcode.JobView `json:"items"`
	NextCursor string              `json:"next_cursor"`
}

func (s *Server) handleTranscodingJobs(e *core.RequestEvent) error {
	limit, err := parseMediaBufferLimit(e.Request.URL.Query().Get("limit"))
	if err != nil {
		return e.BadRequestError("invalid limit", nil)
	}
	snapshot := s.cfg.Telemetry.TranscodingSnapshot()
	var cursor transcodingCursor
	if raw := e.Request.URL.Query().Get("cursor"); raw != "" {
		if len(raw) > 1024 {
			return e.BadRequestError("invalid cursor", nil)
		}
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.CreatedAt.IsZero() || cursor.ID == "" {
			return e.BadRequestError("invalid cursor", nil)
		}
		if cursor.BootID != snapshot.BootID {
			return e.JSON(http.StatusConflict, map[string]string{"error": "stale_cursor", "message": "gateway restarted; refresh the task list"})
		}
	}
	page := transcodingPage{BootID: snapshot.BootID, At: snapshot.At, Aggregate: snapshot.Aggregate, Items: []transcode.JobView{}}
	for _, job := range snapshot.Jobs {
		if cursor.ID != "" && (job.CreatedAt.After(cursor.CreatedAt) || job.CreatedAt.Equal(cursor.CreatedAt) && job.ID >= cursor.ID) {
			continue
		}
		if len(page.Items) == limit {
			last := page.Items[len(page.Items)-1]
			data, _ := json.Marshal(transcodingCursor{snapshot.BootID, last.CreatedAt, last.ID})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
			break
		}
		page.Items = append(page.Items, job)
	}
	return writeBoundedJSON(e, http.StatusOK, page)
}
func (s *Server) handleTranscodingJob(e *core.RequestEvent) error {
	snapshot := s.cfg.Telemetry.TranscodingSnapshot()
	if boot := e.Request.URL.Query().Get("boot_id"); boot != "" && boot != snapshot.BootID {
		return e.JSON(http.StatusConflict, map[string]string{"error": "stale_boot", "message": "gateway restarted"})
	}
	id := e.Request.PathValue("job_id")
	for _, job := range snapshot.Jobs {
		if job.ID == id {
			return writeBoundedJSON(e, http.StatusOK, map[string]any{"boot_id": snapshot.BootID, "at": snapshot.At, "job": job})
		}
	}
	for _, job := range snapshot.Recent {
		if job.ID == id {
			return writeBoundedJSON(e, http.StatusOK, map[string]any{"boot_id": snapshot.BootID, "at": snapshot.At, "job": job})
		}
	}
	return e.NotFoundError("conversion playback is no longer available", nil)
}
func (s *Server) handleTranscodingRecent(e *core.RequestEvent) error {
	limit, err := parseMediaBufferLimit(e.Request.URL.Query().Get("limit"))
	if err != nil {
		return e.BadRequestError("invalid limit", nil)
	}
	snapshot := s.cfg.Telemetry.TranscodingSnapshot()
	items := make([]transcode.JobView, 0, min(limit, len(snapshot.Recent)))
	for i := len(snapshot.Recent) - 1; i >= 0 && len(items) < limit; i-- {
		items = append(items, snapshot.Recent[i])
	}
	return writeBoundedJSON(e, http.StatusOK, map[string]any{"boot_id": snapshot.BootID, "at": snapshot.At, "items": items})
}
