package pbstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

type Store struct {
	app core.App
}

var (
	_ gateway.SessionRepository  = (*Store)(nil)
	_ gateway.PlaybackRepository = (*Store)(nil)
)

const playbackStateItemIDBatchLimit = 50

func New(app core.App) *Store {
	return &Store{app: app}
}

func (s *Store) LoadDefaultUpstreamRuntime(ctx context.Context) (*gateway.UpstreamRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var runtime *gateway.UpstreamRuntime
	err := s.app.RunInTransaction(func(tx core.App) error {
		var err error
		runtime, err = loadDefaultUpstreamRuntime(ctx, tx)
		if err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return nil, classifyUpstreamStoreError(ctx, err)
	}
	return runtime, nil
}

func (s *Store) CompareAndSwapUpstreamAuth(ctx context.Context, update gateway.UpstreamAuthUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := gateway.ValidateUpstreamAuthUpdate(update); err != nil {
		return err
	}
	err := s.app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		at := update.AuthenticatedAt.UTC()
		result, err := tx.DB().NewQuery(`UPDATE upstream_sources
			SET auth_generation_id = {:generationID}, backend_authorization_device_id = {:deviceID}, backend_user_id = {:backendUserID}, backend_token = {:backendToken}, token_updated_at = {:authenticatedAt}, last_login_at = {:authenticatedAt}, last_login_error = ''
			WHERE id = {:sourceID} AND key = 'default' AND auth_generation_id = {:expectedGenerationID}`).
			WithContext(ctx).
			Bind(dbx.Params{
				"generationID": update.GenerationID, "deviceID": update.DeviceID, "backendUserID": update.BackendUserID,
				"backendToken": update.BackendToken, "authenticatedAt": at, "sourceID": update.SourceID,
				"expectedGenerationID": update.ExpectedGenerationID,
			}).Execute()
		if err != nil {
			return fmt.Errorf("%w: update upstream authentication: %w", gateway.ErrStoreUnavailable, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("%w: inspect upstream authentication update: %w", gateway.ErrStoreUnavailable, err)
		}
		if affected == 1 {
			return ctx.Err()
		}
		runtime, loadErr := loadDefaultUpstreamRuntime(ctx, tx)
		if loadErr != nil {
			return loadErr
		}
		if runtime.Source.ID != update.SourceID {
			return gateway.ErrUpstreamNotFound
		}
		return gateway.ErrUpstreamAuthConflict
	})
	return classifyUpstreamStoreError(ctx, err)
}

func (s *Store) UpdateUpstreamServerInfo(ctx context.Context, update gateway.UpstreamServerInfoUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := gateway.ValidateUpstreamServerInfoUpdate(update); err != nil {
		return err
	}
	err := s.app.RunInTransaction(func(tx core.App) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		checkedAt := update.CheckedAt.UTC().Truncate(time.Millisecond).Format(types.DefaultDateLayout)
		result, err := tx.DB().NewQuery(`UPDATE upstream_sources
			SET server_name = CASE WHEN {:serverName} <> '' THEN {:serverName} ELSE server_name END,
				server_version = CASE WHEN {:serverVersion} <> '' THEN {:serverVersion} ELSE server_version END,
				version_checked_at = {:checkedAt}
			WHERE id = {:sourceID} AND key = 'default' AND server_id = {:serverID}`).
			WithContext(ctx).
			Bind(dbx.Params{
				"sourceID": update.SourceID, "serverID": update.ServerID, "serverName": update.ServerName,
				"serverVersion": update.ServerVersion, "checkedAt": checkedAt,
			}).Execute()
		if err != nil {
			return fmt.Errorf("%w: update upstream server info: %w", gateway.ErrStoreUnavailable, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("%w: inspect upstream server info update: %w", gateway.ErrStoreUnavailable, err)
		}
		if affected == 1 {
			return ctx.Err()
		}
		runtime, loadErr := loadDefaultUpstreamRuntime(ctx, tx)
		if loadErr != nil {
			return loadErr
		}
		if runtime.Source.ID != update.SourceID {
			return gateway.ErrUpstreamNotFound
		}
		return gateway.ErrUpstreamServerInfoConflict
	})
	return classifyUpstreamStoreError(ctx, err)
}

