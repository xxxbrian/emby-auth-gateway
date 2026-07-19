package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestEgressCredentialAliasesFoldWithoutChangingIngressSelection(t *testing.T) {
	ingress := httptest.NewRequest(http.MethodGet, "http://gateway.test/path?API_KEY=upper&TOKEN=upper-generic&api_key=selected", nil)
	if got := ExtractToken(ingress); got != "selected" {
		t.Fatalf("ingress selection changed: %q", got)
	}
	query := url.Values{
		"API_KEY":              {"client-a", "client-b"},
		"Token":                {"client-c"},
		"x-emby-token":         {"client-d"},
		"X-MEDIABROWSER-TOKEN": {"client-e"},
		"access_TOKEN":         {"client-f"},
		"signature":            {"gateway-token"},
		"keep":                 {"gateway-user"},
	}
	rewriteProxyQueryValues(query, "gateway-token", &Session{SyntheticUserID: "gateway-user"}, testUpstreamSnapshot("http://backend.invalid"))
	for key := range query {
		if isEgressCredentialAliasQueryKey(key) && key != "api_key" {
			t.Fatalf("credential alias survived: %q=%v", key, query[key])
		}
	}
	if values := query["api_key"]; len(values) != 1 || values[0] != "backend-token" {
		t.Fatalf("canonical credential=%v", values)
	}
	if query.Get("signature") != "" || query.Get("keep") != "backend-user" {
		t.Fatalf("sanitized query=%v", query)
	}
}
