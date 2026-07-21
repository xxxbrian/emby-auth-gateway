package gateway

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

const missingContentTypeSniffSize = 512

func (s *Server) writeMissingContentTypeResponse(w http.ResponseWriter, r *http.Request, rel string, resp *http.Response, session *Session, upstream upstreamRequestSnapshot, gatewayToken, publicGatewayBase string, projection responseProjection, plan *responseHeaderPlan, registration *negotiationLeaseRegistration) {
	started := time.Now()
	data, err := readLimited(resp.Body, proxyJSONLimit)
	if err != nil {
		resetProjectionFailureHeaders(w.Header())
		s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_read_failed", AuditMessage: "backend response read failed", ClientBody: "response read failed", FallbackKind: "upstream_read_error", Duration: time.Since(started), UpstreamStatus: resp.StatusCode})
		return
	}
	if s.meter != nil && len(data) > 0 {
		s.meter.AddIngress(int64(len(data)))
	}
	header := plan.Header()
	commit := func() bool {
		if registration != nil {
			if err := registration.Commit(); err != nil {
				resetProjectionFailureHeaders(w.Header())
				s.writeUpstreamRequestDenied(w, r, rel, session, routeclass.Classify(r.Method, rel), err)
				return false
			}
		}
		plan.Commit(w.Header())
		return true
	}
	if projection.kind == responseProjectionOpaque {
		if looksLikeJSON(data) {
			err = validateCredentialSafeOpaqueJSON(data, proxyJSONLimit, upstream.token)
		} else {
			err = validateCredentialSafeText(data, proxyJSONLimit, upstream.token)
		}
		if err != nil {
			s.writeResponseProjectionFailure(w, r, rel, session)
			return
		}
		setContentLength(header, int64(len(data)))
		if !commit() {
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = countEgressWrite(w, s.meter, nil, data)
		return
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if !commit() {
			return
		}
		w.WriteHeader(resp.StatusCode)
		return
	}
	projectionContext, err := s.responseProjectionContextForDocument(r.Context(), r, session, upstream, gatewayToken, publicGatewayBase, data, projection)
	if err != nil {
		s.writeResponseProjectionFailure(w, r, rel, session)
		return
	}
	projected, err := projectResponseDocument(data, projection, projectionContext)
	if err != nil {
		s.writeResponseProjectionFailure(w, r, rel, session)
		return
	}
	clearProjectedEntityHeaders(header)
	header.Set("Content-Type", "application/json")
	if !commit() {
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = countEgressWrite(w, s.meter, nil, appendJSONNewline(projected))
}

func sniffMissingContentType(body io.Reader) (*bufio.Reader, bool, error) {
	reader := bufio.NewReaderSize(body, missingContentTypeSniffSize)
	for index := 1; index <= missingContentTypeSniffSize; index++ {
		data, err := reader.Peek(index)
		if len(data) >= index {
			switch data[index-1] {
			case ' ', '\t', '\r', '\n':
				// Keep looking for a JSON container marker.
			case '{', '[':
				return reader, true, nil
			default:
				return reader, false, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return reader, false, nil
			}
			return reader, false, err
		}
	}
	return reader, false, nil
}