func classifyUpstreamStoreError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isGatewayDomainError(err) {
		return err
	}
	return fmt.Errorf("%w: %w", gateway.ErrStoreUnavailable, err)
}

func isGatewayDomainError(err error) bool {
	return errors.Is(err, gateway.ErrInvalidCredentials) || errors.Is(err, gateway.ErrNotFound) || errors.Is(err, gateway.ErrDisabled) || errors.Is(err, gateway.ErrUnauthorized) || errors.Is(err, gateway.ErrBadRequest) || errors.Is(err, gateway.ErrUpstreamNotFound) || errors.Is(err, gateway.ErrInvalidUpstreamTopology) || errors.Is(err, gateway.ErrUpstreamAuthConflict) || errors.Is(err, gateway.ErrUpstreamServerInfoConflict) || errors.Is(err, gateway.ErrStoreUnavailable)
}

func loadDefaultUpstreamRuntime(ctx context.Context, app core.App) (*gateway.UpstreamRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := app.FindCollectionByNameOrId("upstream_sources"); err != nil {
		return nil, fmt.Errorf("%w: upstream_sources: %w", gateway.ErrStoreUnavailable, err)
	}
	if _, err := app.FindCollectionByNameOrId("upstream_endpoints"); err != nil {
		return nil, fmt.Errorf("%w: upstream_endpoints: %w", gateway.ErrStoreUnavailable, err)
	}
	sources, err := app.FindAllRecords("upstream_sources")
	if err != nil {
		return nil, fmt.Errorf("%w: load upstream sources: %w", gateway.ErrStoreUnavailable, err)
	}
	endpoints, err := app.FindAllRecords("upstream_endpoints")
	if err != nil {
		return nil, fmt.Errorf("%w: load upstream endpoints: %w", gateway.ErrStoreUnavailable, err)
	}
	if len(sources) == 0 && len(endpoints) == 0 {
		return nil, gateway.ErrUpstreamNotFound
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("%w: endpoints without source", gateway.ErrInvalidUpstreamTopology)
	}
	if len(sources) != 1 {
		return nil, fmt.Errorf("%w: expected one source", gateway.ErrInvalidUpstreamTopology)
	}
	source := upstreamSourceFromRecord(sources[0])
	var active []gateway.UpstreamEndpoint
	for _, record := range endpoints {
		endpoint := gateway.UpstreamEndpoint{ID: record.Id, SourceID: record.GetString("source"), Key: record.GetString("key"), BaseURL: record.GetString("base_url"), Active: record.GetBool("active")}
		if err := gateway.ValidateUpstreamEndpoint(source.ID, endpoint); err != nil {
			return nil, err
		}
		if endpoint.Active {
			active = append(active, endpoint)
		}
	}
	if len(active) != 1 {
		return nil, fmt.Errorf("%w: expected one active endpoint", gateway.ErrInvalidUpstreamTopology)
	}
	runtime := &gateway.UpstreamRuntime{Source: source, Endpoint: active[0]}
	if err := gateway.ValidateUpstreamRuntime(*runtime); err != nil {
		return nil, err
	}
	return runtime, nil
}

func upstreamSourceFromRecord(record *core.Record) gateway.UpstreamSource {
	var versionCheckedAt, tokenUpdatedAt, lastLoginAt *time.Time
	if !record.GetDateTime("version_checked_at").IsZero() {
		t := record.GetDateTime("version_checked_at").Time()
		versionCheckedAt = &t
	}
	if !record.GetDateTime("token_updated_at").IsZero() {
		t := record.GetDateTime("token_updated_at").Time()
		tokenUpdatedAt = &t
	}
	if !record.GetDateTime("last_login_at").IsZero() {
		t := record.GetDateTime("last_login_at").Time()
		lastLoginAt = &t
	}
	return gateway.UpstreamSource{
		ID: record.Id, Key: record.GetString("key"), ServerID: record.GetString("server_id"), ServerName: record.GetString("server_name"), ServerVersion: record.GetString("server_version"), VersionCheckedAt: versionCheckedAt,
		BackendUsername: record.GetString("backend_username"), BackendPassword: record.GetString("backend_password"), BackendUserID: record.GetString("backend_user_id"), BackendToken: record.GetString("backend_token"), AuthGenerationID: record.GetString("auth_generation_id"), TokenUpdatedAt: tokenUpdatedAt, LastLoginAt: lastLoginAt, LastLoginError: record.GetString("last_login_error"),
		ClientIdentity: gateway.BackendClientIdentity{UserAgent: record.GetString("backend_user_agent"), Client: record.GetString("backend_authorization_client"), Device: record.GetString("backend_authorization_device"), DeviceID: record.GetString("backend_authorization_device_id"), Version: record.GetString("backend_authorization_version")},
	}
}

