package pbstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/xxxbrian/emby-auth-gateway/internal/gateway"
)

// Schema text bounds for gateway_current_playbacks (pbschema.CurrentPlaybacks).
const (
	collectionGatewayCurrentPlaybacks    = "gateway_current_playbacks"
	currentPlaybackItemIDMaxBytes        = 80
	currentPlaybackPlaySessionIDMaxBytes = 255
	currentPlaybackMediaSourceIDMaxBytes = 255
	currentPlaybackItemSnapshotJSONMin   = 2
	currentPlaybackItemSnapshotJSONMax   = 65536
	currentPlaybackPlayStateJSONMin      = 2
	currentPlaybackPlayStateJSONMax      = 16384
)

// ListCurrentPlaybacks returns current-playback rows keyed by authoritative
// gateway token hash for the requested hashes.
//
// Empty/whitespace hashes are ignored; remaining hashes are trimmed and
// deduplicated in first-seen order. Unknown tokens and sessions without a
// current row are omitted. Any corrupt current row, relation mismatch,
// parent/token ambiguity, or duplicate current row fails the entire call —
// corrupt active state is never silently presented as idle. Returned values
// are independent clones.
func (s *Store) ListCurrentPlaybacks(ctx context.Context, tokenHashes []string) (map[string]gateway.CurrentPlayback, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requested := normalizeTokenHashes(tokenHashes)
	out := make(map[string]gateway.CurrentPlayback, len(requested))
	if len(requested) == 0 {
		return out, nil
	}

	// Resolve collections first so missing schema is an operational error, not
	// an empty "no current playbacks" result.
	if _, err := s.app.FindCollectionByNameOrId(collectionGatewaySessions); err != nil {
		return nil, err
	}
	if _, err := s.app.FindCollectionByNameOrId(collectionGatewayCurrentPlaybacks); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sessionsByID, err := s.findSessionsByTokenHashes(ctx, requested)
	if err != nil {
		return nil, err
	}
	if len(sessionsByID) == 0 {
		return out, nil
	}

	sessionIDs := make([]string, 0, len(sessionsByID))
	for id := range sessionsByID {
		sessionIDs = append(sessionIDs, id)
	}
	// Deterministic batch order for stable query construction.
	sort.Strings(sessionIDs)

	currentBySessionID, err := s.findCurrentPlaybacksBySessionIDs(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}

	for sessionID, currentRec := range currentBySessionID {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parent, ok := sessionsByID[sessionID]
		if !ok {
			return nil, fmt.Errorf(
				"current playback integrity: current row session %q has no resolved parent session",
				sessionID,
			)
		}
		cp, err := currentPlaybackFromRecord(currentRec, parent)
		if err != nil {
			return nil, err
		}
		// Independent clone for the map value (codec already clones nested fields).
		out[cp.GatewayTokenHash] = cloneCurrentPlaybackValue(cp)
	}
	return out, nil
}

// normalizeTokenHashes trims, drops empty, and deduplicates preserving first-seen order.
func normalizeTokenHashes(tokenHashes []string) []string {
	if len(tokenHashes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tokenHashes))
	out := make([]string, 0, len(tokenHashes))
	for _, h := range tokenHashes {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	return out
}

