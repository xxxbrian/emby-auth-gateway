// Package pbmigrations applies native PocketBase AppMigrations as a
// single-writer application-migration operation.
//
// Production migrations register on core.AppMigrations (via init Register in
// this package). Apply is the Gateway entrypoint that runs those native
// migrations and a write-free final extension validator for sidecar collection
// checks (session profiles and current playbacks). Operational upgrades remain
// single-writer; there is no process-level migration flock beyond SQLite busy
// timeout/retry.
package pbmigrations

import (
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

// Apply runs native AppMigrations and a write-free final extension validator.
//
// The run and validate steps execute inside an atomic nested
// AuxRunInTransaction -> RunInTransaction boundary so nested native migration
// transactions reuse the callback app and any extension validator failure rolls
// back with the migration work.
func Apply(app core.App) error {
	return apply(app, func(tx core.App) error {
		return tx.RunAppMigrations()
	}, validateExtensions)
}

// apply is the testable migration runner. Production uses Apply (native
// AppMigrations + validateExtensions). Tests inject a private
// core.MigrationsList runner and custom validators so they never mutate
// global core.AppMigrations.
func apply(app core.App, run func(tx core.App) error, validate func(tx core.App) error) error {
	return app.AuxRunInTransaction(func(tx core.App) error {
		return tx.RunInTransaction(func(tx core.App) error {
			if err := run(tx); err != nil {
				return err
			}
			return validate(tx)
		})
	})
}

// validateExtensions is the write-free extension validator for all gateway
// sidecars. Every bootstrap strictly validates both gateway_session_profiles
// and gateway_current_playbacks schema/DDL. It deliberately does not require
// row coverage (runtime repair handles auth-row and playback holes).
func validateExtensions(app core.App) error {
	if err := pbschema.ValidateSessionProfiles(app); err != nil {
		return err
	}
	return pbschema.ValidateCurrentPlaybacks(app)
}

// requirePredecessorApplied fails when the exact native migration history row
// for file is missing. Callers must invoke this before any mutation so a
// missing predecessor aborts without partial schema changes.
func requirePredecessorApplied(app core.App, file string) error {
	var exists int
	err := app.DB().Select("(1)").
		From(core.DefaultMigrationsTable).
		Where(dbx.HashExp{"file": file}).
		Limit(1).
		Row(&exists)
	if err != nil || exists == 0 {
		return fmt.Errorf("exact predecessor %q is not applied", file)
	}
	return nil
}
