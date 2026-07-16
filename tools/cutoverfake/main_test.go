package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerProtocol(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		status                   int
		contains                 string
	}{
		{"public", http.MethodGet, "/System/Info/Public", "", http.StatusOK, `"ServerId":"cutoverfake-server"`},
		{"authenticate", http.MethodPost, "/Users/AuthenticateByName", `{"Username":"user","Pw":"password"}`, http.StatusOK, `"AccessToken":"cutoverfake-token"`},
		{"logout", http.MethodPost, "/Sessions/Logout", "", http.StatusNoContent, ""},
		{"wrong method", http.MethodPost, "/System/Info/Public", "", http.StatusMethodNotAllowed, ""},
		{"bad authentication", http.MethodPost, "/Users/AuthenticateByName", `{}`, http.StatusBadRequest, ""},
		{"unknown route", http.MethodGet, "/Users", "", http.StatusNotFound, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if test.contains != "" && !strings.Contains(response.Body.String(), test.contains) {
				t.Fatalf("body %q does not contain %q", response.Body.String(), test.contains)
			}
		})
	}
}