func (s *Store) AuthenticateGatewayUser(ctx context.Context, username, password string) (*gateway.GatewayUser, error) {
	record, err := s.app.FindFirstRecordByData("users", "username", username)
	if err != nil {
		return nil, gateway.ErrInvalidCredentials
	}
	if !record.GetBool("enabled") || !record.ValidatePassword(password) {
		return nil, gateway.ErrInvalidCredentials
	}
	return userFromRecord(record), nil
}

func (s *Store) FindGatewayUserByUsername(ctx context.Context, username string) (*gateway.GatewayUser, error) {
	record, err := s.app.FindFirstRecordByData("users", "username", username)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	return userFromRecord(record), nil
}

func (s *Store) ListPublicUsers(ctx context.Context) ([]gateway.GatewayUser, error) {
	records, err := s.app.FindAllRecords("users", dbx.HashExp{"enabled": true})
	if err != nil {
		return nil, err
	}
	users := make([]gateway.GatewayUser, 0, len(records))
	for _, record := range records {
		users = append(users, *userFromRecord(record))
	}
	return users, nil
}

func (s *Store) FindUserBySyntheticID(ctx context.Context, syntheticID string) (*gateway.GatewayUser, error) {
	record, err := s.app.FindFirstRecordByData("users", "synthetic_user_id", syntheticID)
	if err != nil {
		return nil, gateway.ErrNotFound
	}
	if !record.GetBool("enabled") {
		return nil, gateway.ErrDisabled
	}
	return userFromRecord(record), nil
}

func (s *Store) RecordAudit(ctx context.Context, entry gateway.AuditLog) error {
	collection, err := s.app.FindCollectionByNameOrId("audit_logs")
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	if entry.GatewayUserID != "" {
		record.Set("gateway_user", entry.GatewayUserID)
	}
	record.Set("synthetic_user_id", entry.SyntheticUserID)
	record.Set("event", entry.Event)
	record.Set("message", entry.Message)
	record.Set("remote_ip", entry.RemoteIP)
	record.Set("method", entry.Method)
	record.Set("path", entry.Path)
	record.Set("status", entry.Status)
	record.Set("error_kind", entry.ErrorKind)
	record.Set("direction", entry.Direction)
	record.Set("bytes_transferred", entry.BytesTransferred)
	record.Set("duration_ms", entry.DurationMS)
	record.Set("upstream_status", entry.UpstreamStatus)
	record.Set("response_committed", entry.ResponseCommitted)
	return s.app.Save(record)
}

func (s *Store) CheckPathPolicy(ctx context.Context, method, relativePath string) (gateway.PathPolicyDecision, error) {
	policies, err := s.enabledPathPolicies()
	if err != nil {
		return gateway.PathPolicyDecision{}, err
	}
	return gateway.DecidePathPolicy(policies, method, relativePath), nil
}

func (s *Store) RecordPlaybackEvent(ctx context.Context, event gateway.PlaybackEvent) error {
	collection, err := s.app.FindCollectionByNameOrId("playback_events")
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
		record.Set("occurred_at", event.CreatedAt)
	} else {
		record.Set("occurred_at", time.Now().UTC())
	}
	return s.app.Save(record)
}

func (s *Store) FindPlaybackState(ctx context.Context, gatewayUserID, itemID string) (*gateway.PlaybackState, error) {
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID} && item_id = {:itemID}",
		"",
		1,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID, "itemID": itemID},
	)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, gateway.ErrNotFound
	}
	return playbackStateFromRecord(records[0]), nil
}