// findSessionsByTokenHashes loads gateway_sessions for the requested hashes in
// expression-safe batches. Duplicate session rows for one hash fail the call.
func (s *Store) findSessionsByTokenHashes(ctx context.Context, tokenHashes []string) (map[string]*core.Record, error) {
	byID := make(map[string]*core.Record, len(tokenHashes))
	seenHash := make(map[string]string, len(tokenHashes)) // hash -> session id

	for start := 0; start < len(tokenHashes); start += playbackStateItemIDBatchLimit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + playbackStateItemIDBatchLimit
		if end > len(tokenHashes) {
			end = len(tokenHashes)
		}
		batch := tokenHashes[start:end]
		filterParts := make([]string, 0, len(batch))
		params := dbx.Params{}
		for i, hash := range batch {
			name := fmt.Sprintf("tokenHash%d", i)
			filterParts = append(filterParts, "gateway_token_hash = {:"+name+"}")
			params[name] = hash
		}
		if len(filterParts) == 0 {
			continue
		}
		records, err := s.app.FindRecordsByFilter(
			collectionGatewaySessions,
			strings.Join(filterParts, " || "),
			"",
			0,
			0,
			params,
		)
		if err != nil {
			return nil, err
		}
		for _, rec := range records {
			hash := rec.GetString("gateway_token_hash")
			if hash == "" {
				return nil, fmt.Errorf("current playback integrity: gateway_sessions %q has empty gateway_token_hash", rec.Id)
			}
			if prevID, ok := seenHash[hash]; ok && prevID != rec.Id {
				return nil, fmt.Errorf(
					"current playback integrity: ambiguous gateway_token_hash %q on sessions %q and %q",
					hash,
					prevID,
					rec.Id,
				)
			}
			seenHash[hash] = rec.Id
			byID[rec.Id] = rec
		}
	}
	return byID, nil
}

// findCurrentPlaybacksBySessionIDs loads current-playback rows for the given
// parent session ids in expression-safe batches. Duplicate rows for one session
// fail the call (unique index should prevent this; SQL bypass still surfaces).
func (s *Store) findCurrentPlaybacksBySessionIDs(ctx context.Context, sessionIDs []string) (map[string]*core.Record, error) {
	bySession := make(map[string]*core.Record, len(sessionIDs))

	for start := 0; start < len(sessionIDs); start += playbackStateItemIDBatchLimit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + playbackStateItemIDBatchLimit
		if end > len(sessionIDs) {
			end = len(sessionIDs)
		}
		batch := sessionIDs[start:end]
		filterParts := make([]string, 0, len(batch))
		params := dbx.Params{}
		for i, id := range batch {
			name := fmt.Sprintf("sessionID%d", i)
			filterParts = append(filterParts, "gateway_session = {:"+name+"}")
			params[name] = id
		}
		if len(filterParts) == 0 {
			continue
		}
		records, err := s.app.FindRecordsByFilter(
			collectionGatewayCurrentPlaybacks,
			strings.Join(filterParts, " || "),
			"",
			0,
			0,
			params,
		)
		if err != nil {
			return nil, err
		}
		for _, rec := range records {
			sessionRef := rec.GetString("gateway_session")
			if sessionRef == "" {
				return nil, fmt.Errorf("current playback integrity: empty gateway_session relation on row %q", rec.Id)
			}
			if prev, ok := bySession[sessionRef]; ok {
				return nil, fmt.Errorf(
					"current playback integrity: duplicate current playback rows %q and %q for gateway_session %q",
					prev.Id,
					rec.Id,
					sessionRef,
				)
			}
			bySession[sessionRef] = rec
		}
	}
	return bySession, nil
}

func cloneCurrentPlaybackValue(cp gateway.CurrentPlayback) gateway.CurrentPlayback {
	cp.ItemSnapshot = clonePlaybackItemSnapshot(cp.ItemSnapshot)
	cp.PlayState = clonePlaybackPlayState(cp.PlayState)
	return cp
}

// marshalPlaybackItemSnapshot encodes a typed snapshot as compact deterministic
// JSON within the pbschema item_snapshot_json byte bounds.
func marshalPlaybackItemSnapshot(s gateway.PlaybackItemSnapshot) (string, error) {
	raw, err := json.Marshal(clonePlaybackItemSnapshot(s))
	if err != nil {
		return "", fmt.Errorf("current playback integrity: marshal item snapshot: %w", err)
	}
	if err := validateJSONObjectBytes("item_snapshot_json", raw, currentPlaybackItemSnapshotJSONMin, currentPlaybackItemSnapshotJSONMax); err != nil {
		return "", err
	}
	return string(raw), nil
}

