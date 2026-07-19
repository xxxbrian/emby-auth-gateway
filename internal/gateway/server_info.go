package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const publicInfoProbeTimeout = 10 * time.Second
const publicInfoProbeBodyLimit = 1 << 20

type publicInfoMetadata struct {
	ServerID string
	Name     string
	Version  string
}

// probeUpstreamPublic uses only the persisted client identity; it deliberately
// never carries managed-auth credentials, a user identity, or cookies.
func (s *Server) probeUpstreamPublic(ctx context.Context, runtime *UpstreamRuntime) (publicInfoMetadata, error) {
	if runtime == nil {
		return publicInfoMetadata{}, errors.New("public upstream probe unavailable")
	}
	if err := ValidateUpstreamRuntime(*runtime); err != nil {
		return publicInfoMetadata{}, errors.New("public upstream probe unavailable")
	}
	probeCtx, cancel := context.WithTimeout(ctx, publicInfoProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "/System/Info/Public", nil)
	if err != nil {
		return publicInfoMetadata{}, errors.New("public upstream probe unavailable")
	}
	snapshot := upstreamRequestSnapshot{baseURL: runtime.Endpoint.BaseURL, serverID: runtime.Source.ServerID, identity: runtime.Source.ClientIdentity}
	resp, err := s.metadataUpstream.RoundTripMetadata(metadataUpstreamRequest{upstreamHTTPRequest: upstreamHTTPRequest{Request: req, Snapshot: snapshot}, Internal: true, Public: true})
	if err != nil {
		return publicInfoMetadata{}, errors.New("public upstream probe unavailable")
	}
	owner := wrapResponseBodyOnce(resp)
	defer owner.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return publicInfoMetadata{}, errors.New("public upstream probe unavailable")
	}
	var body struct {
		ID         string `json:"Id"`
		ServerName string `json:"ServerName"`
		Version    string `json:"Version"`
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, publicInfoProbeBodyLimit+1))
	if err != nil || len(payload) > publicInfoProbeBodyLimit || json.Unmarshal(payload, &body) != nil || strings.TrimSpace(body.ID) == "" || body.ID != strings.TrimSpace(body.ID) {
		return publicInfoMetadata{}, errors.New("public upstream probe unavailable")
	}
	if body.ID != runtime.Source.ServerID {
		return publicInfoMetadata{}, ErrUpstreamServerInfoConflict
	}
	return publicInfoMetadata{ServerID: body.ID, Name: strings.TrimSpace(body.ServerName), Version: strings.TrimSpace(body.Version)}, nil
}

// RefreshUpstreamServerInfo refreshes only the configured singleton metadata.
// A probe error leaves authentication and ordinary proxy service untouched.
func (s *Server) RefreshUpstreamServerInfo(ctx context.Context) error {
	var refreshErr error
	runtime, err := s.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil {
		refreshErr = err
	} else if metadata, err := s.probeUpstreamPublic(ctx, runtime); err != nil {
		refreshErr = err
	} else if err := s.store.UpdateUpstreamServerInfo(ctx, UpstreamServerInfoUpdate{
		SourceID: runtime.Source.ID, ServerID: runtime.Source.ServerID, ServerName: metadata.Name, ServerVersion: metadata.Version, CheckedAt: time.Now().UTC(),
	}); err != nil {
		refreshErr = err
	}
	validationErr := s.ValidateAnonymousImageNamespace(ctx)
	if refreshErr != nil && validationErr != nil {
		return errors.Join(refreshErr, validationErr)
	}
	if refreshErr != nil {
		return refreshErr
	}
	return validationErr
}

func compareVersions(a, b string) int {
	aparts := versionParts(a)
	bparts := versionParts(b)
	max := len(aparts)
	if len(bparts) > max {
		max = len(bparts)
	}
	for i := 0; i < max; i++ {
		av, bv := 0, 0
		if i < len(aparts) {
			av = aparts[i]
		}
		if i < len(bparts) {
			bv = bparts[i]
		}
		if av > bv {
			return 1
		}
		if av < bv {
			return -1
		}
	}
	if isPlainNumericVersion(a) && !isPlainNumericVersion(b) {
		return 1
	}
	if !isPlainNumericVersion(a) && isPlainNumericVersion(b) {
		return -1
	}
	return strings.Compare(a, b)
}
func isPlainNumericVersion(version string) bool {
	version = strings.TrimSpace(version)
	if version == "" {
		return false
	}
	for _, r := range version {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}
func versionParts(version string) []int {
	version = numericVersionCore(version)
	parts := []int{}
	start := -1
	for i, r := range version {
		if r >= '0' && r <= '9' {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			parts = append(parts, atoiZero(version[start:i]))
			start = -1
		}
	}
	if start >= 0 {
		parts = append(parts, atoiZero(version[start:]))
	}
	return parts
}
func numericVersionCore(version string) string {
	version = strings.TrimSpace(version)
	for i, r := range version {
		if (r < '0' || r > '9') && r != '.' {
			return version[:i]
		}
	}
	return version
}
func atoiZero(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}
