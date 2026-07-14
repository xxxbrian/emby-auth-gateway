package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistrationHandlerStubs(t *testing.T) {
	h := RegistrationHandler{}
	cases := []struct {
		path string
		want map[string]any
	}{
		{
			path: "/admin/service/registration/validateDevice",
			want: map[string]any{
				"cacheExpirationDays": float64(233),
				"message":             "Device Valid",
				"resultCode":          "GOOD",
			},
		},
		{
			path: "/admin/service/registration/validate",
			want: map[string]any{
				"featId":     "",
				"registered": true,
				"expDate":    "2333-10-01",
				"key":        "",
			},
		},
		{
			path: "/admin/service/registration/getStatus",
			want: map[string]any{
				"deviceStatus":  float64(0),
				"planType":      "Lifetime",
				"subscriptions": []any{},
			},
		},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(method, tc.path, nil)
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("%s %s: status %d", method, tc.path, rr.Code)
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("%s %s: content-type %q", method, tc.path, ct)
			}
			if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
				t.Fatalf("%s %s: cache-control %q", method, tc.path, cc)
			}
			var got map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("%s %s: json: %v body=%q", method, tc.path, err, rr.Body.Bytes())
			}
			if !jsonMapsEqual(got, tc.want) {
				t.Fatalf("%s %s: got %#v want %#v", method, tc.path, got, tc.want)
			}
		}
	}
}

func TestRegistrationHandlerNotFoundAndMethod(t *testing.T) {
	h := RegistrationHandler{}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/service/registration/nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown path status %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/service/registration/validate/extra", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("nested path status %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/admin/service/registration/validate", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete status %d", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "GET, HEAD, POST" {
		t.Fatalf("allow %q", allow)
	}
}

func TestRegistrationHandlerHEAD(t *testing.T) {
	h := RegistrationHandler{}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/admin/service/registration/validateDevice", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	// Encode still writes a body for HEAD via writeJSON; accept either empty or JSON.
	_, _ = io.Copy(io.Discard, rr.Body)
}

func jsonMapsEqual(got, want map[string]any) bool {
	if len(got) != len(want) {
		return false
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			return false
		}
		switch w := wv.(type) {
		case []any:
			g, ok := gv.([]any)
			if !ok || len(g) != len(w) {
				return false
			}
			for i := range w {
				if g[i] != w[i] {
					return false
				}
			}
		default:
			if gv != wv {
				return false
			}
		}
	}
	return true
}
