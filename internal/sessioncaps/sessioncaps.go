// Package sessioncaps validates and canonicalizes Emby ClientCapabilities JSON
// for gateway session profiles. It is stdlib-only and shared by gateway runtime
// hydration and PocketBase migrations so migration acceptance implies runtime
// acceptance for the same document shape and bounds.
package sessioncaps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultJSON is the canonical empty capabilities document.
	DefaultJSON = "{}"
	// MaxBytes is the maximum accepted capabilities document size (256 KiB).
	MaxBytes = 262144

	MaxPlayableMediaTypes   = 32
	MaxPlayableMediaTypeLen = 64
	MaxSupportedCommands    = 128
	MaxSupportedCommandLen  = 128
)

// Document is a validated, canonically encoded capabilities projection.
type Document struct {
	RawJSON              string
	PlayableMediaTypes   []string
	SupportedCommands    []string
	SupportsMediaControl bool
	SupportsSync         bool
}

// Parse validates and returns a canonical document. Empty input defaults to {}
// for repository/new-session semantics; explicit JSON null is rejected.
func Parse(raw string) (Document, error) {
	if raw == "" {
		raw = DefaultJSON
	}
	return parse(raw)
}

// Validate checks a persisted capabilities document. Empty input is rejected
// (persisted rows must carry a real JSON object, min length 2).
func Validate(raw string) error {
	if raw == "" {
		return fmt.Errorf("capabilities document is empty")
	}
	_, err := parse(raw)
	return err
}

func parse(raw string) (Document, error) {
	if len(raw) > MaxBytes {
		return Document{}, fmt.Errorf("session capabilities payload too large")
	}
	fields, err := decodeObjectFields(raw)
	if err != nil {
		return Document{}, err
	}

	if dp, ok := fields["DeviceProfile"]; ok {
		canonicalDP, err := validateAndCanonicalizeDeviceProfile(dp)
		if err != nil {
			return Document{}, err
		}
		fields["DeviceProfile"] = canonicalDP
	}

	// Recursively compact every unknown (and known-adjacent) raw value so
	// whitespace variants canonicalize identically without float64 number loss.
	for k, v := range fields {
		switch k {
		case "PlayableMediaTypes", "SupportedCommands", "SupportsMediaControl", "SupportsSync", "DeviceProfile":
			continue
		default:
			compacted, err := canonicalizeRawValue(v)
			if err != nil {
				return Document{}, fmt.Errorf("field %q: %w", k, err)
			}
			fields[k] = compacted
		}
	}

	media := []string{}
	if rawMedia, ok := fields["PlayableMediaTypes"]; ok {
		media, err = parseBoundedStringArray(rawMedia, MaxPlayableMediaTypes, MaxPlayableMediaTypeLen)
		if err != nil {
			return Document{}, fmt.Errorf("PlayableMediaTypes: %w", err)
		}
		encoded, err := json.Marshal(media)
		if err != nil {
			return Document{}, err
		}
		fields["PlayableMediaTypes"] = encoded
	}

	commands := []string{}
	if rawCmd, ok := fields["SupportedCommands"]; ok {
		commands, err = parseBoundedStringArray(rawCmd, MaxSupportedCommands, MaxSupportedCommandLen)
		if err != nil {
			return Document{}, fmt.Errorf("SupportedCommands: %w", err)
		}
		encoded, err := json.Marshal(commands)
		if err != nil {
			return Document{}, err
		}
		fields["SupportedCommands"] = encoded
	}

	supportsMedia := false
	if rawBool, ok := fields["SupportsMediaControl"]; ok {
		supportsMedia, err = parseBool(rawBool)
		if err != nil {
			return Document{}, fmt.Errorf("SupportsMediaControl: %w", err)
		}
		encoded, err := json.Marshal(supportsMedia)
		if err != nil {
			return Document{}, err
		}
		fields["SupportsMediaControl"] = encoded
	}

	supportsSync := false
	if rawBool, ok := fields["SupportsSync"]; ok {
		supportsSync, err = parseBool(rawBool)
		if err != nil {
			return Document{}, fmt.Errorf("SupportsSync: %w", err)
		}
		encoded, err := json.Marshal(supportsSync)
		if err != nil {
			return Document{}, err
		}
		fields["SupportsSync"] = encoded
	}

	canonical, err := marshalObjectFields(fields)
	if err != nil {
		return Document{}, err
	}
	return Document{
		RawJSON:              canonical,
		PlayableMediaTypes:   media,
		SupportedCommands:    commands,
		SupportsMediaControl: supportsMedia,
		SupportsSync:         supportsSync,
	}, nil
}

