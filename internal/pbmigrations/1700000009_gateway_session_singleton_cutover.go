package pbmigrations

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

const (
	gatewaySessionBackendAccountField = "backend_account"
	itemChildCountAccountField        = "backend_account_id"
	itemChildCountLegacyIndex         = "idx_item_child_counts_account_item"
	itemChildCountItemIndex           = "idx_item_child_counts_item"
)

var gatewaySessionRetiredFields = append([]string{
	gatewaySessionBackendAccountField,
	"backend_token_encrypted",
}, gatewaySessionBackendSnapshotFields...)

func init() {
	migrations.Register(gatewaySessionSingletonCutoverUp, gatewaySessionSingletonCutoverDown)
}

func gatewaySessionSingletonCutoverUp(app core.App) error {
	return app.RunInTransaction(func(txApp core.App) error {
		sessions, err := txApp.FindCollectionByNameOrId("gateway_sessions")
		if err != nil {
			return fmt.Errorf("require gateway_sessions: %w", err)
		}
		counts, err := txApp.FindCollectionByNameOrId("item_child_counts")
		if err != nil {
			return fmt.Errorf("require item_child_counts: %w", err)
		}
		users, err := txApp.FindCollectionByNameOrId("users")
		if err != nil {
			return fmt.Errorf("require users: %w", err)
		}

		if isGatewaySessionFinalSchema(sessions, users.Id) && isItemChildCountFinalSchema(counts) {
			return nil
		}
		accounts, err := txApp.FindCollectionByNameOrId("backend_accounts")
		if err != nil {
			return fmt.Errorf("require backend_accounts: %w", err)
		}
		if err := validateGatewaySessionLegacySchema(sessions, users.Id, accounts.Id); err != nil {
			return err
		}
		if !isItemChildCountLegacySchema(counts) {
			return errors.New("item_child_counts is incompatible with singleton cutover")
		}

		now := time.Now().UTC()
		records, err := txApp.FindRecordsByFilter("gateway_sessions", "", "", 0, 0, nil)
		if err != nil {
			return err
		}
		for _, record := range records {
			record.Set("revoked_at", now)
			if err := txApp.Save(record); err != nil {
				return err
			}
		}

		countRecords, err := txApp.FindRecordsByFilter("item_child_counts", "", "", 0, 0, nil)
		if err != nil {
			return err
		}
		for _, record := range countRecords {
			if err := txApp.Delete(record); err != nil {
				return err
			}
		}

		for _, name := range gatewaySessionRetiredFields {
			sessions.Fields.RemoveByName(name)
		}
		if err := txApp.Save(sessions); err != nil {
			return err
		}

		counts.Fields.RemoveByName(itemChildCountAccountField)
		counts.RemoveIndex(itemChildCountLegacyIndex)
		counts.AddIndex(itemChildCountItemIndex, true, "item_id", "")
		return txApp.Save(counts)
	})
}

func gatewaySessionSingletonCutoverDown(core.App) error {
	return errors.New("gateway session singleton cutover is destructive; restore required")
}

func isGatewaySessionFinalSchema(collection *core.Collection, userCollectionID string) bool {
	return collectionMatches(collection, newGatewaySessionFinalCollection(userCollectionID))
}

func isItemChildCountFinalSchema(collection *core.Collection) bool {
	return collectionMatches(collection, newItemChildCountFinalCollection())
}

func isItemChildCountLegacySchema(collection *core.Collection) bool {
	return collectionMatches(collection, newItemChildCountLegacyCollection())
}

func validateGatewaySessionLegacySchema(collection *core.Collection, userCollectionID, backendAccountID string) error {
	expected := newGatewaySessionFinalCollection(userCollectionID)
	if !collectionBaseMatches(collection, expected) || !indexesMatch(collection, expected.Indexes) {
		return errors.New("gateway_sessions is incompatible with singleton cutover")
	}
	if collection.Fields.GetByName(gatewaySessionBackendAccountField) == nil {
		return errors.New("gateway_sessions is a partial singleton cutover")
	}

	final := expected
	for _, expectedField := range final.Fields {
		field := collection.Fields.GetByName(expectedField.GetName())
		if field == nil || !fieldsMatch(field, expectedField) {
			return fmt.Errorf("gateway_sessions has incompatible field %q", expectedField.GetName())
		}
	}
	for _, field := range collection.Fields {
		if expected := final.Fields.GetByName(field.GetName()); expected != nil {
			continue
		}
		if !isKnownGatewaySessionRetiredField(field, backendAccountID) {
			return fmt.Errorf("gateway_sessions has incompatible retired field %q", field.GetName())
		}
	}
	return nil
}

