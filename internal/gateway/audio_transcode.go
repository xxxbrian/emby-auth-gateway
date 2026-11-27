package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

func (s *Server) negotiateAudioResponse(r *http.Request, value any, session *Session, upstream upstreamRequestSnapshot, gatewayToken string) error {
	if s.cfg.Transcoder == nil || session == nil {
		return nil
	}
	options, _ := r.Context().Value(playbackOptionsKey{}).(*playbackOptions)
	if options == nil || len(options.request.Profile.DirectPlayProfiles)+len(options.request.Profile.TranscodingProfiles) == 0 {
		return nil
	}
	root, ok := value.(map[string]any)
	if !ok || root["ErrorCode"] != nil {
		return nil
	}
	sources, ok := root["MediaSources"].([]any)
	if !ok || len(sources) == 0 {
		return nil
	}
	var selected map[string]any
	for _, raw := range sources {
		source, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := source["Id"].(string)
		if options.sourceID == "" || id == options.sourceID {
			selected = source
			break
		}
	}
	if selected == nil {
		return nil
	}
	data, err := json.Marshal(selected)
	if err != nil {
		return nil
	}
	var media transcode.MediaSource
	if err := json.Unmarshal(data, &media); err != nil || media.ID == "" || len(media.MediaStreams) == 0 {
		return nil
	}
	request := options.request
	if filtered, _ := r.Context().Value(subtitlesFilteredKey{}).(bool); filtered && options.playback {
		request.SubtitleStreamIndex = media.DefaultSubtitleStreamIndex
	}
	plan, err := transcode.Choose(media, request)
	if err != nil {
		selected["SupportsDirectPlay"], selected["SupportsDirectStream"], selected["SupportsTranscoding"] = false, false, false
		delete(selected, "TranscodingUrl")
		return nil
	}
	if plan == nil {
		return nil
	}
	// A metadata preview advertises capability without registering an execution
	// plan; the subsequent IsPlayback request obtains its authenticated URL.
	if !options.playback {
		selected["SupportsTranscoding"] = true
		return nil
	}
	itemID, _ := playbackInfoItemID(r.Method, strings.TrimPrefix(r.URL.Path, s.cfg.GatewayBasePath))
	source := s.audioSource(r, session, upstream, gatewayToken, itemID, media)
	identity := transcode.Identity{Owner: session.GatewayTokenHash, UserID: session.GatewayUserID, Username: session.GatewayUsername, Device: session.Device, ItemID: itemID}
	identity.SourceRef = MediaSourceRef(upstream.serverID, upstream.userID)
	nameCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	if value, status, _, err := s.fetchBackendJSON(nameCtx, r, "/Users/"+session.SyntheticUserID+"/Items/"+url.PathEscape(itemID), "", session, gatewayToken); err == nil && status == http.StatusOK {
		if item, ok := value.(map[string]any); ok {
			identity.ItemName, _ = item["Name"].(string)
		}
	}
	cancel()
	id, err := s.cfg.Transcoder.Create(identity, source, *plan, options.start)
	if err != nil {
		return err
	}
	if options.previousID != "" {
		s.cfg.Transcoder.LinkReplacement(session.GatewayTokenHash, options.previousID, id)
	}
	mediaURL := "/Videos/" + url.PathEscape(itemID) + "/audio/" + id + "/index.m3u8"
	if gatewayToken != "" {
		mediaURL += "?api_key=" + url.QueryEscape(gatewayToken)
	}
	selected["SupportsDirectPlay"], selected["SupportsDirectStream"], selected["SupportsTranscoding"] = false, false, true
	selected["TranscodingUrl"], selected["TranscodingSubProtocol"], selected["TranscodingContainer"] = mediaURL, "hls", plan.Container
	root["PlaySessionId"], root["MediaSources"] = id, []any{selected}
	return nil
}

