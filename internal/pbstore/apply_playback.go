package pbstore

import (
	"context"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

// ApplyPlaybackReport loads session/current/durable state, reduces the report
// once, and commits event/durable/current/profile activity as a single
// PocketBase transaction. Outer Store methods are not used inside the tx.
func (s *Store) ApplyPlaybackReport(ctx context.Context, cmd gateway.PlaybackReportCommand) (gateway.PlaybackReportResult, error) {
	if err := ctx.Err(); err != nil {
		return gateway.PlaybackReportResult{}, err
	}
	preparedCmd, err := gateway.PreparePlaybackReportCommand(cmd)
	if err != nil {
		return gateway.PlaybackReportResult{}, err
	}

	var result gateway.PlaybackReportResult
	err = s.app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		auth, err := tx.FindFirstRecordByData(collectionGatewaySessions, "gateway_token_hash", preparedCmd.GatewayTokenHash)
		if err != nil {
			if isNotFoundError(err) {
				return gateway.ErrNotFound
			}
			return err
		}
		baseSession := sessionFromAuthRecord(auth)
		if !baseSession.Active(time.Now().UTC()) {
			return gateway.ErrUnauthorized
		}

		// Authenticated report path: ensure profile exists (repair hole), then
		// strict hydrate. Corrupt profiles fail closed before mutation.
		profile, err := ensureSessionProfileTx(tx, auth)
		if err != nil {
			return err
		}
		session, err := sessionFromRecords(auth, profile)
		if err != nil {
			return err
		}

		current, err := loadCurrentPlaybackForSessionTx(tx, auth)
		if err != nil {
			return err
		}

		var durable *gateway.PlaybackState
		itemID := preparedCmd.ItemID
		if itemID != "" {
			durable, err = loadDurablePlaybackStateTx(tx, session.GatewayUserID, itemID)
			if err != nil {
				return err
			}
		}

		plan, err := gateway.ReducePlaybackReport(gateway.PlaybackReduceInput{
			Command: preparedCmd,
			Session: *session,
			Current: current,
			Durable: durable,
		})
		if err != nil {
			return err
		}
		if err := gateway.ValidatePlaybackMutationPlan(plan, preparedCmd, *session); err != nil {
			return err
		}

		// Apply mutations in order: event → durable → current → profile activity.
		if plan.Event != nil {
			if err := insertPlaybackEventTx(tx, *plan.Event); err != nil {
				return err
			}
		}
		if plan.WriteDurable && plan.Durable != nil {
			if err := upsertDurablePlaybackStateTx(tx, *plan.Durable); err != nil {
				return err
			}
		}
		if err := applyCurrentPlaybackPlanTx(tx, auth, plan); err != nil {
			return err
		}
		if plan.ActivityAt != nil {
			if err := advanceProfileActivityTx(tx, profile, *plan.ActivityAt); err != nil {
				return err
			}
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		// Result is Applied only after successful commit of this transaction.
		result = plan.Result
		if result.Current != nil {
			cp := cloneCurrentPlaybackValue(*result.Current)
			result.Current = &cp
		}
		if result.Durable != nil {
			d := *result.Durable
			result.Durable = &d
		}
		return nil
	})
	if err != nil {
		return gateway.PlaybackReportResult{}, err
	}
	return result, nil
}

func loadCurrentPlaybackForSessionTx(tx core.App, auth *core.Record) (*gateway.CurrentPlayback, error) {
	if _, err := tx.FindCollectionByNameOrId(collectionGatewayCurrentPlaybacks); err != nil {
		return nil, err
	}
	records, err := tx.FindRecordsByFilter(
		collectionGatewayCurrentPlaybacks,
		"gateway_session = {:sessionID}",
		"",
		0,
		0,
		dbx.Params{"sessionID": auth.Id},
	)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	if len(records) > 1 {
		return nil, fmt.Errorf(
			"current playback integrity: duplicate current rows for gateway_session %q (count=%d)",
			auth.Id,
			len(records),
		)
	}
	cp, err := currentPlaybackFromRecord(records[0], auth)
	if err != nil {
		return nil, err
	}
	out := cloneCurrentPlaybackValue(cp)
	return &out, nil
}

