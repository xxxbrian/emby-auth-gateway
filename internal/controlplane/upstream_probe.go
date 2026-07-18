package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

const upstreamResponseLimit = 1 << 20

// cleanupGraceTimeout is the maximum time a logout may outlive a cancelled
// setup operation. It is deliberately shorter than the normal request limit.
const cleanupGraceTimeout = 2 * time.Second

type upstreamPublicInfo struct {
	ID       string `json:"Id"`
	ServerID string `json:"ServerId"`
	Name     string `json:"ServerName"`
	Version  string `json:"Version"`
}

type upstreamAuthInfo struct {
	AccessToken string `json:"AccessToken"`
	ServerID    string `json:"ServerId"`
	ServerName  string `json:"ServerName"`
	Version     string `json:"Version"`
	User        struct {
		ID string `json:"Id"`
	} `json:"User"`
}

type upstreamProbeResult struct {
	serverID, serverName, version, token, userID string
}

// NewUpstreamHTTPClient is a test hook for injecting HTTP clients.
var NewUpstreamHTTPClient = func() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ProbeUpstream probes the public Emby system info endpoint and returns identity plus latency.
func ProbeUpstream(ctx context.Context, in UpstreamReconfigureInput) (serverID, serverName, serverVersion string, latencyMS int64, err error) {
	baseURL, err := NormalizeUpstreamURL(in.EmbyBaseURL)
	if err != nil {
		return "", "", "", 0, err
	}
	identity := in.identity()
	deviceID, err := newBackendDeviceID()
	if err != nil {
		return "", "", "", 0, err
	}
	start := time.Now()
	public, publicID, err := probeUpstreamPublic(ctx, NewUpstreamHTTPClient(), baseURL, deviceID, "", identity)
	latencyMS = time.Since(start).Milliseconds()
	if err != nil {
		return "", "", "", latencyMS, err
	}
	return publicID, public.Name, public.Version, latencyMS, nil
}

// NormalizeUpstreamURL validates and normalizes an Emby base URL.
func NormalizeUpstreamURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("--emby-url must be an absolute http(s) URL without userinfo, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func embyAuthorization(identity gateway.BackendClientIdentity, deviceID, userID, token string) string {
	identity = identity.WithDefaults()
	return gateway.AuthHeader{Scheme: "Emby", Client: identity.Client, Device: identity.Device, DeviceID: deviceID, Version: identity.Version, UserID: userID, Token: token}.String()
}

func upstreamURL(baseURL, suffix string) string { return strings.TrimRight(baseURL, "/") + suffix }

func probeUpstream(ctx context.Context, baseURL, username, password, deviceID, expectedServerID string, identity gateway.BackendClientIdentity) (upstreamProbeResult, error) {
	client := NewUpstreamHTTPClient()
	public, publicID, err := probeUpstreamPublic(ctx, client, baseURL, deviceID, expectedServerID, identity)
	if err != nil {
		return upstreamProbeResult{}, err
	}
	return authenticateUpstream(ctx, client, baseURL, username, password, deviceID, identity, public, publicID)
}

func probeUpstreamPublic(ctx context.Context, client *http.Client, baseURL, deviceID, expectedServerID string, identity gateway.BackendClientIdentity) (upstreamPublicInfo, string, error) {
	public := upstreamPublicInfo{}
	if err := UpstreamRequest(ctx, client, http.MethodGet, upstreamURL(baseURL, "/System/Info/Public"), nil, identity, deviceID, "", "", &public, false); err != nil {
		return public, "", fmt.Errorf("public info probe: %w", err)
	}
	publicID := firstNonEmptyTrimmed(public.ID, public.ServerID)
	if publicID == "" {
		return public, "", fmt.Errorf("public info probe: response missing server ID")
	}
	if expectedServerID != "" && publicID != strings.TrimSpace(expectedServerID) {
		return public, "", fmt.Errorf("public info probe: server ID differs from the stored source")
	}
	return public, publicID, nil
}

func authenticateUpstream(ctx context.Context, client *http.Client, baseURL, username, password, deviceID string, identity gateway.BackendClientIdentity, public upstreamPublicInfo, publicID string) (upstreamProbeResult, error) {
	body, err := json.Marshal(map[string]string{"Username": username, "Pw": password})
	if err != nil {
		return upstreamProbeResult{}, fmt.Errorf("encode authentication request: %w", err)
	}
	var rawAuth json.RawMessage
	if err := UpstreamRequest(ctx, client, http.MethodPost, upstreamURL(baseURL, "/Users/AuthenticateByName"), body, identity, deviceID, "", "", &rawAuth, false); err != nil {
		return upstreamProbeResult{}, fmt.Errorf("authentication probe: %w", err)
	}
	result, auth, err := decodeUpstreamAuth(rawAuth)
	if err != nil {
		return result, fmt.Errorf("authentication probe: invalid response")
	}
	if strings.TrimSpace(auth.AccessToken) == "" || auth.AccessToken != strings.TrimSpace(auth.AccessToken) || strings.TrimSpace(auth.User.ID) == "" || auth.User.ID != strings.TrimSpace(auth.User.ID) || strings.TrimSpace(auth.ServerID) == "" {
		return result, fmt.Errorf("authentication probe: response missing required authentication fields")
	}
	authServerID := strings.TrimSpace(auth.ServerID)
	if authServerID != publicID {
		return result, fmt.Errorf("authentication probe: server ID does not match public info")
	}
	result.serverID, result.serverName, result.version = publicID, firstNonEmpty(auth.ServerName, public.Name), firstNonEmpty(auth.Version, public.Version)
	return result, nil
}

// decodeUpstreamAuth extracts AccessToken independently so callers can revoke
// a valid token even when unrelated response fields have incompatible shapes.
func decodeUpstreamAuth(raw json.RawMessage) (upstreamProbeResult, upstreamAuthInfo, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return upstreamProbeResult{}, upstreamAuthInfo{}, err
	}
	result := upstreamProbeResult{}
	if token, ok := fields["AccessToken"]; ok {
		_ = json.Unmarshal(token, &result.token)
	}
	var auth upstreamAuthInfo
	if err := json.Unmarshal(raw, &auth); err != nil {
		return result, auth, err
	}
	result.userID = auth.User.ID
	return result, auth, nil
}

// UpstreamRequest performs an Emby HTTP request (exported for tests via pbsetup wrappers).
func UpstreamRequest(ctx context.Context, client *http.Client, method, endpoint string, body []byte, identity gateway.BackendClientIdentity, deviceID, userID, token string, output any, allowEmptySuccess bool) error {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", identity.WithDefaults().UserAgent)
	req.Header.Set("X-Emby-Authorization", embyAuthorization(identity, deviceID, userID, token))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, upstreamResponseLimit+1))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if len(data) > upstreamResponseLimit {
		return fmt.Errorf("response exceeds 1 MiB limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	if allowEmptySuccess && len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("invalid JSON response: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
