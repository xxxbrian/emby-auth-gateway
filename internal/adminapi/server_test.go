package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminquery"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/telemetry"
)

// testHost is used with httptest; Origin must match requestOrigin (http://Host).
const testHost = "admin.example.test"
const testOrigin = "http://" + testHost

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

// withSameOrigin sets Host + Origin so CSRF same-origin checks pass.
func withSameOrigin(req *http.Request) *http.Request {
	req.Host = testHost
	req.Header.Set("Origin", testOrigin)
	return req
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
		req := withSameOrigin(httptest.NewRequest(http.MethodGet, path, nil))
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
	req := withSameOrigin(httptest.NewRequest(http.MethodGet, "/admin/api/v1/overview", nil))
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
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
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
	req = withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/users", bytes.NewReader([]byte(`{"username":"u","password":"p","synthetic_user_id":"s"}`))))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("write without csrf code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSessionCreateEvilOriginRejected(t *testing.T) {
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
	req.Host = testHost
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSessionCreateMatchingOriginOK(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "match@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	h := buildHandler(t, app, adminauth.NewStore(10))
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSessionCreateSecFetchSiteSameOrigin(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "sfs@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	h := buildHandler(t, app, adminauth.NewStore(10))
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body))
	req.Host = testHost
	req.Header.Set("Content-Type", "application/json")
	// No Origin header; Sec-Fetch-Site same-origin is enough.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSessionCreateMissingOriginRejected(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "noorigin@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	h := buildHandler(t, app, adminauth.NewStore(10))
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body))
	req.Host = testHost
	req.Header.Set("Content-Type", "application/json")
	// No Origin, no Sec-Fetch-Site.
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestNewAllowsEmptyOrigin(t *testing.T) {
	app := newTestApp(t)
	if _, err := New(Config{App: app, Origin: ""}); err != nil {
		t.Fatalf("empty Origin must be allowed: %v", err)
	}
}

func TestRequestOrigin(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "http://example.test:8090/admin", nil)
	req.Host = "example.test:8090"
	if got := requestOrigin(req); got != "http://example.test:8090" {
		t.Fatalf("got=%q", got)
	}
	req.Header.Set("X-Forwarded-Proto", "https, http")
	if got := requestOrigin(req); got != "https://example.test:8090" {
		t.Fatalf("forwarded got=%q", got)
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
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
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
	req = withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/users", bytes.NewReader(userBody)))
	req.Header.Set("Content-Type", "application/json")
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

	// Successful create must write admin audit log (no secrets).
	audits, err := app.FindRecordsByFilter("audit_logs", `event = "admin_user_create"`, "-created", 5, 0, nil)
	if err != nil {
		t.Fatalf("list audits: %v", err)
	}
	if len(audits) == 0 {
		t.Fatal("expected admin_user_create audit log")
	}
	msg := audits[0].GetString("message")
	if !strings.Contains(msg, "admin-create@example.test") || !strings.Contains(msg, "dto_user") {
		t.Fatalf("audit message missing actor/username: %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "password") || strings.Contains(msg, "DtoPass") {
		t.Fatalf("audit message must not contain password material: %q", msg)
	}

	// Duplicate must be 409, not password overwrite.
	rr = httptest.NewRecorder()
	req = withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/users", bytes.NewReader(userBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(sessionCookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("duplicate code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUpstreamProbeFailsOnBadPassword(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "probe@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewStore(10)
	h := buildHandler(t, app, sessions)

	// Session + CSRF.
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
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

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info/Public":
			_, _ = w.Write([]byte(`{"Id":"server","ServerName":"Probe","Version":"1.0"}`))
		case "/Users/AuthenticateByName":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Invalid user or password"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer backend.Close()

	probeBody, _ := json.Marshal(map[string]any{
		"emby_base_url":                 backend.URL,
		"backend_username":              "u",
		"backend_password":              "bad",
		"backend_user_agent":            "ua",
		"backend_authorization_client":  "c",
		"backend_authorization_device":  "d",
		"backend_authorization_version": "v",
	})
	rr = httptest.NewRecorder()
	req = withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/upstream/probe", bytes.NewReader(probeBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(adminauth.CSRFHeader, csrf)
	req.AddCookie(sessionCookie)
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("probe with bad password must not succeed: body=%s", rr.Body.String())
	}
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("probe code=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOverviewWindowQuery(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "window@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewStore(10)
	reg := telemetry.New(nil)
	srv, err := New(Config{
		App:       app,
		Sessions:  sessions,
		Query:     adminquery.New(app, 2),
		Telemetry: reg,
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
	r.Bind(apis.CORS(apis.CORSConfig{AllowOrigins: []string{"*"}}))
	srv.Mount(r)
	h, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}

	// Create session cookie.
	body, _ := json.Marshal(map[string]string{"token": token})
	rr := httptest.NewRecorder()
	req := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create session code=%d body=%s", rr.Code, rr.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == adminauth.CookieDev || c.Name == adminauth.CookieSecure {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing session cookie")
	}

	cases := []struct {
		q        string
		window   string
		interval string
		points   int
	}{
		{"", "15m", "1s", 900},
		{"15m", "15m", "1s", 900},
		{"1h", "1h", "1m", 60},
		{"6h", "6h", "1m", 360},
		{"24h", "24h", "1m", 1440},
		{"nope", "15m", "1s", 900},
	}
	for _, tc := range cases {
		path := "/admin/api/v1/overview"
		if tc.q != "" {
			path += "?window=" + tc.q
		}
		rr = httptest.NewRecorder()
		req = httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(sessionCookie)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: code=%d body=%s", path, rr.Code, rr.Body.String())
		}
		var snap telemetry.Snapshot
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if snap.Series.Window != tc.window {
			t.Fatalf("%s: window=%q want %q", path, snap.Series.Window, tc.window)
		}
		if snap.Series.Interval != tc.interval {
			t.Fatalf("%s: interval=%q want %q", path, snap.Series.Interval, tc.interval)
		}
		if len(snap.Series.RPS) != tc.points {
			t.Fatalf("%s: rps points=%d want %d", path, len(snap.Series.RPS), tc.points)
		}
		if len(snap.Series.MbpsIn) != tc.points {
			t.Fatalf("%s: mbps_in points=%d want %d", path, len(snap.Series.MbpsIn), tc.points)
		}
	}
}

