package gateway

import (
	"reflect"
	"testing"
)

func TestUpstreamPurposeStringsArePrivateAndStable(t *testing.T) {
	tests := []struct {
		purpose upstreamPurpose
		want    string
	}{
		{upstreamPurposeMetadata, "metadata"},
		{upstreamPurposeMedia, "media"},
		{upstreamPurposeNegotiation, "negotiation"},
		{upstreamPurposeManagedAuth, "managed_auth"},
		{upstreamPurposeLegacy, "legacy"},
	}
	for _, tt := range tests {
		if got := tt.purpose.String(); got != tt.want {
			t.Errorf("purpose %d = %q, want %q", tt.purpose, got, tt.want)
		}
	}
	if got := upstreamPurpose(0).String(); got != "unknown" {
		t.Fatalf("zero purpose = %q, want unknown", got)
	}
	if reflect.TypeOf(upstreamPurposeMetadata).Name() != "upstreamPurpose" {
		t.Fatalf("purpose type became exported: %T", upstreamPurposeMetadata)
	}
}

func TestPhase5ContractShapes(t *testing.T) {
	var _ MetadataUpstream
	var _ MediaUpstream
	var _ ManagedAuthUpstream
	var _ LegacyHTTPUpstream
	var _ SessionHub
	var _ MediaLeaseRegistry

	if got := reflect.TypeOf(SessionCommandEnvelope{}).NumField(); got != 4 {
		t.Fatalf("command envelope fields = %d, want 4", got)
	}
	if reflect.TypeOf(SessionCommandEnvelope{}).Field(1).Type != reflect.TypeOf((*GeneralCommand)(nil)) {
		t.Fatal("general command is not a typed envelope member")
	}
}
