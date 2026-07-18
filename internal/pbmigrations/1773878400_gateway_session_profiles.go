package pbmigrations

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessioncaps"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"
)

// sessionCapabilitiesJSONMaxBytes aliases the shared sessioncaps bound so
// migration acceptance matches runtime hydration limits.
const sessionCapabilitiesJSONMaxBytes = sessioncaps.MaxBytes

// Stable native AppMigration filename for gateway_session_profiles.
const migrationGatewaySessionProfiles = "1773878400_gateway_session_profiles.go"

// publicSessionIDAttempts is the maximum number of sessionid.New attempts when
// a recognized unique collision is observed during backfill.
const publicSessionIDAttempts = 8

func init() {
	core.AppMigrations.Register(upGatewaySessionProfiles, downGatewaySessionProfiles, migrationGatewaySessionProfiles)
}

// upGatewaySessionProfiles creates or validates the session-profiles sidecar,
// backfills missing profiles for every gateway_sessions row, and requires exact
// schema plus full coverage before returning.
func upGatewaySessionProfiles(app core.App) error {
	// 1. Strict base schema before any sidecar mutation.
	if err := pbschema.Ensure(app); err != nil {
		return fmt.Errorf("base schema ensure: %w", err)
	}

	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		return fmt.Errorf("find gateway_sessions: %w", err)
	}

	existing, found, err := findCollectionByName(app, pbschema.SessionProfilesCollection)
	if err != nil {
		return err
	}
	if !found {
		// 2a. Absent -> create exact locked builder.
		if err := app.Save(pbschema.SessionProfiles(sessions.Id)); err != nil {
			return fmt.Errorf("create %s: %w", pbschema.SessionProfilesCollection, err)
		}
	} else {
		_ = existing
		// 2b. Present -> exact schema validation.
		if err := pbschema.ValidateSessionProfiles(app); err != nil {
			return fmt.Errorf("validate existing session profiles schema: %w", err)
		}
		// 3. Reject malformed/duplicate/orphan rows before mutation; holes OK.
		if err := rejectInvalidSessionProfiles(app); err != nil {
			return err
		}
	}

	// 4. Backfill every session missing a profile (sorted by session id).
	if err := backfillSessionProfiles(app); err != nil {
		return err
	}

	// 5. Exact sidecar schema AND exactly one valid profile per session.
	if err := pbschema.ValidateSessionProfiles(app); err != nil {
		return fmt.Errorf("post-backfill schema validation: %w", err)
	}
	if err := requireFullSessionProfileCoverage(app); err != nil {
		return err
	}
	return nil
}

// downGatewaySessionProfiles refuses destructive rollback.
//
// Production rollback policy is full pb_data backup/restore. Removing the
// sidecar would destroy public session IDs and capability/activity state, so
// Down is intentionally unsupported.
func downGatewaySessionProfiles(core.App) error {
	return fmt.Errorf("down migration %s is unsupported; restore from pb_data backup (destructive sidecar rollback is not supported)", migrationGatewaySessionProfiles)
}

func findCollectionByName(app core.App, name string) (*core.Collection, bool, error) {
	all, err := app.FindAllCollections()
	if err != nil {
		return nil, false, fmt.Errorf("list collections: %w", err)
	}
	for _, c := range all {
		if c != nil && c.Name == name {
			return c, true, nil
		}
	}
	return nil, false, nil
}

func rejectInvalidSessionProfiles(app core.App) error {
	sessionIDs, err := loadSessionIDSet(app)
	if err != nil {
		return err
	}
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		return fmt.Errorf("list session profiles: %w", err)
	}

	bySession := make(map[string]string, len(profiles))
	byPublic := make(map[string]string, len(profiles))
	for _, profile := range profiles {
		if err := validateProfileRow(profile, sessionIDs); err != nil {
			return fmt.Errorf("malformed session profile %q: %w", profile.Id, err)
		}
		sessionRef := profile.GetString("gateway_session")
		publicID := profile.GetString("public_session_id")
		if prev, ok := bySession[sessionRef]; ok {
			return fmt.Errorf("duplicate gateway_session relation %q on profiles %q and %q", sessionRef, prev, profile.Id)
		}
		if prev, ok := byPublic[publicID]; ok {
			return fmt.Errorf("duplicate public_session_id %q on profiles %q and %q", publicID, prev, profile.Id)
		}
		bySession[sessionRef] = profile.Id
		byPublic[publicID] = profile.Id
	}
	return nil
}