func loadDurablePlaybackStateTx(tx core.App, gatewayUserID, itemID string) (*gateway.PlaybackState, error) {
	records, err := tx.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID} && item_id = {:itemID}",
		"",
		0,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID, "itemID": itemID},
	)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	if len(records) > 1 {
		return nil, fmt.Errorf(
			"playback state integrity: duplicate user_item_data rows for user %q item %q (count=%d)",
			gatewayUserID,
			itemID,
			len(records),
		)
	}
	return playbackStateFromRecord(records[0]), nil
}

func insertPlaybackEventTx(tx core.App, event gateway.PlaybackEvent) error {
	collection, err := tx.FindCollectionByNameOrId("playback_events")
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	record.Set("gateway_user", event.GatewayUserID)
	record.Set("synthetic_user_id", event.SyntheticUserID)
	record.Set("item_id", event.ItemID)
	record.Set("item_name", event.ItemName)
	record.Set("event", event.Event)
	record.Set("playback_position_ticks", event.PositionTicks)
	if event.Played != nil {
		record.Set("played", *event.Played)
	}
	if event.PlayedPercentage != nil {
		record.Set("played_percentage", *event.PlayedPercentage)
	}
	record.Set("remote_ip", event.RemoteIP)
	if !event.CreatedAt.IsZero() {
		record.Set("occurred_at", event.CreatedAt.UTC())
	} else {
		record.Set("occurred_at", time.Now().UTC())
	}
	return tx.Save(record)
}

func upsertDurablePlaybackStateTx(tx core.App, state gateway.PlaybackState) error {
	records, err := tx.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID} && item_id = {:itemID}",
		"",
		0,
		0,
		dbx.Params{"gatewayUserID": state.GatewayUserID, "itemID": state.ItemID},
	)
	if err != nil {
		return err
	}
	if len(records) > 1 {
		return fmt.Errorf(
			"playback state integrity: duplicate user_item_data rows for user %q item %q (count=%d)",
			state.GatewayUserID,
			state.ItemID,
			len(records),
		)
	}
	var record *core.Record
	if len(records) == 1 {
		record = records[0]
	} else {
		collection, err := tx.FindCollectionByNameOrId("user_item_data")
		if err != nil {
			return err
		}
		record = core.NewRecord(collection)
		record.Set("gateway_user", state.GatewayUserID)
		record.Set("item_id", state.ItemID)
	}
	applyPlaybackStateFields(record, state)
	if err := tx.Save(record); err != nil {
		if !record.IsNew() || !isUniqueConstraintError(err) {
			return err
		}
		// Concurrent insert of the same (user, item) unique pair: re-load winner and update.
		records, loadErr := tx.FindRecordsByFilter(
			"user_item_data",
			"gateway_user = {:gatewayUserID} && item_id = {:itemID}",
			"",
			0,
			0,
			dbx.Params{"gatewayUserID": state.GatewayUserID, "itemID": state.ItemID},
		)
		if loadErr != nil {
			return loadErr
		}
		if len(records) != 1 {
			return err
		}
		applyPlaybackStateFields(records[0], state)
		return tx.Save(records[0])
	}
	return nil
}

// applyPlaybackStateFields mirrors SavePlaybackState field mapping without
// performing collection lookup or outer method calls.
func applyPlaybackStateFields(record *core.Record, state gateway.PlaybackState) {
	record.Set("synthetic_user_id", state.SyntheticUserID)
	record.Set("item_name", state.ItemName)
	record.Set("item_type", state.ItemType)
	record.Set("series_id", state.SeriesID)
	record.Set("series_name", state.SeriesName)
	record.Set("season_id", state.SeasonID)
	record.Set("index_number", state.IndexNumber)
	record.Set("parent_index_number", state.ParentIndexNumber)
	record.Set("run_time_ticks", state.RunTimeTicks)
	record.Set("played", state.Played)
	record.Set("playback_position_ticks", state.PlaybackPositionTicks)
	if state.PlayedPercentage != nil {
		record.Set("played_percentage", *state.PlayedPercentage)
		record.Set("played_percentage_set", true)
	} else {
		record.Set("played_percentage", nil)
		record.Set("played_percentage_set", false)
	}
	if state.LastPlayedDate != nil {
		record.Set("last_played_date", *state.LastPlayedDate)
	} else {
		record.Set("last_played_date", nil)
	}
	record.Set("play_count", state.PlayCount)
	record.Set("is_favorite", state.IsFavorite)
	if state.Likes != nil {
		record.Set("likes", *state.Likes)
		record.Set("likes_set", true)
	} else {
		record.Set("likes", false)
		record.Set("likes_set", false)
	}
	record.Set("fingerprint", state.Fingerprint)
	if state.OrphanedAt != nil {
		record.Set("orphaned_at", *state.OrphanedAt)
	} else {
		record.Set("orphaned_at", nil)
	}
	if state.LastSeenAt != nil {
		record.Set("last_seen_at", *state.LastSeenAt)
	} else {
		record.Set("last_seen_at", nil)
	}
}