func TestOverviewMediaBufferStatus(t *testing.T) {
	enabled := telemetry.MediaBufferStatus{
		Enabled:          true,
		HardBudgetBytes:  2 << 30,
		AllocatedBytes:   96 << 10,
		OwnedBytes:       64 << 10,
		FreeBytes:        32 << 10,
		ActiveRequests:   3,
		BaseOnlyRequests: 1,
		IndebtedRequests: 1,
		RequestDebtBytes: 32 << 10,
	}
	for _, tt := range []struct {
		name     string
		callback func() telemetry.MediaBufferStatus
		want     telemetry.MediaBufferStatus
	}{
		{name: "enabled", callback: func() telemetry.MediaBufferStatus { return enabled }, want: enabled},
		{name: "disabled stable zeros"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler, cookie := buildMediaBufferAdminHandler(t, tt.callback)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/admin/api/v1/overview", nil)
			request.AddCookie(cookie)
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var snapshot telemetry.Snapshot
			if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.MediaBuffer != tt.want {
				t.Fatalf("media_buffer=%+v want %+v", snapshot.MediaBuffer, tt.want)
			}
		})
	}
}

func TestMetricsStreamFirstFrameMatchesOverviewMediaBuffer(t *testing.T) {
	status := telemetry.MediaBufferStatus{Enabled: true, HardBudgetBytes: 1 << 30, AllocatedBytes: 64 << 10, OwnedBytes: 32 << 10, FreeBytes: 32 << 10, ActiveRequests: 1}
	handler, cookie := buildMediaBufferAdminHandler(t, func() telemetry.MediaBufferStatus { return status })

	overviewRecorder := httptest.NewRecorder()
	overviewRequest := httptest.NewRequest(http.MethodGet, "/admin/api/v1/overview", nil)
	overviewRequest.AddCookie(cookie)
	handler.ServeHTTP(overviewRecorder, overviewRequest)
	if overviewRecorder.Code != http.StatusOK {
		t.Fatalf("overview code=%d body=%s", overviewRecorder.Code, overviewRecorder.Body.String())
	}
	var overview telemetry.Snapshot
	if err := json.Unmarshal(overviewRecorder.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	streamRequest := httptest.NewRequest(http.MethodGet, "/admin/api/v1/metrics/stream", nil).WithContext(ctx)
	streamRequest.AddCookie(cookie)
	streamWriter := newFirstSSEWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(streamWriter, streamRequest)
	}()
	frame := <-streamWriter.firstFrame
	cancel()
	<-done
	if !bytes.HasPrefix(frame, []byte("data: ")) {
		t.Fatalf("frame=%q", frame)
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(frame, []byte("data: ")))
	var streamed telemetry.Snapshot
	if err := json.Unmarshal(payload, &streamed); err != nil {
		t.Fatalf("decode frame %q: %v", frame, err)
	}
	if !reflect.DeepEqual(streamed, overview) {
		t.Fatalf("streamed snapshot=%+v overview=%+v", streamed, overview)
	}
}

func buildMediaBufferAdminHandler(t *testing.T, callback func() telemetry.MediaBufferStatus) (http.Handler, *http.Cookie) {
	t.Helper()
	app := newTestApp(t)
	superuser := createSuperuser(t, app, "buffer@example.test", "SuperSecret1!")
	token, err := superuser.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewStore(10)
	server, err := New(Config{
		App:                 app,
		Sessions:            sessions,
		Query:               adminquery.New(app, 2),
		MediaBufferSnapshot: callback,
		StartedAt:           time.Now().UTC(),
		BootID:              "test-boot",
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	server.Mount(r)
	handler, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := withSameOrigin(httptest.NewRequest(http.MethodPost, "/admin/api/v1/session", bytes.NewReader(body)))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create session code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == adminauth.CookieDev || cookie.Name == adminauth.CookieSecure {
			return handler, cookie
		}
	}
	t.Fatal("missing session cookie")
	return nil, nil
}

type firstSSEWriter struct {
	header     http.Header
	mu         sync.Mutex
	body       bytes.Buffer
	firstFrame chan []byte
	once       sync.Once
}

func newFirstSSEWriter() *firstSSEWriter {
	return &firstSSEWriter{header: make(http.Header), firstFrame: make(chan []byte, 1)}
}

func (w *firstSSEWriter) Header() http.Header { return w.header }
func (*firstSSEWriter) WriteHeader(int)       {}

func (w *firstSSEWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *firstSSEWriter) Flush() {
	w.mu.Lock()
	frame := append([]byte(nil), w.body.Bytes()...)
	w.mu.Unlock()
	w.once.Do(func() { w.firstFrame <- frame })
}