func (s *Store) ListPlaybackStatesByItemIDs(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*gateway.PlaybackState, error) {
	if len(itemIDs) == 0 {
		return map[string]*gateway.PlaybackState{}, nil
	}
	states := map[string]*gateway.PlaybackState{}
	for start := 0; start < len(itemIDs); start += playbackStateItemIDBatchLimit {
		end := start + playbackStateItemIDBatchLimit
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		batchStates, err := s.listPlaybackStatesByItemIDBatch(ctx, gatewayUserID, itemIDs[start:end])
		if err != nil {
			return nil, err
		}
		for id, state := range batchStates {
			states[id] = state
		}
	}
	return states, nil
}

func (s *Store) listPlaybackStatesByItemIDBatch(ctx context.Context, gatewayUserID string, itemIDs []string) (map[string]*gateway.PlaybackState, error) {
	filterParts := make([]string, 0, len(itemIDs))
	params := dbx.Params{"gatewayUserID": gatewayUserID}
	for i, itemID := range itemIDs {
		if itemID == "" {
			continue
		}
		name := fmt.Sprintf("itemID%d", i)
		filterParts = append(filterParts, "item_id = {:"+name+"}")
		params[name] = itemID
	}
	if len(filterParts) == 0 {
		return map[string]*gateway.PlaybackState{}, nil
	}
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID} && ("+strings.Join(filterParts, " || ")+")",
		"",
		0,
		0,
		params,
	)
	if err != nil {
		return nil, err
	}
	states := make(map[string]*gateway.PlaybackState, len(records))
	for _, record := range records {
		state := playbackStateFromRecord(record)
		if state.OrphanedAt != nil {
			continue
		}
		states[state.ItemID] = state
	}
	return states, nil
}

func (s *Store) ListPlaybackAggregates(ctx context.Context, gatewayUserID string, seriesIDs, seasonIDs []string) (gateway.PlaybackAggregates, error) {
	aggregates := gateway.PlaybackAggregates{Series: map[string]gateway.PlaybackAggregate{}, Seasons: map[string]gateway.PlaybackAggregate{}}
	seriesSet := stringSet(seriesIDs)
	seasonSet := stringSet(seasonIDs)
	if len(seriesSet) == 0 && len(seasonSet) == 0 {
		return aggregates, nil
	}
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID}",
		"",
		0,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID},
	)
	if err != nil {
		return aggregates, err
	}
	for _, record := range records {
		state := playbackStateFromRecord(record)
		if state.OrphanedAt != nil {
			continue
		}
		if seriesSet[state.SeriesID] {
			aggregates.Series[state.SeriesID] = addPlaybackAggregate(aggregates.Series[state.SeriesID], *state)
		}
		if seasonSet[state.SeasonID] {
			aggregates.Seasons[state.SeasonID] = addPlaybackAggregate(aggregates.Seasons[state.SeasonID], *state)
		}
	}
	return aggregates, nil
}

func (s *Store) ListItemChildCounts(ctx context.Context, itemIDs []string) (map[string]gateway.ItemChildCount, error) {
	counts := map[string]gateway.ItemChildCount{}
	if len(itemIDs) == 0 {
		return counts, nil
	}
	for start := 0; start < len(itemIDs); start += playbackStateItemIDBatchLimit {
		end := start + playbackStateItemIDBatchLimit
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		batch, err := s.listItemChildCountBatch(ctx, itemIDs[start:end])
		if err != nil {
			return nil, err
		}
		for itemID, count := range batch {
			counts[itemID] = count
		}
	}
	return counts, nil
}

func (s *Store) listItemChildCountBatch(ctx context.Context, itemIDs []string) (map[string]gateway.ItemChildCount, error) {
	filterParts := make([]string, 0, len(itemIDs))
	params := dbx.Params{}
	for i, itemID := range itemIDs {
		if itemID == "" {
			continue
		}
		name := fmt.Sprintf("itemID%d", i)
		filterParts = append(filterParts, "item_id = {:"+name+"}")
		params[name] = itemID
	}
	if len(filterParts) == 0 {
		return map[string]gateway.ItemChildCount{}, nil
	}
	records, err := s.app.FindRecordsByFilter(
		"item_child_counts",
		strings.Join(filterParts, " || "),
		"",
		0,
		0,
		params,
	)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]gateway.ItemChildCount, len(records))
	for _, record := range records {
		count := itemChildCountFromRecord(record)
		counts[count.ItemID] = count
	}
	return counts, nil
}

