// Package pbmigrations applies native PocketBase AppMigrations as a
// single-writer application-migration operation.
//
// Production migrations register on core.AppMigrations (via init Register in
// this package). Apply is the Gateway entrypoint that runs those native
// migrations and a write-free final extension validator for Phase 3 sidecar
// collection checks. Operational upgrades remain single-writer; there is no
// process-level migration flock beyond SQLite busy timeout/retry.
package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

// Apply runs native AppMigrations and a write-free final extension validator.
//
// The run and validate steps execute inside an atomic nested
// AuxRunInTransaction -> RunInTransaction boundary so nested native migration
// transactions reuse the callback app and any Phase 3 validator failure rolls
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

// validateExtensions is the write-free Phase 3 extension validator.
// It asserts exact gateway_session_profiles schema/DDL only and deliberately
// does not require row coverage (runtime repair handles auth-row holes).
func validateExtensions(app core.App) error {
	return pbschema.ValidateSessionProfiles(app)
}
