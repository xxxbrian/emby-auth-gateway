package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) RefreshBackendServerInfo(ctx context.Context) error {
	servers, err := s.store.ListEnabledServers(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, server := range servers {
		if err := s.refreshOneBackendServerInfo(ctx, server); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Server) refreshOneBackendServerInfo(ctx context.Context, server EmbyServer) error {
	u, err := backendURL(server.BaseURL, "/System/Info/Public")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	identity := server.ClientIdentity.WithDefaults()
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("X-Emby-Authorization", backendAuthHeader(identity, "", "").String())
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return err
	}
	serverID, _ := raw["Id"].(string)
	if serverID == "" {
		serverID, _ = raw["ServerId"].(string)
	}
	serverName, _ := raw["ServerName"].(string)
	version, _ := raw["Version"].(string)
	if strings.TrimSpace(version) == "" {
		return nil
	}
	return s.store.UpdateServerInfo(ctx, server.ID, serverID, serverName, version, time.Now().UTC())
}

func highestServerVersion(servers []EmbyServer) string {
	best := ""
	for _, server := range servers {
		version := strings.TrimSpace(server.ServerVersion)
		if version == "" {
			continue
		}
		if best == "" || compareVersions(version, best) > 0 {
			best = version
		}
	}
	return best
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
	aRelease := isPlainNumericVersion(a)
	bRelease := isPlainNumericVersion(b)
	if aRelease && !bRelease {
		return 1
	}
	if !aRelease && bRelease {
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

func atoiZero(value string) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}