// marshalPlaybackPlayState encodes a typed play state as compact deterministic
// JSON within the pbschema play_state_json byte bounds.
func marshalPlaybackPlayState(ps gateway.PlaybackPlayState) (string, error) {
	raw, err := json.Marshal(clonePlaybackPlayState(ps))
	if err != nil {
		return "", fmt.Errorf("current playback integrity: marshal play state: %w", err)
	}
	if err := validateJSONObjectBytes("play_state_json", raw, currentPlaybackPlayStateJSONMin, currentPlaybackPlayStateJSONMax); err != nil {
		return "", err
	}
	return string(raw), nil
}

// setCurrentPlaybackRecord populates a new or existing current-playback record
// from domain state and the authoritative parent gateway_sessions row.
//
// The parent supplies the relation target and authoritative token hash. Persisted
// corruption is never repaired or defaulted; validation failures return
// descriptive integrity errors.
func setCurrentPlaybackRecord(record, parent *core.Record, cp gateway.CurrentPlayback) error {
	if record == nil {
		return fmt.Errorf("current playback integrity: record is nil")
	}
	if parent == nil {
		return fmt.Errorf("current playback integrity: parent gateway_session is nil")
	}
	if parent.Id == "" {
		return fmt.Errorf("current playback integrity: parent gateway_session id is empty")
	}

	tokenHash := parent.GetString("gateway_token_hash")
	if tokenHash == "" {
		return fmt.Errorf("current playback integrity: parent gateway_session has empty gateway_token_hash")
	}
	if cp.GatewayTokenHash != "" && cp.GatewayTokenHash != tokenHash {
		return fmt.Errorf(
			"current playback integrity: gateway token hash %q does not match parent session hash %q",
			cp.GatewayTokenHash,
			tokenHash,
		)
	}

	itemID := cp.ItemID
	if err := validateItemID(itemID); err != nil {
		return err
	}
	if cp.ItemSnapshot.ID != itemID {
		return fmt.Errorf(
			"current playback integrity: item snapshot Id %q does not equal item_id %q",
			cp.ItemSnapshot.ID,
			itemID,
		)
	}
	if err := validateOptionalText("play_session_id", cp.PlaySessionID, currentPlaybackPlaySessionIDMaxBytes); err != nil {
		return err
	}
	if err := validateOptionalText("media_source_id", cp.MediaSourceID, currentPlaybackMediaSourceIDMaxBytes); err != nil {
		return err
	}
	if cp.RunTimeTicks < 0 {
		return fmt.Errorf("current playback integrity: run_time_ticks %d is negative", cp.RunTimeTicks)
	}
	startedAt, lastReportedAt, err := validatePlaybackDates(cp.StartedAt, cp.LastReportedAt)
	if err != nil {
		return err
	}

	snapshotJSON, err := marshalPlaybackItemSnapshot(cp.ItemSnapshot)
	if err != nil {
		return err
	}
	playStateJSON, err := marshalPlaybackPlayState(cp.PlayState)
	if err != nil {
		return err
	}

	record.Set("gateway_session", parent.Id)
	record.Set("item_id", itemID)
	record.Set("play_session_id", cp.PlaySessionID)
	record.Set("media_source_id", cp.MediaSourceID)
	record.Set("item_snapshot_json", snapshotJSON)
	record.Set("play_state_json", playStateJSON)
	record.Set("run_time_ticks", cp.RunTimeTicks)
	record.Set("started_at", startedAt)
	record.Set("last_reported_at", lastReportedAt)
	return nil
}

