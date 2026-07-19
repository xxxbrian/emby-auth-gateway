package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	sessionEnvelopeMaxBytes    = 256 << 10
	sessionMessageTypeMaxBytes = 128
	sessionMessageIDMaxBytes   = 256
)

var errInvalidSessionEnvelope = errors.New("invalid session envelope")

type sessionEnvelopeCloseError struct {
	Code websocket.StatusCode
	Err  error
}

func (e *sessionEnvelopeCloseError) Error() string {
	return fmt.Sprintf("session envelope close %d: %v", e.Code, e.Err)
}

func (e *sessionEnvelopeCloseError) Unwrap() error {
	return e.Err
}

type sessionEnvelopeWire struct {
	MessageType string          `json:"MessageType"`
	Data        json.RawMessage `json:"Data,omitempty"`
	MessageID   string          `json:"MessageId,omitempty"`
}

func decodeSessionEnvelope(payload []byte) (SessionOutboundEnvelope, error) {
	if len(payload) > sessionEnvelopeMaxBytes {
		return SessionOutboundEnvelope{}, &sessionEnvelopeCloseError{
			Code: websocket.StatusMessageTooBig,
			Err:  fmt.Errorf("%w: payload exceeds %d bytes", errInvalidSessionEnvelope, sessionEnvelopeMaxBytes),
		}
	}
	if !utf8.Valid(payload) {
		return SessionOutboundEnvelope{}, &sessionEnvelopeCloseError{
			Code: websocket.StatusInvalidFramePayloadData,
			Err:  fmt.Errorf("%w: payload is not UTF-8", errInvalidSessionEnvelope),
		}
	}

	var wire sessionEnvelopeWire
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&wire); err != nil {
		return SessionOutboundEnvelope{}, invalidSessionEnvelopeError(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return SessionOutboundEnvelope{}, invalidSessionEnvelopeError(err)
		}
		return SessionOutboundEnvelope{}, invalidSessionEnvelopeError(errors.New("multiple JSON values"))
	}
	if !validSessionMessageType(wire.MessageType) {
		return SessionOutboundEnvelope{}, invalidSessionEnvelopeError(errors.New("MessageType must be bounded PascalCase"))
	}
	if len(wire.MessageID) > sessionMessageIDMaxBytes {
		return SessionOutboundEnvelope{}, invalidSessionEnvelopeError(errors.New("MessageId is too long"))
	}

	return SessionOutboundEnvelope{
		MessageType: wire.MessageType,
		Data:        cloneRawMessage(wire.Data),
		MessageID:   wire.MessageID,
	}, nil
}

func encodeSessionEnvelope(envelope SessionOutboundEnvelope) ([]byte, error) {
	if !validSessionMessageType(envelope.MessageType) {
		return nil, fmt.Errorf("%w: MessageType must be bounded PascalCase", errInvalidSessionEnvelope)
	}
	if len(envelope.MessageID) > sessionMessageIDMaxBytes {
		return nil, fmt.Errorf("%w: MessageId is too long", errInvalidSessionEnvelope)
	}
	if envelope.Data != nil && !json.Valid(envelope.Data) {
		return nil, fmt.Errorf("%w: Data is not valid JSON", errInvalidSessionEnvelope)
	}
	payload, err := json.Marshal(sessionEnvelopeWire{
		MessageType: envelope.MessageType,
		Data:        envelope.Data,
		MessageID:   envelope.MessageID,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidSessionEnvelope, err)
	}
	if len(payload) > sessionEnvelopeMaxBytes {
		return nil, fmt.Errorf("%w: encoded payload exceeds %d bytes", errInvalidSessionEnvelope, sessionEnvelopeMaxBytes)
	}
	return payload, nil
}

func invalidSessionEnvelopeError(err error) error {
	return &sessionEnvelopeCloseError{
		Code: websocket.StatusInvalidFramePayloadData,
		Err:  fmt.Errorf("%w: %v", errInvalidSessionEnvelope, err),
	}
}

func validSessionMessageType(messageType string) bool {
	if len(messageType) == 0 || len(messageType) > sessionMessageTypeMaxBytes || messageType[0] < 'A' || messageType[0] > 'Z' {
		return false
	}
	for i := 1; i < len(messageType); i++ {
		c := messageType[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
