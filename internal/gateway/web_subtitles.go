package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/subtitles"
	"github.com/xxxbrian/emby-auth-gateway/internal/transcode"
)

type subtitlesFilteredKey struct{}
type subtitleOptionsInvalidKey struct{}
type subtitleExplicitIndexKey struct{}

// Optional subtitle parsing must not add new failures to a working proxy.
// Replay every consumed byte and retain the original request body's ownership.
func withOptionalSubtitleNegotiation(r *http.Request) *http.Request {
	if r.Method != http.MethodPost || r.Body == nil {
		return r
	}
	original := r.Body
	data, err := io.ReadAll(io.LimitReader(original, (2<<20)+1))
	var replay io.Reader = bytes.NewReader(data)
	if len(data) > 2<<20 {
		replay = io.MultiReader(bytes.NewReader(data), original)
	}
	// PocketBase's rereadable body resets after EOF. Appending it after a
	// complete read would duplicate the JSON before audio/proxy processing.
	if err != nil {
		replay = &replayReadErrorReader{prefix: data, err: err, errorPending: true, remainder: original}
	}
	r.Body = &subtitleReplayBody{Reader: replay, Closer: original}
	if err != nil || len(data) > 2<<20 {
		return r.WithContext(context.WithValue(r.Context(), subtitleOptionsInvalidKey{}, true))
	}
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(data)) != 0 && json.Unmarshal(data, &fields) != nil {
		return r.WithContext(context.WithValue(r.Context(), subtitleOptionsInvalidKey{}, true))
	}
	ctx := r.Context()
	var selected *int
	addIndex := func(value string) bool {
		index, err := strconv.Atoi(value)
		if err != nil || index < -1 || index > 1024 || selected != nil && *selected != index {
			return false
		}
		selected = &index
		return true
	}
	for name, raw := range fields {
		if !strings.EqualFold(name, "SubtitleStreamIndex") {
			continue
		}
		value := string(raw)
		if strings.HasPrefix(value, `"`) {
			if json.Unmarshal(raw, &value) != nil {
				return r.WithContext(context.WithValue(ctx, subtitleOptionsInvalidKey{}, true))
			}
		}
		if !addIndex(value) {
			return r.WithContext(context.WithValue(ctx, subtitleOptionsInvalidKey{}, true))
		}
	}
	for name, values := range r.URL.Query() {
		if strings.EqualFold(name, "SubtitleStreamIndex") {
			for _, value := range values {
				if !addIndex(value) {
					return r.WithContext(context.WithValue(ctx, subtitleOptionsInvalidKey{}, true))
				}
			}
		}
	}
	if selected != nil {
		ctx = context.WithValue(ctx, subtitleExplicitIndexKey{}, *selected)
	}
	r = r.WithContext(ctx)
	copy := r.Clone(r.Context())
	copy.Body = io.NopCloser(bytes.NewReader(data))
	parsed, err := withAudioNegotiation(copy)
	if err != nil {
		return r.WithContext(context.WithValue(r.Context(), subtitleOptionsInvalidKey{}, true))
	}
	return r.WithContext(parsed.Context())
}

type subtitleReplayBody struct {
	io.Reader
	io.Closer
}

// This predicate is deliberately narrower than browser detection. Neither a
// DeviceProfile nor a VTT request opts a native session into subtitle work.
func (s *Server) isSubtitleWebRequest(r *http.Request, session *Session) bool {
	if s.cfg.Subtitles == nil && !s.cfg.WebSubtitlesEnabled || s.cfg.WebContext == nil || session == nil || r == nil || !strings.EqualFold(session.Client, "Emby Web") || !s.cfg.WebContext.Valid(r) {
		return false
	}
	client := ExtractClientIdentity(r)
	if client.Client != "" && !strings.EqualFold(client.Client, session.Client) || client.DeviceID != "" && client.DeviceID != session.DeviceID {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) || u.User != nil {
			return false
		}
	}
	return true
}

func subtitlePlaybackID(value any) string {
	root, _ := value.(map[string]any)
	id, _ := root["PlaySessionId"].(string)
	return id
}