// DecodeObjectFields decodes a top-level JSON object into raw fields without
// validating known capability shapes. Used by HTTP handlers that must strip
// transport-only fields (e.g. Id) before Parse.
func DecodeObjectFields(raw string) (map[string]json.RawMessage, error) {
	return decodeObjectFields(raw)
}

// MarshalObjectFields encodes fields as compact JSON with sorted keys.
func MarshalObjectFields(fields map[string]json.RawMessage) (string, error) {
	return marshalObjectFields(fields)
}

func decodeObjectFields(raw string) (map[string]json.RawMessage, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("session capabilities payload too large")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	var top json.RawMessage
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("malformed session capabilities: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("malformed session capabilities: trailing data")
		}
		return nil, fmt.Errorf("malformed session capabilities: %w", err)
	}
	top = bytes.TrimSpace(top)
	if len(top) == 0 || top[0] != '{' {
		return nil, fmt.Errorf("session capabilities must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(top, &fields); err != nil {
		return nil, fmt.Errorf("malformed session capabilities: %w", err)
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	return fields, nil
}

func marshalObjectFields(fields map[string]json.RawMessage) (string, error) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.Grow(64)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		b.Write(keyJSON)
		b.WriteByte(':')
		val := bytes.TrimSpace(fields[k])
		if len(val) == 0 {
			b.WriteString("null")
		} else {
			b.Write(val)
		}
	}
	b.WriteByte('}')
	out := b.String()
	if len(out) > MaxBytes {
		return "", fmt.Errorf("session capabilities payload too large")
	}
	return out, nil
}

func validateAndCanonicalizeDeviceProfile(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("null"), nil
	}
	if raw[0] != '{' {
		return nil, fmt.Errorf("DeviceProfile must be a JSON object or null")
	}
	// Ensure it is a JSON object, then recursively compact while preserving numbers.
	compacted, err := canonicalizeRawValue(raw)
	if err != nil {
		return nil, fmt.Errorf("DeviceProfile: %w", err)
	}
	compacted = bytes.TrimSpace(compacted)
	if len(compacted) == 0 || compacted[0] != '{' {
		return nil, fmt.Errorf("DeviceProfile must be a JSON object or null")
	}
	return compacted, nil
}

// canonicalizeRawValue produces compact deterministic JSON for any value using
// json.Decoder.UseNumber so integers larger than 2^53 are preserved exactly.
func canonicalizeRawValue(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("malformed JSON value: %w", err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing data in JSON value")
		}
		return nil, fmt.Errorf("malformed JSON value: %w", err)
	}
	return marshalCanonicalValue(v)
}

func marshalCanonicalValue(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if t {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case json.Number:
		if t == "" {
			return nil, fmt.Errorf("empty number")
		}
		return []byte(t.String()), nil
	case string:
		return json.Marshal(t)
	case []any:
		var b strings.Builder
		b.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			part, err := marshalCanonicalValue(item)
			if err != nil {
				return nil, err
			}
			b.Write(part)
		}
		b.WriteByte(']')
		return []byte(b.String()), nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			keyJSON, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			b.Write(keyJSON)
			b.WriteByte(':')
			part, err := marshalCanonicalValue(t[k])
			if err != nil {
				return nil, err
			}
			b.Write(part)
		}
		b.WriteByte('}')
		return []byte(b.String()), nil
	default:
		// float64 should not appear when UseNumber is set; reject unexpected types.
		return nil, fmt.Errorf("unsupported JSON value type %T", v)
	}
}

func parseBoundedStringArray(raw json.RawMessage, maxItems, maxLen int) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return []string{}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("not an array")
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		var s string
		if err := json.Unmarshal(item, &s); err != nil {
			return nil, fmt.Errorf("non-string array item")
		}
		parts = append(parts, s)
	}
	return normalizeStringList(parts, maxItems, maxLen)
}

func parseBool(raw json.RawMessage) (bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("not a bool")
	}
	return b, nil
}

// NormalizeStringList trims, drops empties, deduplicates (first-wins), and bounds.
func NormalizeStringList(parts []string, maxItems, maxLen int) ([]string, error) {
	return normalizeStringList(parts, maxItems, maxLen)
}

func normalizeStringList(parts []string, maxItems, maxLen int) ([]string, error) {
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if utf8.RuneCountInString(p) > maxLen {
			return nil, fmt.Errorf("value too long")
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
		if len(out) > maxItems {
			return nil, fmt.Errorf("too many values")
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
