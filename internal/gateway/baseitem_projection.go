package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

var errInvalidBaseItemProjection = errors.New("invalid BaseItem projection")

var baseItemKnownFields = []string{"Id", "Type", "RunTimeTicks", "RecursiveItemCount", "ChildCount", "Count", "ServerId", "UserId", "UserData", "ExternalUrls", "RemoteTrailers", "MediaSources", "MediaStreams", "DirectStreamUrl", "TranscodingUrl", "LiveStreamUrl"}
var userDataKnownFields = []string{"Rating", "PlaybackPositionTicks", "PlayCount", "IsFavorite", "Played", "PlayedPercentage", "LastPlayedDate", "UnplayedItemCount", "Likes", "Key", "ItemId"}

type baseItemDocument struct {
	doc *rawJSONObject
}

func parseBaseItemDocument(data []byte) (*baseItemDocument, error) {
	doc, err := parseRawJSONObject(data, baseItemKnownFields...)
	if err != nil {
		return nil, err
	}
	return &baseItemDocument{doc: doc}, nil
}

func (d *baseItemDocument) itemID() (string, bool) {
	raw, ok := d.doc.GetFold("Id")
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", false
	}
	return value, true
}

func (d *baseItemDocument) projectIdentity(ctx responseProjectionContext) error {
	if d == nil || d.doc == nil {
		return errInvalidBaseItemProjection
	}
	if err := projectExactStringField(d.doc, "ServerId", ctx.upstream.serverID, ctx.gatewayServerID, false); err != nil {
		return err
	}
	if ctx.session != nil {
		if err := projectExactStringField(d.doc, "UserId", ctx.upstream.userID, ctx.session.SyntheticUserID, false); err != nil {
			return err
		}
	}
	return nil
}

func (d *baseItemDocument) overlayPlaybackState(state PlaybackState, aggregate *PlaybackAggregate) error {
	if d == nil || d.doc == nil {
		return errInvalidBaseItemProjection
	}
	itemID, ok := d.itemID()
	if !ok || state.ItemID == "" || itemID != state.ItemID {
		return errInvalidBaseItemProjection
	}

	userData := &rawJSONObject{fields: make(map[string]json.RawMessage), known: knownFieldMap(userDataKnownFields)}
	if raw, exists := d.doc.GetFold("UserData"); exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		parsed, err := parseRawJSONObject(raw, userDataKnownFields...)
		if err != nil {
			return fmt.Errorf("UserData: %w", err)
		}
		userData = parsed
	}

	itemMap := make(map[string]any)
	if raw, exists := d.doc.GetFold("Id"); exists {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		itemMap["Id"] = value
	}
	for _, name := range []string{"Type", "RunTimeTicks", "RecursiveItemCount", "ChildCount", "Count"} {
		if raw, exists := d.doc.GetFold(name); exists {
			var value any
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				return err
			}
			itemMap[name] = value
		}
	}
	knownValues := make(map[string]any)
	applyPlaybackStateToUserData(knownValues, &state, itemMap, aggregate)
	for _, name := range userDataKnownFields {
		value, exists := knownValues[name]
		if !exists {
			userData.RemoveFold(name)
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := userData.SetSemantic(name, raw); err != nil {
			return err
		}
	}
	raw, err := userData.MarshalJSON()
	if err != nil {
		return err
	}
	return d.doc.SetSemantic("UserData", raw)
}

func (d *baseItemDocument) projectURLs(ctx responseProjectionContext) error {
	if d == nil || d.doc == nil {
		return errInvalidBaseItemProjection
	}
	for _, name := range []string{"DirectStreamUrl", "TranscodingUrl", "LiveStreamUrl"} {
		if err := projectURLField(d.doc, name, ctx); err != nil {
			return err
		}
	}
	for _, name := range []string{"ExternalUrls", "RemoteTrailers"} {
		if err := projectObjectArrayField(d.doc, name, []string{"Url"}, func(child *rawJSONObject) error {
			return projectURLField(child, "Url", ctx)
		}); err != nil {
			return err
		}
	}
	if err := projectObjectArrayField(d.doc, "MediaSources", mediaSourceKnownFields, func(child *rawJSONObject) error {
		return projectMediaSourceURLs(child, ctx)
	}); err != nil {
		return err
	}
	return projectObjectArrayField(d.doc, "MediaStreams", []string{"DeliveryUrl"}, func(child *rawJSONObject) error {
		return projectURLField(child, "DeliveryUrl", ctx)
	})
}

