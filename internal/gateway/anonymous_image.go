package gateway

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const anonymousImageBodyLimit = 32 << 20
const anonymousImageValidationConcurrency = 4

func isAnonymousItemImageRoute(r *http.Request, rel string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !canonicalResourceRoute(r, rel) {
		return false
	}
	parts := strings.Split(rel[1:], "/")
	if len(parts) != 4 && len(parts) != 5 {
		return false
	}
	if !strings.EqualFold(parts[0], "Items") || parts[1] == "" || !strings.EqualFold(parts[2], "Images") || parts[3] == "" {
		return false
	}
	if len(parts) == 5 {
		if parts[4] == "" {
			return false
		}
		for _, r := range parts[4] {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func hasReservedResourceCookie(r *http.Request) bool {
	for _, value := range r.Header.Values("Cookie") {
		for _, part := range strings.Split(value, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name == resourceCookieName {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleAnonymousItemImage(w http.ResponseWriter, r *http.Request, rel string) {
	w.Header().Set("Cache-Control", "no-store")
	origin, ok := s.ValidatedAnonymousImageOrigin(r.Context())
	if !ok {
		http.Error(w, "anonymous image service unavailable", http.StatusServiceUnavailable)
		return
	}
	query, err := anonymousImageQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u, err := backendURL(origin.BaseURL, rel)
	if err != nil {
		http.Error(w, "anonymous image service unavailable", http.StatusServiceUnavailable)
		return
	}
	upstreamURL, err := url.Parse(u)
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	upstreamURL.RawQuery = query
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	copyAnonymousImageRequestHeaders(req.Header, r.Header)
	identity := origin.ClientIdentity.WithDefaults()
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("X-Emby-Authorization", backendAuthHeader(identity, "", "").String())
	client := *s.client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		writeAnonymousImageError(w, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	s.writeAnonymousImageResponse(w, r, resp)
}

func anonymousImageQuery(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	parts := strings.Split(raw, "&")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		key, value, hasValue := strings.Cut(part, "=")
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			return "", errMalformedQuery
		}
		if isStrictQueryAuthKey(decodedKey) || decodedKey == genericQueryAuthKey {
			continue
		}
		if strings.EqualFold(decodedKey, "UserId") || strings.EqualFold(decodedKey, "ServerId") {
			if hasValue {
				decodedValue, err := url.QueryUnescape(value)
				if err != nil {
					return "", errMalformedQuery
				}
				if strings.TrimSpace(decodedValue) != "" {
					return "", errors.New("identity selector")
				}
			}
			continue
		}
		if hasValue {
			if _, err := url.QueryUnescape(value); err != nil {
				return "", errMalformedQuery
			}
		}
		out = append(out, part)
	}
	return strings.Join(out, "&"), nil
}

func copyAnonymousImageRequestHeaders(dst, src http.Header) {
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since", "Accept"} {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}

func (s *Server) writeAnonymousImageResponse(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	w.Header().Set("Cache-Control", "no-store")
	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest && resp.StatusCode != http.StatusNotModified {
		writeAnonymousImageError(w, http.StatusBadGateway)
		return
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		copyAnonymousImageResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(http.StatusNotModified)
		return
	case http.StatusOK:
		s.writeAnonymousFullImage(w, r, resp)
		return
	case http.StatusPartialContent:
		s.writeAnonymousPartialImage(w, r, resp)
		return
	default:
		w.WriteHeader(resp.StatusCode)
		return
	}
}

func (s *Server) writeAnonymousFullImage(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	if !isImageContentType(resp.Header.Get("Content-Type")) {
		writeAnonymousImageError(w, http.StatusBadGateway)
		return
	}
	if r.Method == http.MethodHead {
		copyAnonymousImageResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(http.StatusOK)
		return
	}
	select {
	case s.anonymousImageSlots <- struct{}{}:
		defer func() { <-s.anonymousImageSlots }()
	default:
		writeAnonymousImageError(w, http.StatusServiceUnavailable)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, anonymousImageBodyLimit+1))
	if err != nil || len(data) == 0 || len(data) > anonymousImageBodyLimit {
		writeAnonymousImageError(w, http.StatusBadGateway)
		return
	}
	if !validAnonymousFullImage(data, resp.Header.Get("Content-Type")) {
		writeAnonymousImageError(w, http.StatusBadGateway)
		return
	}
	copyAnonymousImageResponseHeaders(w.Header(), resp.Header)
	setContentLength(w.Header(), int64(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) writeAnonymousPartialImage(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	expectedLength, ok := anonymousContentRangeLength(resp.Header.Get("Content-Range"), resp.ContentLength)
	if !isImageContentType(resp.Header.Get("Content-Type")) || !ok {
		writeAnonymousImageError(w, http.StatusBadGateway)
		return
	}
	if r.Method == http.MethodHead {
		copyAnonymousImageResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(http.StatusPartialContent)
		return
	}
	var first [1]byte
	n, err := resp.Body.Read(first[:])
	if (err != nil && err != io.EOF) || n != 1 {
		writeAnonymousImageError(w, http.StatusBadGateway)
		return
	}
	copyAnonymousImageResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusPartialContent)
	s.copyMediaReaderOrAbort(w, r, r.URL.Path, io.MultiReader(bytes.NewReader(first[:]), resp.Body), expectedLength, http.StatusPartialContent, nil)
}

func validAnonymousContentRange(value string, contentLength int64) bool {
	_, ok := anonymousContentRangeLength(value, contentLength)
	return ok
}

func anonymousContentRangeLength(value string, contentLength int64) (int64, bool) {
	parts := strings.Fields(value)
	if len(parts) != 2 || parts[0] != "bytes" {
		return 0, false
	}
	rangeAndTotal := strings.Split(parts[1], "/")
	if len(rangeAndTotal) != 2 {
		return 0, false
	}
	rangeParts := strings.Split(rangeAndTotal[0], "-")
	if len(rangeParts) != 2 {
		return 0, false
	}
	start, err := strconv.ParseInt(rangeParts[0], 10, 64)
	if err != nil || start < 0 {
		return 0, false
	}
	end, err := strconv.ParseInt(rangeParts[1], 10, 64)
	if err != nil || end < start {
		return 0, false
	}
	if rangeAndTotal[1] != "*" {
		total, err := strconv.ParseInt(rangeAndTotal[1], 10, 64)
		if err != nil || total <= end {
			return 0, false
		}
	}
	expectedLength := end - start + 1
	if contentLength >= 0 && expectedLength != contentLength {
		return 0, false
	}
	return expectedLength, true
}

func validAnonymousFullImage(data []byte, contentType string) bool {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch mediaType {
	case "image/jpeg":
		return len(data) >= 4 && data[0] == 0xff && data[1] == 0xd8 && data[len(data)-2] == 0xff && data[len(data)-1] == 0xd9
	case "image/png":
		return len(data) >= 20 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) && bytes.Equal(data[len(data)-12:], []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82})
	case "image/webp":
		return len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" && int64(binary.LittleEndian.Uint32(data[4:8]))+8 == int64(len(data))
	case "image/gif":
		return len(data) >= 14 && (bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))) && data[len(data)-1] == 0x3b
	default:
		return false
	}
}

func copyAnonymousImageResponseHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "ETag", "Last-Modified", "Accept-Ranges", "Content-Range", "Content-Disposition"} {
		for _, value := range src.Values(name) {
			if name == "Content-Disposition" && (strings.ContainsAny(value, "\r\n") || strings.Contains(strings.ToLower(value), "filename*=")) {
				continue
			}
			dst.Add(name, value)
		}
	}
	if value := src.Get("Content-Length"); value != "" {
		dst.Set("Content-Length", value)
	}
}

func writeAnonymousImageError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("Content-Length")
	http.Error(w, http.StatusText(status), status)
}