// currentPlaybackFromRecord hydrates domain state from a current-playback row
// and the authoritative parent gateway_sessions record. Token hash is taken
// only from the parent. ImageTags and pointer fields are cloned. Persisted
// corruption is reported, never repaired.
func currentPlaybackFromRecord(record, parent *core.Record) (gateway.CurrentPlayback, error) {
	var zero gateway.CurrentPlayback
	if record == nil {
		return zero, fmt.Errorf("current playback integrity: record is nil")
	}
	if parent == nil {
		return zero, fmt.Errorf("current playback integrity: parent gateway_session is nil")
	}
	if parent.Id == "" {
		return zero, fmt.Errorf("current playback integrity: parent gateway_session id is empty")
	}

	rel := record.GetString("gateway_session")
	if rel == "" {
		return zero, fmt.Errorf("current playback integrity: empty gateway_session relation")
	}
	if rel != parent.Id {
		return zero, fmt.Errorf(
			"current playback integrity: gateway_session relation %q does not match parent id %q",
			rel,
			parent.Id,
		)
	}

	tokenHash := parent.GetString("gateway_token_hash")
	if tokenHash == "" {
		return zero, fmt.Errorf("current playback integrity: parent gateway_session has empty gateway_token_hash")
	}

	itemID := record.GetString("item_id")
	if err := validateItemID(itemID); err != nil {
		return zero, err
	}
	playSessionID := record.GetString("play_session_id")
	if err := validateOptionalText("play_session_id", playSessionID, currentPlaybackPlaySessionIDMaxBytes); err != nil {
		return zero, err
	}
	mediaSourceID := record.GetString("media_source_id")
	if err := validateOptionalText("media_source_id", mediaSourceID, currentPlaybackMediaSourceIDMaxBytes); err != nil {
		return zero, err
	}

	snapshot, err := parsePlaybackItemSnapshot(record.GetString("item_snapshot_json"))
	if err != nil {
		return zero, err
	}
	if snapshot.ID != itemID {
		return zero, fmt.Errorf(
			"current playback integrity: item snapshot Id %q does not equal item_id %q",
			snapshot.ID,
			itemID,
		)
	}
	playState, err := parsePlaybackPlayState(record.GetString("play_state_json"))
	if err != nil {
		return zero, err
	}

	runtime, err := readRunTimeTicks(record)
	if err != nil {
		return zero, err
	}

	startedAt := record.GetDateTime("started_at").Time()
	lastReportedAt := record.GetDateTime("last_reported_at").Time()
	startedAt, lastReportedAt, err = validatePlaybackDates(startedAt, lastReportedAt)
	if err != nil {
		return zero, err
	}

	cp := gateway.CurrentPlayback{
		GatewayTokenHash: tokenHash,
		ItemID:           itemID,
		PlaySessionID:    playSessionID,
		MediaSourceID:    mediaSourceID,
		ItemSnapshot:     snapshot,
		PlayState:        playState,
		RunTimeTicks:     runtime,
		StartedAt:        startedAt,
		LastReportedAt:   lastReportedAt,
	}
	if err := gateway.ValidateCurrentPlayback(&cp, tokenHash); err != nil {
		return zero, err
	}
	return cp, nil
}

// parsePlaybackItemSnapshot strictly decodes a canonical allowlisted snapshot
// document: non-null object, EOF, no unknown fields, no duplicate keys, and
// exact-byte equality with the codec's compact marshal output.
func parsePlaybackItemSnapshot(raw string) (gateway.PlaybackItemSnapshot, error) {
	var zero gateway.PlaybackItemSnapshot
	payload := []byte(raw)
	if err := validateCanonicalJSONEnvelope("item_snapshot_json", payload, currentPlaybackItemSnapshotJSONMin, currentPlaybackItemSnapshotJSONMax); err != nil {
		return zero, err
	}
	var snap gateway.PlaybackItemSnapshot
	if err := strictDecodeJSONObject("item_snapshot_json", payload, &snap); err != nil {
		return zero, err
	}
	if err := validateFiniteSnapshotNumbers(snap); err != nil {
		return zero, err
	}
	canonical, err := marshalPlaybackItemSnapshot(snap)
	if err != nil {
		return zero, err
	}
	if canonical != raw {
		return zero, fmt.Errorf("current playback integrity: item_snapshot_json is not the exact canonical document")
	}
	return clonePlaybackItemSnapshot(snap), nil
}