func newGatewaySessionFinalCollection(userCollectionID string) *core.Collection {
	collection := core.NewBaseCollection("gateway_sessions")
	lockCollection(collection)
	collection.Fields.Add(&core.TextField{Name: "gateway_token_hash", Required: true, Max: 128})
	collection.Fields.Add(&core.RelationField{Name: "gateway_user", CollectionId: userCollectionID, Required: true, MaxSelect: 1})
	collection.Fields.Add(&core.TextField{Name: "gateway_username", Max: 255})
	collection.Fields.Add(&core.TextField{Name: "synthetic_user_id", Required: true, Max: 80})
	collection.Fields.Add(&core.TextField{Name: "client", Max: 255})
	collection.Fields.Add(&core.TextField{Name: "device", Max: 255})
	collection.Fields.Add(&core.TextField{Name: "device_id", Max: 255})
	collection.Fields.Add(&core.TextField{Name: "version", Max: 80})
	collection.Fields.Add(&core.TextField{Name: "remote_ip", Max: 80})
	collection.Fields.Add(&core.DateField{Name: "expires_at", Required: true})
	collection.Fields.Add(&core.DateField{Name: "revoked_at"})
	collection.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	collection.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	collection.AddIndex("idx_gateway_sessions_token_hash", true, "gateway_token_hash", "")
	return collection
}

func newItemChildCountLegacyCollection() *core.Collection {
	collection := core.NewBaseCollection("item_child_counts")
	lockCollection(collection)
	collection.Fields.Add(&core.TextField{Name: itemChildCountAccountField, Required: true, Max: 80})
	collection.Fields.Add(&core.TextField{Name: "item_id", Required: true, Max: 80})
	collection.Fields.Add(&core.NumberField{Name: "child_count", Required: true, OnlyInt: true})
	collection.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	collection.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	collection.AddIndex(itemChildCountLegacyIndex, true, "backend_account_id, item_id", "")
	return collection
}

func newItemChildCountFinalCollection() *core.Collection {
	collection := newItemChildCountLegacyCollection()
	collection.Fields.RemoveByName(itemChildCountAccountField)
	collection.RemoveIndex(itemChildCountLegacyIndex)
	collection.AddIndex(itemChildCountItemIndex, true, "item_id", "")
	return collection
}

func isKnownGatewaySessionRetiredField(field core.Field, backendAccountID string) bool {
	switch field.GetName() {
	case gatewaySessionBackendAccountField:
		return fieldsMatch(field, &core.RelationField{Name: gatewaySessionBackendAccountField, CollectionId: backendAccountID, Required: true, MaxSelect: 1})
	case "backend_token_encrypted", "backend_token":
		return fieldsMatch(field, &core.TextField{Name: field.GetName(), Required: true})
	case "backend_server_id", "backend_username":
		return fieldsMatch(field, &core.TextField{Name: field.GetName(), Max: 255})
	case "backend_base_url":
		return matchesFieldVariants(field,
			&core.URLField{Name: field.GetName()},
			&core.URLField{Name: field.GetName(), Required: true},
		)
	case "backend_user_id":
		return matchesFieldVariants(field,
			&core.TextField{Name: field.GetName(), Max: 80},
			&core.TextField{Name: field.GetName(), Required: true, Max: 80},
		)
	case "backend_user_agent", "backend_authorization_client", "backend_authorization_device", "backend_authorization_device_id":
		return matchesFieldVariants(field,
			&core.TextField{Name: field.GetName(), Max: 255},
			&core.TextField{Name: field.GetName(), Required: true, Max: 255},
		)
	case "backend_authorization_version":
		return matchesFieldVariants(field,
			&core.TextField{Name: field.GetName(), Max: 80},
			&core.TextField{Name: field.GetName(), Required: true, Max: 80},
		)
	default:
		return false
	}
}

func matchesFieldVariants(field core.Field, variants ...core.Field) bool {
	for _, variant := range variants {
		if fieldsMatch(field, variant) {
			return true
		}
	}
	return false
}

func collectionMatches(collection, expected *core.Collection) bool {
	return collectionBaseMatches(collection, expected) && fieldsMatchInOrder(collection, expected) && indexesMatch(collection, expected.Indexes)
}

func collectionBaseMatches(collection, expected *core.Collection) bool {
	return collection.Type == expected.Type && collection.System == expected.System && collection.ListRule == nil && collection.ViewRule == nil && collection.CreateRule == nil && collection.UpdateRule == nil && collection.DeleteRule == nil
}

func fieldsMatchInOrder(collection, expected *core.Collection) bool {
	if len(collection.Fields) != len(expected.Fields) {
		return false
	}
	for i, field := range expected.Fields {
		if !fieldsMatch(collection.Fields[i], field) {
			return false
		}
	}
	return true
}

func fieldsMatch(actual, expected core.Field) bool {
	if actual.Type() != expected.Type() || actual.GetName() != expected.GetName() {
		return false
	}
	actualOptions := fieldOptions(actual)
	expectedOptions := fieldOptions(expected)
	return reflect.DeepEqual(actualOptions, expectedOptions)
}

func fieldOptions(field core.Field) map[string]any {
	data, _ := json.Marshal(field)
	options := map[string]any{}
	_ = json.Unmarshal(data, &options)
	delete(options, "id")
	return options
}

func indexesMatch(collection *core.Collection, expected []string) bool {
	if len(collection.Indexes) != len(expected) {
		return false
	}
	for i, index := range expected {
		if collection.Indexes[i] != index {
			return false
		}
	}
	return true
}
