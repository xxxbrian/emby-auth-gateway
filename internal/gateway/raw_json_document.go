package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

var (
	errRawJSONObjectRequired = errors.New("JSON object required")
	errDuplicateJSONKey      = errors.New("duplicate JSON key")
	errKnownFieldCollision   = errors.New("case-colliding known JSON field")
)

// rawJSONObject retains each field value as JSON source rather than decoding
// through interface{}, preserving integer precision and wire presence states.
type rawJSONObject struct {
	fields map[string]json.RawMessage
	known  map[string]string
}

func parseRawJSONObject(data []byte, knownFields ...string) (*rawJSONObject, error) {
	known := make(map[string]string, len(knownFields))
	for _, field := range knownFields {
		if field == "" {
			continue
		}
		known[strings.ToLower(field)] = field
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errRawJSONObjectRequired
	}

	doc := &rawJSONObject{fields: make(map[string]json.RawMessage), known: known}
	seenKnown := make(map[string]string)
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errRawJSONObjectRequired
		}
		if _, exists := doc.fields[name]; exists {
			return nil, fmt.Errorf("%w: %q", errDuplicateJSONKey, name)
		}
		folded := strings.ToLower(name)
		if _, isKnown := known[folded]; isKnown {
			if previous, exists := seenKnown[folded]; exists {
				return nil, fmt.Errorf("%w: %q and %q", errKnownFieldCollision, previous, name)
			}
			seenKnown[folded] = name
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		if err := validateJSONNoDuplicateKeys(raw); err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		doc.fields[name] = cloneRawMessage(raw)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := requireRawJSONEOF(decoder); err != nil {
		return nil, err
	}
	return doc, nil
}

func (d *rawJSONObject) Get(name string) (json.RawMessage, bool) {
	if d == nil {
		return nil, false
	}
	raw, ok := d.fields[name]
	return cloneRawMessage(raw), ok
}

func (d *rawJSONObject) GetFold(name string) (json.RawMessage, bool) {
	if d == nil {
		return nil, false
	}
	for field, raw := range d.fields {
		if strings.EqualFold(field, name) {
			return cloneRawMessage(raw), true
		}
	}
	return nil, false
}

func (d *rawJSONObject) Set(name string, raw json.RawMessage) error {
	if d == nil || name == "" {
		return errRawJSONObjectRequired
	}
	if err := validateJSONNoDuplicateKeys(raw); err != nil {
		return err
	}
	folded := strings.ToLower(name)
	if _, isKnown := d.known[folded]; isKnown {
		for existing := range d.fields {
			if existing != name && strings.EqualFold(existing, name) {
				return fmt.Errorf("%w: %q and %q", errKnownFieldCollision, existing, name)
			}
		}
	}
	if d.fields == nil {
		d.fields = make(map[string]json.RawMessage)
	}
	d.fields[name] = cloneRawMessage(raw)
	return nil
}

func (d *rawJSONObject) SetSemantic(name string, raw json.RawMessage) error {
	if d == nil {
		return errRawJSONObjectRequired
	}
	d.RemoveFold(name)
	return d.Set(name, raw)
}

func (d *rawJSONObject) Remove(name string) {
	if d != nil {
		delete(d.fields, name)
	}
}

func (d *rawJSONObject) RemoveFold(name string) {
	if d == nil {
		return
	}
	for field := range d.fields {
		if strings.EqualFold(field, name) {
			delete(d.fields, field)
		}
	}
}

func (d *rawJSONObject) Clone() *rawJSONObject {
	if d == nil {
		return nil
	}
	clone := &rawJSONObject{fields: make(map[string]json.RawMessage, len(d.fields)), known: make(map[string]string, len(d.known))}
	for name, raw := range d.fields {
		clone.fields[name] = cloneRawMessage(raw)
	}
	for folded, canonical := range d.known {
		clone.known[folded] = canonical
	}
	return clone
}

func (d *rawJSONObject) MarshalJSON() ([]byte, error) {
	if d == nil {
		return []byte("null"), nil
	}
	names := make([]string, 0, len(d.fields))
	for name := range d.fields {
		names = append(names, name)
	}
	sort.Strings(names)
	var out bytes.Buffer
	out.WriteByte('{')
	for index, name := range names {
		raw := d.fields[name]
		if err := validateJSONNoDuplicateKeys(raw); err != nil {
			return nil, fmt.Errorf("field %q: %w", name, err)
		}
		if index > 0 {
			out.WriteByte(',')
		}
		encodedName, _ := json.Marshal(name)
		out.Write(encodedName)
		out.WriteByte(':')
		out.Write(raw)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func validateJSONNoDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, nil); err != nil {
		return err
	}
	return requireRawJSONEOF(decoder)
}

func scanJSONValue(decoder *json.Decoder, visitString func(string) error) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case string:
		if visitString != nil {
			return visitString(value)
		}
	case json.Delim:
		switch value {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errRawJSONObjectRequired
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("%w: %q", errDuplicateJSONKey, key)
				}
				seen[key] = struct{}{}
				if visitString != nil {
					if err := visitString(key); err != nil {
						return err
					}
				}
				if err := scanJSONValue(decoder, visitString); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := scanJSONValue(decoder, visitString); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	return nil
}

func scanOpaqueJSONValue(decoder *json.Decoder, visitString func(string) error) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case string:
		if visitString != nil {
			return visitString(value)
		}
	case json.Delim:
		switch value {
		case '{':
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errRawJSONObjectRequired
				}
				if visitString != nil {
					if err := visitString(key); err != nil {
						return err
					}
				}
				if err := scanOpaqueJSONValue(decoder, visitString); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := scanOpaqueJSONValue(decoder, visitString); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	return nil
}

func requireRawJSONEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("multiple JSON values")
}
