package pbmigrations

import (
	"errors"
	"fmt"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestApplyOrderedUpHistoryAndIdempotentSecondApply(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)

	list := core.MigrationsList{}
	// Register out of order so the predecessor assertion proves native filename sorting.
	list.Register(func(tx core.App) error {
		if err := requirePredecessorApplied(tx, "1_first.go"); err != nil {
			return err
		}
		_, err := tx.DB().NewQuery(`INSERT INTO apply_marker (id, step) VALUES ('m2', 'two')`).Execute()
		return err
	}, nil, "2_second.go")
	list.Register(func(tx core.App) error {
		if _, err := tx.DB().NewQuery(`CREATE TABLE IF NOT EXISTS apply_marker (id TEXT PRIMARY KEY NOT NULL, step TEXT NOT NULL)`).Execute(); err != nil {
			return err
		}
		_, err := tx.DB().NewQuery(`INSERT INTO apply_marker (id, step) VALUES ('m1', 'one')`).Execute()
		return err
	}, nil, "1_first.go")

	if err := applyPrivate(app, list, nil); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	assertMigrationHistory(t, app, "1_first.go", true)
	assertMigrationHistory(t, app, "2_second.go", true)
	assertMarkerCount(t, app, 2)

	if err := applyPrivate(app, list, nil); err != nil {
		t.Fatalf("second apply (idempotent): %v", err)
	}

	assertMigrationHistory(t, app, "1_first.go", true)
	assertMigrationHistory(t, app, "2_second.go", true)
	assertMarkerCount(t, app, 2)
}

func TestApplyUpErrorRollsBackMutationAndHistoryThenRetrySucceeds(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)

	// First private list: first migration succeeds, second mutates then fails.
	failList := core.MigrationsList{}
	failList.Register(func(tx core.App) error {
		if _, err := tx.DB().NewQuery(`CREATE TABLE IF NOT EXISTS apply_marker (id TEXT PRIMARY KEY NOT NULL, step TEXT NOT NULL)`).Execute(); err != nil {
			return err
		}
		_, err := tx.DB().NewQuery(`INSERT INTO apply_marker (id, step) VALUES ('ok', 'first')`).Execute()
		return err
	}, nil, "1_ok.go")
	failList.Register(func(tx core.App) error {
		if err := requirePredecessorApplied(tx, "1_ok.go"); err != nil {
			return err
		}
		if _, err := tx.DB().NewQuery(`INSERT INTO apply_marker (id, step) VALUES ('boom', 'second')`).Execute(); err != nil {
			return err
		}
		return errors.New("forced up failure after mutation")
	}, nil, "2_fail.go")

	err := applyPrivate(app, failList, nil)
	if err == nil {
		t.Fatal("expected apply to fail on second migration")
	}

	// Entire nested transaction rolls back: no history, no marker table rows.
	assertMigrationHistory(t, app, "1_ok.go", false)
	assertMigrationHistory(t, app, "2_fail.go", false)
	assertTableMissingOrEmpty(t, app, "apply_marker")

	// Retry with a corrected private list succeeds.
	okList := core.MigrationsList{}
	okList.Register(func(tx core.App) error {
		if _, err := tx.DB().NewQuery(`CREATE TABLE IF NOT EXISTS apply_marker (id TEXT PRIMARY KEY NOT NULL, step TEXT NOT NULL)`).Execute(); err != nil {
			return err
		}
		_, err := tx.DB().NewQuery(`INSERT INTO apply_marker (id, step) VALUES ('ok', 'first')`).Execute()
		return err
	}, nil, "1_ok.go")
	okList.Register(func(tx core.App) error {
		if err := requirePredecessorApplied(tx, "1_ok.go"); err != nil {
			return err
		}
		_, err := tx.DB().NewQuery(`INSERT INTO apply_marker (id, step) VALUES ('fixed', 'second')`).Execute()
		return err
	}, nil, "2_fail.go")

	if err := applyPrivate(app, okList, nil); err != nil {
		t.Fatalf("retry apply: %v", err)
	}

	assertMigrationHistory(t, app, "1_ok.go", true)
	assertMigrationHistory(t, app, "2_fail.go", true)
	assertMarkerCount(t, app, 2)
}

