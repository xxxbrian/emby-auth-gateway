package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestCurrentUserIsAuthenticatedAndMatchesLoginUser(t *testing.T) {
	store := testStore("http://backend.invalid/emby")
	gw := httptest.NewServer(NewServer(Config{GatewayBasePath: "/emby", GatewayServerID: "gateway-server"}, store))
	defer gw.Close()

	login := do(t, mustJSONLoginRequest(t, gw.URL+"/emby/Users/AuthenticateByName", `{"Username":"alice","Pw":"alice-pass"}`))
	defer login.Body.Close()
	var authenticated map[string]any
	decodeJSON(t, login.Body, &authenticated)
	token := authenticated["AccessToken"].(string)
	loginUser := authenticated["User"].(map[string]any)
	for _, path := range []string{"/Users/gateway-user?api_key=" + token, "/Users/Me?api_key=" + token, "/Users/gateway-user?api_key=" + token + "&Reconnect=true"} {
		resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby"+path, nil))
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s status/cache = %d/%q", path, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
		var current map[string]any
		decodeJSON(t, resp.Body, &current)
		_ = resp.Body.Close()
		if current["Id"] != loginUser["Id"] || current["Name"] != loginUser["Name"] || !reflect.DeepEqual(current["Policy"], loginUser["Policy"]) || !reflect.DeepEqual(current["Configuration"], loginUser["Configuration"]) {
			t.Fatalf("current user drifted: %#v", current)
		}
		policy := current["Policy"].(map[string]any)
		configuration := current["Configuration"].(map[string]any)
		if policy["IsAdministrator"] != false || policy["EnableContentDownloading"] != true || policy["EnableLiveTvAccess"] != false || policy["EnableLiveTvManagement"] != false || policy["EnableContentDeletion"] != false || policy["EnablePublicSharing"] != false || policy["EnableMediaConversion"] != false || policy["EnableDeletion"] != nil || policy["EnableSharing"] != nil || configuration["EnableNextEpisodeAutoPlay"] != true {
			t.Fatalf("missing compatibility defaults: %#v %#v", policy, configuration)
		}
	}

	for _, tt := range []struct {
		path string
		want int
	}{
		{"/Users/gateway-user", http.StatusUnauthorized},
		{"/Users/gateway-user?api_key=bad", http.StatusUnauthorized},
		{"/Users/other?api_key=" + token, http.StatusForbidden},
		{"/Users/Gateway-User?api_key=" + token, http.StatusForbidden},
		{"/Users/gateway-user?api_key=%ZZ", http.StatusBadRequest},
	} {
		resp := do(t, mustRequest(t, http.MethodGet, gw.URL+"/emby"+tt.path, nil))
		_ = resp.Body.Close()
		if resp.StatusCode != tt.want {
			t.Fatalf("%s status = %d, want %d", tt.path, resp.StatusCode, tt.want)
		}
	}
	resourceOnly := mustRequest(t, http.MethodGet, gw.URL+"/emby/Users/gateway-user", nil)
	resourceOnly.AddCookie(&http.Cookie{Name: resourceCookieName, Value: token})
	resourceResp := do(t, resourceOnly)
	_ = resourceResp.Body.Close()
	if resourceResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("resource cookie current user status = %d", resourceResp.StatusCode)
	}
}

func TestCurrentUserRejectsExpiredAndRevokedSessions(t *testing.T) {
	for _, state := range []string{"expired", "revoked"} {
		t.Run(state, func(t *testing.T) {
			store := NewMemoryStore()
			session := testSession()
			if state == "expired" {
				session.ExpiresAt = time.Now().UTC().Add(-time.Minute)
			} else {
				now := time.Now().UTC()
				session.RevokedAt = &now
			}
			store.Sessions[HashToken("gateway-token")] = session
			server := NewServer(Config{GatewayBasePath: "/emby"}, store)
			writer := httptest.NewRecorder()
			server.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/gateway-user?api_key=gateway-token", nil))
			if writer.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d", writer.Code)
			}
		})
	}
}

func TestCurrentUserRejectsStoredGenericCredentialConflict(t *testing.T) {
	store := NewMemoryStore()
	store.Sessions[HashToken("gateway-token")] = testSession()
	otherToken, otherHash, err := NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	store.Sessions[otherHash] = testSession()
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	writer := httptest.NewRecorder()
	server.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/gateway-user?api_key=gateway-token&token="+otherToken, nil))
	if writer.Code != http.StatusBadRequest || bytes.Contains(writer.Body.Bytes(), []byte("backend")) {
		t.Fatalf("status/body = %d/%q", writer.Code, writer.Body.String())
	}
}

func TestPublicUsersRemainMinimalAndAnonymous(t *testing.T) {
	store := testStore("http://backend.invalid/emby")
	server := NewServer(Config{GatewayBasePath: "/emby"}, store)
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "http://gateway.test/emby/Users/Public", nil))
	if resp.Code != http.StatusOK || !jsonValueLacksUserDetails(resp.Body.Bytes()) {
		t.Fatalf("public users response = %s", resp.Body.String())
	}
}

func jsonValueLacksUserDetails(data []byte) bool {
	return !bytes.Contains(data, []byte(`"Policy"`)) && !bytes.Contains(data, []byte(`"Configuration"`))
}