func (s *Server) handleAudioRoute(w http.ResponseWriter, r *http.Request, rel string, session *Session, token string) bool {
	m := s.cfg.Transcoder
	if r.Method == http.MethodPost && equalPath(rel, "/Videos/ActiveEncodings/Delete") || r.Method == http.MethodDelete && equalPath(rel, "/Videos/ActiveEncodings") {
		id := r.URL.Query().Get("PlaySessionId")
		if strings.HasPrefix(id, "eag-") {
			m.StopEncoding(session.GatewayTokenHash, id)
			w.WriteHeader(http.StatusNoContent)
			return true
		}
		return false
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 5 || !strings.EqualFold(parts[0], "Videos") || parts[2] != "audio" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	w.Header().Set("Cache-Control", "private, no-store")
	id, file := parts[3], parts[4]
	if !m.OwnsItem(session.GatewayTokenHash, id, parts[1]) {
		http.NotFound(w, r)
		return true
	}
	if file == "index.m3u8" {
		playlist, err := m.Manifest(r.Context(), session.GatewayTokenHash, id)
		if err != nil {
			writeAudioError(w, err)
			return true
		}
		if token != "" {
			q := "?api_key=" + url.QueryEscape(token)
			lines := strings.Split(playlist, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "#EXT-X-MAP:") {
					lines[i] = strings.Replace(line, "init.mp4\"", "init.mp4"+q+"\"", 1)
				} else if line != "" && !strings.HasPrefix(line, "#") {
					lines[i] += q
				}
			}
			playlist = strings.Join(lines, "\n")
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Content-Length", strconv.Itoa(len(playlist)))
		if r.Method == http.MethodGet {
			_, _ = countEgressWrite(w, s.meter, nil, []byte(playlist))
		}
		return true
	}
	index := -1
	container := m.Container(session.GatewayTokenHash, id)
	extension := ".m4s"
	if container == "ts" {
		extension = ".ts"
	}
	if file == "init.mp4" && container != "mp4" {
		http.NotFound(w, r)
		return true
	}
	if file != "init.mp4" {
		if !strings.HasSuffix(file, extension) {
			http.NotFound(w, r)
			return true
		}
		var err error
		index, err = strconv.Atoi(strings.TrimSuffix(file, extension))
		if err != nil || index < 0 {
			http.NotFound(w, r)
			return true
		}
	}
	reader, _, err := m.Open(r.Context(), session.GatewayTokenHash, id, index)
	if err != nil {
		writeAudioError(w, err)
		return true
	}
	defer reader.Close()
	w.Header().Set("Content-Type", "video/mp4")
	if container == "ts" {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	s.clearMediaWriteDeadlineNow(w, r, rel, &http.Response{StatusCode: http.StatusOK}, session)
	s.beginMediaCopy()
	defer s.endMediaCopy()
	var transfer *telemetry.TransferHandle
	if s.meter != nil {
		transfer = s.meter.BeginTransfer(telemetry.TransferMeta{
			SourceRef: m.SourceRef(session.GatewayTokenHash, id),
			SessionID: session.GatewayTokenHash, UserID: session.GatewayUserID, Username: session.GatewayUsername,
			Device: session.Device, ItemID: parts[1], MediaMode: observe.MediaHLS, Method: r.Method,
		})
		defer func() { transfer.End(r.Context().Err()) }()
	}
	http.ServeContent(newCountedWriter(w, s.meter, transfer), r, file, time.Time{}, reader)
	return true
}

func writeAudioError(w http.ResponseWriter, err error) {
	status, code := http.StatusBadGateway, "audio_conversion_failed"
	switch {
	case errors.Is(err, transcode.ErrNotFound):
		status, code = http.StatusNotFound, "playback_unavailable"
	case errors.Is(err, transcode.ErrCapacity):
		status, code = http.StatusServiceUnavailable, "conversion_capacity_exhausted"
	case errors.Is(err, transcode.ErrIndex), errors.Is(err, transcode.ErrUnsupported):
		status, code = http.StatusUnprocessableEntity, "media_not_supported"
	case errors.Is(err, context.Canceled):
		panic(http.ErrAbortHandler)
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, map[string]string{"error": code, "message": code})
}

func (s *Server) noteAudioPlayback(rel string, session *Session, details playbackDetails) (string, bool) {
	playID := details.PlaySessionID
	if s.cfg.Transcoder == nil || !strings.HasPrefix(playID, "eag-") {
		return "", false
	}
	if !s.cfg.Transcoder.OwnsItem(session.GatewayTokenHash, playID, details.ItemID) {
		return "", false
	}
	var position *int64
	if details.HasPositionTicks {
		ticks := max(0, details.PositionTicks)
		position = &ticks
	}
	s.cfg.Transcoder.NotePlayback(session.GatewayTokenHash, playID, position, details.IsPaused)
	if equalPath(rel, "/Sessions/Playing/Stopped") {
		s.cfg.Transcoder.End(session.GatewayTokenHash, playID)
		return playID, false
	}
	return playID, details.IsPaused != nil && *details.IsPaused
}
