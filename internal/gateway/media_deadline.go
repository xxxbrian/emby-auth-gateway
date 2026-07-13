package gateway

import (
	"net/http"
	"strings"
	"time"
)

func (s *Server) clearMediaWriteDeadline(w http.ResponseWriter, r *http.Request, rel string, resp *http.Response, session *Session) {
	if !isMediaStreamResponse(r, rel, resp) {
		return
	}
	s.clearMediaWriteDeadlineNow(w, r, rel, resp, session)
}

func (s *Server) clearMediaWriteDeadlineNow(w http.ResponseWriter, r *http.Request, rel string, resp *http.Response, session *Session) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil && s.mediaDeadlineWarning.CompareAndSwap(false, true) {
		s.audit(r.Context(), AuditLog{
			GatewayUserID:   sessionGatewayUserID(session),
			SyntheticUserID: sessionSyntheticUserID(session),
			Event:           "media_write_deadline_clear_failed",
			Message:         "unable to clear media stream write deadline",
			RemoteIP:        remoteIP(r),
			Method:          r.Method,
			Path:            rel,
			Status:          resp.StatusCode,
		})
	}
}

func isMediaStreamResponse(r *http.Request, rel string, resp *http.Response) bool {
	if !responseAllowsBody(r.Method, resp.StatusCode) {
		return false
	}
	contentType := resp.Header.Get("Content-Type")
	if isImageContentType(contentType) || isJSONContentType(contentType) || isM3U8ContentType(contentType) || responseIsM3U8(resp) {
		return false
	}
	if isStreamingContentType(contentType) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(rel), "/videos/") &&
		(strings.TrimSpace(r.Header.Get("Range")) != "" || resp.StatusCode == http.StatusPartialContent || strings.TrimSpace(resp.Header.Get("Content-Range")) != "")
}

func responseIsM3U8(resp *http.Response) bool {
	return resp.Request != nil && strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".m3u8")
}