func (s *Store) SaveItemChildCount(ctx context.Context, count gateway.ItemChildCount) error {
	return s.SaveItemChildCounts(ctx, []gateway.ItemChildCount{count})
}

func (s *Store) SaveItemChildCounts(ctx context.Context, counts []gateway.ItemChildCount) error {
	// Dedupe by ItemID (last wins) while filtering invalid entries.
	deduped := make([]gateway.ItemChildCount, 0, len(counts))
	indexByItemID := map[string]int{}
	for _, count := range counts {
		if count.ItemID == "" || count.ChildCount <= 0 {
			continue
		}
		if idx, ok := indexByItemID[count.ItemID]; ok {
			deduped[idx] = count
			continue
		}
		indexByItemID[count.ItemID] = len(deduped)
		deduped = append(deduped, count)
	}
	if len(deduped) == 0 {
		return nil
	}

	itemIDs := make([]string, len(deduped))
	for i, count := range deduped {
		itemIDs[i] = count.ItemID
	}
	existing, err := s.findItemChildCountRecords(ctx, itemIDs)
	if err != nil {
		return err
	}

	// Best-effort per entry: one failure must not prevent later valid counts
	// from being written (matches pre-batch independent SaveItemChildCount calls).
	var collection *core.Collection
	var firstErr error
	for _, count := range deduped {
		if err := s.upsertItemChildCount(ctx, count, existing, &collection); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Store) upsertItemChildCount(ctx context.Context, count gateway.ItemChildCount, existing map[string]*core.Record, collection **core.Collection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record := existing[count.ItemID]
	if record == nil {
		if *collection == nil {
			c, err := s.app.FindCollectionByNameOrId("item_child_counts")
			if err != nil {
				return err
			}
			*collection = c
		}
		record = core.NewRecord(*collection)
		record.Set("item_id", count.ItemID)
	}
	record.Set("child_count", count.ChildCount)
	if err := s.app.Save(record); err != nil {
		if record.IsNew() && isUniqueConstraintError(err) {
			// Concurrent create: reload and update the winner row.
			records, loadErr := s.app.FindRecordsByFilter(
				"item_child_counts",
				"item_id = {:itemID}",
				"",
				1,
				0,
				dbx.Params{"itemID": count.ItemID},
			)
			if loadErr != nil {
				return loadErr
			}
			if len(records) == 0 {
				return err
			}
			record = records[0]
			record.Set("child_count", count.ChildCount)
			return s.app.Save(record)
		}
		return err
	}
	existing[count.ItemID] = record
	return nil
}

func (s *Store) findItemChildCountRecords(ctx context.Context, itemIDs []string) (map[string]*core.Record, error) {
	recordsByItemID := map[string]*core.Record{}
	if len(itemIDs) == 0 {
		return recordsByItemID, nil
	}
	for start := 0; start < len(itemIDs); start += playbackStateItemIDBatchLimit {
		end := start + playbackStateItemIDBatchLimit
		if end > len(itemIDs) {
			end = len(itemIDs)
		}
		filterParts := make([]string, 0, end-start)
		params := dbx.Params{}
		for i, itemID := range itemIDs[start:end] {
			if itemID == "" {
				continue
			}
			name := fmt.Sprintf("itemID%d", i)
			filterParts = append(filterParts, "item_id = {:"+name+"}")
			params[name] = itemID
		}
		if len(filterParts) == 0 {
			continue
		}
		records, err := s.app.FindRecordsByFilter(
			"item_child_counts",
			strings.Join(filterParts, " || "),
			"",
			0,
			0,
			params,
		)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			recordsByItemID[record.GetString("item_id")] = record
		}
	}
	return recordsByItemID, nil
}

