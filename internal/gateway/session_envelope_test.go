package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestSessionEnvelopeDecodeAcceptsTextOrBinaryPayloadBytes(t *testing.T) {
	payload := []byte(`{"MessageType":"GeneralCommand","Data":{"Name":"GoHome"},"MessageId":"opaque-1"}`)
	for _, frameKind := range []string{"text", "binary"} {
		t.Run(frameKind, func(t *testing.T) {
			got, err := decodeSessionEnvelope(payload)
			if err != nil {
				t.Fatal(err)
			}
			if got.MessageType != "GeneralCommand" || got.MessageID != "opaque-1" || string(got.Data) != `{"Name":"GoHome"}` {
				t.Fatalf("decoded envelope = %#v", got)
			}
		})
	}
}

func TestSessionEnvelopeDataFormsAndAbsentData(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"object", `{"A":1}`},
		{"array", `[1,"two"]`},
		{"string", `"value"`},
		{"number", `42`},
		{"boolean", `true`},
		{"null", `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"MessageType":"Sessions","Data":` + tt.data + `}`)
			got, err := decodeSessionEnvelope(payload)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Data) != tt.data {
				t.Fatalf("Data = %s, want %s", got.Data, tt.data)
			}
		})
	}

	got, err := decodeSessionEnvelope([]byte(`{"MessageType":"Sessions"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Data != nil {
		t.Fatalf("absent Data = %s, want nil", got.Data)
	}
}

func TestSessionEnvelopeUnknownFieldsIgnoredAndOutboundDeterministic(t *testing.T) {
	got, err := decodeSessionEnvelope([]byte(`{"Unknown":{"nested":true},"MessageId":"id","Data":[1],"MessageType":"Sessions"}`))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encodeSessionEnvelope(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"MessageType":"Sessions","Data":[1],"MessageId":"id"}`
	if string(payload) != want {
		t.Fatalf("encoded = %s, want %s", payload, want)
	}
}

func TestSessionEnvelopeRejectsInvalidPayloadsWithCloseClassification(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		code    websocket.StatusCode
	}{
		{"malformed", []byte(`{"MessageType":`), websocket.StatusInvalidFramePayloadData},
		{"non UTF-8", []byte{0xff, 0xfe}, websocket.StatusInvalidFramePayloadData},
		{"oversize", bytes.Repeat([]byte{'x'}, sessionEnvelopeMaxBytes+1), websocket.StatusMessageTooBig},
		{"missing type", []byte(`{"Data":{}}`), websocket.StatusInvalidFramePayloadData},
		{"non PascalCase", []byte(`{"MessageType":"sessions"}`), websocket.StatusInvalidFramePayloadData},
		{"punctuated type", []byte(`{"MessageType":"Session-Update"}`), websocket.StatusInvalidFramePayloadData},
		{"long id", []byte(`{"MessageType":"Sessions","MessageId":"` + strings.Repeat("x", sessionMessageIDMaxBytes+1) + `"}`), websocket.StatusInvalidFramePayloadData},
		{"trailing value", []byte(`{"MessageType":"Sessions"} {}`), websocket.StatusInvalidFramePayloadData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeSessionEnvelope(tt.payload)
			var closeErr *sessionEnvelopeCloseError
			if !errors.As(err, &closeErr) || closeErr.Code != tt.code {
				t.Fatalf("error = %v, want close code %d", err, tt.code)
			}
		})
	}
}

func TestSessionEnvelopeEncodeRejectsInvalidDataAndKeepsRawJSON(t *testing.T) {
	if _, err := encodeSessionEnvelope(SessionOutboundEnvelope{MessageType: "Sessions", Data: json.RawMessage(`{`)}); !errors.Is(err, errInvalidSessionEnvelope) {
		t.Fatalf("invalid Data error = %v", err)
	}

	payload, err := encodeSessionEnvelope(SessionOutboundEnvelope{MessageType: "Sessions", Data: json.RawMessage(` {"B":2,"A":1} `)})
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"MessageType":"Sessions","Data":{"B":2,"A":1}}` {
		t.Fatalf("encoded raw JSON = %s", payload)
	}
}
