package pbstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

const (
	collectionGatewaySessions        = "gateway_sessions"
	collectionGatewaySessionProfiles = "gateway_session_profiles"
	sessionPublicIDCollisionRetries  = 8
)

func (s *Store) CreateSession(ctx context.Context, session gateway.Session) (*gateway.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if session.GatewayTokenHash == "" {
		return nil, fmt.Errorf("gateway token hash is required")
	}
	providedID := session.PublicID
	if providedID != "" && !sessionid.Valid(providedID) {
		return nil, fmt.Errorf("invalid public session id %q", providedID)
	}

	maxAttempts := 1
	if providedID == "" {
		maxAttempts = sessionPublicIDCollisionRetries
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		publicID := providedID
		if publicID == "" {
			id, err := sessionid.New()
			if err != nil {
				return nil, err
			}
			publicID = id
		}
		created, err := s.createSessionOnce(ctx, session, publicID)
		if err == nil {
			return created, nil
		}
		lastErr = err
		if providedID != "" || !isUniqueConstraintError(err) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("create session: public id collision retries exhausted")
	}
	return nil, lastErr
}

func (s *Store) createSessionOnce(ctx context.Context, session gateway.Session, publicID string) (*gateway.Session, error) {
	raw := session.Capabilities.RawJSON
	if raw == "" {
		raw = "{}"
	}
	caps, err := gateway.ParseSessionCapabilities(raw)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	createdAt := session.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	activityAt := session.LastActivityAt
	if activityAt.IsZero() {
		activityAt = createdAt
	}

	var result *gateway.Session
	err = s.app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sessions, err := tx.FindCollectionByNameOrId(collectionGatewaySessions)
		if err != nil {
			return err
		}
		profiles, err := tx.FindCollectionByNameOrId(collectionGatewaySessionProfiles)
		if err != nil {
			return err
		}

		auth := core.NewRecord(sessions)
		auth.Set("gateway_token_hash", session.GatewayTokenHash)
		auth.Set("gateway_user", session.GatewayUserID)
		auth.Set("gateway_username", session.GatewayUsername)
		auth.Set("synthetic_user_id", session.SyntheticUserID)
		auth.Set("client", session.Client)
		auth.Set("device", session.Device)
		auth.Set("device_id", session.DeviceID)
		auth.Set("version", session.Version)
		auth.Set("remote_ip", session.RemoteIP)
		auth.Set("expires_at", session.ExpiresAt)
		if session.RevokedAt != nil {
			auth.Set("revoked_at", *session.RevokedAt)
		}
		if err := tx.Save(auth); err != nil {
			return err
		}

		profile := core.NewRecord(profiles)
		profile.Set("gateway_session", auth.Id)
		profile.Set("public_session_id", publicID)
		profile.Set("capabilities_json", caps.RawJSON)
		profile.Set("last_activity_at", activityAt)
		if err := tx.Save(profile); err != nil {
			return err
		}

		hydrated, err := sessionFromRecords(auth, profile)
		if err != nil {
			return err
		}
		result = hydrated
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) SessionTokenExists(ctx context.Context, tokenHash string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Resolve the collection first so a missing/broken schema is an operational
	// error rather than a false "not found".
	if _, err := s.app.FindCollectionByNameOrId(collectionGatewaySessions); err != nil {
		return false, err
	}
	_, err := s.app.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", tokenHash)
	if err != nil {
		if isNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*gateway.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	auth, err := s.app.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", tokenHash)
	if err != nil {
		if isNotFoundError(err) {
			return nil, gateway.ErrNotFound
		}
		return nil, err
	}
	return s.hydrateOrRepairSession(ctx, auth)
}

func (s *Store) RevokeSession(ctx context.Context, tokenHash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record, err := s.app.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", tokenHash)
	if err != nil {
		if isNotFoundError(err) {
			return gateway.ErrNotFound
		}
		return err
	}
	record.Set("revoked_at", time.Now().UTC())
	return s.app.Save(record)
}

func (s *Store) UpdateSessionCapabilities(ctx context.Context, tokenHash string, capabilities gateway.SessionCapabilities, at time.Time) (*gateway.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := capabilities.RawJSON
	if raw == "" {
		raw = "{}"
	}
	caps, err := gateway.ParseSessionCapabilities(raw)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}

	var result *gateway.Session
	err = s.app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		auth, err := tx.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", tokenHash)
		if err != nil {
			if isNotFoundError(err) {
				return gateway.ErrNotFound
			}
			return err
		}
		profile, err := ensureSessionProfileTx(tx, auth)
		if err != nil {
			return err
		}
		profile.Set("capabilities_json", caps.RawJSON)
		profile.Set("last_activity_at", at)
		if err := tx.Save(profile); err != nil {
			return err
		}
		hydrated, err := sessionFromRecords(auth, profile)
		if err != nil {
			return err
		}
		result = hydrated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) TouchSessionActivity(ctx context.Context, tokenHash string, at time.Time, minInterval time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}

	var changed bool
	err := s.app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		auth, err := tx.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", tokenHash)
		if err != nil {
			if isNotFoundError(err) {
				return gateway.ErrNotFound
			}
			return err
		}
		profile, err := ensureSessionProfileTx(tx, auth)
		if err != nil {
			return err
		}
		last := profile.GetDateTime("last_activity_at").Time()
		if !last.IsZero() && at.Sub(last) < minInterval {
			changed = false
			return nil
		}
		profile.Set("last_activity_at", at)
		if err := tx.Save(profile); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func (s *Store) ListActiveSessions(ctx context.Context, gatewayUserID string, now time.Time) ([]gateway.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if _, err := s.app.FindCollectionByNameOrId(collectionGatewaySessions); err != nil {
		return nil, err
	}
	if _, err := s.app.FindCollectionByNameOrId(collectionGatewaySessionProfiles); err != nil {
		return nil, err
	}

	records, err := s.app.FindRecordsByFilter(
		collectionGatewaySessions,
		"gateway_user = {:gatewayUserID}",
		"",
		0,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID},
	)
	if err != nil {
		return nil, err
	}

	out := make([]gateway.Session, 0, len(records))
	for _, auth := range records {
		hydrated, err := s.hydrateOrRepairSession(ctx, auth)
		if err != nil {
			return nil, err
		}
		if !hydrated.Active(now) {
			continue
		}
		out = append(out, *hydrated)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].LastActivityAt.Equal(out[j].LastActivityAt) {
			return out[i].LastActivityAt.After(out[j].LastActivityAt)
		}
		return out[i].PublicID < out[j].PublicID
	})
	return out, nil
}