// parsePlaybackPlayState strictly decodes a canonical allowlisted play-state
// document with the same exact-document rules as item snapshots.
func parsePlaybackPlayState(raw string) (gateway.PlaybackPlayState, error) {
	var zero gateway.PlaybackPlayState
	payload := []byte(raw)
	if err := validateCanonicalJSONEnvelope("play_state_json", payload, currentPlaybackPlayStateJSONMin, currentPlaybackPlayStateJSONMax); err != nil {
		return zero, err
	}
	var ps gateway.PlaybackPlayState
	if err := strictDecodeJSONObject("play_state_json", payload, &ps); err != nil {
		return zero, err
	}
	if err := validateFinitePlayStateNumbers(ps); err != nil {
		return zero, err
	}
	canonical, err := marshalPlaybackPlayState(ps)
	if err != nil {
		return zero, err
	}
	if canonical != raw {
		return zero, fmt.Errorf("current playback integrity: play_state_json is not the exact canonical document")
	}
	return clonePlaybackPlayState(ps), nil
}

// validateJSONObjectBytes is used by the marshal path for bounds + object shape
// on codec-produced compact documents (no whitespace, already canonical).
func validateJSONObjectBytes(field string, raw []byte, minBytes, maxBytes int) error {
	return validateCanonicalJSONEnvelope(field, raw, minBytes, maxBytes)
}

// validateCanonicalJSONEnvelope enforces schema byte bounds, no surrounding
// whitespace, a top-level non-null object, no trailing tokens, and no duplicate
// keys at any object depth (including nested ImageTags).
func validateCanonicalJSONEnvelope(field string, raw []byte, minBytes, maxBytes int) error {
	n := len(raw)
	if n < minBytes || n > maxBytes {
		return fmt.Errorf("current playback integrity: %s length %d out of bounds [%d,%d]", field, n, minBytes, maxBytes)
	}
	if n == 0 {
		return fmt.Errorf("current playback integrity: %s is empty", field)
	}
	// Exact document: reject leading/trailing whitespace variants.
	if !bytes.Equal(raw, bytes.TrimSpace(raw)) {
		return fmt.Errorf("current playback integrity: %s has surrounding whitespace", field)
	}
	if raw[0] != '{' {
		return fmt.Errorf("current playback integrity: %s must be a non-null JSON object", field)
	}
	if err := rejectDuplicateJSONObjectKeys(raw); err != nil {
		return fmt.Errorf("current playback integrity: %s: %w", field, err)
	}
	// Confirm a single top-level object value and EOF.
	dec := json.NewDecoder(bytes.NewReader(raw))
	var probe any
	if err := dec.Decode(&probe); err != nil {
		return fmt.Errorf("current playback integrity: %s: %w", field, err)
	}
	if _, ok := probe.(map[string]any); !ok {
		return fmt.Errorf("current playback integrity: %s must be a non-null JSON object", field)
	}
	if err := ensureJSONDecoderEOF(dec); err != nil {
		return fmt.Errorf("current playback integrity: %s: %w", field, err)
	}
	return nil
}

// strictDecodeJSONObject decodes into dst with DisallowUnknownFields and requires EOF.
func strictDecodeJSONObject(field string, raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("current playback integrity: %s: %w", field, err)
	}
	if err := ensureJSONDecoderEOF(dec); err != nil {
		return fmt.Errorf("current playback integrity: %s: %w", field, err)
	}
	return nil
}

func ensureJSONDecoderEOF(dec *json.Decoder) error {
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing data")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

// rejectDuplicateJSONObjectKeys walks the JSON token stream and rejects any
// object (top-level or nested) that repeats a key. encoding/json last-wins on
// duplicates; exact remarshal also rejects them, but an explicit scan yields a
// clearer integrity error and closes any theoretical rematerialization hole.
func rejectDuplicateJSONObjectKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := rejectDuplicateKeysInValue(dec); err != nil {
		return err
	}
	return ensureJSONDecoderEOF(dec)
}

func rejectDuplicateKeysInValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// Primitive value already consumed.
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, dup := seen[key]; dup {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = struct{}{}
			if err := rejectDuplicateKeysInValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("expected end of object")
		}
		return nil
	case '[':
		for dec.More() {
			if err := rejectDuplicateKeysInValue(dec); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("expected end of array")
		}
		return nil
	default:
		return fmt.Errorf("unexpected delimiter %v", delim)
	}
}

func validateFiniteSnapshotNumbers(s gateway.PlaybackItemSnapshot) error {
	if math.IsNaN(s.CommunityRating) || math.IsInf(s.CommunityRating, 0) {
		return fmt.Errorf("current playback integrity: item_snapshot_json CommunityRating is not a finite number")
	}
	return nil
}

func validateFinitePlayStateNumbers(ps gateway.PlaybackPlayState) error {
	if ps.PlaybackRate != nil {
		if math.IsNaN(*ps.PlaybackRate) || math.IsInf(*ps.PlaybackRate, 0) {
			return fmt.Errorf("current playback integrity: play_state_json PlaybackRate is not a finite number")
		}
	}
	if ps.SubtitleOffset != nil {
		if math.IsNaN(*ps.SubtitleOffset) || math.IsInf(*ps.SubtitleOffset, 0) {
			return fmt.Errorf("current playback integrity: play_state_json SubtitleOffset is not a finite number")
		}
	}
	return nil
}

func validateItemID(itemID string) error {
	if itemID == "" {
		return fmt.Errorf("current playback integrity: item_id is required")
	}
	// TextField Max is byte-oriented; match pbschema item_id Max=80.
	if len(itemID) > currentPlaybackItemIDMaxBytes {
		return fmt.Errorf("current playback integrity: item_id length %d exceeds max %d", len(itemID), currentPlaybackItemIDMaxBytes)
	}
	if !utf8.ValidString(itemID) {
		return fmt.Errorf("current playback integrity: item_id is not valid UTF-8")
	}
	return nil
}

func validateOptionalText(field, value string, maxBytes int) error {
	if len(value) > maxBytes {
		return fmt.Errorf("current playback integrity: %s length %d exceeds max %d", field, len(value), maxBytes)
	}
	return nil
}

func validatePlaybackDates(startedAt, lastReportedAt time.Time) (time.Time, time.Time, error) {
	if startedAt.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf("current playback integrity: started_at is zero")
	}
	if lastReportedAt.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf("current playback integrity: last_reported_at is zero")
	}
	startedAt = startedAt.UTC()
	lastReportedAt = lastReportedAt.UTC()
	if lastReportedAt.Before(startedAt) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"current playback integrity: last_reported_at %s is before started_at %s",
			lastReportedAt.Format(time.RFC3339Nano),
			startedAt.Format(time.RFC3339Nano),
		)
	}
	return startedAt, lastReportedAt, nil
}