func TestApplyFinalValidatorFailureRollsBackMigrationAndHistory(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)

	list := core.MigrationsList{}
	list.Register(func(tx core.App) error {
		if _, err := tx.DB().NewQuery(`CREATE TABLE IF NOT EXISTS apply_marker (id TEXT PRIMARY KEY NOT NULL, step TEXT NOT NULL)`).Execute(); err != nil {
			return err
		}
		_, err := tx.DB().NewQuery(`INSERT INTO apply_marker (id, step) VALUES ('v', 'validate')`).Execute()
		return err
	}, nil, "1_validate.go")

	validatorErr := errors.New("final extension validator failed")
	err := applyPrivate(app, list, func(core.App) error {
		return validatorErr
	})
	if !errors.Is(err, validatorErr) {
		t.Fatalf("expected validator error, got %v", err)
	}

	assertMigrationHistory(t, app, "1_validate.go", false)
	assertTableMissingOrEmpty(t, app, "apply_marker")
}

func TestApplyExactPredecessorCheckFailsBeforeMutationAndHistory(t *testing.T) {
	t.Parallel()

	app := newTestApp(t)

	// Only the second migration is registered; it requires a missing predecessor.
	list := core.MigrationsList{}
	list.Register(func(tx core.App) error {
		if err := requirePredecessorApplied(tx, "1_missing.go"); err != nil {
			return err
		}
		if _, err := tx.DB().NewQuery(`CREATE TABLE IF NOT EXISTS apply_marker (id TEXT PRIMARY KEY NOT NULL, step TEXT NOT NULL)`).Execute(); err != nil {
			return err
		}
		_, err := tx.DB().NewQuery(`INSERT INTO apply_marker (id, step) VALUES ('bad', 'should-not-exist')`).Execute()
		return err
	}, nil, "2_depends.go")

	err := applyPrivate(app, list, nil)
	if err == nil {
		t.Fatal("expected predecessor check to fail")
	}

	assertMigrationHistory(t, app, "2_depends.go", false)
	assertMigrationHistory(t, app, "1_missing.go", false)
	assertTableMissingOrEmpty(t, app, "apply_marker")
}

// applyPrivate runs the unexported apply helper with a private MigrationsList
// runner. Tests must never touch global core.AppMigrations.
//
// A nil validate callback is a no-op so runner-unit tests stay independent of
// the production Phase 3 sidecar validator (use validateExtensions explicitly
// when testing that seam).
func applyPrivate(app core.App, list core.MigrationsList, validate func(core.App) error) error {
	if validate == nil {
		validate = func(core.App) error { return nil }
	}
	return apply(app, func(tx core.App) error {
		_, err := core.NewMigrationsRunner(tx, list).Up()
		return err
	}, validate)
}

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

func newTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "test",
	})
	if err != nil {
		t.Fatalf("new test app: %v", err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func assertMigrationHistory(t *testing.T, app core.App, file string, want bool) {
	t.Helper()
	var exists int
	err := app.DB().Select("(1)").
		From(core.DefaultMigrationsTable).
		Where(dbx.HashExp{"file": file}).
		Limit(1).
		Row(&exists)
	got := err == nil && exists > 0
	if got != want {
		t.Fatalf("migration history for %q: got %v, want %v (query err=%v)", file, got, want, err)
	}
}

func assertMarkerCount(t *testing.T, app core.App, want int) {
	t.Helper()
	var count int
	if err := app.DB().NewQuery(`SELECT COUNT(*) FROM apply_marker`).Row(&count); err != nil {
		t.Fatalf("count apply_marker: %v", err)
	}
	if count != want {
		t.Fatalf("apply_marker row count: got %d, want %d", count, want)
	}
}

func assertTableMissingOrEmpty(t *testing.T, app core.App, table string) {
	t.Helper()
	var count int
	err := app.DB().NewQuery(fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Row(&count)
	if err != nil {
		// table does not exist after rollback of CREATE TABLE — acceptable
		return
	}
	if count != 0 {
		t.Fatalf("expected %s empty after rollback, got %d rows", table, count)
	}
}