func subtitleSourceKey(upstream upstreamRequestSnapshot, itemID string, media transcode.MediaSource) string {
	facts := make([]any, 0, len(media.MediaStreams))
	for _, stream := range media.MediaStreams {
		// Delivery URLs carry rotating credentials; delivery choices change
		// during negotiation. Neither identifies the original media bytes.
		if !stream.IsExternal && (stream.Type == "Video" || stream.Type == "Audio") {
			facts = append(facts, []any{stream.Index, stream.Type, stream.Codec, stream.Profile, stream.BitDepth, stream.Width, stream.Height, stream.Channels})
		}
	}
	data, _ := json.Marshal([]any{upstream.baseURL, upstream.serverID, upstream.userID, itemID, media.ID, media.Size, media.RunTimeTicks, media.Container, facts})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Only deliberate detail/PlaybackInfo requests can probe upstream subtitle
// delivery. Listing a library merely projects already-ready results.
func subtitleDetailRequest(r *http.Request, base string) bool {
	rel := strings.TrimPrefix(r.URL.Path, base)
	if _, ok := playbackInfoItemID(r.Method, rel); ok {
		return true
	}
	if r.Method != http.MethodGet {
		return false
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	return len(parts) == 2 && strings.EqualFold(parts[0], "Items") || len(parts) == 4 && strings.EqualFold(parts[0], "Users") && strings.EqualFold(parts[2], "Items")
}

func (s *Server) filterWebSubtitles(ctx context.Context, r *http.Request, value any, session *Session, upstream upstreamRequestSnapshot, token string, playbackResponse bool) {
	// One small deadline bounds all sources in the response, including probes.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	n, _ := r.Context().Value(audioNegotiationKey{}).(*audioNegotiation)
	itemID, isInfo := playbackInfoItemID(r.Method, strings.TrimPrefix(r.URL.Path, s.cfg.GatewayBasePath))
	playID := subtitlePlaybackID(value)
	probe := subtitleDetailRequest(r, s.cfg.GatewayBasePath)
	if invalid, _ := ctx.Value(subtitleOptionsInvalidKey{}).(bool); invalid {
		probe = false
	}
	var walk func(any, string, int)
	walk = func(value any, inherited string, depth int) {
		if depth > 64 {
			return
		}
		switch obj := value.(type) {
		case []any:
			for _, child := range obj {
				walk(child, inherited, depth+1)
			}
		case map[string]any:
			id := inherited
			if own, _ := obj["Id"].(string); own != "" && isBaseItemJSON(obj) {
				id = own
			}
			if sources, ok := obj["MediaSources"].([]any); ok {
				var firstStreams []any
				hasSubtitles := false
				for i, raw := range sources {
					media, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					ready := s.prepareWebSubtitleSource(ctx, r, media, id, playID, session, upstream, token, probe && i < 4, playbackResponse && isInfo, n)
					if i == 0 {
						firstStreams, _ = media["MediaStreams"].([]any)
					}
					hasSubtitles = hasSubtitles || ready
				}
				if _, exists := obj["MediaStreams"]; exists && firstStreams != nil {
					obj["MediaStreams"] = firstStreams
				}
				obj["HasSubtitles"] = hasSubtitles
			} else if streams, ok := obj["MediaStreams"].([]any); ok {
				// Metadata without a source binding cannot advertise a local URL.
				obj["MediaStreams"] = filterSubtitleStreams(streams, nil)
				obj["HasSubtitles"], obj["DefaultSubtitleStreamIndex"] = false, -1
			}
			for key, child := range obj {
				if key == "MediaSources" || key == "MediaStreams" || key == "UserData" {
					continue
				}
				walk(child, id, depth+1)
			}
		}
	}
	walk(value, itemID, 0)
}

func (s *Server) prepareWebSubtitleSource(ctx context.Context, r *http.Request, obj map[string]any, itemID, playID string, session *Session, upstream upstreamRequestSnapshot, token string, probe, info bool, n *audioNegotiation) bool {
	streams, ok := obj["MediaStreams"].([]any)
	if !ok {
		return false
	}
	data, err := json.Marshal(obj)
	var media transcode.MediaSource
	if err != nil || json.Unmarshal(data, &media) != nil || itemID == "" || media.ID == "" {
		obj["MediaStreams"] = filterSubtitleStreams(streams, nil)
		obj["HasSubtitles"], obj["DefaultSubtitleStreamIndex"] = false, -1
		return false
	}
	input := s.webSubtitleInput(r, obj, media, itemID, playID, session, upstream, token)
	selection := subtitles.Selection{Probe: probe, Index: media.DefaultSubtitleStreamIndex, Recover: probe}
	if index, ok := r.Context().Value(subtitleExplicitIndexKey{}).(int); ok {
		selection.Index = &index
	}
	// A resource request can omit DeviceProfile. Respect explicit Off even
	// when the audio negotiation parser consequently has no typed context.
	if n == nil {
		for name, values := range r.URL.Query() {
			if !strings.EqualFold(name, "SubtitleStreamIndex") {
				continue
			}
			if len(values) != 1 {
				selection.Recover = false
				continue
			}
			index, err := strconv.Atoi(values[0])
			if err != nil || index < -1 || index > 1024 {
				selection.Recover = false
				continue
			}
			selection.Index = &index
		}
	}
	selectedSource := n == nil || n.sourceID == "" || n.sourceID == media.ID
	if info && n != nil && selectedSource {
		selection.Playback = n.playback
		if n.request.SubtitleStreamIndex != nil {
			selection.Index = n.request.SubtitleStreamIndex
		}
	}
	if !selectedSource {
		selection.Recover = false
	}
	var ready []subtitles.Ready
	if s.cfg.Subtitles != nil {
		ready = s.cfg.Subtitles.Prepare(ctx, input, selection)
	}
	available := make(map[int]map[string]any, len(ready))
	for _, entry := range ready {
		for _, raw := range streams {
			stream, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			index, _ := int64Field(stream, "Index")
			if stream["Type"] != "Subtitle" || int(index) != entry.Index {
				continue
			}
			urlValue := "/Videos/" + url.PathEscape(itemID) + "/gateway-subtitles/" + entry.ID + "/stream." + entry.Format
			if token != "" {
				urlValue += "?api_key=" + url.QueryEscape(token)
			}
			stream["DeliveryUrl"], stream["DeliveryMethod"] = urlValue, "External"
			stream["IsExternalUrl"], stream["IsChunkedResponse"], stream["IsTextSubtitleStream"] = false, false, true
			available[entry.Index] = stream
			break
		}
	}
	obj["MediaStreams"] = filterSubtitleStreams(streams, available)
	obj["HasSubtitles"] = len(available) != 0
	chosen := -1
	if selection.Index != nil {
		if _, ok := available[*selection.Index]; ok {
			chosen = *selection.Index
		}
	}
	obj["DefaultSubtitleStreamIndex"] = chosen
	if info && n != nil && n.playback && selectedSource {
		// Recovery failure must not make the audio-only planner reject video.
		n.request.SubtitleStreamIndex = &chosen
	}
	return len(available) > 0
}

func filterSubtitleStreams(streams []any, ready map[int]map[string]any) []any {
	out := make([]any, 0, len(streams))
	for _, raw := range streams {
		stream, ok := raw.(map[string]any)
		if !ok || stream["Type"] != "Subtitle" {
			out = append(out, raw)
			continue
		}
		index, _ := int64Field(stream, "Index")
		if visible := ready[int(index)]; visible != nil {
			out = append(out, visible)
		}
	}
	return out
}

func (s *Server) webSubtitleInput(r *http.Request, obj map[string]any, media transcode.MediaSource, itemID, playID string, session *Session, upstream upstreamRequestSnapshot, token string) subtitles.Input {
	access := s.newAudioSourceAccess(r, session, upstream, token, itemID, media)
	if playID == "" {
		playID = "preview:" + itemID + ":" + media.ID
	}
	input := subtitles.Input{Owner: session.GatewayTokenHash, PlaybackID: playID, ItemID: itemID, SourceID: media.ID,
		SourceKey: subtitleSourceKey(upstream, itemID, media), SourceName: media.Name,
		Size: media.Size, Container: media.Container, Open: access.open}
	// Cached caption delivery must retain the original upstream binding even
	// when it needs no source read. Loading runtime state does not authenticate
	// upstream or perform an origin request, unlike upstreamAuth.Ensure.
	input.Valid = func(ctx context.Context) bool {
		if !access.valid(ctx) {
			return false
		}
		runtime, err := s.store.LoadDefaultUpstreamRuntime(ctx)
		if err != nil {
			return false
		}
		current, err := upstreamRequestSnapshotFromRuntimeEndpoint(runtime, upstream.endpointKey)
		return err == nil && sameSubtitleBinding(current, upstream)
	}
	var versionMu sync.Mutex
	var expectedETag string
	input.Open = func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
		body, err := access.open(ctx, offset, length)
		if err != nil {
			return nil, err
		}
		versionMu.Lock()
		expected := expectedETag
		versionMu.Unlock()
		current, _ := body.(*audioSourceBody)
		if expected != "" && (current == nil || current.etag != expected) {
			_ = body.Close()
			return nil, transcode.ErrSource
		}
		return body, nil
	}
	paths := make(map[int]string)
	for _, raw := range obj["MediaStreams"].([]any) {
		stream, ok := raw.(map[string]any)
		if !ok || stream["Type"] != "Subtitle" {
			continue
		}
		index, ok := int64Field(stream, "Index")
		if !ok || index < 0 || index > 1024 || len(input.Tracks) >= 64 {
			continue
		}
		codec, _ := stream["Codec"].(string)
		language, _ := stream["Language"].(string)
		name, _ := stream["DisplayTitle"].(string)
		external, _ := stream["IsExternal"].(bool)
		input.Tracks = append(input.Tracks, subtitles.Track{Index: int(index), Codec: codec, Language: language, Name: name, External: external})
		paths[int(index)], _ = stream["DeliveryUrl"].(string)
	}
	input.ValidTrack = func(ctx context.Context, track subtitles.Track) bool {
		delivery, ok := s.webSubtitleDeliveryURL(r, session, upstream, token, itemID, paths[track.Index])
		if !ok {
			return false
		}
		path := delivery.Path
		decision, err := s.store.CheckPathPolicy(ctx, http.MethodGet, path)
		if err != nil || !decision.Allowed {
			return false
		}
		runtime, err := s.store.LoadDefaultUpstreamRuntime(ctx)
		if err != nil {
			return false
		}
		request := r.Clone(ctx)
		request.Method, request.Body = http.MethodGet, nil
		current, err := s.selectUpstreamSnapshot(ctx, runtime, request, path)
		return err == nil && sameSubtitleBinding(current, upstream)
	}
	input.Validate = func(ctx context.Context) (string, bool, error) {
		if media.Size <= 0 {
			return "", false, subtitles.ErrUnavailable
		}
		body, err := access.open(ctx, 0, min(4096, media.Size))
		if err != nil {
			return "", false, err
		}
		data, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			return "", false, err
		}
		etag := ""
		if current, ok := body.(*audioSourceBody); ok {
			etag = current.etag
		}
		strong := strings.HasPrefix(etag, "\"") && strings.HasSuffix(etag, "\"")
		if strong {
			versionMu.Lock()
			expectedETag = etag
			versionMu.Unlock()
			return etag, true, nil
		}
		versionMu.Lock()
		expectedETag = ""
		versionMu.Unlock()
		hash := sha256.Sum256(data)
		return hex.EncodeToString(hash[:]), false, nil
	}
	input.Fetch = func(ctx context.Context, track subtitles.Track) ([]byte, string, error) {
		return s.fetchWebSubtitle(ctx, r, session, upstream, token, itemID, paths[track.Index])
	}
	return input
}