func readRunTimeTicks(record *core.Record) (int64, error) {
	raw := record.GetRaw("run_time_ticks")
	if raw == nil {
		return 0, nil
	}
	switch v := raw.(type) {
	case int:
		if v < 0 {
			return 0, fmt.Errorf("current playback integrity: run_time_ticks %d is negative", v)
		}
		return int64(v), nil
	case int8:
		if v < 0 {
			return 0, fmt.Errorf("current playback integrity: run_time_ticks %d is negative", v)
		}
		return int64(v), nil
	case int16:
		if v < 0 {
			return 0, fmt.Errorf("current playback integrity: run_time_ticks %d is negative", v)
		}
		return int64(v), nil
	case int32:
		if v < 0 {
			return 0, fmt.Errorf("current playback integrity: run_time_ticks %d is negative", v)
		}
		return int64(v), nil
	case int64:
		if v < 0 {
			return 0, fmt.Errorf("current playback integrity: run_time_ticks %d is negative", v)
		}
		return v, nil
	case uint:
		if uint64(v) > math.MaxInt64 {
			return 0, fmt.Errorf("current playback integrity: run_time_ticks %d overflows int64", v)
		}
		return int64(v), nil
	case uint64:
		if v > math.MaxInt64 {
			return 0, fmt.Errorf("current playback integrity: run_time_ticks %d overflows int64", v)
		}
		return int64(v), nil
	case float32:
		return runTimeFromFloat(float64(v))
	case float64:
		return runTimeFromFloat(v)
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			f, ferr := v.Float64()
			if ferr != nil {
				return 0, fmt.Errorf("current playback integrity: run_time_ticks %q is not an integer", v.String())
			}
			return runTimeFromFloat(f)
		}
		if i < 0 {
			return 0, fmt.Errorf("current playback integrity: run_time_ticks %d is negative", i)
		}
		return i, nil
	case string:
		// PocketBase number fields should not surface as strings for valid rows;
		// treat as integrity failure rather than parsing/repairing.
		if strings.TrimSpace(v) == "" {
			return 0, nil
		}
		return 0, fmt.Errorf("current playback integrity: run_time_ticks has non-numeric type %T", raw)
	default:
		// Fall back through GetFloat for unexpected numeric wrappers.
		f := record.GetFloat("run_time_ticks")
		return runTimeFromFloat(f)
	}
}

func runTimeFromFloat(f float64) (int64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("current playback integrity: run_time_ticks is not a finite number")
	}
	if f < 0 {
		return 0, fmt.Errorf("current playback integrity: run_time_ticks %v is negative", f)
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("current playback integrity: run_time_ticks %v is not an integer", f)
	}
	if f > float64(math.MaxInt64) {
		return 0, fmt.Errorf("current playback integrity: run_time_ticks %v overflows int64", f)
	}
	return int64(f), nil
}

func clonePlaybackItemSnapshot(s gateway.PlaybackItemSnapshot) gateway.PlaybackItemSnapshot {
	if len(s.ImageTags) > 0 {
		tags := make(map[string]string, len(s.ImageTags))
		for k, v := range s.ImageTags {
			tags[k] = v
		}
		s.ImageTags = tags
	}
	return s
}

func clonePlaybackPlayState(ps gateway.PlaybackPlayState) gateway.PlaybackPlayState {
	out := gateway.PlaybackPlayState{}
	if ps.PositionTicks != nil {
		v := *ps.PositionTicks
		out.PositionTicks = &v
	}
	if ps.CanSeek != nil {
		v := *ps.CanSeek
		out.CanSeek = &v
	}
	if ps.IsPaused != nil {
		v := *ps.IsPaused
		out.IsPaused = &v
	}
	if ps.IsMuted != nil {
		v := *ps.IsMuted
		out.IsMuted = &v
	}
	if ps.VolumeLevel != nil {
		v := *ps.VolumeLevel
		out.VolumeLevel = &v
	}
	if ps.AudioStreamIndex != nil {
		v := *ps.AudioStreamIndex
		out.AudioStreamIndex = &v
	}
	if ps.SubtitleStreamIndex != nil {
		v := *ps.SubtitleStreamIndex
		out.SubtitleStreamIndex = &v
	}
	if ps.MediaSourceID != nil {
		v := *ps.MediaSourceID
		out.MediaSourceID = &v
	}
	if ps.PlayMethod != nil {
		v := *ps.PlayMethod
		out.PlayMethod = &v
	}
	if ps.PlaybackRate != nil {
		v := *ps.PlaybackRate
		out.PlaybackRate = &v
	}
	if ps.RepeatMode != nil {
		v := *ps.RepeatMode
		out.RepeatMode = &v
	}
	if ps.Shuffle != nil {
		v := *ps.Shuffle
		out.Shuffle = &v
	}
	if ps.SubtitleOffset != nil {
		v := *ps.SubtitleOffset
		out.SubtitleOffset = &v
	}
	return out
}
