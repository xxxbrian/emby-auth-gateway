package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminquery"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

const testOrigin = "https://admin.example.test"

func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	t.Cleanup(app.Cleanup)
	if err := pbschema.Ensure(app); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return app
}

func createSuperuser(t *testing.T, app core.App, email, password string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	rec := core.NewRecord(col)
	rec.SetEmail(email)
	rec.SetPassword(password)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func buildHandler(t *testing.T, app core.App, sessions *adminauth.Store) http.Handler {
	t.Helper()
	srv, err := New(Config{
		App:       app,
		Origin:    testOrigin,
		Sessions:  sessions,
		Query:     adminquery.New(app, 2),
		StartedAt: time.Now().UTC(),
		BootID:    "test-boot",
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	// Match production CORS binding so Unbind is exercised.
	r.Bind(apis.CORS(apis.CORSConfig{AllowOrigins: []string{"*"}}))
	srv.Mount(r)
	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	return mux
}

func TestAuthzMatrixNoCookie(t *testing.T) {
	app := newTestApp(t)
	h := buildHandler(t, app, adminauth.NewStore(10))

	paths := []string{
		"/admin/api/v1/session",
		"/admin/api/v1/overview",
		"/admin/api/v1/users",
		"/admin/api/v1/system",
	}
	for _, path := range paths {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Origin", testOrigin)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: code=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestAuthzMatrixBadCookie(t *testing.T) {
	app := newTestApp(t)
	h := buildHandler(t, app, adminauth.NewStore(10))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1/overview", nil)
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: adminauth.CookieDev, Value: "not-a-real-session", Path: "/admin"})
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSessionCreateAndGet(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "admin@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewStore(10)
	h := buildHandler(t, app, sessions)

	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create code=%d body=%s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created["csrf"] == nil || created["csrf"] == "" {
		t.Fatalf("missing csrf: %v", created)
	}
	cookies := rr.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == adminauth.CookieDev || c.Name == adminauth.CookieSecure {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing session cookie")
	}

	// GET session with cookie.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/v1/session", nil)
	req.AddCookie(sessionCookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "admin@example.test") {
		t.Fatalf("body=%s", rr.Body.String())
	}

	// Overview with cookie.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/api/v1/overview", nil)
	req.AddCookie(sessionCookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("overview code=%d body=%s", rr.Code, rr.Body.String())
	}

	// Write without CSRF fails.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/api/v1/users", bytes.NewReader([]byte(`{"username":"u","password":"p","synthetic_user_id":"s"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(sessionCookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("write without csrf code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSessionCreateOriginRequired(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "admin2@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	h := buildHandler(t, app, adminauth.NewStore(10))
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// wrong origin
	req.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestNewRequiresOrigin(t *testing.T) {
	app := newTestApp(t)
	if _, err := New(Config{App: app, Origin: ""}); err == nil {
		t.Fatal("expected error")
	}
}

func TestCreateUserReturnsUserDTO(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "admin-create@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewStore(10)
	h := buildHandler(t, app, sessions)

	// Establish admin session.
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("session create code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sess map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	csrf, _ := sess["csrf"].(string)
	var sessionCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == adminauth.CookieDev || c.Name == adminauth.CookieSecure {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil || csrf == "" {
		t.Fatal("missing session cookie or csrf")
	}

	userBody, _ := json.Marshal(map[string]string{
		"username":          "dto_user",
		"password":          "DtoPass123!",
		"synthetic_user_id": "syn-dto-1",
	})
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/api/v1/users", bytes.NewReader(userBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(sessionCookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create user code=%d body=%s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if _, ok := created["ok"]; ok {
		t.Fatalf("must not return bare ok payload: %v", created)
	}
	if created["id"] == nil || created["id"] == "" {
		t.Fatalf("missing id: %v", created)
	}
	if created["username"] != "dto_user" {
		t.Fatalf("username=%v", created["username"])
	}
	if created["synthetic_user_id"] != "syn-dto-1" {
		t.Fatalf("synthetic_user_id=%v", created["synthetic_user_id"])
	}
	if created["enabled"] != true {
		t.Fatalf("enabled=%v", created["enabled"])
	}

	// Duplicate must be 409, not password overwrite.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/api/v1/users", bytes.NewReader(userBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(sessionCookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate code=%d body=%s", rr.Code, rr.Body.String())
	}
}
