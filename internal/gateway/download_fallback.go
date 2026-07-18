package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var errDownloadFallbackUnavailable = errors.New("download direct-stream fallback unavailable")

// embyPlaybackInfoRequestDTO is the JSON representation of Emby's official
// MediaBrowser.Model.MediaInfo.PlaybackInfoRequest fields used by this flow.
type embyPlaybackInfoRequestDTO struct {
	ID                 string `json:"Id"`
	UserID             string `json:"UserId"`
	MediaSourceID      string `json:"MediaSourceId,omitempty"`
	EnableDirectPlay   bool   `json:"EnableDirectPlay"`
	EnableDirectStream bool   `json:"EnableDirectStream"`
	EnableTranscoding  bool   `json:"EnableTranscoding"`
	IsPlayback         bool   `json:"IsPlayback"`
}

// embyPlaybackInfoResponseDTO is Emby's official
// MediaBrowser.Model.MediaInfo.PlaybackInfoResponse DTO.
type embyPlaybackInfoResponseDTO struct {
	ErrorCode     *string                  `json:"ErrorCode"`
	MediaSources  []embyMediaSourceInfoDTO `json:"MediaSources"`
	PlaySessionID string                   `json:"PlaySessionId"`
}

// embyMediaSourceInfoDTO projects the official
// MediaBrowser.Model.Dto.MediaSourceInfo fields needed to deliver the source.
type embyMediaSourceInfoDTO struct {
	ID                   string            `json:"Id"`
	Name                 string            `json:"Name"`
	Path                 string            `json:"Path"`
	Protocol             string            `json:"Protocol"`
	RequiredHTTPHeaders  map[string]string `json:"RequiredHttpHeaders"`
	Container            string            `json:"Container"`
	Size                 *int64            `json:"Size"`
	DirectStreamURL      string            `json:"DirectStreamUrl"`
	SupportsDirectPlay   bool              `json:"SupportsDirectPlay"`
	SupportsDirectStream bool              `json:"SupportsDirectStream"`
}

func downloadItemID(method, rel string) (string, bool) {
	if method != http.MethodGet && method != http.MethodHead {
		return "", false
	}
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) != 3 || !strings.EqualFold(parts[0], "Items") || strings.TrimSpace(parts[1]) == "" || !strings.EqualFold(parts[2], "Download") {
		return "", false
	}
	return parts[1], true
}

func (s *Server) tryDownloadDirectStreamFallback(r *http.Request, rel string, session *Session, upstream upstreamRequestSnapshot, gatewayToken string) (*http.Response, error) {
	itemID, ok := downloadItemID(r.Method, rel)
	if !ok {
		return nil, errDownloadFallbackUnavailable
	}
	mediaSourceID := strings.TrimSpace(r.URL.Query().Get("MediaSourceId"))
	playback, err := s.fetchDownloadPlaybackInfo(r.Context(), itemID, mediaSourceID, upstream)
	if err != nil || playback.ErrorCode != nil {
		return nil, errDownloadFallbackUnavailable
	}
	source, ok := selectDownloadMediaSource(playback.MediaSources, mediaSourceID)
	if !ok {
		return nil, errDownloadFallbackUnavailable
	}
	mediaURL, err := s.downloadMediaURL(source.DirectStreamURL, itemID, source.ID, session, upstream, gatewayToken, s.gatewayBaseForRequest(r))
	if err != nil {
		return nil, errDownloadFallbackUnavailable
	}
	request, err := http.NewRequestWithContext(withRedirectCredentialTokens(r.Context(), gatewayToken, upstream.token), r.Method, mediaURL.String(), nil)
	if err != nil {
		return nil, errDownloadFallbackUnavailable
	}
	copyRequestHeaders(request.Header, r.Header)
	for name, value := range source.RequiredHTTPHeaders {
		if validDownloadRequiredHeader(name, value) {
			request.Header.Set(name, value)
		}
	}
	s.rewriteRequestHeaders(request.Header, upstream)
	request.Host = mediaURL.Host

	attemptStarted := time.Now()
	response, err := s.proxyClient.Do(request)
	if err != nil {
		s.emitUpstreamAttempt(attemptStarted, 0, err)
		closeResponseOnError(response)
		return nil, errDownloadFallbackUnavailable
	}
	s.emitUpstreamAttempt(attemptStarted, response.StatusCode, nil)
	if !downloadFallbackResponseAllowed(response.StatusCode) {
		_ = response.Body.Close()
		return nil, errDownloadFallbackUnavailable
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		response.Header.Set("Content-Disposition", downloadContentDisposition(source, itemID))
	}
	return response, nil
}

