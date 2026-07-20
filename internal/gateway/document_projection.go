package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

const controlledProjectionMaxBytes int64 = 32 << 20

var errUnsupportedResponseProjection = errors.New("unsupported response projection")

var mediaSourceKnownFields = []string{"ServerId", "DirectStreamUrl", "TranscodingUrl", "LiveStreamUrl", "MediaStreams"}

type responseProjectionContext struct {
	session           *Session
	upstream          upstreamRequestSnapshot
	gatewayToken      string
	publicGatewayBase string
	gatewayServerID   string
	overlayBaseItem   func(*baseItemDocument) error
}

func projectResponseDocument(data []byte, projection responseProjection, ctx responseProjectionContext) ([]byte, error) {
	if !projection.valid() {
		return nil, errUnsupportedResponseProjection
	}
	if int64(len(data)) > controlledProjectionMaxBytes {
		return nil, errDocumentTooLarge
	}
	var projected []byte
	var err error
	switch projection.kind {
	case responseProjectionOpaque:
		if err = validateCredentialSafeOpaqueJSON(data, controlledProjectionMaxBytes, ctx.upstream.token); err == nil {
			projected = append([]byte(nil), data...)
		}
	case responseProjectionBaseItem:
		projected, err = projectBaseItemDocument(data, ctx)
	case responseProjectionBaseItemEnvelope:
		projected, err = projectBaseItemEnvelope(data, ctx)
	case responseProjectionBaseItemArray:
		projected, err = projectBaseItemArray(data, ctx)
	case responseProjectionSystemInfo:
		projected, err = projectSystemInfoDocument(data, ctx)
	case responseProjectionPlaybackInfo:
		projected, err = projectPlaybackInfoDocument(data, ctx)
	case responseProjectionBaseItemEnvelopeArray:
		projected, err = projectBaseItemEnvelopeArray(data, ctx)
	case responseProjectionAllThemeMedia:
		projected, err = projectAllThemeMediaDocument(data, ctx)
	case responseProjectionLiveStreamResponse:
		projected, err = projectLiveStreamResponseDocument(data, ctx)
	case responseProjectionMediaSource:
		projected, err = projectMediaSourceDocument(data, ctx)
	default:
		return nil, errUnsupportedResponseProjection
	}
	if err != nil {
		return nil, err
	}
	if projection.kind != responseProjectionOpaque {
		if err := validateCredentialSafeJSON(projected, controlledProjectionMaxBytes, ctx.upstream.token); err != nil {
			return nil, err
		}
	}
	return projected, nil
}

func projectSessionInfoNowPlayingItem(data json.RawMessage, ctx responseProjectionContext) (json.RawMessage, error) {
	projected, err := projectBaseItemDocument(data, ctx)
	if err != nil {
		return nil, err
	}
	if err := validateCredentialSafeJSON(projected, controlledProjectionMaxBytes, ctx.upstream.token); err != nil {
		return nil, err
	}
	return projected, nil
}

func projectBaseItemEnvelope(data []byte, ctx responseProjectionContext) ([]byte, error) {
	doc, err := parseRawJSONObject(data, "Item", "Items")
	if err != nil {
		return nil, err
	}
	if raw, ok := doc.GetFold("Item"); ok && !isJSONNull(raw) {
		projected, err := projectBaseItemDocument(raw, ctx)
		if err != nil {
			return nil, fmt.Errorf("Item: %w", err)
		}
		if err := doc.SetSemantic("Item", projected); err != nil {
			return nil, err
		}
	}
	if raw, ok := doc.GetFold("Items"); ok && !isJSONNull(raw) {
		projected, err := projectBaseItemArray(raw, ctx)
		if err != nil {
			return nil, fmt.Errorf("Items: %w", err)
		}
		if err := doc.SetSemantic("Items", projected); err != nil {
			return nil, err
		}
	}
	return doc.MarshalJSON()
}

func projectBaseItemArray(data []byte, ctx responseProjectionContext) ([]byte, error) {
	items, err := parseRawJSONArray(data)
	if err != nil {
		return nil, err
	}
	projected := make([]json.RawMessage, len(items))
	for i, item := range items {
		projected[i], err = projectBaseItemDocument(item, ctx)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
	}
	return json.Marshal(projected)
}

func projectSystemInfoDocument(data []byte, ctx responseProjectionContext) ([]byte, error) {
	doc, err := parseRawJSONObject(data, "Id", "ServerId", "LocalAddress", "WanAddress", "RemoteAddresses", "LocalAddresses")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Id", "ServerId"} {
		if err := projectExactStringField(doc, name, ctx.upstream.serverID, ctx.gatewayServerID, true); err != nil {
			return nil, err
		}
	}
	if ctx.publicGatewayBase != "" {
		address, _ := json.Marshal(ctx.publicGatewayBase)
		addresses, _ := json.Marshal([]string{ctx.publicGatewayBase})
		for _, name := range []string{"LocalAddress", "WanAddress"} {
			if _, exists := doc.GetFold(name); exists {
				if err := doc.SetSemantic(name, address); err != nil {
					return nil, err
				}
			}
		}
		for _, name := range []string{"RemoteAddresses", "LocalAddresses"} {
			if _, exists := doc.GetFold(name); exists {
				if err := doc.SetSemantic(name, addresses); err != nil {
					return nil, err
				}
			}
		}
	}
	return doc.MarshalJSON()
}

