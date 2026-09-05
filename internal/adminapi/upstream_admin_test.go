package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
)

func seedSourceWithEndpoint(t *testing.T, app core.App) {
	t.Helper()
	srcCollection, err := app.FindCollectionByNameOrId("upstream_sources")
	if err != nil {
		t.Fatal(err)
	}
	source := core.NewRecord(srcCollection)
	for field, value := range map[string]any{
		"key": "default", "server_id": "server", "backend_username": "backend", "backend_password": "secret",
		"backend_user_agent": "agent", "backend_authorization_client": "client", "backend_authorization_device": "device",
		"backend_authorization_device_id": "device-id", "backend_authorization_version": "1.0",
	} {
		source.Set(field, value)
	}
	if err := app.Save(source); err != nil {
		t.Fatal(err)
	}
	epCollection, err := app.FindCollectionByNameOrId("upstream_endpoints")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := core.NewRecord(epCollection)
	endpoint.Set("source", source.Id)
	endpoint.Set("key", "primary")
	endpoint.Set("base_url", "https://primary.example")
	endpoint.Set("enabled", true)
	endpoint.Set("is_default", true)
	if err := app.Save(endpoint); err != nil {
		t.Fatal(err)
	}
}

// setupEndpointAdmin returns handler, csrf, cookie for a fresh app with source+endpoint.
func setupEndpointAdmin(t *testing.T) (http.Handler, string, *http.Cookie) {
	t.Helper()
	app := newTestApp(t)
	seedSourceWithEndpoint(t, app)
	su := createSuperuser(t, app, "ep-admin@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewStore(10)
	h := buildHandler(t, app, sessions)

	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("session create code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sess map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &sess)
	csrf, _ := sess["csrf"].(string)
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == adminauth.CookieDev || c.Name == adminauth.CookieSecure {
			cookie = c
			break
		}
	}
	if csrf == "" || cookie == nil {
		t.Fatal("missing csrf/cookie")
	}
	return h, csrf, cookie
}

func TestAdminEndpointCRUDAndProbeWS(t *testing.T) {
	h, csrf, cookie := setupEndpointAdmin(t)

	// Create a second endpoint.
	body, _ := json.Marshal(map[string]any{"key": "cf", "base_url": "https://cf.example", "enabled": true, "is_default": false})
	rr := httptest.NewRecorder()
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/upstream/endpoints", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create endpoint code=%d body=%s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created["id"] == nil || created["key"] != "cf" {
		t.Fatalf("created endpoint: %v", created)
	}
	// List.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/v1/upstream/endpoints", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list code=%d", rr.Code)
	}
	var listResp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &listResp)
	if items, _ := listResp["items"].([]any); len(items) != 2 {
		t.Fatalf("list items = %v", listResp["items"])
	}
	// Probe WS on the default endpoint: httptest endpoint is not a real WS
	// server, so probe must fail gracefully with 200 + capable=false.
	rr = httptest.NewRecorder()
	req = withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/upstream/endpoints/"+created["id"].(string)+"/probe-ws", nil))
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("probe-ws code=%d body=%s", rr.Code, rr.Body.String())
	}
	var probe map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &probe)
	if probe["websocket_capable"] != false {
		t.Fatalf("probe result = %v", probe)
	}
	// Delete the non-default endpoint.
	rr = httptest.NewRecorder()
	req = withSameOrigin(httptest.NewRequest(http.MethodDelete, "/admin/api/v1/upstream/endpoints/"+created["id"].(string), nil))
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete code=%d body=%s", rr.Code, rr.Body.String())
	}
	// Deleting default (only one left) must be refused.
	state, _ := loadFirstEndpointID(t, h, cookie)
	rr = httptest.NewRecorder()
	req = withSameOrigin(httptest.NewRequest(http.MethodDelete, "/admin/api/v1/upstream/endpoints/"+state, nil))
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("delete only endpoint code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func loadFirstEndpointID(t *testing.T, h http.Handler, cookie *http.Cookie) (string, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/upstream/endpoints", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list code=%d", rr.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	items, _ := resp["items"].([]any)
	if len(items) == 0 {
		t.Fatal("no endpoints")
	}
	first := items[0].(map[string]any)
	return first["id"].(string), first["key"].(string)
}

func TestAdminRouteRulesCRUD(t *testing.T) {
	h, csrf, cookie := setupEndpointAdmin(t)
	// Create a rule.
	body, _ := json.Marshal(map[string]any{
		"method": "GET", "path": "/embywebsocket", "transport": "websocket", "target": "primary", "priority": 10, "enabled": true,
	})
	rr := httptest.NewRecorder()
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/route-rules", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create rule code=%d body=%s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created["id"] == nil || created["target"] != "primary" {
		t.Fatalf("created rule: %v", created)
	}
	// List.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/v1/route-rules", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	var listResp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &listResp)
	if rr.Code != http.StatusOK {
		t.Fatalf("rule list code=%d body=%s", rr.Code, rr.Body.String())
	}
	if items, _ := listResp["items"].([]any); len(items) != 1 {
		t.Fatalf("rule list = %v", listResp["items"])
	}
	// Delete.
	rr = httptest.NewRecorder()
	req = withSameOrigin(httptest.NewRequest(http.MethodDelete, "/admin/api/v1/route-rules/"+created["id"].(string), nil))
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("delete rule code=%d body=%s", rr.Code, rr.Body.String())
	}
}
