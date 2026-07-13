package gateway

import (
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	mediaCopyBufferSize      = 32 << 10
	mediaDirectionUpstream   = "upstream"
	mediaDirectionDownstream = "downstream"
)

var errMediaLengthMismatch = errors.New("upstream media length mismatch")

type mediaCopyResult struct {
	BytesRead    int64
	BytesWritten int64
	Direction    string
	Duration     time.Duration
	Err          error
}

func copyMediaBody(dst io.Writer, src io.Reader, expectedLength int64) mediaCopyResult {
	started := time.Now()
	result := mediaCopyResult{}
	finish := func(direction string, err error) mediaCopyResult {
		result.Direction = direction
		result.Duration = time.Since(started)
		result.Err = err
		return result
	}

	buffer := make([]byte, mediaCopyBufferSize)
	emptyReads := 0
	for {
		n, readErr := src.Read(buffer)
		if n < 0 || n > len(buffer) {
			return finish(mediaDirectionUpstream, errors.New("invalid upstream media read"))
		}
		if n > 0 {
			emptyReads = 0
			result.BytesRead += int64(n)
			writeLength := n
			lengthMismatch := false
			if expectedLength >= 0 && result.BytesRead > expectedLength {
				writeLength = n - int(result.BytesRead-expectedLength)
				lengthMismatch = true
			}
			if writeLength > 0 {
				written, writeErr := dst.Write(buffer[:writeLength])
				if written < 0 || written > writeLength {
					return finish(mediaDirectionDownstream, errors.New("invalid downstream media write"))
				}
				result.BytesWritten += int64(written)
				if writeErr != nil {
					return finish(mediaDirectionDownstream, writeErr)
				}
				if written < writeLength {
					return finish(mediaDirectionDownstream, io.ErrShortWrite)
				}
			}
			if lengthMismatch {
				return finish(mediaDirectionUpstream, errMediaLengthMismatch)
			}
		} else if readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return finish(mediaDirectionUpstream, io.ErrNoProgress)
			}
		}

		if readErr == nil {
			continue
		}
		if readErr != io.EOF {
			return finish(mediaDirectionUpstream, readErr)
		}
		if expectedLength >= 0 && result.BytesRead < expectedLength {
			return finish(mediaDirectionUpstream, io.ErrUnexpectedEOF)
		}
		return finish("", nil)
	}
}

func (s *Server) auditMediaCopyFailure(r *http.Request, rel string, upstreamStatus int, session *Session, result mediaCopyResult) {
	if result.Direction == mediaDirectionDownstream && !isTimeoutError(result.Err) {
		return
	}
	event := "proxy_media_upstream_failed"
	message := "upstream media stream failed"
	errorKind := "upstream_read_error"
	if result.Direction == mediaDirectionDownstream {
		event = "proxy_media_downstream_timeout"
		message = "downstream media stream timed out"
		errorKind = "downstream_timeout"
	} else if errors.Is(result.Err, io.ErrUnexpectedEOF) {
		errorKind = "upstream_unexpected_eof"
	} else if errors.Is(result.Err, errMediaLengthMismatch) {
		errorKind = "upstream_length_mismatch"
	}
	s.audit(r.Context(), AuditLog{
		GatewayUserID:     sessionGatewayUserID(session),
		SyntheticUserID:   sessionSyntheticUserID(session),
		Event:             event,
		Message:           message,
		RemoteIP:          remoteIP(r),
		Method:            r.Method,
		Path:              rel,
		Status:            upstreamStatus,
		ErrorKind:         errorKind,
		Direction:         result.Direction,
		BytesTransferred:  result.BytesWritten,
		DurationMS:        int(result.Duration.Milliseconds()),
		UpstreamStatus:    upstreamStatus,
		ResponseCommitted: true,
	})
}
