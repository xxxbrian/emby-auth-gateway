package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MediaSourceRef identifies a catalogue and its visibility scope. Authentication
// token rotations intentionally do not change it. No credentials are encoded.
func MediaSourceRef(serverID, backendUserID string) string {
	if serverID == "" || backendUserID == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("admin-media-v1\x00" + serverID + "\x00" + backendUserID))
	return hex.EncodeToString(digest[:16])
}

// AdminMediaSourceRef reads the current, pinned catalogue identity without
// logging in a synthetic gateway user or resolving personal playback data.
func (s *Server) AdminMediaSourceRef(ctx context.Context) (string, error) {
	runtime, err := s.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil {
		return "", err
	}
	if runtime == nil {
		return "", errors.New("media source unavailable")
	}
	ref := MediaSourceRef(runtime.Source.ServerID, runtime.Source.BackendUserID)
	if ref == "" {
		return "", errors.New("media source unavailable")
	}
	return ref, nil
}

// AdminMediaItems is a narrow read-only transport adapter. It shares the
// gateway authenticator, but never calls user-data resolution or persistence.
func (s *Server) AdminMediaItems(ctx context.Context, sourceRef string, ids []string, detail bool) ([]byte, int, error) {
	if len(ids) == 0 || len(ids) > 50 || (detail && len(ids) != 1) {
		return nil, 400, errors.New("invalid media IDs")
	}
	for _, id := range ids {
		if !validAdminMediaID(id) {
			return nil, 400, errors.New("invalid media ID")
		}
	}
	returnBody, _, status, err := s.adminMediaRead(ctx, sourceRef, func(up upstreamRequestSnapshot) (string, url.Values) {
		path := "/Users/" + url.PathEscape(up.userID) + "/Items"
		// Season/episode and parent image references are standard DTO fields.
		// Fields accepts ItemFields enum names, not arbitrary DTO properties.
		fields := "PrimaryImageAspectRatio"
		q := url.Values{"Ids": {strings.Join(ids, ",")}, "Recursive": {"true"}, "EnableUserData": {"false"}, "EnableImages": {"true"}, "ImageTypeLimit": {"1"}, "Limit": {strconv.Itoa(len(ids))}}
		if detail {
			path += "/" + url.PathEscape(ids[0])
			// The dedicated item endpoint returns the full BaseItemDto; its
			// supported parameters are the two path values. Projection is local.
			return path, nil
		}
		q.Set("Fields", fields)
		return path, q
	}, 2<<20)
	return returnBody, status, err
}

// AdminMediaImage accepts only bounded, fixed Emby image endpoints. URLs and
// upstream credentials never cross this adapter's public interface.
func (s *Server) AdminMediaImage(ctx context.Context, sourceRef, id, imageType, size string) ([]byte, string, int, error) {
	if !validAdminMediaID(id) || (imageType != "Primary" && imageType != "Thumb" && imageType != "Backdrop") {
		return nil, "", 400, errors.New("invalid image")
	}
	width := "160"
	if size == "large" {
		width = "720"
	} else if size != "small" {
		return nil, "", 400, errors.New("invalid image size")
	}
	return s.adminMediaRead(ctx, sourceRef, func(up upstreamRequestSnapshot) (string, url.Values) {
		return "/Items/" + url.PathEscape(id) + "/Images/" + imageType, url.Values{"MaxWidth": {width}, "Quality": {"85"}, "Format": {"jpg"}}
	}, 4<<20)
}

func validAdminMediaID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *Server) adminMediaRead(ctx context.Context, sourceRef string, endpoint func(upstreamRequestSnapshot) (string, url.Values), limit int64) ([]byte, string, int, error) {
	// Verify before Ensure: stale references must not even initiate a login to
	// a different source. Verify again after every credential refresh/read.
	current, err := s.AdminMediaSourceRef(ctx)
	if err != nil {
		return nil, "", 503, errors.New("media source unavailable")
	}
	if sourceRef == "" || current != sourceRef {
		return nil, "", 409, nil
	}
	runtime, err := s.upstreamAuth.Ensure(ctx)
	if err != nil {
		return nil, "", 503, errors.New("media authentication unavailable")
	}
	up, err := upstreamRequestSnapshotFromRuntime(runtime)
	if err != nil {
		return nil, "", 503, errors.New("media source unavailable")
	}
	client := *s.client
	client.Jar = nil
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || (len(via) > 0 && !sameOrigin(req.URL, via[0].URL)) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		if MediaSourceRef(up.serverID, up.userID) != sourceRef {
			return nil, "", 409, nil
		}
		path, query := endpoint(up)
		rawURL, err := backendURL(up.baseURL, path)
		if err != nil {
			return nil, "", 503, errors.New("media source unavailable")
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, "", 503, errors.New("media source unavailable")
		}
		u.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, "", 503, errors.New("media request unavailable")
		}
		req.Header.Set("User-Agent", up.identity.UserAgent)
		req.Header.Set("X-Emby-Token", up.token)
		req.Header.Set("X-Emby-Authorization", backendAuthHeader(up.identity, up.userID, up.token).String())
		resp, err := client.Do(req)
		if err != nil {
			return nil, "", 503, errors.New("media request failed")
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			up, _, err = s.refreshAfterUnauthorized(ctx, up)
			if err != nil {
				return nil, "", 503, errors.New("media authentication unavailable")
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if readErr != nil || int64(len(data)) > limit {
			return nil, "", 503, errors.New("media response unreadable or too large")
		}
		current, err = s.AdminMediaSourceRef(ctx)
		if err != nil {
			return nil, "", 503, errors.New("media source unavailable")
		}
		if current != sourceRef {
			return nil, "", 409, nil
		}
		return data, resp.Header.Get("Content-Type"), resp.StatusCode, nil
	}
	return nil, "", 503, errors.New("media authentication unavailable")
}