func applyCurrentPlaybackPlanTx(tx core.App, auth *core.Record, plan gateway.PlaybackMutationPlan) error {
	switch plan.CurrentAction {
	case gateway.PlaybackCurrentNone, gateway.PlaybackCurrentPreserve:
		return nil
	case gateway.PlaybackCurrentDelete:
		return deleteCurrentPlaybackForSessionTx(tx, auth.Id)
	case gateway.PlaybackCurrentUpsert:
		if plan.Current == nil {
			return fmt.Errorf("current playback integrity: upsert plan missing Current")
		}
		return upsertCurrentPlaybackTx(tx, auth, *plan.Current)
	default:
		return fmt.Errorf("current playback integrity: unknown current action %d", plan.CurrentAction)
	}
}

func deleteCurrentPlaybackForSessionTx(tx core.App, sessionID string) error {
	if _, err := tx.FindCollectionByNameOrId(collectionGatewayCurrentPlaybacks); err != nil {
		// Collection absent: nothing to delete (pre-Phase-4 schema).
		return nil
	}
	records, err := tx.FindRecordsByFilter(
		collectionGatewayCurrentPlaybacks,
		"gateway_session = {:sessionID}",
		"",
		0,
		0,
		dbx.Params{"sessionID": sessionID},
	)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if err := tx.Delete(rec); err != nil {
			return err
		}
	}
	return nil
}

func upsertCurrentPlaybackTx(tx core.App, auth *core.Record, cp gateway.CurrentPlayback) error {
	records, err := tx.FindRecordsByFilter(
		collectionGatewayCurrentPlaybacks,
		"gateway_session = {:sessionID}",
		"",
		0,
		0,
		dbx.Params{"sessionID": auth.Id},
	)
	if err != nil {
		return err
	}
	if len(records) > 1 {
		return fmt.Errorf(
			"current playback integrity: duplicate current rows for gateway_session %q (count=%d)",
			auth.Id,
			len(records),
		)
	}
	if len(records) == 1 {
		if err := setCurrentPlaybackRecord(records[0], auth, cp); err != nil {
			return err
		}
		return tx.Save(records[0])
	}

	collection, err := tx.FindCollectionByNameOrId(collectionGatewayCurrentPlaybacks)
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	if err := setCurrentPlaybackRecord(record, auth, cp); err != nil {
		return err
	}
	if err := tx.Save(record); err != nil {
		if !isUniqueConstraintError(err) {
			return err
		}
		// Concurrent insert on unique session index: re-load winner and update.
		records, loadErr := tx.FindRecordsByFilter(
			collectionGatewayCurrentPlaybacks,
			"gateway_session = {:sessionID}",
			"",
			0,
			0,
			dbx.Params{"sessionID": auth.Id},
		)
		if loadErr != nil {
			return loadErr
		}
		if len(records) != 1 {
			return err
		}
		if setErr := setCurrentPlaybackRecord(records[0], auth, cp); setErr != nil {
			return setErr
		}
		return tx.Save(records[0])
	}
	return nil
}

func advanceProfileActivityTx(tx core.App, profile *core.Record, at time.Time) error {
	at = at.UTC()
	last := profile.GetDateTime("last_activity_at").Time()
	if !last.IsZero() && !at.After(last) {
		return nil
	}
	profile.Set("last_activity_at", at)
	return tx.Save(profile)
}