func sameSubtitleBinding(current, captured upstreamRequestSnapshot) bool {
	return current.serverID == captured.serverID && current.userID == captured.userID && current.baseURL == captured.baseURL
}

func (s *Server) webSubtitleDeliveryURL(r *http.Request, session *Session, captured upstreamRequestSnapshot, token, itemID, raw string) (*url.URL, bool) {
	if raw == "" {
		return nil, false
	}
	ref := rewriteMediaReference(raw, session, captured, token, s.gatewayBaseForRequest(r), s.cfg.GatewayServerID, false)
	u, err := url.Parse(ref)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(strings.ToLower(u.Path), "/videos/"+strings.ToLower(itemID)+"/") || !strings.Contains(strings.ToLower(u.Path), "/subtitles/") {
		return nil, false
	}
	return u, true
}

// Fetch only an owned delivery path using the existing routing/auth/policy
// stack. Unrecognized external URLs never create a new generic fetch proxy.
func (s *Server) fetchWebSubtitle(ctx context.Context, original *http.Request, session *Session, captured upstreamRequestSnapshot, token, itemID, raw string) ([]byte, string, error) {
	u, ok := s.webSubtitleDeliveryURL(original, session, captured, token, itemID, raw)
	if !ok {
		return nil, "", subtitles.ErrUnavailable
	}
	decision, err := s.store.CheckPathPolicy(ctx, http.MethodGet, u.Path)
	if err != nil || !decision.Allowed {
		return nil, "", subtitles.ErrUnavailable
	}
	runtime, err := s.upstreamAuth.Ensure(ctx)
	if err != nil {
		return nil, "", err
	}
	inbound := original.Clone(ctx)
	inbound.Method, inbound.Body = http.MethodGet, nil
	upstream, err := s.selectUpstreamSnapshot(ctx, runtime, inbound, u.Path)
	if err != nil || upstream.serverID != captured.serverID || upstream.userID != captured.userID {
		return nil, "", subtitles.ErrUnavailable
	}
	target, err := s.proxyURL(upstream, session, u.Path, u.RawQuery, token)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(withRedirectCredentialTokens(ctx, token, upstream.token), http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, "", err
	}
	s.rewriteRequestHeaders(req.Header, upstream)
	resp, err := s.proxyClient.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, "", subtitles.ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", subtitles.ErrUnavailable
	}
	data, err := readLimited(resp.Body, 8<<20)
	if s.meter != nil {
		s.meter.AddIngress(int64(len(data)))
	}
	if err != nil {
		return nil, "", subtitles.ErrUnavailable
	}
	format := "vtt"
	switch {
	case strings.HasSuffix(strings.ToLower(u.Path), ".ass"), strings.HasSuffix(strings.ToLower(u.Path), ".ssa"):
		format = "ass"
	case strings.HasSuffix(strings.ToLower(u.Path), ".srt"):
		format = "srt"
	}
	return data, format, nil
}

