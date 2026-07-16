package pbschema

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/pocketbase/pocketbase/core"
)

// validateCollectionRaw compares the stored JSON, retaining every unknown key.
// PocketBase's decoded Collection model intentionally drops option keys it does
// not understand, so it is insufficient for exact-schema validation.
func validateCollectionRaw(app core.App, got, want *core.Collection) error {
	var fieldsJSON, optionsJSON string
	if err := app.DB().NewQuery("SELECT fields, options FROM _collections WHERE id = {:id}").Bind(map[string]any{"id": got.Id}).Row(&fieldsJSON, &optionsJSON); err != nil {
		return fmt.Errorf("read raw collection %q: %w", got.Name, err)
	}
	actualFields, actualOptions, err := decodeRawCollection(fieldsJSON, optionsJSON)
	if err != nil {
		return fmt.Errorf("decode raw collection %q: %w", got.Name, err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("marshal canonical collection %q: %w", want.Name, err)
	}
	var canonical map[string]any
	if err := json.Unmarshal(wantJSON, &canonical); err != nil {
		return fmt.Errorf("decode canonical collection %q: %w", want.Name, err)
	}
	wantFields, ok := canonical["fields"].([]any)
	if !ok {
		return fmt.Errorf("canonical fields %q: %w", want.Name, errPhysicalSchemaMismatch)
	}
	delete(canonical, "id")
	delete(canonical, "name")
	delete(canonical, "type")
	delete(canonical, "system")
	delete(canonical, "fields")
	delete(canonical, "indexes")
	delete(canonical, "listRule")
	delete(canonical, "viewRule")
	delete(canonical, "createRule")
	delete(canonical, "updateRule")
	delete(canonical, "deleteRule")
	delete(canonical, "created")
	delete(canonical, "updated")
	// Collection.MarshalJSON renders nil providers as [], while the persisted
	// canonical auth options retain nil. Restore the model's exact raw value.
	if want.IsAuth() && want.OAuth2.Providers == nil {
		if oauth, ok := canonical["oauth2"].(map[string]any); ok {
			oauth["providers"] = nil
		}
	}
	normalizeRawFields(actualFields)
	normalizeRawFields(wantFields)
	normalizeAuthSecrets(actualOptions)
	normalizeAuthSecrets(canonical)
	if !reflect.DeepEqual(actualFields, wantFields) || !reflect.DeepEqual(actualOptions, canonical) {
		return fmt.Errorf("raw metadata differs: %w", errPhysicalSchemaMismatch)
	}
	return nil
}

func decodeRawCollection(fieldsJSON, optionsJSON string) ([]any, map[string]any, error) {
	var fields []any
	var options map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal([]byte(optionsJSON), &options); err != nil {
		return nil, nil, err
	}
	return fields, options, nil
}

func normalizeRawFields(fields []any) {
	for _, raw := range fields {
		if field, ok := raw.(map[string]any); ok {
			delete(field, "id")
		}
	}
}

func normalizeAuthSecrets(options map[string]any) {
	for _, name := range []string{"authToken", "passwordResetToken", "emailChangeToken", "verificationToken", "fileToken"} {
		if token, ok := options[name].(map[string]any); ok {
			delete(token, "secret")
		}
	}
}
