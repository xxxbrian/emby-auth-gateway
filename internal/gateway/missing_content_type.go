package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const missingContentTypeSniffSize = 512

func (s *Server) writeMissingContentTypeResponse(w http.ResponseWriter, r *http.Request, rel string, resp *http.Response, session *Session, gatewayToken, publicGatewayBase string) {
	started := time.Now()
	reader, jsonCandidate, err := sniffMissingContentType(resp.Body)
	if err != nil {
		s.handlePreHeaderProxyFailure(w, r, rel, session, err, proxyFailureDetails{Event: "proxy_read_failed", AuditMessage: "backend response read failed", ClientBody: "response read failed", FallbackKind: "upstream_read_error", Duration: time.Since(started), UpstreamStatus: resp.StatusCode})
		return
	}
	if jsonCandidate {
		data, readErr := readLimited(reader, proxyJSONLimit)
		if readErr != nil {
			s.handlePreHeaderProxyFailure(w, r, rel, session, readErr, proxyFailureDetails{Event: "proxy_read_failed", AuditMessage: "backend response read failed", ClientBody: "response read failed", FallbackKind: "upstream_read_error", Duration: time.Since(started), UpstreamStatus: resp.StatusCode})
			return
		}
		var value any
		if json.Unmarshal(data, &value) == nil {
			w.Header().Del("Content-Length")
			writeJSON(w, resp.StatusCode, s.rewriteProxyJSONValueForRequest(r.Context(), r, value, session, gatewayToken, publicGatewayBase))
			return
		}
		reader = bufio.NewReader(bytes.NewReader(data))
	}

	s.clearMediaWriteDeadlineNow(w, r, rel, resp, session)
	setContentLength(w.Header(), resp.ContentLength)
	w.WriteHeader(resp.StatusCode)
	s.copyMediaReaderOrAbort(w, r, rel, reader, resp.ContentLength, resp.StatusCode, session)
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
