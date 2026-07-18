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

	"github.com/pocketbase/pocketbase/core"
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
// Redirects are not followed automatically: a gateway root often 308s to
// /emby/web/ (HTML), which is not a valid Emby API base. Callers should use
// the Emby API base path (e.g. https://host/emby). See probeUpstreamPublic.
var NewUpstreamHTTPClient = func() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// ResolveBackendPassword returns a non-empty backend password for probe/reconfigure.
// When password is empty and an existing upstream source stores a password, that
// stored secret is reused only if targetBaseURL exactly matches the currently
// configured upstream endpoint base after NormalizeUpstreamURL (no path
// equivalence shortcuts). This prevents a compromised admin session from
// probing attacker URLs with the stored credential. Empty password with no
// reusable secret returns a clear error.
func ResolveBackendPassword(app core.App, password, targetBaseURL string) (string, error) {
	password = strings.TrimSpace(password)
	if password != "" {
		return password, nil
	}
	if app == nil {
		return "", fmt.Errorf("backend password is required")
	}
	state, err := LoadUpstreamStateForCreate(app)
	if err != nil {
		return "", err
	}
	if state.Source == nil {
		return "", fmt.Errorf("backend password is required (no stored upstream password to reuse)")
	}
	stored := strings.TrimSpace(state.Source.GetString("backend_password"))
	if stored == "" {
		return "", fmt.Errorf("backend password is required (no stored upstream password to reuse)")
	}
	target, err := NormalizeUpstreamURL(targetBaseURL)
	if err != nil {
		return "", err
	}
	// Prefer active endpoint base_url; fall back to any endpoint.
	var configured string
	for _, ep := range state.Endpoints {
		if ep == nil {
			continue
		}
		base := strings.TrimSpace(ep.GetString("base_url"))
		if base == "" {
			continue
		}
		if ep.GetBool("active") || configured == "" {
			configured = base
		}
		if ep.GetBool("active") {
			break
		}
	}
	if configured == "" {
		return "", fmt.Errorf("backend password is required (no configured upstream URL to match for reuse)")
	}
	cfgNorm, err := NormalizeUpstreamURL(configured)
	if err != nil {
		return "", fmt.Errorf("backend password is required (stored upstream URL invalid)")
	}
	// Exact normalized base only. Do not treat /emby as equivalent: different
	// path roots can route to different services and would leak credentials.
	if target != cfgNorm {
		return "", fmt.Errorf("backend password is required when probing a URL other than the configured upstream (%s)", cfgNorm)
	}
	return stored, nil
}

// ProbeUpstream performs a full upstream check: public system info plus AuthenticateByName.
// On success it returns server identity, backend user id, and latency. The temporary
// access token is logged out and never returned. Wrong credentials fail the probe.
// When password is empty, app may be used to reuse the stored upstream password
// (same as reconfigure); pass nil app to require an explicit password.
func ProbeUpstream(ctx context.Context, app core.App, in UpstreamReconfigureInput) (serverID, serverName, serverVersion, backendUserID string, latencyMS int64, err error) {
	baseURL, err := NormalizeUpstreamURL(in.EmbyBaseURL)
	if err != nil {
		return "", "", "", "", 0, err
	}
	username := strings.TrimSpace(in.BackendUsername)
	password, err := ResolveBackendPassword(app, in.BackendPassword, baseURL)
	if err != nil {
		return "", "", "", "", 0, err
	}
	if username == "" {
		return "", "", "", "", 0, fmt.Errorf("backend username is required")
	}
	identity := in.identity()
	deviceID, err := newBackendDeviceID()
	if err != nil {
		return "", "", "", "", 0, err
	}
	start := time.Now()
	probe, effectiveBase, err := probeUpstream(ctx, baseURL, username, password, deviceID, "", identity)
	latencyMS = time.Since(start).Milliseconds()
	if effectiveBase != "" {
		baseURL = effectiveBase
	}
	if probe.token != "" {
		_ = logoutUpstream(ctx, baseURL, identity, deviceID, probe.userID, probe.token)
	}
	if err != nil {
		return "", "", "", "", latencyMS, err
	}
	return probe.serverID, probe.serverName, probe.version, probe.userID, latencyMS, nil
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

func probeUpstream(ctx context.Context, baseURL, username, password, deviceID, expectedServerID string, identity gateway.BackendClientIdentity) (upstreamProbeResult, string, error) {
	client := NewUpstreamHTTPClient()
	public, publicID, effectiveBase, err := probeUpstreamPublic(ctx, client, baseURL, deviceID, expectedServerID, identity)
	if err != nil {
		return upstreamProbeResult{}, baseURL, err
	}
	result, err := authenticateUpstream(ctx, client, effectiveBase, username, password, deviceID, identity, public, publicID)
	return result, effectiveBase, err
}

// probeUpstreamPublic returns public info, server ID, and the effective base URL
// (may append /emby after a redirect-style failure on the bare host).
func probeUpstreamPublic(ctx context.Context, client *http.Client, baseURL, deviceID, expectedServerID string, identity gateway.BackendClientIdentity) (upstreamPublicInfo, string, string, error) {
	public := upstreamPublicInfo{}
	effective := baseURL
	err := UpstreamRequest(ctx, client, http.MethodGet, upstreamURL(baseURL, "/System/Info/Public"), nil, identity, deviceID, "", "", &public, false)
	if err != nil {
		// Common misconfig: gateway public origin without /emby API base.
		if alt := embyBaseRetryURL(baseURL, err); alt != "" && alt != baseURL {
			public = upstreamPublicInfo{}
			if err2 := UpstreamRequest(ctx, client, http.MethodGet, upstreamURL(alt, "/System/Info/Public"), nil, identity, deviceID, "", "", &public, false); err2 == nil {
				effective = alt
				err = nil
			} else {
				return public, "", baseURL, fmt.Errorf("public info probe: %w (also tried %s: %v)", err, alt, err2)
			}
		} else {
			return public, "", baseURL, fmt.Errorf("public info probe: %w", err)
		}
	}
	publicID := firstNonEmptyTrimmed(public.ID, public.ServerID)
	if publicID == "" {
		return public, "", effective, fmt.Errorf("public info probe: response missing server ID")
	}
	if expectedServerID != "" && publicID != strings.TrimSpace(expectedServerID) {
		return public, "", effective, fmt.Errorf("public info probe: server ID differs from the stored source")
	}
	return public, publicID, effective, nil
}

// embyBaseRetryURL suggests an alternate Emby API base when the first probe
// hits a redirect/HTML gateway root. Returns "" when no retry is warranted.
func embyBaseRetryURL(baseURL string, probeErr error) string {
	if probeErr == nil {
		return ""
	}
	msg := probeErr.Error()
	// Only retry on redirect / non-JSON failures typical of missing /emby.
	if !strings.Contains(msg, "308") && !strings.Contains(msg, "301") && !strings.Contains(msg, "302") && !strings.Contains(msg, "307") && !strings.Contains(msg, "invalid JSON") && !strings.Contains(msg, "redirect") {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return ""
	}
	path := strings.TrimRight(u.Path, "/")
	if path == "" {
		u.Path = "/emby"
		return u.String()
	}
	if !strings.HasSuffix(path, "/emby") {
		u.Path = path + "/emby"
		return u.String()
	}
	return ""
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
		if loc := strings.TrimSpace(resp.Header.Get("Location")); loc != "" && resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return fmt.Errorf("unexpected HTTP status %d (redirect to %s); if probing a gateway, use the Emby base path (e.g. https://host/emby)", resp.StatusCode, loc)
		}
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