func (s *Store) ListPlaybackStates(ctx context.Context, gatewayUserID string, filter gateway.PlaybackStateFilter) ([]gateway.PlaybackState, error) {
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID}",
		"-updated",
		0,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID},
	)
	if err != nil {
		return nil, err
	}
	states := make([]gateway.PlaybackState, 0, len(records))
	for _, record := range records {
		state := playbackStateFromRecord(record)
		if !filter.IncludeOrphaned && state.OrphanedAt != nil {
			continue
		}
		if filter.Played != nil && state.Played != *filter.Played {
			continue
		}
		if filter.Favorite != nil && state.IsFavorite != *filter.Favorite {
			continue
		}
		if filter.Resumable != nil {
			resumable := state.PlaybackPositionTicks > 0 && !state.Played
			if resumable != *filter.Resumable {
				continue
			}
		}
		if filter.SeriesID != "" && state.SeriesID != filter.SeriesID {
			continue
		}
		if filter.SeasonID != "" && state.SeasonID != filter.SeasonID {
			continue
		}
		states = append(states, *state)
	}
	return states, nil
}

func (s *Store) SavePlaybackState(ctx context.Context, state gateway.PlaybackState) error {
	records, err := s.app.FindRecordsByFilter(
		"user_item_data",
		"gateway_user = {:gatewayUserID} && item_id = {:itemID}",
		"",
		1,
		0,
		dbx.Params{"gatewayUserID": state.GatewayUserID, "itemID": state.ItemID},
	)
	if err != nil {
		return err
	}
	var record *core.Record
	if len(records) > 0 {
		record = records[0]
	} else {
		collection, err := s.app.FindCollectionByNameOrId("user_item_data")
		if err != nil {
			return err
		}
		record = core.NewRecord(collection)
		record.Set("gateway_user", state.GatewayUserID)
		record.Set("item_id", state.ItemID)
	}
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
	return s.app.Save(record)
}

func (s *Store) SavePlaybackResolution(ctx context.Context, state gateway.PlaybackState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	params := playbackResolutionParams(state)
	updated, err := s.updatePlaybackResolution(ctx, params)
	if err != nil {
		return err
	}
	if updated {
		return nil
	}
	if err := s.createPlaybackResolution(ctx, state); err != nil {
		if !isUniqueConstraintError(err) {
			return err
		}
		updated, retryErr := s.updatePlaybackResolution(ctx, params)
		if retryErr != nil {
			return retryErr
		}
		if updated {
			return nil
		}
		return err
	}
	return nil
}

func playbackResolutionParams(state gateway.PlaybackState) dbx.Params {
	// PocketBase date columns are TEXT DEFAULT '' NOT NULL; empty string clears the value.
	orphanedAt := ""
	if state.OrphanedAt != nil {
		orphanedAt = state.OrphanedAt.UTC().Truncate(time.Millisecond).Format(types.DefaultDateLayout)
	}
	lastSeenAt := ""
	if state.LastSeenAt != nil {
		lastSeenAt = state.LastSeenAt.UTC().Truncate(time.Millisecond).Format(types.DefaultDateLayout)
	}
	return dbx.Params{
		"gatewayUserID":     state.GatewayUserID,
		"itemID":            state.ItemID,
		"syntheticUserID":   state.SyntheticUserID,
		"itemName":          state.ItemName,
		"itemType":          state.ItemType,
		"seriesID":          state.SeriesID,
		"seriesName":        state.SeriesName,
		"seasonID":          state.SeasonID,
		"indexNumber":       state.IndexNumber,
		"parentIndexNumber": state.ParentIndexNumber,
		"runTimeTicks":      state.RunTimeTicks,
		"fingerprint":       state.Fingerprint,
		"orphanedAt":        orphanedAt,
		"lastSeenAt":        lastSeenAt,
	}
}

func (s *Store) updatePlaybackResolution(ctx context.Context, params dbx.Params) (bool, error) {
	result, err := s.app.DB().NewQuery(`UPDATE user_item_data
		SET synthetic_user_id = {:syntheticUserID},
			item_name = {:itemName},
			item_type = {:itemType},
			series_id = {:seriesID},
			series_name = {:seriesName},
			season_id = {:seasonID},
			index_number = {:indexNumber},
			parent_index_number = {:parentIndexNumber},
			run_time_ticks = {:runTimeTicks},
			fingerprint = {:fingerprint},
			orphaned_at = {:orphanedAt},
			last_seen_at = {:lastSeenAt}
		WHERE gateway_user = {:gatewayUserID} AND item_id = {:itemID}`).
		WithContext(ctx).
		Bind(params).
		Execute()
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected >= 1, nil
}