func (s *Store) hydrateOrRepairSession(ctx context.Context, auth *core.Record) (*gateway.Session, error) {
	profile, err := s.app.FindFirstRecordByData(collectionGatewaySessionProfiles, "gateway_session", auth.Id)
	if err == nil {
		return sessionFromRecords(auth, profile)
	}
	if !isNotFoundError(err) {
		return nil, err
	}

	var result *gateway.Session
	txErr := s.app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Re-check under the transaction for a stable race winner.
		existing, findErr := tx.FindFirstRecordByData(collectionGatewaySessionProfiles, "gateway_session", auth.Id)
		if findErr == nil {
			hydrated, err := sessionFromRecords(auth, existing)
			if err != nil {
				return err
			}
			result = hydrated
			return nil
		}
		if !isNotFoundError(findErr) {
			return findErr
		}
		profile, err := createSessionProfileTx(tx, auth, "", "{}", auth.GetDateTime("created").Time())
		if err != nil {
			// Another writer may have won the unique relation race.
			if isUniqueConstraintError(err) {
				winner, winErr := tx.FindFirstRecordByData(collectionGatewaySessionProfiles, "gateway_session", auth.Id)
				if winErr != nil {
					return winErr
				}
				hydrated, err := sessionFromRecords(auth, winner)
				if err != nil {
					return err
				}
				result = hydrated
				return nil
			}
			return err
		}
		hydrated, err := sessionFromRecords(auth, profile)
		if err != nil {
			return err
		}
		result = hydrated
		return nil
	})
	if txErr != nil {
		return nil, txErr
	}
	return result, nil
}

func ensureSessionProfileTx(tx core.App, auth *core.Record) (*core.Record, error) {
	profile, err := tx.FindFirstRecordByData(collectionGatewaySessionProfiles, "gateway_session", auth.Id)
	if err == nil {
		return profile, nil
	}
	if !isNotFoundError(err) {
		return nil, err
	}
	profile, err = createSessionProfileTx(tx, auth, "", "{}", auth.GetDateTime("created").Time())
	if err != nil {
		if isUniqueConstraintError(err) {
			return tx.FindFirstRecordByData(collectionGatewaySessionProfiles, "gateway_session", auth.Id)
		}
		return nil, err
	}
	return profile, nil
}