func (s *Server) fetchDownloadPlaybackInfo(ctx context.Context, itemID, mediaSourceID string, upstream upstreamRequestSnapshot) (embyPlaybackInfoResponseDTO, error) {
	payload, err := json.Marshal(embyPlaybackInfoRequestDTO{
		ID:                 itemID,
		UserID:             upstream.userID,
		MediaSourceID:      mediaSourceID,
		EnableDirectPlay:   true,
		EnableDirectStream: true,
		EnableTranscoding:  false,
		IsPlayback:         false,
	})
	if err != nil {
		return embyPlaybackInfoResponseDTO{}, err
	}
	target, err := backendURL(upstream.baseURL, "/Items/"+url.PathEscape(itemID)+"/PlaybackInfo")
	if err != nil {
		return embyPlaybackInfoResponseDTO{}, err
	}
	request, err := http.NewRequestWithContext(withRedirectCredentialTokens(ctx, upstream.token), http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return embyPlaybackInfoResponseDTO{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	s.rewriteRequestHeaders(request.Header, upstream)
	request.Host = request.URL.Host

	attemptStarted := time.Now()
	response, err := s.proxyClient.Do(request)
	if err != nil {
		s.emitUpstreamAttempt(attemptStarted, 0, err)
		closeResponseOnError(response)
		return embyPlaybackInfoResponseDTO{}, err
	}
	s.emitUpstreamAttempt(attemptStarted, response.StatusCode, nil)
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return embyPlaybackInfoResponseDTO{}, errDownloadFallbackUnavailable
	}
	data, err := readLimited(response.Body, proxyJSONLimit)
	if err != nil {
		return embyPlaybackInfoResponseDTO{}, err
	}
	var playback embyPlaybackInfoResponseDTO
	if err := json.Unmarshal(data, &playback); err != nil {
		return embyPlaybackInfoResponseDTO{}, err
	}
	return playback, nil
}

func selectDownloadMediaSource(sources []embyMediaSourceInfoDTO, requestedID string) (embyMediaSourceInfoDTO, bool) {
	for _, source := range sources {
		if requestedID != "" && source.ID != requestedID {
			continue
		}
		if strings.TrimSpace(source.DirectStreamURL) == "" || (!source.SupportsDirectPlay && !source.SupportsDirectStream) {
			if requestedID != "" {
				return embyMediaSourceInfoDTO{}, false
			}
			continue
		}
		return source, true
	}
	return embyMediaSourceInfoDTO{}, false
}

func (s *Server) downloadMediaURL(raw, itemID, mediaSourceID string, session *Session, upstream upstreamRequestSnapshot, gatewayToken, publicGatewayBase string) (*url.URL, error) {
	reference := rewriteMediaReference(raw, session, upstream, gatewayToken, publicGatewayBase, s.cfg.GatewayServerID, false)
	parsed, err := url.Parse(reference)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Fragment != "" {
		return nil, errDownloadFallbackUnavailable
	}
	mediaPath, ok := relativeMediaPath(parsed, publicGatewayBase)
	if !ok || mediaPath != parsed.EscapedPath() {
		return nil, errDownloadFallbackUnavailable
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(parts) < 3 || parts[1] != itemID {
		return nil, errDownloadFallbackUnavailable
	}
	if signedSourceID := parsed.Query().Get("MediaSourceId"); signedSourceID != "" && signedSourceID != mediaSourceID {
		return nil, errDownloadFallbackUnavailable
	}
	return s.proxyURL(upstream, session, parsed.Path, parsed.RawQuery, gatewayToken)
}

func downloadFallbackResponseAllowed(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices || status == http.StatusNotModified || status == http.StatusRequestedRangeNotSatisfiable
}

func validDownloadRequiredHeader(name, value string) bool {
	if name == "" || protectedDownloadRequestHeader(name) || strings.ContainsAny(value, "\r\n") {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

func protectedDownloadRequestHeader(name string) bool {
	if isHopHeader(name) {
		return true
	}
	switch strings.ToLower(name) {
	case "accept-encoding", "authorization", "content-length", "host", "if-match", "if-modified-since", "if-none-match", "if-range", "if-unmodified-since", "range", "x-emby-authorization", "x-emby-token", "x-mediabrowser-token":
		return true
	default:
		return false
	}
}

func downloadContentDisposition(source embyMediaSourceInfoDTO, itemID string) string {
	name := sanitizeDownloadFilename(source.Name)
	if name == "" {
		name = itemID
	}
	container := strings.TrimSpace(source.Container)
	if validDownloadContainer(container) && !strings.HasSuffix(strings.ToLower(name), "."+strings.ToLower(container)) {
		name += "." + container
	}
	if value := mime.FormatMediaType("attachment", map[string]string{"filename": name}); value != "" {
		return value
	}
	return "attachment"
}

func sanitizeDownloadFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	name = strings.TrimRight(name, " .")
	for len(name) > 220 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return strings.TrimRight(name, " .")
}

func validDownloadContainer(container string) bool {
	if container == "" || len(container) > 16 {
		return false
	}
	for _, r := range container {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