func validateProfileRow(profile *core.Record, sessionIDs map[string]struct{}) error {
	sessionRef := profile.GetString("gateway_session")
	if sessionRef == "" {
		return fmt.Errorf("empty gateway_session relation")
	}
	if _, ok := sessionIDs[sessionRef]; !ok {
		return fmt.Errorf("orphan gateway_session relation %q", sessionRef)
	}
	publicID := profile.GetString("public_session_id")
	if !sessionid.Valid(publicID) {
		return fmt.Errorf("invalid public_session_id %q", publicID)
	}
	if err := validateCapabilitiesJSON(profile.GetString("capabilities_json")); err != nil {
		return err
	}
	if profile.GetDateTime("last_activity_at").IsZero() {
		return fmt.Errorf("missing last_activity_at")
	}
	return nil
}

// validateCapabilitiesJSON uses the shared sessioncaps validator so migration
// acceptance implies runtime ParseSessionCapabilities acceptance for the same
// bounds, known field types, array limits, and DeviceProfile shape.
func validateCapabilitiesJSON(raw string) error {
	if n := len(raw); n < 2 || n > sessionCapabilitiesJSONMaxBytes {
		return fmt.Errorf("capabilities_json length %d out of bounds", n)
	}
	if err := sessioncaps.Validate(raw); err != nil {
		return fmt.Errorf("capabilities_json: %w", err)
	}
	return nil
}

func backfillSessionProfiles(app core.App) error {
	sessions, err := app.FindAllRecords("gateway_sessions")
	if err != nil {
		return fmt.Errorf("list gateway_sessions: %w", err)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Id < sessions[j].Id })

	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		return fmt.Errorf("list session profiles: %w", err)
	}
	covered := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		covered[profile.GetString("gateway_session")] = struct{}{}
	}

	collection, err := app.FindCollectionByNameOrId(pbschema.SessionProfilesCollection)
	if err != nil {
		return fmt.Errorf("find session profiles collection: %w", err)
	}

	for _, session := range sessions {
		if _, ok := covered[session.Id]; ok {
			continue
		}
		activity := session.GetDateTime("created")
		if activity.IsZero() {
			return fmt.Errorf("gateway_sessions %q has malformed/missing created timestamp", session.Id)
		}
		if err := insertSessionProfile(app, collection, session.Id, activity); err != nil {
			return err
		}
		covered[session.Id] = struct{}{}
	}
	return nil
}

func insertSessionProfile(app core.App, collection *core.Collection, sessionID string, activity any) error {
	var lastErr error
	for attempt := 0; attempt < publicSessionIDAttempts; attempt++ {
		publicID, err := sessionid.New()
		if err != nil {
			return fmt.Errorf("generate public session id: %w", err)
		}
		record := core.NewRecord(collection)
		record.Set("gateway_session", sessionID)
		record.Set("public_session_id", publicID)
		record.Set("capabilities_json", "{}")
		record.Set("last_activity_at", activity)
		if err := app.Save(record); err != nil {
			if isRecognizedUniqueCollision(err) {
				lastErr = err
				continue
			}
			return fmt.Errorf("create profile for session %q: %w", sessionID, err)
		}
		return nil
	}
	return fmt.Errorf("create profile for session %q: exhausted %d public id attempts: %w", sessionID, publicSessionIDAttempts, lastErr)
}

func requireFullSessionProfileCoverage(app core.App) error {
	sessionIDs, err := loadSessionIDSet(app)
	if err != nil {
		return err
	}
	profiles, err := app.FindAllRecords(pbschema.SessionProfilesCollection)
	if err != nil {
		return fmt.Errorf("list session profiles: %w", err)
	}

	bySession := make(map[string]struct{}, len(profiles))
	byPublic := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if err := validateProfileRow(profile, sessionIDs); err != nil {
			return fmt.Errorf("post-backfill invalid profile %q: %w", profile.Id, err)
		}
		sessionRef := profile.GetString("gateway_session")
		publicID := profile.GetString("public_session_id")
		if _, ok := bySession[sessionRef]; ok {
			return fmt.Errorf("post-backfill duplicate gateway_session %q", sessionRef)
		}
		if _, ok := byPublic[publicID]; ok {
			return fmt.Errorf("post-backfill duplicate public_session_id %q", publicID)
		}
		bySession[sessionRef] = struct{}{}
		byPublic[publicID] = struct{}{}
	}
	if len(bySession) != len(sessionIDs) {
		return fmt.Errorf("session profile coverage incomplete: profiles=%d sessions=%d", len(bySession), len(sessionIDs))
	}
	for id := range sessionIDs {
		if _, ok := bySession[id]; !ok {
			return fmt.Errorf("session %q missing profile after backfill", id)
		}
	}
	return nil
}

func loadSessionIDSet(app core.App) (map[string]struct{}, error) {
	sessions, err := app.FindAllRecords("gateway_sessions")
	if err != nil {
		return nil, fmt.Errorf("list gateway_sessions: %w", err)
	}
	ids := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		ids[session.Id] = struct{}{}
	}
	return ids, nil
}

func isRecognizedUniqueCollision(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "validation_not_unique") ||
		strings.Contains(msg, "value must be unique")
}
