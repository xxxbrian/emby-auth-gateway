package pbsetup

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

var genDeviceAuthorizationField = regexp.MustCompile(`([A-Za-z]+)="([^"]*)"`)

func TestGenDeviceCredentialAuthUsesPersistedGeneratedDevice(t *testing.T) {
	for _, test := range []struct {
		name     string
		isImport bool
		repair   bool
	}{
		{name: "setup update"},
		{name: "import create", isImport: true},
		{name: "import repair", isImport: true, repair: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var authDevices []string
			authCount := 0
			server := genDeviceServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/Users/AuthenticateByName" {
					authDevices = append(authDevices, genDeviceHeader(r, "DeviceId"))
					authCount++
					token := "new-token"
					if authCount == 1 {
						token = "bootstrap-token"
					}
					_, _ = w.Write([]byte(`{"AccessToken":"` + token + `","ServerId":"server","User":{"Id":"new-user"}}`))
				}
			})
			defer server.Close()

			app, legacyServer, account := genDeviceLegacy(t, server.URL)
			if !test.isImport {
				if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "legacy-user", BackendPassword: "legacy-password"}); err != nil {
					t.Fatal(err)
				}
				source := genDeviceSource(t, app)
				old := source.GetString("backend_authorization_device_id")
				if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "legacy-user", BackendPassword: "changed-password"}); err != nil {
					t.Fatal(err)
				}
				source = genDeviceSource(t, app)
				genDeviceAssertAuthPersisted(t, authDevices[len(authDevices)-1], source.GetString("backend_authorization_device_id"), old)
				return
			}

			old := ""
			if test.repair {
				if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "legacy-user", BackendPassword: "legacy-password"}); err != nil {
					t.Fatal(err)
				}
				source := genDeviceSource(t, app)
				old = source.GetString("backend_authorization_device_id")
				source.Set("backend_user_id", "")
				if err := app.Save(source); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			source := genDeviceSource(t, app)
			genDeviceAssertAuthPersisted(t, authDevices[len(authDevices)-1], source.GetString("backend_authorization_device_id"), old)
		})
	}
}

func TestGenDeviceDryRunUsesOneUnpersistedTemporaryDevice(t *testing.T) {
	var authDevice, logoutDevice string
	server := genDeviceServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			authDevice = genDeviceHeader(r, "DeviceId")
			_, _ = w.Write([]byte(`{"AccessToken":"temporary-token","ServerId":"server","User":{"Id":"user"}}`))
		case "/Sessions/Logout":
			logoutDevice = genDeviceHeader(r, "DeviceId")
		}
	})
	defer server.Close()
	app, legacyServer, account := genDeviceLegacy(t, server.URL)
	legacyServer.Set("backend_authorization_device_id", "target-device")
	if err := app.Save(legacyServer); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	summary, data, err := runUpstreamImportLegacyPrepared(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id}, &stderr)
	if err != nil || summary.ValidationToken != "logged_out" {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if authDevice == "" || authDevice != logoutDevice || authDevice == "target-device" {
		t.Fatalf("temporary device auth=%q logout=%q", authDevice, logoutDevice)
	}
	if count, _ := app.CountRecords(upstreamSources); count != 0 {
		t.Fatalf("dry run persisted %d sources", count)
	}
	for _, output := range []string{string(data), stderr.String(), genDeviceSummaryText(summary)} {
		if strings.Contains(output, authDevice) || strings.Contains(output, "target-device") {
			t.Fatalf("dry-run output leaked device ID: %q", output)
		}
	}
}

func TestGenDeviceManagedOldTokenRetirementUsesStoredSourceRequest(t *testing.T) {
	for _, test := range []struct {
		name     string
		isImport bool
	}{
		{name: "setup"},
		{name: "import repair", isImport: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var app core.App
			logoutCount := 0
			server := genDeviceServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/Users/AuthenticateByName":
					_, _ = w.Write([]byte(`{"AccessToken":"new-token","ServerId":"server","User":{"Id":"new-user"}}`))
				case "/Sessions/Logout":
					logoutCount++
					if r.Header.Get("User-Agent") != "old-agent" || genDeviceHeader(r, "DeviceId") != "old-device" || genDeviceHeader(r, "Client") != "old-client" || genDeviceHeader(r, "Device") != "old-name" || genDeviceHeader(r, "Version") != "old-version" || genDeviceHeader(r, "Token") != "old-token" {
						t.Errorf("old logout did not use stored source identity: %q / %q", r.Header.Get("User-Agent"), r.Header.Get("X-Emby-Authorization"))
					}
					source := genDeviceSource(t, app)
					if source.GetString("backend_token") != "new-token" {
						t.Error("old logout occurred before replacement commit")
					}
				}
			})
			defer server.Close()
			var legacyServer, account *core.Record
			app, legacyServer, account = genDeviceLegacy(t, server.URL)
			if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "legacy-user", BackendPassword: "legacy-password"}); err != nil {
				t.Fatal(err)
			}
			source := genDeviceSource(t, app)
			source.Set("backend_token", "old-token")
			source.Set("backend_user_id", "old-user")
			source.Set("auth_generation_id", "managed")
			source.Set("backend_user_agent", "old-agent")
			source.Set("backend_authorization_client", "old-client")
			source.Set("backend_authorization_device", "old-name")
			source.Set("backend_authorization_device_id", "old-device")
			source.Set("backend_authorization_version", "old-version")
			if err := app.Save(source); err != nil {
				t.Fatal(err)
			}
			if test.isImport {
				legacyServer.Set("backend_user_agent", "old-agent")
				legacyServer.Set("backend_authorization_client", "old-client")
				legacyServer.Set("backend_authorization_device", "old-name")
				legacyServer.Set("backend_authorization_version", "old-version")
				if err := app.Save(legacyServer); err != nil {
					t.Fatal(err)
				}
				source.Set("backend_user_id", "")
				if err := app.Save(source); err != nil {
					t.Fatal(err)
				}
				_, err := runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &bytes.Buffer{})
				if err != nil {
					t.Fatal(err)
				}
			} else if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "legacy-user", BackendPassword: "changed-password"}); err != nil {
				t.Fatal(err)
			}
			if logoutCount != 1 {
				t.Fatalf("old token logouts=%d", logoutCount)
			}
		})
	}
}

