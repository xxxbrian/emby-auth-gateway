package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

type audioNegotiationKey struct{}
type audioNegotiation struct {
	previousID string
	request    transcode.Request
	sourceID   string
	start      int64
	playback   bool
}

func withAudioNegotiation(r *http.Request) (*http.Request, error) {
	if r.Method != http.MethodPost || r.Body == nil {
		return r, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return nil, errors.New("playback request too large")
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	if len(bytes.TrimSpace(data)) == 0 {
		return r, nil
	}
	var body map[string]json.RawMessage
	if err = json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	var profile json.RawMessage
	for key, v := range body {
		if strings.EqualFold(key, "DeviceProfile") {
			profile = v
		}
	}
	if len(profile) == 0 || string(profile) == "null" {
		return r, nil
	}
	var n audioNegotiation
	if err = json.Unmarshal(profile, &n.request.Profile); err != nil {
		return nil, err
	}
	value := func(name string) (string, bool, error) {
		var result string
		found := false
		add := func(v string) error {
			if found && result != v {
				return errors.New("conflicting playback option")
			}
			result, found = v, true
			return nil
		}
		for key, values := range r.URL.Query() {
			if strings.EqualFold(key, name) {
				for _, v := range values {
					if err := add(v); err != nil {
						return "", false, err
					}
				}
			}
		}
		for key, raw := range body {
			if strings.EqualFold(key, name) {
				var v string
				if len(raw) > 0 && raw[0] == '"' {
					if err := json.Unmarshal(raw, &v); err != nil {
						return "", false, err
					}
				} else {
					v = string(raw)
				}
				if err := add(v); err != nil {
					return "", false, err
				}
			}
		}
		return result, found, nil
	}
	for _, field := range []struct {
		name string
		dst  **bool
	}{{"EnableDirectPlay", &n.request.EnableDirectPlay}, {"EnableDirectStream", &n.request.EnableDirectStream}, {"EnableTranscoding", &n.request.EnableTranscoding}, {"AllowAudioStreamCopy", &n.request.AllowAudioStreamCopy}, {"AllowVideoStreamCopy", &n.request.AllowVideoStreamCopy}} {
		v, ok, e := value(field.name)
		if e != nil {
			return nil, e
		}
		if ok {
			b, e := strconv.ParseBool(v)
			if e != nil {
				return nil, e
			}
			*field.dst = &b
		}
	}
	for _, field := range []string{"AudioStreamIndex", "SubtitleStreamIndex", "MaxAudioChannels", "MaxStreamingBitrate", "StartTimeTicks"} {
		v, ok, e := value(field)
		if e != nil {
			return nil, e
		}
		if !ok {
			continue
		}
		num, e := strconv.ParseInt(v, 10, 64)
		if e != nil || num < 0 && !(field == "SubtitleStreamIndex" && num == -1) {
			return nil, errors.New("invalid playback number")
		}
		switch field {
		case "SubtitleStreamIndex":
			if num > 1024 {
				return nil, errors.New("invalid subtitle index")
			}
			index := int(num)
			n.request.SubtitleStreamIndex = &index
		case "AudioStreamIndex":
			if num > 1024 {
				return nil, errors.New("invalid audio index")
			}
			i := int(num)
			n.request.AudioStreamIndex = &i
		case "MaxAudioChannels":
			if num > 32 {
				return nil, errors.New("invalid audio channels")
			}
			n.request.MaxAudioChannels = int(num)
		case "MaxStreamingBitrate":
			n.request.MaxStreamingBitrate = num
		case "StartTimeTicks":
			n.start = num
		}
	}
	if n.sourceID, _, err = value("MediaSourceId"); err != nil {
		return nil, err
	}
	if n.previousID, _, err = value("CurrentPlaySessionId"); err != nil {
		return nil, err
	}
	if v, ok, e := value("IsPlayback"); e != nil {
		return nil, e
	} else if ok {
		n.playback, err = strconv.ParseBool(v)
		if err != nil {
			return nil, err
		}
	}
	return r.WithContext(context.WithValue(r.Context(), audioNegotiationKey{}, &n)), nil
}

func (s *Server) negotiateAudioResponse(r *http.Request, value any, session *Session, upstream upstreamRequestSnapshot, gatewayToken string) error {
	if s.cfg.Transcoder == nil || session == nil {
		return nil
	}
	n, _ := r.Context().Value(audioNegotiationKey{}).(*audioNegotiation)
	if n == nil {
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
		if n.sourceID == "" || id == n.sourceID {
			selected = source
			break
		}
	}
	if selected == nil {
		return nil
	}
	data, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	var media transcode.MediaSource
	if err = json.Unmarshal(data, &media); err != nil {
		return err
	}
	plan, err := transcode.Choose(media, n.request)
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
	if !n.playback {
		selected["SupportsTranscoding"] = true
		return nil
	}
	itemID, _ := playbackInfoItemID(r.Method, strings.TrimPrefix(r.URL.Path, s.cfg.GatewayBasePath))
	source := s.audioSource(r, session, upstream, gatewayToken, itemID, media)
	identity := transcode.Identity{Owner: session.GatewayTokenHash, UserID: session.GatewayUserID, Username: session.GatewayUsername, Device: session.Device, ItemID: itemID}
	nameCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	if value, status, _, err := s.fetchBackendJSON(nameCtx, r, "/Users/"+session.SyntheticUserID+"/Items/"+url.PathEscape(itemID), "", session, gatewayToken); err == nil && status == http.StatusOK {
		if item, ok := value.(map[string]any); ok {
			identity.ItemName, _ = item["Name"].(string)
		}
	}
	cancel()
	id, err := s.cfg.Transcoder.Create(identity, source, *plan, n.start)
	if err != nil {
		return err
	}
	if n.previousID != "" {
		s.cfg.Transcoder.LinkReplacement(session.GatewayTokenHash, n.previousID, id)
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

func (s *Server) noteAudioPlayback(rel string, session *Session, data []byte, itemID string) (string, bool) {
	if s.cfg.Transcoder == nil {
		return "", false
	}
	var report struct {
		PlaySessionID string          `json:"PlaySessionId"`
		PositionTicks json.RawMessage `json:"PositionTicks"`
		IsPaused      *bool           `json:"IsPaused"`
	}
	if json.Unmarshal(data, &report) != nil || !strings.HasPrefix(report.PlaySessionID, "eag-") {
		return "", false
	}
	if !s.cfg.Transcoder.OwnsItem(session.GatewayTokenHash, report.PlaySessionID, itemID) {
		return "", false
	}
	if equalPath(rel, "/Sessions/Playing/Stopped") {
		s.cfg.Transcoder.End(session.GatewayTokenHash, report.PlaySessionID)
		return report.PlaySessionID, false
	}
	var position int64
	raw := strings.Trim(string(report.PositionTicks), "\"")
	if raw != "" {
		position, _ = strconv.ParseInt(raw, 10, 64)
	}
	s.cfg.Transcoder.NotePlayback(session.GatewayTokenHash, report.PlaySessionID, max(0, position), report.IsPaused)
	return report.PlaySessionID, report.IsPaused != nil && *report.IsPaused
}
