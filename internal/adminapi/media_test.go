package adminapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminauth"
	"github.com/xxxbrian/emby-auth-gateway/internal/adminmedia"
)

type adminMediaReaderStub struct{}

func (adminMediaReaderStub) AdminMediaSourceRef(context.Context) (string, error) {
	return "source-a", nil
}
func (adminMediaReaderStub) AdminMediaItems(_ context.Context, _ string, _ []string, detail bool) ([]byte, int, error) {
	if detail {
		return []byte(`{"Id":"a","Name":"Example episode","UserData":{"Played":true}}`), 200, nil
	}
	return []byte(`{"Items":[{"Id":"a","Name":"Example episode","ImageTags":{"Primary":"tag"}}]}`), 200, nil
}
func (adminMediaReaderStub) AdminMediaImage(context.Context, string, string, string, string) ([]byte, string, int, error) {
	return []byte{0xff, 0xd8, 0xff, 0xe0}, "image/jpeg", 200, nil
}

func TestMediaRoutesRequireAdminAndKeepIndependentBudgets(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "media@example.test", "SuperSecret1!")
	token, err := su.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions := adminauth.NewStore(10)
	session, err := sessions.Create(token, adminauth.Claims{SuperuserID: su.Id, Email: su.Email()})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{App: app, Sessions: sessions, Media: adminmedia.New(adminMediaReaderStub{}), APILimit: adminauth.NewRateLimiter(1, time.Minute), MediaLimit: adminauth.NewRateLimiter(2, time.Minute), ImageLimit: adminauth.NewRateLimiter(60, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	r.Bind(apis.CORS(apis.CORSConfig{AllowOrigins: []string{"*"}}))
	srv.Mount(r)
	handler, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if auth {
			req.AddCookie(&http.Cookie{Name: adminauth.CookieDev, Value: session.ID})
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr
	}
	for _, path := range []string{"/admin/api/v1/media/items?ids=a&source_ref=source-a", "/admin/api/v1/media/items/a?source_ref=source-a", "/admin/api/v1/media/items/a/images/Primary?source_ref=source-a"} {
		if rr := request(path, false); rr.Code != 401 {
			t.Fatalf("unprotected %s: %d", path, rr.Code)
		}
	}
	for range 50 {
		rr := request("/admin/api/v1/media/items/a/images/Primary?source_ref=source-a", true)
		if rr.Code != 200 || rr.Header().Get("Content-Type") != "image/jpeg" || rr.Header().Get("Cache-Control") != "private, no-store" || rr.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("image response code=%d headers=%v", rr.Code, rr.Header())
		}
	}
	if rr := request("/admin/api/v1/system", true); rr.Code != 200 {
		t.Fatalf("images exhausted monitoring budget: %d %s", rr.Code, rr.Body.String())
	}
	if rr := request("/admin/api/v1/media/items?ids=a&source_ref=source-a", true); rr.Code != 200 || !strings.Contains(rr.Body.String(), "Example episode") {
		t.Fatalf("metadata status=%d %s", rr.Code, rr.Body.String())
	}
	rr := request("/admin/api/v1/media/items/a?source_ref=source-a", true)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "Played") || strings.Contains(rr.Body.String(), "UserData") {
		t.Fatalf("detail status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := request("/admin/api/v1/media/items/a?source_ref=source-a", true); rr.Code != 429 {
		t.Fatalf("metadata budget status=%d", rr.Code)
	}
	// A revoked admin cookie must fail even for a server-cached image.
	sessions.Delete(session.ID)
	if rr := request("/admin/api/v1/media/items/a/images/Primary?source_ref=source-a", true); rr.Code != 401 {
		t.Fatalf("revoked image status=%d", rr.Code)
	}
}

func TestMediaRoutesRejectBadIDsAndUnverifiedSource(t *testing.T) {
	app := newTestApp(t)
	su := createSuperuser(t, app, "media@example.test", "SuperSecret1!")
	token, _ := su.NewAuthToken()
	sessions := adminauth.NewStore(10)
	session, _ := sessions.Create(token, adminauth.Claims{SuperuserID: su.Id, Email: su.Email()})
	srv, err := New(Config{App: app, Sessions: sessions, Media: adminmedia.New(adminMediaReaderStub{})})
	if err != nil {
		t.Fatal(err)
	}
	r, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	srv.Mount(r)
	handler, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path     string
		status   int
		contains string
	}{
		{"/admin/api/v1/media/items?ids=../System", 400, "provide 1 to 50"},
		{"/admin/api/v1/media/items?ids=a", 200, "source_changed"},
		{"/admin/api/v1/media/items/a/images/Logo?source_ref=source-a", 400, "unavailable"},
		{"/admin/api/v1/media/items/a/images/Primary?source_ref=old", 409, "unavailable"},
		{"/admin/api/v1/media/items/a/images/Primary?source_ref=source-a&size=original", 400, "unavailable"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.AddCookie(&http.Cookie{Name: adminauth.CookieDev, Value: session.ID})
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.status || !strings.Contains(strings.ToLower(rr.Body.String()), tc.contains) {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}