func createSessionProfileTx(tx core.App, auth *core.Record, publicID, capabilitiesJSON string, activityAt time.Time) (*core.Record, error) {
	profiles, err := tx.FindCollectionByNameOrId(collectionGatewaySessionProfiles)
	if err != nil {
		return nil, err
	}
	if publicID == "" {
		// Bounded collision retries for generated public IDs.
		var lastErr error
		for attempt := 0; attempt < sessionPublicIDCollisionRetries; attempt++ {
			id, err := sessionid.New()
			if err != nil {
				return nil, err
			}
			profile, err := saveNewProfile(tx, profiles, auth.Id, id, capabilitiesJSON, activityAt)
			if err == nil {
				return profile, nil
			}
			lastErr = err
			if !isUniqueConstraintError(err) {
				return nil, err
			}
			// If the relation already exists, surface unique so callers can re-read winner.
			if strings.Contains(strings.ToLower(err.Error()), "gateway_session") {
				return nil, err
			}
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("create session profile: public id collision retries exhausted")
		}
		return nil, lastErr
	}
	if !sessionid.Valid(publicID) {
		return nil, fmt.Errorf("invalid public session id %q", publicID)
	}
	return saveNewProfile(tx, profiles, auth.Id, publicID, capabilitiesJSON, activityAt)
}

func saveNewProfile(tx core.App, profiles *core.Collection, authID, publicID, capabilitiesJSON string, activityAt time.Time) (*core.Record, error) {
	if capabilitiesJSON == "" {
		capabilitiesJSON = "{}"
	}
	if activityAt.IsZero() {
		activityAt = time.Now().UTC()
	}
	profile := core.NewRecord(profiles)
	profile.Set("gateway_session", authID)
	profile.Set("public_session_id", publicID)
	profile.Set("capabilities_json", capabilitiesJSON)
	profile.Set("last_activity_at", activityAt)
	if err := tx.Save(profile); err != nil {
		return nil, err
	}
	return profile, nil
}

func sessionFromAuthRecord(auth *core.Record) *gateway.Session {
	createdAt := auth.GetDateTime("created").Time()
	expiresAt := auth.GetDateTime("expires_at").Time()
	var revokedAt *time.Time
	if !auth.GetDateTime("revoked_at").IsZero() {
		t := auth.GetDateTime("revoked_at").Time()
		revokedAt = &t
	}
	return &gateway.Session{
		GatewayTokenHash: auth.GetString("gateway_token_hash"),
		GatewayUserID:    auth.GetString("gateway_user"),
		GatewayUsername:  auth.GetString("gateway_username"),
		SyntheticUserID:  auth.GetString("synthetic_user_id"),
		Client:           auth.GetString("client"),
		Device:           auth.GetString("device"),
		DeviceID:         auth.GetString("device_id"),
		Version:          auth.GetString("version"),
		RemoteIP:         auth.GetString("remote_ip"),
		CreatedAt:        createdAt,
		ExpiresAt:        expiresAt,
		RevokedAt:        revokedAt,
	}
}

func sessionFromRecords(auth, profile *core.Record) (*gateway.Session, error) {
	session := sessionFromAuthRecord(auth)
	publicID := profile.GetString("public_session_id")
	if !sessionid.Valid(publicID) {
		return nil, fmt.Errorf("invalid persisted public session id %q", publicID)
	}
	rawCapabilities := profile.GetString("capabilities_json")
	if rawCapabilities == "" {
		return nil, fmt.Errorf("session profile integrity: empty capabilities_json")
	}
	caps, err := gateway.ParseSessionCapabilities(rawCapabilities)
	if err != nil {
		return nil, err
	}
	session.PublicID = publicID
	session.Capabilities = caps
	session.LastActivityAt = profile.GetDateTime("last_activity_at").Time()
	if session.LastActivityAt.IsZero() {
		// Existing profile rows must carry a real activity timestamp. Missing
		// profile rows are repaired/backfilled separately; corrupt persisted
		// values are operational integrity errors (no silent CreatedAt fallback).
		return nil, fmt.Errorf("session profile integrity: missing or zero last_activity_at")
	}
	return session, nil
}

func isNotFoundError(err error) bool {
	return err != nil && errors.Is(err, sql.ErrNoRows)
}
