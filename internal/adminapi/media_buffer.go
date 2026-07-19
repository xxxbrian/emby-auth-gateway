package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

const (
	mediaBufferDefaultLimit = 50
	mediaBufferMaxLimit     = 200
	mediaBufferMaxJSON      = 2 << 20
)

type adminSnapshot struct {
	telemetry.Snapshot
	MediaBuffer telemetry.MediaBufferAggregate `json:"media_buffer"`
}

type mediaBufferCursor struct {
	BootID string `json:"boot_id"`
	RawID  uint64 `json:"raw_id"`
}

type adminTransfer struct {
	SessionID   string                          `json:"session_id"`
	UserID      string                          `json:"user_id"`
	Username    string                          `json:"username"`
	Device      string                          `json:"device"`
	ItemID      string                          `json:"item_id"`
	MediaMode   string                          `json:"media_mode"`
	BytesIn     int64                           `json:"bytes_in"`
	BytesOut    int64                           `json:"bytes_out"`
	StartedAt   time.Time                       `json:"started_at"`
	LastSeen    time.Time                       `json:"last_seen"`
	MediaBuffer *telemetry.MediaBufferReference `json:"media_buffer"`
}

func newAdminTransfer(v telemetry.Transfer) adminTransfer {
	return adminTransfer{SessionID: v.SessionID, UserID: v.UserID, Username: v.Username, Device: v.Device, ItemID: v.ItemID, MediaMode: v.MediaMode, BytesIn: v.BytesIn, BytesOut: v.BytesOut, StartedAt: v.StartedAt, LastSeen: v.LastSeen, MediaBuffer: v.MediaBuffer}
}

func (s *Server) mediaBufferAggregate() telemetry.MediaBufferAggregate {
	if s != nil && s.cfg.Telemetry != nil {
		return s.cfg.Telemetry.MediaBufferAggregateSnapshot()
	}
	if s != nil && s.cfg.MediaBufferSnapshot != nil {
		v := s.cfg.MediaBufferSnapshot()
		a := telemetry.MediaBufferAggregate{
			Enabled: v.Enabled, HardBudgetBytes: v.HardBudgetBytes, AllocatedBytes: v.AllocatedBytes,
			OwnedBytes: v.OwnedBytes, FreeBytes: v.FreeBytes, UnallocatedOptionalBytes: v.HardBudgetBytes - v.AllocatedBytes,
			PrivateBaseBytes: int64(maxInt(v.ActiveRequests, 0)) * 32768, ActiveRequests: maxInt(v.ActiveRequests, 0),
			BaseOnlyRequests: maxInt(v.BaseOnlyRequests, 0), IndebtedRequests: maxInt(v.IndebtedRequests, 0),
			RequestDebtBytes: max64(v.RequestDebtBytes, 0), HealthReasons: []string{}, ObservationCompleteness: telemetry.ObservationUnavailable,
		}
		if a.Enabled {
			a.Health = telemetry.MediaBufferHealthIdle
			a.ObservationCompleteness = telemetry.ObservationComplete
			if a.ActiveRequests > 0 {
				a.Health = telemetry.MediaBufferHealthHealthy
			}
		} else {
			a.Health = telemetry.MediaBufferHealthDisabled
		}
		if a.UnallocatedOptionalBytes < 0 {
			a.UnallocatedOptionalBytes = 0
		}
		return a
	}
	return telemetry.MediaBufferAggregate{Health: telemetry.MediaBufferHealthDisabled, HealthReasons: []string{}, ObservationCompleteness: telemetry.ObservationUnavailable}
}

func max64(v, zero int64) int64 {
	if v < zero {
		return zero
	}
	return v
}
func maxInt(v, zero int) int {
	if v < zero {
		return zero
	}
	return v
}

func writeBoundedJSON(e *core.RequestEvent, status int, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return e.InternalServerError("failed to encode response", nil)
	}
	if len(b) > mediaBufferMaxJSON {
		return e.JSON(http.StatusInternalServerError, map[string]string{"error": "response_too_large", "message": "response exceeds bounded size"})
	}
	e.Response.Header().Set("Content-Type", "application/json")
	e.Response.WriteHeader(status)
	_, err = e.Response.Write(b)
	return err
}

func mediaBufferError(e *core.RequestEvent, status int, code, message string) error {
	return writeBoundedJSON(e, status, map[string]string{"error": code, "message": message})
}

func parseMediaBufferLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return mediaBufferDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, strconv.ErrSyntax
	}
	if n > mediaBufferMaxLimit {
		n = mediaBufferMaxLimit
	}
	return n, nil
}

func encodeMediaBufferCursor(boot string, raw uint64) string {
	b, _ := json.Marshal(mediaBufferCursor{BootID: boot, RawID: raw})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeMediaBufferCursor(raw string) (mediaBufferCursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) == 0 {
		return mediaBufferCursor{}, false
	}
	var c mediaBufferCursor
	if json.Unmarshal(b, &c) != nil || c.BootID == "" {
		return mediaBufferCursor{}, false
	}
	return c, true
}