func (s *Store) createPlaybackResolution(ctx context.Context, state gateway.PlaybackState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	collection, err := s.app.FindCollectionByNameOrId("user_item_data")
	if err != nil {
		return err
	}
	record := core.NewRecord(collection)
	record.Set("gateway_user", state.GatewayUserID)
	record.Set("item_id", state.ItemID)
	record.Set("synthetic_user_id", state.SyntheticUserID)
	record.Set("item_name", state.ItemName)
	record.Set("item_type", state.ItemType)
	record.Set("series_id", state.SeriesID)
	record.Set("series_name", state.SeriesName)
	record.Set("season_id", state.SeasonID)
	record.Set("index_number", state.IndexNumber)
	record.Set("parent_index_number", state.ParentIndexNumber)
	record.Set("run_time_ticks", state.RunTimeTicks)
	record.Set("fingerprint", state.Fingerprint)
	if state.OrphanedAt != nil {
		record.Set("orphaned_at", *state.OrphanedAt)
	}
	if state.LastSeenAt != nil {
		record.Set("last_seen_at", *state.LastSeenAt)
	}
	return s.app.Save(record)
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "value must be unique") ||
		(strings.Contains(msg, "constraint failed") && strings.Contains(msg, "unique"))
}

func (s *Store) FindDisplayPreference(ctx context.Context, gatewayUserID, preferenceID, client string) (*gateway.DisplayPreference, error) {
	records, err := s.app.FindRecordsByFilter(
		"display_preferences",
		"gateway_user = {:gatewayUserID} && preference_id = {:preferenceID} && client = {:client}",
		"",
		1,
		0,
		dbx.Params{"gatewayUserID": gatewayUserID, "preferenceID": preferenceID, "client": client},
	)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, gateway.ErrNotFound
	}
	return displayPreferenceFromRecord(records[0]), nil
}

func (s *Store) SaveDisplayPreference(ctx context.Context, preference gateway.DisplayPreference) error {
	records, err := s.app.FindRecordsByFilter(
		"display_preferences",
		"gateway_user = {:gatewayUserID} && preference_id = {:preferenceID} && client = {:client}",
		"",
		1,
		0,
		dbx.Params{"gatewayUserID": preference.GatewayUserID, "preferenceID": preference.PreferenceID, "client": preference.Client},
	)
	if err != nil {
		return err
	}
	var record *core.Record
	if len(records) > 0 {
		record = records[0]
	} else {
		collection, err := s.app.FindCollectionByNameOrId("display_preferences")
		if err != nil {
			return err
		}
		record = core.NewRecord(collection)
		record.Set("gateway_user", preference.GatewayUserID)
		record.Set("preference_id", preference.PreferenceID)
		record.Set("client", preference.Client)
	}
	record.Set("synthetic_user_id", preference.SyntheticUserID)
	record.Set("payload_json", preference.PayloadJSON)
	return s.app.Save(record)
}

func stringSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	return set
}

func addPlaybackAggregate(aggregate gateway.PlaybackAggregate, state gateway.PlaybackState) gateway.PlaybackAggregate {
	aggregate.KnownItemCount++
	if state.Played {
		aggregate.PlayedCount++
	}
	if state.LastPlayedDate != nil && (aggregate.LastPlayedDate == nil || state.LastPlayedDate.After(*aggregate.LastPlayedDate)) {
		t := *state.LastPlayedDate
		aggregate.LastPlayedDate = &t
	}
	activity := state.UpdatedAt
	if state.LastPlayedDate != nil && state.LastPlayedDate.After(activity) {
		activity = *state.LastPlayedDate
	}
	if !activity.IsZero() && (aggregate.LastActivityDate == nil || activity.After(*aggregate.LastActivityDate)) {
		t := activity
		aggregate.LastActivityDate = &t
	}
	return aggregate
}

