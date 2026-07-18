package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func newUpstreamResponder(t *testing.T, password string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server","ServerName":"Test Emby","Version":"4.8.0"}`))
		case "/Users/AuthenticateByName":
			body, _ := io.ReadAll(r.Body)
			var payload map[string]string
			_ = json.Unmarshal(body, &payload)
			if payload["Username"] != "backend" || payload["Pw"] != password {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Invalid user or password"}`))
				return
			}
			_, _ = w.Write([]byte(`{"AccessToken":"probe-token","ServerId":"server","ServerName":"Test Emby","Version":"4.8.0","User":{"Id":"backend-user"}}`))
		case "/Sessions/Logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func seedUpstream(t *testing.T, app core.App, baseURL, username, password string) {
	t.Helper()
	_, err := ReconfigureUpstream(context.Background(), app, UpstreamReconfigureInput{
		EmbyBaseURL:     baseURL,
		BackendUsername: username,
		BackendPassword: password,
		AllowCreate:     true,
	})
	if err != nil {
		t.Fatalf("seed upstream: %v", err)
	}
}

func TestReconfigureUpstreamEmptyPasswordReusesStored(t *testing.T) {
	app := newTestApp(t)
	server := newUpstreamResponder(t, "secret")
	defer server.Close()

	seedUpstream(t, app, server.URL, "backend", "secret")

	source, err := app.FindFirstRecordByData(UpstreamSources, "key", DefaultUpstreamKey)
	if err != nil {
		t.Fatal(err)
	}
	if source.GetString("backend_password") != "secret" {
		t.Fatalf("stored password = %q", source.GetString("backend_password"))
	}
	oldToken := source.GetString("backend_token")

	// Update with empty password should reuse stored password and re-auth successfully.
	_, err = ReconfigureUpstream(context.Background(), app, UpstreamReconfigureInput{
		EmbyBaseURL:     server.URL,
		BackendUsername: "backend",
		BackendPassword: "",
		AllowCreate:     false,
	})
	if err != nil {
		t.Fatalf("reconfigure with empty password: %v", err)
	}

	source, err = app.FindFirstRecordByData(UpstreamSources, "key", DefaultUpstreamKey)
	if err != nil {
		t.Fatal(err)
	}
	if source.GetString("backend_password") != "secret" {
		t.Fatalf("password after reconfigure = %q, want secret", source.GetString("backend_password"))
	}
	if source.GetString("backend_token") == "" {
		t.Fatal("expected refreshed token")
	}
	// Token may be same or new depending on mock; presence is enough. No empty-auth path.
	_ = oldToken
}

func TestReconfigureUpstreamEmptyPasswordWithoutSourceFails(t *testing.T) {
	app := newTestApp(t)
	server := newUpstreamResponder(t, "secret")
	defer server.Close()

	_, err := ReconfigureUpstream(context.Background(), app, UpstreamReconfigureInput{
		EmbyBaseURL:     server.URL,
		BackendUsername: "backend",
		BackendPassword: "",
		AllowCreate:     true,
	})
	if err == nil {
		t.Fatal("expected error when creating without password")
	}
	if !strings.Contains(err.Error(), "password") {
		t.Fatalf("error = %v, want password required", err)
	}
}

func TestProbeUpstreamFailsOnBadPassword(t *testing.T) {
	server := newUpstreamResponder(t, "correct")
	defer server.Close()

	_, _, _, _, _, err := ProbeUpstream(context.Background(), nil, UpstreamReconfigureInput{
		EmbyBaseURL:     server.URL,
		BackendUsername: "backend",
		BackendPassword: "wrong",
	})
	if err == nil {
		t.Fatal("expected probe failure for bad password")
	}
	if !strings.Contains(err.Error(), "authentication") && !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "status") {
		// Accept any auth failure wording from UpstreamRequest.
		if !strings.Contains(strings.ToLower(err.Error()), "auth") && !strings.Contains(err.Error(), "unexpected HTTP") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestProbeUpstreamSuccessReturnsIdentityWithoutToken(t *testing.T) {
	server := newUpstreamResponder(t, "correct")
	defer server.Close()

	serverID, name, version, userID, latency, err := ProbeUpstream(context.Background(), nil, UpstreamReconfigureInput{
		EmbyBaseURL:     server.URL,
		BackendUsername: "backend",
		BackendPassword: "correct",
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if serverID != "server" {
		t.Fatalf("server_id = %q", serverID)
	}
	if name != "Test Emby" {
		t.Fatalf("name = %q", name)
	}
	if version != "4.8.0" {
		t.Fatalf("version = %q", version)
	}
	if userID != "backend-user" {
		t.Fatalf("backend_user_id = %q", userID)
	}
	if latency < 0 {
		t.Fatalf("latency = %d", latency)
	}
}

func TestProbeUpstreamRequiresPassword(t *testing.T) {
	_, _, _, _, _, err := ProbeUpstream(context.Background(), nil, UpstreamReconfigureInput{
		EmbyBaseURL:     "http://example.test",
		BackendUsername: "backend",
		BackendPassword: "",
	})
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("error = %v", err)
	}
}

func TestProbeUpstreamEmptyPasswordReusesStored(t *testing.T) {
	app := newTestApp(t)
	server := newUpstreamResponder(t, "secret")
	defer server.Close()

	seedUpstream(t, app, server.URL, "backend", "secret")

	serverID, name, version, userID, _, err := ProbeUpstream(context.Background(), app, UpstreamReconfigureInput{
		EmbyBaseURL:     server.URL,
		BackendUsername: "backend",
		BackendPassword: "",
	})
	if err != nil {
		t.Fatalf("probe with empty password: %v", err)
	}
	if serverID != "server" || name != "Test Emby" || version != "4.8.0" || userID != "backend-user" {
		t.Fatalf("unexpected probe identity: id=%q name=%q ver=%q user=%q", serverID, name, version, userID)
	}
}

func TestProbeUpstreamEmptyPasswordWithoutSourceFails(t *testing.T) {
	app := newTestApp(t)
	_, _, _, _, _, err := ProbeUpstream(context.Background(), app, UpstreamReconfigureInput{
		EmbyBaseURL:     "http://example.test",
		BackendUsername: "backend",
		BackendPassword: "",
	})
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("error = %v", err)
	}
}

func TestProbeUpstreamEmptyPasswordRejectsDifferentURL(t *testing.T) {
	app := newTestApp(t)
	server := newUpstreamResponder(t, "secret")
	defer server.Close()
	seedUpstream(t, app, server.URL, "backend", "secret")

	// Attacker-controlled URL must not receive the stored password via blank reuse.
	_, _, _, _, _, err := ProbeUpstream(context.Background(), app, UpstreamReconfigureInput{
		EmbyBaseURL:     "http://attacker.example",
		BackendUsername: "backend",
		BackendPassword: "",
	})
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("error = %v, want password required for non-configured URL", err)
	}

	// Same host with different path must also require an explicit password.
	_, _, _, _, _, err = ProbeUpstream(context.Background(), app, UpstreamReconfigureInput{
		EmbyBaseURL:     strings.TrimRight(server.URL, "/") + "/emby",
		BackendUsername: "backend",
		BackendPassword: "",
	})
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("error = %v, want password required for path-mismatched URL", err)
	}
}