func (s *Server) mediaBufferReady(e *core.RequestEvent) (string, int, error) {
	boot := s.cfg.BootID
	if s.cfg.Telemetry != nil {
		boot = s.cfg.Telemetry.BootID()
	}
	expected := false
	if s.cfg.MediaBufferEnabled != nil {
		expected = s.cfg.MediaBufferEnabled()
	} else if s.cfg.MediaBufferSnapshot != nil {
		expected = s.cfg.MediaBufferSnapshot().Enabled
	} else if s.cfg.Telemetry != nil {
		// Without explicit wiring, only an enabled aggregate can establish that
		// buffering is expected. A nil provider otherwise remains disabled.
		expected = s.cfg.Telemetry.MediaBufferAggregateSnapshot().Enabled
	}
	if !expected {
		return boot, -1, nil
	}
	if s.cfg.Telemetry == nil {
		return boot, 0, mediaBufferError(e, http.StatusServiceUnavailable, "provider_unavailable", "media buffer provider unavailable")
	}
	a := s.cfg.Telemetry.MediaBufferAggregateSnapshot()
	if a.ObservationCompleteness == telemetry.ObservationUnavailable {
		return boot, 0, mediaBufferError(e, http.StatusServiceUnavailable, "provider_unavailable", "media buffer provider unavailable")
	}
	if !a.Enabled {
		return boot, -1, nil
	}
	return boot, 1, nil
}

func (s *Server) handleMediaBufferStreams(e *core.RequestEvent) error {
	limit, err := parseMediaBufferLimit(e.Request.URL.Query().Get("limit"))
	if err != nil {
		return mediaBufferError(e, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
	}
	rawCursor := e.Request.URL.Query().Get("cursor")
	var parsedCursor mediaBufferCursor
	if rawCursor != "" {
		c, ok := decodeMediaBufferCursor(rawCursor)
		if !ok {
			return mediaBufferError(e, http.StatusBadRequest, "invalid_cursor", "invalid cursor")
		}
		parsedCursor = c
	}
	boot, state, err := s.mediaBufferReady(e)
	if err != nil {
		return err
	}
	if rawCursor != "" && parsedCursor.BootID != boot {
		return mediaBufferError(e, http.StatusConflict, "stale_cursor", "cursor belongs to another boot")
	}
	if state <= 0 || s.cfg.Telemetry == nil {
		return writeBoundedJSON(e, http.StatusOK, telemetry.MediaBufferLivePageDTO{BootID: boot, Items: []telemetry.MediaBufferStream{}, ObservationCompleteness: telemetry.ObservationUnavailable})
	}
	page := s.cfg.Telemetry.MediaBufferLivePage(parsedCursor.RawID, limit)
	if page.NextCursor != "0" {
		page.NextCursor = encodeMediaBufferCursor(boot, mustUint(page.NextCursor))
	} else {
		page.NextCursor = ""
	}
	return writeBoundedJSON(e, http.StatusOK, page)
}

func mustUint(v string) uint64 { n, _ := strconv.ParseUint(v, 10, 64); return n }

func (s *Server) handleMediaBufferStreamDetail(e *core.RequestEvent) error {
	raw := e.Request.PathValue("stream_id")
	id, parseErr := strconv.ParseUint(raw, 10, 64)
	if parseErr != nil || id == 0 {
		return mediaBufferError(e, http.StatusBadRequest, "invalid_stream_id", "stream_id must be an unsigned decimal")
	}
	boot, state, err := s.mediaBufferReady(e)
	if err != nil {
		return err
	}
	if requested := e.Request.URL.Query().Get("boot_id"); requested != "" && requested != boot {
		return mediaBufferError(e, http.StatusConflict, "stale_boot", "boot_id is stale")
	}
	if state < 0 || s.cfg.Telemetry == nil {
		return writeBoundedJSON(e, http.StatusOK, map[string]any{"boot_id": boot, "item": nil})
	}
	if state == 0 {
		return mediaBufferError(e, http.StatusServiceUnavailable, "provider_unavailable", "media buffer provider unavailable")
	}
	item, ok := s.cfg.Telemetry.MediaBufferStreamDetail(id)
	if !ok {
		return mediaBufferError(e, http.StatusNotFound, "stream_not_found", "stream not found")
	}
	return writeBoundedJSON(e, http.StatusOK, map[string]any{"boot_id": boot, "item": item})
}

func (s *Server) handleMediaBufferSeries(e *core.RequestEvent) error {
	boot, state, err := s.mediaBufferReady(e)
	if err != nil {
		return err
	}
	window := telemetry.ParseSeriesWindow(e.Request.URL.Query().Get("window"))
	if state < 0 || s.cfg.Telemetry == nil {
		interval := "1m"
		if window == telemetry.Window15m {
			interval = "1s"
		}
		return writeBoundedJSON(e, http.StatusOK, telemetry.MediaBufferSeries{BootID: boot, Window: string(window), Interval: interval, Points: []telemetry.MediaBufferSeriesPoint{}})
	}
	return writeBoundedJSON(e, http.StatusOK, s.cfg.Telemetry.MediaBufferSeries(window))
}

func (s *Server) handleMediaBufferRecent(e *core.RequestEvent) error {
	limit, err := parseMediaBufferLimit(e.Request.URL.Query().Get("limit"))
	if err != nil {
		return mediaBufferError(e, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
	}
	boot, state, err := s.mediaBufferReady(e)
	if err != nil {
		return err
	}
	if state < 0 || s.cfg.Telemetry == nil {
		return writeBoundedJSON(e, http.StatusOK, telemetry.MediaBufferRecentPage{BootID: boot, Items: []telemetry.MediaBufferCompletionDTO{}})
	}
	return writeBoundedJSON(e, http.StatusOK, s.cfg.Telemetry.MediaBufferRecent(limit))
}
