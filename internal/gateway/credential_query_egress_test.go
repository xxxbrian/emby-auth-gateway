package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEgressCredentialAliasesFoldWithoutChangingIngressSelection(t *testing.T) {
	ingress := httptest.NewRequest(http.MethodGet, "http://gateway.test/path?API_KEY=upper&TOKEN=upper-generic&api_key=selected", nil)
	if got := ExtractToken(ingress); got != "selected" {
		t.Fatalf("ingress selection changed: %q", got)
	}
	raw := "API_KEY=client-a&API_KEY=client-b&Token=client-c&x-emby-token=client-d&X-MEDIABROWSER-TOKEN=client-e&access_TOKEN=client-f&signature=gateway-token&keep=gateway-user&UserId=gateway-user"
	got, err := rewriteProxyRawQuery(raw, &Session{SyntheticUserID: "gateway-user"}, testUpstreamSnapshot("http://backend.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	want := "signature=gateway-token&keep=gateway-user&UserId=backend-user&api_key=backend-token"
	if got != want {
		t.Fatalf("sanitized query=%q, want %q", got, want)
	}
}

func TestEgressCredentialRemovesSelectedValueUnderArbitraryKey(t *testing.T) {
	selected := "selected-gateway-credential"
	raw := "before=one&signature=" + selected + "&signature=ordinary-signed-value&after=two"
	got, err := rewriteProxyRawQuery(raw, &Session{
		GatewayTokenHash: HashToken(selected),
		SyntheticUserID:  "gateway-user",
	}, testUpstreamSnapshot("http://backend.invalid"))
	if err != nil {
		t.Fatal(err)
	}
	want := "before=one&signature=ordinary-signed-value&after=two&api_key=backend-token"
	if got != want {
		t.Fatalf("sanitized query=%q, want %q", got, want)
	}
}
