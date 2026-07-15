package pbmigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

const upstreamAuthGenerationField = "auth_generation_id"

func init() {
	migrations.Register(upstreamAuthGenerationUp, upstreamAuthGenerationDown)
}

func upstreamAuthGenerationUp(app core.App) error {
	sources, err := app.FindCollectionByNameOrId(upstreamSourcesCollection)
	if err != nil {
		return fmt.Errorf("require existing compatible %s collection: %w", upstreamSourcesCollection, err)
	}

	if err := validateUpstreamSourcesWithAuthGeneration(sources); err != nil {
		return err
	}
	if sources.Fields.GetByName(upstreamAuthGenerationField) == nil {
		sources.Fields.Add(newUpstreamAuthGenerationField())
		return app.Save(sources)
	}

	return nil
}

func upstreamAuthGenerationDown(app core.App) error {
	sources, err := app.FindCollectionByNameOrId(upstreamSourcesCollection)
	if err != nil {
		return err
	}

	if sources.Fields.GetByName(upstreamAuthGenerationField) == nil {
		return nil
	}
	if err := validateUpstreamSourcesWithAuthGeneration(sources); err != nil {
		return err
	}

	records, err := app.FindRecordsByFilter(upstreamSourcesCollection, "", "", 0, 0, nil)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.GetString(upstreamAuthGenerationField) != "" {
			return fmt.Errorf("cannot remove %s: source %q has an auth generation", upstreamAuthGenerationField, record.Id)
		}
	}

	sources.Fields.RemoveByName(upstreamAuthGenerationField)
	return app.Save(sources)
}

func newUpstreamAuthGenerationField() *core.TextField {
	return &core.TextField{Name: upstreamAuthGenerationField, Max: 128}
}

func validateUpstreamSourcesWithAuthGeneration(collection *core.Collection) error {
	expected := newUpstreamSourcesCollection()
	expected.Fields.Add(newUpstreamAuthGenerationField())

	if collection.Fields.GetByName(upstreamAuthGenerationField) == nil {
		expected.Fields.RemoveByName(upstreamAuthGenerationField)
	}
	return validateUpstreamCollection(collection, expected)
}