func TestGenDeviceOldTokenRetirementOwnershipTable(t *testing.T) {
	for _, test := range []struct {
		name, generation string
		protect          string
		collision        bool
		logout           int
	}{
		{name: "managed distinct", generation: "managed", logout: 1},
		{name: "unmanaged generation", logout: 0},
		{name: "selected legacy owned", generation: "managed", protect: "selected", logout: 0},
		{name: "another legacy account owned", generation: "managed", protect: "other", logout: 0},
		{name: "old equals new collision", generation: "managed", collision: true, logout: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, imported := range []bool{false, true} {
				t.Run(map[bool]string{false: "setup", true: "import"}[imported], func(t *testing.T) {
					logouts := 0
					server := genDeviceServer(t, func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/Users/AuthenticateByName" {
							token := "new-token"
							if test.collision {
								token = "old-token"
							}
							_, _ = w.Write([]byte(`{"AccessToken":"` + token + `","ServerId":"server","User":{"Id":"new-user"}}`))
						} else if r.URL.Path == "/Sessions/Logout" {
							logouts++
						}
					})
					defer server.Close()
					app, legacyServer, account := genDeviceLegacy(t, server.URL)
					if err := runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "legacy-user", BackendPassword: "legacy-password"}); err != nil {
						t.Fatal(err)
					}
					source := genDeviceSource(t, app)
					source.Set("backend_token", "old-token")
					source.Set("auth_generation_id", test.generation)
					if imported {
						source.Set("backend_user_id", "")
					}
					if err := app.Save(source); err != nil {
						t.Fatal(err)
					}
					if test.protect == "selected" {
						account.Set("backend_token", "old-token")
						if err := app.Save(account); err != nil {
							t.Fatal(err)
						}
					}
					if test.protect == "other" {
						other := newBackendAccount(t, app, "other", legacyServer.Id, "other", "password", false, source.GetDateTime("last_login_at").Time())
						other.Set("backend_token", "old-token")
						if err := app.Save(other); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					if imported {
						_, err = runUpstreamImportLegacy(context.Background(), app, importLegacyOptions{ServerRecordID: legacyServer.Id, AccountRecordID: account.Id, Apply: true}, &bytes.Buffer{})
					} else {
						err = runUpstreamCreate(context.Background(), app, upstreamOptions{EmbyBaseURL: server.URL, BackendUsername: "legacy-user", BackendPassword: "changed-password"})
					}
					if test.collision {
						if err == nil {
							t.Fatal("old-token collision persisted")
						}
					} else if err != nil {
						t.Fatal(err)
					}
					source = genDeviceSource(t, app)
					if logouts != test.logout || test.collision && source.GetString("backend_token") != "old-token" {
						t.Fatalf("logouts=%d token=%q", logouts, source.GetString("backend_token"))
					}
				})
			}
		})
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

func genDeviceLegacy(t *testing.T, url string) (core.App, *core.Record, *core.Record) {
	t.Helper()
	app := newTestApp(t)
	if err := run(app, options{GatewayUsername: "gateway", GatewayPassword: "password", SyntheticUserID: "user", EmbyServerName: "legacy", EmbyBaseURL: url, BackendAccountName: "legacy", BackendUsername: "legacy-user", BackendPassword: "legacy-password"}); err != nil {
		t.Fatal(err)
	}
	server, err := app.FindFirstRecordByData("emby_servers", "name", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	account, err := app.FindFirstRecordByData("backend_accounts", "name", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	return app, server, account
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
	if auth == "" || auth != persisted || old != "" && auth == old {
		t.Fatalf("auth device=%q persisted=%q old=%q", auth, persisted, old)
	}
}

func genDeviceSummaryText(summary importSummary) string {
	return summary.DeviceIDSource + summary.DeviceIDDisposition + summary.DeviceIDFingerprint
}
