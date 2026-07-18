package pbschema

import (
	"errors"
	"fmt"

	"github.com/pocketbase/pocketbase/core"
)

// CurrentPlaybacksCollection is the locked one-to-one current-playback sidecar.
// It is intentionally not part of the base requiredNames / Ensure create set so
// old binaries continue to accept upgraded databases as an extra locked collection.
const CurrentPlaybacksCollection = "gateway_current_playbacks"

// CurrentPlaybacks builds the exact locked gateway_current_playbacks collection.
// sessionCollectionID must be the persisted gateway_sessions collection id.
func CurrentPlaybacks(sessionCollectionID string) *core.Collection {
	c := base(CurrentPlaybacksCollection)
	c.Fields.Add(&core.RelationField{
		Name:          "gateway_session",
		CollectionId:  sessionCollectionID,
		Required:      true,
		MaxSelect:     1,
		CascadeDelete: true,
	})
	c.Fields.Add(&core.TextField{
		Name:     "item_id",
		Required: true,
		Min:      1,
		Max:      80,
	})
	c.Fields.Add(&core.TextField{
		Name: "play_session_id",
		Max:  255,
	})
	c.Fields.Add(&core.TextField{
		Name: "media_source_id",
		Max:  255,
	})
	c.Fields.Add(&core.TextField{
		Name:     "item_snapshot_json",
		Required: true,
		Min:      2,
		Max:      65536,
	})
	c.Fields.Add(&core.TextField{
		Name:     "play_state_json",
		Required: true,
		Min:      2,
		Max:      16384,
	})
	c.Fields.Add(&core.NumberField{Name: "run_time_ticks", OnlyInt: true})
	c.Fields.Add(&core.DateField{Name: "started_at", Required: true})
	c.Fields.Add(&core.DateField{Name: "last_reported_at", Required: true})
	dates(c)
	c.AddIndex("idx_gateway_current_playbacks_session", true, "gateway_session", "")
	return c
}

// ValidateCurrentPlaybacks is a write-free exact-schema check for the current
// playbacks sidecar (metadata, raw JSON, physical columns, rules, and indexes).
// It does not require row coverage; missing playbacks are repaired at runtime.
func ValidateCurrentPlaybacks(app core.App) error {
	snapshot, err := loadCollections(app)
	if err != nil {
		return err
	}
	return validateCurrentPlaybacksSnapshot(app, snapshot)
}

func validateCurrentPlaybacksSnapshot(app core.App, snapshot *collectionSnapshot) error {
	sessions := snapshot.byName["gateway_sessions"]
	if sessions == nil {
		return fmt.Errorf("%w: collection %q missing", ErrUnsupportedSchema, "gateway_sessions")
	}
	if !validApplicationID(sessions.Id) {
		return fmt.Errorf("%w: invalid collection identity for %q", ErrUnsupportedSchema, "gateway_sessions")
	}
	got := snapshot.byName[CurrentPlaybacksCollection]
	if got == nil {
		return fmt.Errorf("%w: collection %q missing", ErrUnsupportedSchema, CurrentPlaybacksCollection)
	}
	if !validApplicationID(got.Id) {
		return fmt.Errorf("%w: invalid collection identity for %q", ErrUnsupportedSchema, CurrentPlaybacksCollection)
	}
	want := CurrentPlaybacks(sessions.Id)
	if !collectionEqual(got, want) {
		return fmt.Errorf("%w: collection %q differs: %s", ErrUnsupportedSchema, CurrentPlaybacksCollection, collectionDifference(got, want))
	}
	if err := validateCollectionRaw(app, got, want); err != nil {
		if errors.Is(err, errPhysicalSchemaMismatch) {
			return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
		}
		return fmt.Errorf("validate raw collection %q: %w", CurrentPlaybacksCollection, err)
	}
	if err := validateTable(app, got); err != nil {
		if errors.Is(err, errPhysicalSchemaMismatch) {
			return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
		}
		return fmt.Errorf("validate table %q: %w", CurrentPlaybacksCollection, err)
	}
	return nil
}