func projectPlaybackInfoDocument(data []byte, ctx responseProjectionContext) ([]byte, error) {
	doc, err := parseRawJSONObject(data, "MediaSources")
	if err != nil {
		return nil, err
	}
	if err := projectObjectArrayField(doc, "MediaSources", mediaSourceKnownFields, func(child *rawJSONObject) error {
		return projectMediaSourceURLs(child, ctx)
	}); err != nil {
		return nil, err
	}
	return doc.MarshalJSON()
}

func projectBaseItemEnvelopeArray(data []byte, ctx responseProjectionContext) ([]byte, error) {
	values, err := parseRawJSONArray(data)
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		projected, err := projectBaseItemItemsEnvelope(value, ctx)
		if err != nil {
			return nil, fmt.Errorf("envelope %d: %w", i, err)
		}
		values[i] = projected
	}
	return json.Marshal(values)
}

func projectBaseItemItemsEnvelope(data []byte, ctx responseProjectionContext) ([]byte, error) {
	doc, err := parseRawJSONObject(data, "Items")
	if err != nil {
		return nil, err
	}
	raw, ok := doc.GetFold("Items")
	if ok && !isJSONNull(raw) {
		projected, err := projectBaseItemArray(raw, ctx)
		if err != nil {
			return nil, fmt.Errorf("Items: %w", err)
		}
		if err := doc.SetSemantic("Items", projected); err != nil {
			return nil, err
		}
	}
	return doc.MarshalJSON()
}

func projectAllThemeMediaDocument(data []byte, ctx responseProjectionContext) ([]byte, error) {
	doc, err := parseRawJSONObject(data, "ThemeVideosResult", "ThemeSongsResult", "SoundtrackSongsResult")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"ThemeVideosResult", "ThemeSongsResult", "SoundtrackSongsResult"} {
		raw, ok := doc.GetFold(name)
		if !ok || isJSONNull(raw) {
			continue
		}
		projected, err := projectBaseItemEnvelope(raw, ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := doc.SetSemantic(name, projected); err != nil {
			return nil, err
		}
	}
	return doc.MarshalJSON()
}

func projectLiveStreamResponseDocument(data []byte, ctx responseProjectionContext) ([]byte, error) {
	doc, err := parseRawJSONObject(data, "MediaSource")
	if err != nil {
		return nil, err
	}
	raw, ok := doc.GetFold("MediaSource")
	if ok && !isJSONNull(raw) {
		projected, err := projectMediaSourceDocument(raw, ctx)
		if err != nil {
			return nil, fmt.Errorf("MediaSource: %w", err)
		}
		if err := doc.SetSemantic("MediaSource", projected); err != nil {
			return nil, err
		}
	}
	return doc.MarshalJSON()
}

func projectMediaSourceDocument(data []byte, ctx responseProjectionContext) ([]byte, error) {
	doc, err := parseRawJSONObject(data, mediaSourceKnownFields...)
	if err != nil {
		return nil, err
	}
	if err := projectMediaSourceURLs(doc, ctx); err != nil {
		return nil, err
	}
	return doc.MarshalJSON()
}

func projectMediaSourceURLs(doc *rawJSONObject, ctx responseProjectionContext) error {
	if err := projectExactStringField(doc, "ServerId", ctx.upstream.serverID, ctx.gatewayServerID, false); err != nil {
		return err
	}
	for _, name := range []string{"DirectStreamUrl", "TranscodingUrl", "LiveStreamUrl"} {
		if err := projectURLField(doc, name, ctx); err != nil {
			return err
		}
	}
	return projectObjectArrayField(doc, "MediaStreams", []string{"DeliveryUrl"}, func(child *rawJSONObject) error {
		return projectURLField(child, "DeliveryUrl", ctx)
	})
}

func projectURLField(doc *rawJSONObject, name string, ctx responseProjectionContext) error {
	raw, ok := doc.GetFold(name)
	if !ok || isJSONNull(raw) {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if ctx.session == nil {
		return nil
	}
	projected := rewriteMediaReference(value, ctx.session, ctx.upstream, ctx.gatewayToken, ctx.publicGatewayBase, ctx.gatewayServerID, false)
	encoded, err := json.Marshal(projected)
	if err != nil {
		return err
	}
	return doc.SetSemantic(name, encoded)
}

func projectObjectArrayField(parent *rawJSONObject, name string, known []string, project func(*rawJSONObject) error) error {
	raw, ok := parent.GetFold(name)
	if !ok || isJSONNull(raw) {
		return nil
	}
	values, err := parseRawJSONArray(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	for i, value := range values {
		child, err := parseRawJSONObject(value, known...)
		if err != nil {
			return fmt.Errorf("%s[%d]: %w", name, i, err)
		}
		if err := project(child); err != nil {
			return fmt.Errorf("%s[%d]: %w", name, i, err)
		}
		values[i], err = child.MarshalJSON()
		if err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return err
	}
	return parent.SetSemantic(name, encoded)
}

func projectExactStringField(doc *rawJSONObject, name, oldValue, newValue string, replaceEmpty bool) error {
	raw, ok := doc.GetFold(name)
	if !ok || isJSONNull(raw) {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if (oldValue == "" || value != oldValue) && !(replaceEmpty && value == "") {
		return nil
	}
	encoded, _ := json.Marshal(newValue)
	return doc.SetSemantic(name, encoded)
}

func parseRawJSONArray(data []byte) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '[' {
		return nil, errors.New("JSON array required")
	}
	result := make([]json.RawMessage, 0)
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		if err := validateJSONNoDuplicateKeys(raw); err != nil {
			return nil, err
		}
		result = append(result, cloneRawMessage(raw))
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := requireRawJSONEOF(decoder); err != nil {
		return nil, err
	}
	return result, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
