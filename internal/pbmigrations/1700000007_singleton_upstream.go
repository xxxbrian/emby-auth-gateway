package pbmigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

const (
	upstreamSourcesCollection   = "upstream_sources"
	upstreamEndpointsCollection = "upstream_endpoints"
)

func init() {
	migrations.Register(upstreamCollectionsUp, upstreamCollectionsDown)
}

func upstreamCollectionsUp(app core.App) error {
	sources, err := app.FindCollectionByNameOrId(upstreamSourcesCollection)
	if err != nil {
		sources = newUpstreamSourcesCollection()
		if err := app.Save(sources); err != nil {
			return err
		}
	} else if err := validateUpstreamSourcesCollection(sources); err != nil {
		return err
	}

	endpoints, err := app.FindCollectionByNameOrId(upstreamEndpointsCollection)
	if err != nil {
		endpoints = newUpstreamEndpointsCollection(sources.Id)
		return app.Save(endpoints)
	}

	return validateUpstreamEndpointsCollection(endpoints, sources.Id)
}

func upstreamCollectionsDown(app core.App) error {
	endpoints, err := app.FindCollectionByNameOrId(upstreamEndpointsCollection)
	if err != nil {
		return err
	}
	sources, err := app.FindCollectionByNameOrId(upstreamSourcesCollection)
	if err != nil {
		return err
	}

	endpointCount, err := app.CountRecords(endpoints)
	if err != nil {
		return err
	}
	sourceCount, err := app.CountRecords(sources)
	if err != nil {
		return err
	}
	if endpointCount != 0 || sourceCount != 0 {
		return fmt.Errorf("cannot remove singleton upstream collections with records: upstream_endpoints=%d upstream_sources=%d", endpointCount, sourceCount)
	}

	if err := app.Delete(endpoints); err != nil {
		return err
	}
	return app.Delete(sources)
}

func newUpstreamSourcesCollection() *core.Collection {
	collection := core.NewBaseCollection(upstreamSourcesCollection)
	lockCollection(collection)
	collection.Fields.Add(&core.SelectField{Name: "key", Required: true, MaxSelect: 1, Values: []string{"default"}})
	collection.Fields.Add(&core.TextField{Name: "server_id", Required: true, Max: 255})
	collection.Fields.Add(&core.TextField{Name: "server_name", Max: 255})
	collection.Fields.Add(&core.TextField{Name: "server_version", Max: 80})
	collection.Fields.Add(&core.DateField{Name: "version_checked_at"})
	collection.Fields.Add(&core.TextField{Name: "backend_username", Required: true, Max: 255})
	collection.Fields.Add(&core.TextField{Name: "backend_password", Required: true})
	collection.Fields.Add(&core.TextField{Name: "backend_user_id", Max: 80})
	collection.Fields.Add(&core.TextField{Name: "backend_token"})
	collection.Fields.Add(&core.DateField{Name: "token_updated_at"})
	collection.Fields.Add(&core.DateField{Name: "last_login_at"})
	collection.Fields.Add(&core.TextField{Name: "last_login_error"})
	collection.Fields.Add(&core.TextField{Name: "backend_user_agent", Required: true, Max: 255})
	collection.Fields.Add(&core.TextField{Name: "backend_authorization_client", Required: true, Max: 255})
	collection.Fields.Add(&core.TextField{Name: "backend_authorization_device", Required: true, Max: 255})
	collection.Fields.Add(&core.TextField{Name: "backend_authorization_device_id", Required: true, Max: 255})
	collection.Fields.Add(&core.TextField{Name: "backend_authorization_version", Required: true, Max: 80})
	collection.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	collection.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	collection.AddIndex("idx_upstream_sources_key", true, "key", "")
	return collection
}

func newUpstreamEndpointsCollection(sourceCollectionID string) *core.Collection {
	collection := core.NewBaseCollection(upstreamEndpointsCollection)
	lockCollection(collection)
	collection.Fields.Add(&core.RelationField{Name: "source", CollectionId: sourceCollectionID, Required: true, MaxSelect: 1})
	collection.Fields.Add(&core.TextField{Name: "key", Required: true, Max: 80})
	collection.Fields.Add(&core.URLField{Name: "base_url", Required: true})
	collection.Fields.Add(&core.BoolField{Name: "active"})
	collection.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})
	collection.Fields.Add(&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	collection.AddIndex("idx_upstream_endpoints_source_key", true, "source, key", "")
	collection.AddIndex("idx_upstream_endpoints_source_base_url", true, "source, base_url", "")
	collection.AddIndex("idx_upstream_endpoints_active_source", true, "source", "active = 1")
	return collection
}

func validateUpstreamSourcesCollection(collection *core.Collection) error {
	expected := newUpstreamSourcesCollection()
	return validateUpstreamCollection(collection, expected)
}

func validateUpstreamEndpointsCollection(collection *core.Collection, sourceCollectionID string) error {
	expected := newUpstreamEndpointsCollection(sourceCollectionID)
	return validateUpstreamCollection(collection, expected)
}

func validateUpstreamCollection(collection, expected *core.Collection) error {
	if collection.Type != expected.Type || collection.System != expected.System || collection.ListRule != nil || collection.ViewRule != nil || collection.CreateRule != nil || collection.UpdateRule != nil || collection.DeleteRule != nil {
		return fmt.Errorf("incompatible existing collection %q", collection.Name)
	}
	if len(collection.Fields) != len(expected.Fields) || len(collection.Indexes) != len(expected.Indexes) {
		return fmt.Errorf("incompatible existing collection %q", collection.Name)
	}
	for _, expectedField := range expected.Fields {
		field := collection.Fields.GetByName(expectedField.GetName())
		if field == nil || field.Type() != expectedField.Type() || fmt.Sprintf("%#v", field) != fmt.Sprintf("%#v", expectedField) {
			return fmt.Errorf("incompatible existing collection %q field %q", collection.Name, expectedField.GetName())
		}
	}
	for _, expectedIndex := range expected.Indexes {
		found := false
		for _, index := range collection.Indexes {
			if index == expectedIndex {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("incompatible existing collection %q index %q", collection.Name, expectedIndex)
		}
	}
	return nil
}