func (s *Store) enabledPathPolicies() ([]gateway.PathPolicy, error) {
	records, err := s.app.FindRecordsByFilter("path_policies", "enabled = true", "", 0, 0)
	if err != nil {
		return nil, err
	}
	policies := make([]gateway.PathPolicy, 0, len(records))
	for _, record := range records {
		policies = append(policies, gateway.PathPolicy{
			ID:       record.Id,
			Method:   record.GetString("method"),
			Path:     record.GetString("path"),
			Action:   record.GetString("action"),
			Reason:   record.GetString("reason"),
			Priority: record.GetInt("priority"),
			Enabled:  record.GetBool("enabled"),
		})
	}
	return policies, nil
}

func playbackStateFromRecord(record *core.Record) *gateway.PlaybackState {
	updatedAt := record.GetDateTime("updated").Time()
	var percentage *float64
	if record.GetBool("played_percentage_set") {
		v := record.GetFloat("played_percentage")
		percentage = &v
	}
	var lastPlayedDate *time.Time
	if !record.GetDateTime("last_played_date").IsZero() {
		t := record.GetDateTime("last_played_date").Time()
		lastPlayedDate = &t
	}
	var likes *bool
	if record.GetBool("likes_set") {
		v := record.GetBool("likes")
		likes = &v
	}
	var orphanedAt *time.Time
	if !record.GetDateTime("orphaned_at").IsZero() {
		t := record.GetDateTime("orphaned_at").Time()
		orphanedAt = &t
	}
	var lastSeenAt *time.Time
	if !record.GetDateTime("last_seen_at").IsZero() {
		t := record.GetDateTime("last_seen_at").Time()
		lastSeenAt = &t
	}
	return &gateway.PlaybackState{
		ID:                    record.Id,
		GatewayUserID:         record.GetString("gateway_user"),
		SyntheticUserID:       record.GetString("synthetic_user_id"),
		ItemID:                record.GetString("item_id"),
		ItemName:              record.GetString("item_name"),
		ItemType:              record.GetString("item_type"),
		SeriesID:              record.GetString("series_id"),
		SeriesName:            record.GetString("series_name"),
		SeasonID:              record.GetString("season_id"),
		IndexNumber:           record.GetInt("index_number"),
		ParentIndexNumber:     record.GetInt("parent_index_number"),
		RunTimeTicks:          int64(record.GetFloat("run_time_ticks")),
		PlaybackPositionTicks: int64(record.GetFloat("playback_position_ticks")),
		Played:                record.GetBool("played"),
		PlayedPercentage:      percentage,
		LastPlayedDate:        lastPlayedDate,
		PlayCount:             record.GetInt("play_count"),
		IsFavorite:            record.GetBool("is_favorite"),
		Likes:                 likes,
		Fingerprint:           record.GetString("fingerprint"),
		OrphanedAt:            orphanedAt,
		LastSeenAt:            lastSeenAt,
		UpdatedAt:             updatedAt,
	}
}

func displayPreferenceFromRecord(record *core.Record) *gateway.DisplayPreference {
	return &gateway.DisplayPreference{
		ID:              record.Id,
		GatewayUserID:   record.GetString("gateway_user"),
		SyntheticUserID: record.GetString("synthetic_user_id"),
		PreferenceID:    record.GetString("preference_id"),
		Client:          record.GetString("client"),
		PayloadJSON:     record.GetString("payload_json"),
		UpdatedAt:       record.GetDateTime("updated").Time(),
	}
}

func itemChildCountFromRecord(record *core.Record) gateway.ItemChildCount {
	return gateway.ItemChildCount{
		ItemID:     record.GetString("item_id"),
		ChildCount: record.GetInt("child_count"),
		UpdatedAt:  record.GetDateTime("updated").Time(),
	}
}

func userFromRecord(record *core.Record) *gateway.GatewayUser {
	return &gateway.GatewayUser{
		ID:              record.Id,
		Username:        record.GetString("username"),
		SyntheticUserID: record.GetString("synthetic_user_id"),
		Enabled:         record.GetBool("enabled"),
	}
}
