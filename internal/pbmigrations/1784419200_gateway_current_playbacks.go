package pbmigrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/pbschema"
)

// Stable native AppMigration filename for gateway_current_playbacks.
const migrationGatewayCurrentPlaybacks = "1784419200_gateway_current_playbacks.go"

func init() {
	core.AppMigrations.Register(upGatewayCurrentPlaybacks, downGatewayCurrentPlaybacks, migrationGatewayCurrentPlaybacks)
}

// upGatewayCurrentPlaybacks creates or validates the current-playbacks sidecar.
//
// Semantics:
//  1. Exact Phase 3 predecessor history is required before any mutation.
//  2. Strict base Ensure and SessionProfiles validation run before mutation.
//  3. Missing collection -> create exact pbschema.CurrentPlaybacks.
//  4. Existing collection -> accept only exact schema with zero rows.
//  5. Drift or any preexisting row fails atomically (no backfill, no synthesis
//     from playback_events).
//  6. Postcondition re-validates base, SessionProfiles, CurrentPlaybacks, and
//     zero rows before history commits.
func upGatewayCurrentPlaybacks(app core.App) error {
	// 0. Exact predecessor history before any mutation.
	if err := requirePredecessorApplied(app, migrationGatewaySessionProfiles); err != nil {
		return err
	}

	// 1. Strict base schema before any sidecar mutation.
	if err := pbschema.Ensure(app); err != nil {
		return fmt.Errorf("base schema ensure: %w", err)
	}

	// 2. Session-profiles extension must already be exact.
	if err := pbschema.ValidateSessionProfiles(app); err != nil {
		return fmt.Errorf("validate session profiles: %w", err)
	}

	sessions, err := app.FindCollectionByNameOrId("gateway_sessions")
	if err != nil {
		return fmt.Errorf("find gateway_sessions: %w", err)
	}

	_, found, err := findCollectionByName(app, pbschema.CurrentPlaybacksCollection)
	if err != nil {
		return err
	}
	if !found {
		// 3a. Absent -> create exact locked builder.
		if err := app.Save(pbschema.CurrentPlaybacks(sessions.Id)); err != nil {
			return fmt.Errorf("create %s: %w", pbschema.CurrentPlaybacksCollection, err)
		}
	} else {
		// 3b. Present -> exact schema validation AND zero rows.
		// There is no valid predecessor data and no backfill path.
		if err := pbschema.ValidateCurrentPlaybacks(app); err != nil {
			return fmt.Errorf("validate existing current playbacks schema: %w", err)
		}
		if err := requireZeroCurrentPlaybacks(app); err != nil {
			return err
		}
	}

	// 4. Postcondition: base + both sidecars + zero current rows.
	if err := pbschema.Ensure(app); err != nil {
		return fmt.Errorf("postcondition base schema ensure: %w", err)
	}
	if err := pbschema.ValidateSessionProfiles(app); err != nil {
		return fmt.Errorf("postcondition session profiles: %w", err)
	}
	if err := pbschema.ValidateCurrentPlaybacks(app); err != nil {
		return fmt.Errorf("postcondition current playbacks: %w", err)
	}
	if err := requireZeroCurrentPlaybacks(app); err != nil {
		return fmt.Errorf("postcondition: %w", err)
	}
	return nil
}

// downGatewayCurrentPlaybacks refuses destructive rollback.
//
// Production rollback policy is full pb_data backup/restore. Removing the
// sidecar would destroy in-flight NowPlaying state, so Down is intentionally
// unsupported. Restore the pre-upgrade pb_data directory from backup rather
// than attempting a partial reverse migration.
func downGatewayCurrentPlaybacks(core.App) error {
	return fmt.Errorf(
		"down migration %s is unsupported; restore from pb_data backup (destructive sidecar rollback is not supported — copy the pre-upgrade pb_data directory back into place)",
		migrationGatewayCurrentPlaybacks,
	)
}

func requireZeroCurrentPlaybacks(app core.App) error {
	records, err := app.FindAllRecords(pbschema.CurrentPlaybacksCollection)
	if err != nil {
		return fmt.Errorf("list current playbacks: %w", err)
	}
	if len(records) != 0 {
		return fmt.Errorf(
			"%s has %d preexisting rows; empty collection required (no backfill from playback_events or any other source)",
			pbschema.CurrentPlaybacksCollection,
			len(records),
		)
	}
	return nil
}
