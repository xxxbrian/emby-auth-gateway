// Package pbmigrations applies native PocketBase AppMigrations as a
// single-writer application-migration operation.
//
// Production migrations register on core.AppMigrations (via migrations.Register
// or equivalent). Apply is the Gateway entrypoint that runs those native
// migrations and a write-free final extension validator seam reserved for
// Phase 3 sidecar collection checks. Operational upgrades remain single-writer;
// there is no process-level migration flock beyond SQLite busy timeout/retry.
package pbmigrations

import (
	"github.com/pocketbase/pocketbase/core"
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
// AppMigrations + no-op validateExtensions). Tests inject a private
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

// validateExtensions is the Phase 3 extension-validator seam.
// Phase 2 is intentionally a no-op; Phase 3 will assert sidecar collections.
func validateExtensions(core.App) error {
	return nil
}