func (d *baseItemDocument) marshal() (json.RawMessage, error) {
	if d == nil || d.doc == nil {
		return nil, errInvalidBaseItemProjection
	}
	return d.doc.MarshalJSON()
}

func projectBaseItemDocument(data json.RawMessage, ctx responseProjectionContext) (json.RawMessage, error) {
	doc, err := parseBaseItemDocument(data)
	if err != nil {
		return nil, err
	}
	if err := doc.projectIdentity(ctx); err != nil {
		return nil, err
	}
	if ctx.overlayBaseItem != nil {
		if err := ctx.overlayBaseItem(doc); err != nil {
			return nil, err
		}
	}
	if err := doc.projectURLs(ctx); err != nil {
		return nil, err
	}
	return doc.marshal()
}

func projectedBaseItemDocuments(data []byte, projection responseProjection) ([]json.RawMessage, error) {
	switch projection.kind {
	case responseProjectionBaseItem:
		if _, err := parseBaseItemDocument(data); err != nil {
			return nil, err
		}
		return []json.RawMessage{cloneRawMessage(data)}, nil
	case responseProjectionBaseItemArray:
		return parseBaseItemArrayDocuments(data)
	case responseProjectionBaseItemEnvelope:
		return baseItemEnvelopeDocuments(data)
	case responseProjectionBaseItemEnvelopeArray:
		envelopes, err := parseRawJSONArray(data)
		if err != nil {
			return nil, err
		}
		result := make([]json.RawMessage, 0)
		for i, envelope := range envelopes {
			documents, err := baseItemItemsEnvelopeDocuments(envelope)
			if err != nil {
				return nil, fmt.Errorf("envelope %d: %w", i, err)
			}
			result = append(result, documents...)
		}
		return result, nil
	case responseProjectionAllThemeMedia:
		doc, err := parseRawJSONObject(data, "ThemeVideosResult", "ThemeSongsResult", "SoundtrackSongsResult")
		if err != nil {
			return nil, err
		}
		result := make([]json.RawMessage, 0)
		for _, name := range []string{"ThemeVideosResult", "ThemeSongsResult", "SoundtrackSongsResult"} {
			raw, ok := doc.GetFold(name)
			if !ok || isJSONNull(raw) {
				continue
			}
			documents, err := baseItemEnvelopeDocuments(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			result = append(result, documents...)
		}
		return result, nil
	default:
		return []json.RawMessage{}, nil
	}
}

func baseItemItemsEnvelopeDocuments(data []byte) ([]json.RawMessage, error) {
	doc, err := parseRawJSONObject(data, "Items")
	if err != nil {
		return nil, err
	}
	raw, ok := doc.GetFold("Items")
	if !ok || isJSONNull(raw) {
		return []json.RawMessage{}, nil
	}
	items, err := parseBaseItemArrayDocuments(raw)
	if err != nil {
		return nil, fmt.Errorf("Items: %w", err)
	}
	return items, nil
}

func baseItemEnvelopeDocuments(data []byte) ([]json.RawMessage, error) {
	doc, err := parseRawJSONObject(data, "Item", "Items")
	if err != nil {
		return nil, err
	}
	result := make([]json.RawMessage, 0)
	if raw, ok := doc.GetFold("Item"); ok && !isJSONNull(raw) {
		if _, err := parseBaseItemDocument(raw); err != nil {
			return nil, fmt.Errorf("Item: %w", err)
		}
		result = append(result, raw)
	}
	if raw, ok := doc.GetFold("Items"); ok && !isJSONNull(raw) {
		items, err := parseBaseItemArrayDocuments(raw)
		if err != nil {
			return nil, fmt.Errorf("Items: %w", err)
		}
		result = append(result, items...)
	}
	return result, nil
}

func parseBaseItemArrayDocuments(data []byte) ([]json.RawMessage, error) {
	items, err := parseRawJSONArray(data)
	if err != nil {
		return nil, err
	}
	for i, item := range items {
		if _, err := parseBaseItemDocument(item); err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
	}
	return items, nil
}

func knownFieldMap(fields []string) map[string]string {
	known := make(map[string]string, len(fields))
	for _, field := range fields {
		known[lowerASCII(field)] = field
	}
	return known
}

func lowerASCII(value string) string {
	result := make([]byte, len(value))
	for i := range value {
		c := value[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		result[i] = c
	}
	return string(result)
}
