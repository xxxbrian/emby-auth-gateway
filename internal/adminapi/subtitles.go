package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/subtitles"
)

const subtitleMaxJSON = 100 << 10

type subtitleCursor struct {
	BootID    string    `json:"boot"`
	CreatedAt time.Time `json:"created"`
	ID        string    `json:"id"`
}

type subtitlePage struct {
	subtitles.Snapshot
	TotalJobs  int    `json:"total_jobs"`
	NextCursor string `json:"next_cursor"`
}

func (s *Server) handleSubtitles(e *core.RequestEvent) error {
	limit := 8
	if raw := e.Request.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 32 {
			return e.BadRequestError("limit must be between 1 and 32", nil)
		}
		limit = n
	}
	snapshot := s.cfg.Telemetry.SubtitleSnapshot()
	if snapshot.BootID == "" {
		snapshot.BootID = s.cfg.BootID
	}
	var cursor subtitleCursor
	if raw := e.Request.URL.Query().Get("cursor"); raw != "" {
		if len(raw) > 1024 {
			return e.BadRequestError("invalid cursor", nil)
		}
		data, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.CreatedAt.IsZero() || cursor.ID == "" {
			return e.BadRequestError("invalid cursor", nil)
		}
		if cursor.BootID != snapshot.BootID {
			return writeBoundedJSON(e, http.StatusConflict, map[string]string{"error": "stale_cursor", "message": "gateway restarted; refresh the subtitle list"})
		}
	}
	// The service has these bounds too. Copy metadata before clipping it, so
	// reading Admin can never mutate the service's projection or decisions.
	jobs := make([]subtitles.JobView, min(64, len(snapshot.Jobs)))
	for i := range jobs {
		jobs[i] = subtitleJobView(snapshot.Jobs[i])
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID > jobs[j].ID
		}
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})
	page := subtitlePage{Snapshot: snapshot, TotalJobs: len(jobs)}
	page.Jobs = []subtitles.JobView{}
	base, _ := json.Marshal(page)
	bytes := len(base)
	for _, job := range jobs {
		if cursor.ID != "" && (job.CreatedAt.After(cursor.CreatedAt) || job.CreatedAt.Equal(cursor.CreatedAt) && job.ID >= cursor.ID) {
			continue
		}
		encoded, _ := json.Marshal(job)
		// Reserve room for a cursor and commas. Pagination is bounded by bytes
		// as well as rows because each job can contain 64 subtitle tracks.
		if len(page.Jobs) >= limit || len(page.Jobs) > 0 && bytes+len(encoded)+1024 > subtitleMaxJSON {
			last := page.Jobs[len(page.Jobs)-1]
			data, _ := json.Marshal(subtitleCursor{snapshot.BootID, last.CreatedAt, last.ID})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
			break
		}
		page.Jobs = append(page.Jobs, job)
		bytes += len(encoded) + 1
	}
	encoded, err := json.Marshal(page)
	if err != nil || len(encoded) > subtitleMaxJSON {
		return e.InternalServerError("subtitle observation exceeds bounded size", nil)
	}
	return writeBoundedJSON(e, http.StatusOK, page)
}

func subtitleJobView(job subtitles.JobView) subtitles.JobView {
	job.ID = subtitleLabel(job.ID, 128)
	job.ItemID = subtitleLabel(job.ItemID, 128)
	job.SourceID = subtitleLabel(job.SourceID, 128)
	job.SourceName = subtitleLabel(job.SourceName, 256)
	job.State = subtitleCode(job.State, 32)
	tracks := make([]subtitles.TrackView, min(64, len(job.Tracks)))
	for i := range tracks {
		track := job.Tracks[i]
		track.Name = subtitleLabel(track.Name, 160)
		track.Language = subtitleLabel(track.Language, 32)
		track.State = subtitleCode(track.State, 32)
		track.Format = subtitleCode(track.Format, 16)
		track.Mode = subtitleCode(track.Mode, 16)
		track.Reason = subtitleCode(track.Reason, 64)
		tracks[i] = track
	}
	job.Tracks = tracks
	return job
}

func subtitleCode(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, value)
	return value[:min(len(value), limit)]
}

func subtitleLabel(value string, limit int) string {
	if strings.Contains(value, "://") {
		return "[media source]"
	}
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}
