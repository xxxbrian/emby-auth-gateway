package pbschema

import (
	"errors"
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"
)

// SessionProfilesCollection is the locked one-to-one session-plane sidecar.
// It is intentionally not part of the base requiredNames / Ensure create set so
// old binaries continue to accept upgraded databases as an extra locked collection.
const SessionProfilesCollection = "gateway_session_profiles"

// SessionProfiles builds the exact locked gateway_session_profiles collection.
// sessionCollectionID must be the persisted gateway_sessions collection id.
func SessionProfiles(sessionCollectionID string) *core.Collection {
	c := base(SessionProfilesCollection)
	c.Fields.Add(&core.RelationField{
		Name:          "gateway_session",
		CollectionId:  sessionCollectionID,
		Required:      true,
		MaxSelect:     1,
		CascadeDelete: true,
	})
	c.Fields.Add(&core.TextField{
		Name:     "public_session_id",
		Required: true,
		Min:      sessionid.Length,
		Max:      sessionid.Length,
		Pattern:  sessionid.Pattern,
	})
	c.Fields.Add(&core.TextField{
		Name:     "capabilities_json",
		Required: true,
		Min:      2,
		Max:      262144,
	})
	c.Fields.Add(&core.DateField{Name: "last_activity_at", Required: true})
	dates(c)
	c.AddIndex("idx_gateway_session_profiles_session", true, "gateway_session", "")
	c.AddIndex("idx_gateway_session_profiles_public_id", true, "public_session_id", "")
	return c
}

// ValidateSessionProfiles is a write-free exact-schema check for the session
// profiles sidecar (metadata, raw JSON, physical columns, rules, and indexes).
// It does not require row coverage; missing profiles are repaired at runtime.
func ValidateSessionProfiles(app core.App) error {
	snapshot, err := loadCollections(app)
	if err != nil {
		return err
	}
	return validateSessionProfilesSnapshot(app, snapshot)
}

func validateSessionProfilesSnapshot(app core.App, snapshot *collectionSnapshot) error {
	sessions := snapshot.byName["gateway_sessions"]
	if sessions == nil {
		return fmt.Errorf("%w: collection %q missing", ErrUnsupportedSchema, "gateway_sessions")
	}
	if !validApplicationID(sessions.Id) {
		return fmt.Errorf("%w: invalid collection identity for %q", ErrUnsupportedSchema, "gateway_sessions")
	}
	got := snapshot.byName[SessionProfilesCollection]
	if got == nil {
		return fmt.Errorf("%w: collection %q missing", ErrUnsupportedSchema, SessionProfilesCollection)
	}
	if !validApplicationID(got.Id) {
		return fmt.Errorf("%w: invalid collection identity for %q", ErrUnsupportedSchema, SessionProfilesCollection)
	}
	want := SessionProfiles(sessions.Id)
	if !collectionEqual(got, want) {
		return fmt.Errorf("%w: collection %q differs: %s", ErrUnsupportedSchema, SessionProfilesCollection, collectionDifference(got, want))
	}
	if err := validateCollectionRaw(app, got, want); err != nil {
		if errors.Is(err, errPhysicalSchemaMismatch) {
			return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
		}
		return fmt.Errorf("validate raw collection %q: %w", SessionProfilesCollection, err)
	}
	if err := validateTable(app, got); err != nil {
		if errors.Is(err, errPhysicalSchemaMismatch) {
			return fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
		}
		return fmt.Errorf("validate table %q: %w", SessionProfilesCollection, err)
	}
	return nil
}
