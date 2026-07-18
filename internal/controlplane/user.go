package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// UpsertUserInput creates or updates a gateway user by username.
type UpsertUserInput struct {
	Username        string
	Password        string
	SyntheticUserID string
	// Enabled defaults true on create; ignored on update.
}

// UpsertUser creates a gateway user or updates the password for an existing username.
// Username and synthetic_user_id are immutable after create; a synthetic ID mismatch returns an error.
func UpsertUser(ctx context.Context, app core.App, in UpsertUserInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	username := strings.TrimSpace(in.Username)
	password := in.Password
	syntheticID := strings.TrimSpace(in.SyntheticUserID)
	if username == "" || password == "" || syntheticID == "" {
		return fmt.Errorf("username, password, and synthetic_user_id are required")
	}
	return app.RunInTransaction(func(txApp core.App) error {
		record, err := txApp.FindFirstRecordByData("users", "username", username)
		if err != nil {
			collection, findErr := txApp.FindCollectionByNameOrId("users")
			if findErr != nil {
				return findErr
			}
			record = core.NewRecord(collection)
			record.Set("username", username)
			record.SetEmail(internalEmail(username))
			record.SetPassword(password)
			record.SetVerified(true)
			record.Set("synthetic_user_id", syntheticID)
			record.Set("enabled", true)
			return txApp.Save(record)
		}
		if existing := record.GetString("synthetic_user_id"); existing != "" && existing != syntheticID {
			return fmt.Errorf("synthetic_user_id mismatch for user %q: stored %q, requested %q", username, existing, syntheticID)
		}
		record.SetEmail(internalEmail(username))
		if !record.ValidatePassword(password) {
			record.SetPassword(password)
		}
		record.SetVerified(true)
		if record.GetString("synthetic_user_id") == "" {
			record.Set("synthetic_user_id", syntheticID)
		}
		return txApp.Save(record)
	})
}

// SetUserEnabled sets the enabled flag for a gateway user.
// When disabling, all non-revoked sessions for that user are revoked in the same transaction.
func SetUserEnabled(ctx context.Context, app core.App, userID string, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("user id is required")
	}
	return app.RunInTransaction(func(txApp core.App) error {
		record, err := txApp.FindRecordById("users", userID)
		if err != nil {
			return err
		}
		record.Set("enabled", enabled)
		if err := txApp.Save(record); err != nil {
			return err
		}
		if !enabled {
			if _, err := revokeUserSessionsTx(txApp, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ResetUserPassword sets a new password for a gateway user and revokes all sessions.
func ResetUserPassword(ctx context.Context, app core.App, userID, password string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("user id is required")
	}
	if password == "" {
		return fmt.Errorf("password is required")
	}
	return app.RunInTransaction(func(txApp core.App) error {
		record, err := txApp.FindRecordById("users", userID)
		if err != nil {
			return err
		}
		record.SetPassword(password)
		if err := txApp.Save(record); err != nil {
			return err
		}
		if _, err := revokeUserSessionsTx(txApp, userID); err != nil {
			return err
		}
		return nil
	})
}

// RevokeUserSessions revokes all non-revoked gateway sessions for a user.
func RevokeUserSessions(ctx context.Context, app core.App, userID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0, fmt.Errorf("user id is required")
	}
	var n int
	err := app.RunInTransaction(func(txApp core.App) error {
		var err error
		n, err = revokeUserSessionsTx(txApp, userID)
		return err
	})
	return n, err
}

// RevokeSessionByID revokes a single gateway session by record id.
func RevokeSessionByID(ctx context.Context, app core.App, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("session id is required")
	}
	return app.RunInTransaction(func(txApp core.App) error {
		record, err := txApp.FindRecordById("gateway_sessions", sessionID)
		if err != nil {
			return err
		}
		if !record.GetDateTime("revoked_at").IsZero() {
			return nil
		}
		record.Set("revoked_at", time.Now().UTC())
		return txApp.Save(record)
	})
}

func revokeUserSessionsTx(txApp core.App, userID string) (int, error) {
	records, err := txApp.FindRecordsByFilter(
		"gateway_sessions",
		"gateway_user = {:user} && revoked_at = ''",
		"",
		0,
		0,
		dbx.Params{"user": userID},
	)
	if err != nil {
		// Fallback: load by relation and filter in process (PocketBase empty-date filters vary).
		records, err = txApp.FindRecordsByFilter(
			"gateway_sessions",
			"gateway_user = {:user}",
			"",
			0,
			0,
			dbx.Params{"user": userID},
		)
		if err != nil {
			return 0, err
		}
	}
	now := time.Now().UTC()
	n := 0
	for _, record := range records {
		if !record.GetDateTime("revoked_at").IsZero() {
			continue
		}
		record.Set("revoked_at", now)
		if err := txApp.Save(record); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func internalEmail(username string) string {
	replacer := strings.NewReplacer("@", "_at_", " ", "_", "/", "_", "\\", "_")
	return strings.ToLower(replacer.Replace(username)) + "@gateway.local"
}