func (s *Server) handleWebSubtitleRoute(w http.ResponseWriter, r *http.Request, rel string, session *Session) bool {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 5 || !strings.EqualFold(parts[0], "Videos") || parts[2] != "gateway-subtitles" {
		return false
	}
	if s.cfg.Subtitles == nil || !s.isSubtitleWebRequest(r, session) {
		http.NotFound(w, r)
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	reader, format, err := s.cfg.Subtitles.Open(r.Context(), session.GatewayTokenHash, parts[1], parts[3])
	if err != nil {
		if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
			panic(http.ErrAbortHandler)
		}
		http.NotFound(w, r)
		return true
	}
	defer reader.Close()
	if parts[4] != "stream."+format {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	if format == "ass" {
		w.Header().Set("Content-Type", "text/x-ssa; charset=utf-8")
	}
	http.ServeContent(newCountedWriter(w, s.meter, nil), r, parts[4], time.Time{}, reader)
	return true
}

func (s *Server) noteWebSubtitlePlayback(r *http.Request, rel string, session *Session, data []byte) {
	if s.cfg.Subtitles == nil || !s.isSubtitleWebRequest(r, session) {
		return
	}
	var report struct {
		PlaySessionID string `json:"PlaySessionId"`
	}
	if json.Unmarshal(data, &report) != nil || report.PlaySessionID == "" {
		return
	}
	if equalPath(rel, "/Sessions/Playing/Stopped") {
		s.cfg.Subtitles.End(session.GatewayTokenHash, report.PlaySessionID)
	} else {
		s.cfg.Subtitles.Touch(session.GatewayTokenHash, report.PlaySessionID)
	}
}
