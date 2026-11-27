package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

func decodeEmbyJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if len(bytes.Trim(data[decoder.InputOffset():], " \t\r\n")) != 0 {
		return errors.New("trailing JSON data")
	}
	return nil
}

func (s *Server) tryWriteProxyJSON(w http.ResponseWriter, r *http.Request, rel string, resp *http.Response, session *Session, upstream upstreamRequestSnapshot, gatewayToken, publicGatewayBase string, data []byte, started time.Time) bool {
	var value any
	if err := decodeEmbyJSON(data, &value); err != nil {
		return false
	}
	oldPlayID := ""
	if s.isSubtitleWebRequest(r, session) {
		oldPlayID = subtitlePlaybackID(value)
		s.filterWebSubtitles(r.Context(), r, value, session, upstream, gatewayToken, true)
		r = r.WithContext(context.WithValue(r.Context(), subtitlesFilteredKey{}, true))
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		if err := s.negotiateAudioResponse(r, value, session, upstream, gatewayToken); err != nil {
			writeAudioError(w, err)
			return true
		}
	}
	if next := subtitlePlaybackID(value); s.cfg.Subtitles != nil && oldPlayID != "" && next != "" && next != oldPlayID {
		s.cfg.Subtitles.MovePlayback(session.GatewayTokenHash, oldPlayID, next)
		if options, _ := r.Context().Value(playbackOptionsKey{}).(*playbackOptions); options != nil && options.previousID != "" && options.previousID != next {
			s.cfg.Subtitles.MovePlayback(session.GatewayTokenHash, options.previousID, next)
		}
	}
	if s.meter != nil && len(data) > 0 {
		s.meter.AddIngress(int64(len(data)))
	}
	rewritten := s.rewriteProxyJSONValueForRequestWithSnapshot(r.Context(), r, value, session, upstream, gatewayToken, publicGatewayBase)
	payload, err := json.Marshal(rewritten)
	if err != nil {
		s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_rewrite_failed", AuditMessage: "proxy json encode failed", ClientBody: "response encode failed", FallbackKind: "upstream_read_error", Duration: time.Since(started), UpstreamStatus: resp.StatusCode})
		return true
	}
	payload = append(payload, '\n')
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = countEgressWrite(w, s.meter, nil, payload)
	return true
}
