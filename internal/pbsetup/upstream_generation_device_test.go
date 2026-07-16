package pbsetup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

var genDeviceAuthorizationField = regexp.MustCompile(`([A-Za-z]+)="([^"]*)"`)

func TestCredentialAuthUsesFreshPersistedDevice(t *testing.T) {
	var authDevices []string
	authCount := 0
	server := genDeviceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			authDevices = append(authDevices, genDeviceHeader(r, "DeviceId"))
			authCount++
			_, _ = w.Write([]byte(`{"AccessToken":"token-` + string(rune('0'+authCount)) + `","ServerId":"server","User":{"Id":"user"}}`))
		}
	})
	defer server.Close()
	app := newTestApp(t)
	opts := upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "user", BackendPassword: "first"}
	if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
		t.Fatal(err)
	}
	source := genDeviceSource(t, app)
	old := source.GetString("backend_authorization_device_id")
	opts.BackendPassword = "second"
	if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
		t.Fatal(err)
	}
	source = genDeviceSource(t, app)
	genDeviceAssertAuthPersisted(t, authDevices[len(authDevices)-1], source.GetString("backend_authorization_device_id"), old)
}

func TestManagedOldTokenRetirementUsesStoredSourceRequest(t *testing.T) {
	var appSource string
	logoutCount := 0
	server := genDeviceServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"new-token","ServerId":"server","User":{"Id":"new-user"}}`))
		case "/Sessions/Logout":
			logoutCount++
			if r.Header.Get("User-Agent") != "old-agent" || genDeviceHeader(r, "DeviceId") != "old-device" || genDeviceHeader(r, "Token") != "old-token" || appSource != "new-token" {
				t.Error("old token logout did not use committed source ownership")
			}
		}
	})
	defer server.Close()
	app := newTestApp(t)
	opts := upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "user", BackendPassword: "first"}
	if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
		t.Fatal(err)
	}
	source := genDeviceSource(t, app)
	for key, value := range map[string]string{"backend_token": "old-token", "backend_user_id": "old-user", "auth_generation_id": "managed", "backend_user_agent": "old-agent", "backend_authorization_client": "old-client", "backend_authorization_device": "old-name", "backend_authorization_device_id": "old-device", "backend_authorization_version": "old-version"} {
		source.Set(key, value)
	}
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	appSource = "new-token"
	opts.BackendPassword = "second"
	if err := runUpstreamCreate(context.Background(), app, opts); err != nil {
		t.Fatal(err)
	}
	if logoutCount != 1 {
		t.Fatalf("old token logouts=%d", logoutCount)
	}
}

func genDeviceServer(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/System/Info/Public" {
			_, _ = w.Write([]byte(`{"Id":"server"}`))
			return
		}
		handler(w, r)
	}))
}

func genDeviceSource(t *testing.T, app core.App) *core.Record {
	t.Helper()
	source, err := app.FindFirstRecordByData(upstreamSources, "key", defaultUpstreamKey)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func genDeviceHeader(r *http.Request, name string) string {
	for _, pair := range genDeviceAuthorizationField.FindAllStringSubmatch(r.Header.Get("X-Emby-Authorization"), -1) {
		if pair[1] == name {
			return pair[2]
		}
	}
	return ""
}

func genDeviceAssertAuthPersisted(t *testing.T, auth, persisted, old string) {
	t.Helper()
	if auth == "" || auth != persisted || auth == old {
		t.Fatalf("auth device=%q persisted=%q old=%q", auth, persisted, old)
	}
}
