package gateway

import (
	"fmt"
	"strings"
	"time"
)

// PlaybackCurrentAction describes how a repository should treat the current-playback row.
type PlaybackCurrentAction int

const (
	// PlaybackCurrentNone leaves current playback unchanged and does not write it.
	PlaybackCurrentNone PlaybackCurrentAction = iota
	// PlaybackCurrentUpsert inserts or replaces the current-playback row.
	PlaybackCurrentUpsert
	// PlaybackCurrentPreserve keeps the existing current-playback row as-is.
	PlaybackCurrentPreserve
	// PlaybackCurrentDelete removes the current-playback row.
	PlaybackCurrentDelete
)

// PlaybackResumePolicy controls durable stop completion thresholds.
// Emby ticks are 100ns units (10_000_000 per second).
type PlaybackResumePolicy struct {
	MinPct             float64
	MaxPct             float64
	MinDurationSeconds float64
}

// DefaultPlaybackResumePolicy returns gateway default resume thresholds.
func DefaultPlaybackResumePolicy() PlaybackResumePolicy {
	return PlaybackResumePolicy{
		MinPct:             defaultMinResumePct,
		MaxPct:             defaultMaxResumePct,
		MinDurationSeconds: defaultMinResumeDurationSeconds,
	}
}

// PlaybackReduceInput is the fully loaded, transaction-local state for a pure reduce.
// Repositories must not call outer store methods to compose this after reduce starts.
type PlaybackReduceInput struct {
	Command PlaybackReportCommand
	// Session is the auth session aggregate for GatewayTokenHash (already repaired).
	Session Session
	// Current is the existing current-playback row, or nil when absent.
	Current *CurrentPlayback
	// Durable is the existing durable item state for Command.ItemID, or nil when absent.
	Durable *PlaybackState
	// Policy is deprecated for reduce authority; prefer Command.Policy.
	// When Command.Policy is all-zero before prepare, this value is copied onto the command.
	Policy PlaybackResumePolicy
}

// PlaybackMutationPlan is the pure effect list for MemoryStore/pbstore to apply
// inside one transaction. No I/O is performed by the reducer.
type PlaybackMutationPlan struct {
	Result PlaybackReportResult

	// Event is non-nil when a PlaybackEvent row should be appended (never for Ping).
	Event *PlaybackEvent

	// Durable is set with WriteDurable when durable item state should be saved.
	Durable      *PlaybackState
	WriteDurable bool

	// CurrentAction and Current describe current-playback mutation.
	// Current is non-nil only for PlaybackCurrentUpsert.
	CurrentAction PlaybackCurrentAction
	Current       *CurrentPlayback

	// ActivityAt, when non-nil, advances session profile activity if strictly newer.
	ActivityAt *time.Time
}

// ReducePlaybackReport is the shared pure reducer for gateway-owned playback reports.
// It never performs I/O. Missing ItemID for Playing/Progress/Stopped yields a no-op
// result (not an error). Invalid command fields return ErrBadRequest.
//
// The command is always prepared via PreparePlaybackReportCommand so lookup keys and
// reducer keys cannot diverge. Resume policy is taken from the prepared command.
func ReducePlaybackReport(in PlaybackReduceInput) (PlaybackMutationPlan, error) {
	cmdIn := in.Command
	if playbackResumePolicyAllZero(cmdIn.Policy) && !playbackResumePolicyAllZero(in.Policy) {
		cmdIn.Policy = in.Policy
	}
	cmd, err := PreparePlaybackReportCommand(cmdIn)
	if err != nil {
		return PlaybackMutationPlan{}, err
	}
	if strings.TrimSpace(in.Session.GatewayTokenHash) == "" {
		return PlaybackMutationPlan{}, fmt.Errorf("%w: session token hash required", ErrBadRequest)
	}
	if in.Session.GatewayTokenHash != cmd.GatewayTokenHash {
		return PlaybackMutationPlan{}, fmt.Errorf("%w: session token hash mismatch", ErrBadRequest)
	}

	policy := cmd.Policy
	base := PlaybackMutationPlan{
		Result: PlaybackReportResult{
			PublicSessionID: in.Session.PublicID,
			GatewayUserID:   in.Session.GatewayUserID,
			SyntheticUserID: in.Session.SyntheticUserID,
			ItemID:          cmd.ItemID,
		},
		CurrentAction: PlaybackCurrentNone,
	}

	switch cmd.Kind {
	case PlaybackReportPing:
		return reducePlaybackPing(cmd, in.Session, in.Current, base), nil
	case PlaybackReportPlaying, PlaybackReportProgress, PlaybackReportStopped:
		if cmd.ItemID == "" {
			return base, nil
		}
		return reducePlaybackItemReport(cmd, in.Session, in.Current, in.Durable, policy, base), nil
	default:
		return PlaybackMutationPlan{}, fmt.Errorf("%w: invalid playback report kind", ErrBadRequest)
	}
}

func playbackResumePolicyAllZero(p PlaybackResumePolicy) bool {
	return p.MinPct == 0 && p.MaxPct == 0 && p.MinDurationSeconds == 0
}

func applyEventNameToPlayState(ps PlaybackPlayState, eventName string) PlaybackPlayState {
	switch strings.ToLower(eventName) {
	case "pause":
		v := true
		ps.IsPaused = &v
	case "unpause":
		v := false
		ps.IsPaused = &v
	}
	return ps
}

func reducePlaybackPing(cmd PlaybackReportCommand, session Session, current *CurrentPlayback, plan PlaybackMutationPlan) PlaybackMutationPlan {
	if current == nil {
		return plan
	}
	if cmd.PlaySessionID != "" {
		if current.PlaySessionID != cmd.PlaySessionID {
			return plan
		}
		if cmd.ItemID != "" && current.ItemID != cmd.ItemID {
			return plan
		}
	}

	updated := cloneCurrentPlaybackValue(*current)
	updated.LastReportedAt = cmd.ReceivedAt
	plan.CurrentAction = PlaybackCurrentUpsert
	plan.Current = &updated
	at := cmd.ReceivedAt
	plan.ActivityAt = &at
	plan.Result.Applied = true
	plan.Result.ItemID = updated.ItemID
	plan.Result.Current = cloneCurrentPlayback(&updated)
	return plan
}

func reducePlaybackItemReport(
	cmd PlaybackReportCommand,
	session Session,
	current *CurrentPlayback,
	durable *PlaybackState,
	policy PlaybackResumePolicy,
	plan PlaybackMutationPlan,
) PlaybackMutationPlan {
	nextDurable := buildDurableFromReport(cmd, session, durable, policy)
	plan.Durable = &nextDurable
	plan.WriteDurable = true
	plan.Event = buildPlaybackEvent(cmd, session, nextDurable)
	at := cmd.ReceivedAt
	plan.ActivityAt = &at
	plan.Result.Applied = true
	plan.Result.Durable = clonePlaybackState(&nextDurable)

	switch cmd.Kind {
	case PlaybackReportPlaying:
		startedAt := cmd.ReceivedAt
		var prior *CurrentPlayback
		if current != nil && current.ItemID == cmd.ItemID && !playSessionIDsConflict(current.PlaySessionID, cmd.PlaySessionID) {
			if !current.StartedAt.IsZero() {
				startedAt = current.StartedAt
			}
			prior = current
		}
		next := buildCurrentPlayback(cmd, startedAt, prior)
		plan.CurrentAction = PlaybackCurrentUpsert
		plan.Current = &next
		plan.Result.Current = cloneCurrentPlayback(&next)

	case PlaybackReportProgress:
		if current == nil {
			next := buildCurrentPlayback(cmd, cmd.ReceivedAt, nil)
			plan.CurrentAction = PlaybackCurrentUpsert
			plan.Current = &next
			plan.Result.Current = cloneCurrentPlayback(&next)
		} else if currentMatchesReport(current, cmd) {
			startedAt := current.StartedAt
			if startedAt.IsZero() {
				startedAt = cmd.ReceivedAt
			}
			next := buildCurrentPlayback(cmd, startedAt, current)
			plan.CurrentAction = PlaybackCurrentUpsert
			plan.Current = &next
			plan.Result.Current = cloneCurrentPlayback(&next)
		} else {
			// Mismatched item or conflicting play session: keep active current.
			plan.CurrentAction = PlaybackCurrentPreserve
			plan.Result.Current = cloneCurrentPlayback(current)
		}

	case PlaybackReportStopped:
		if current != nil && currentMatchesReport(current, cmd) {
			plan.CurrentAction = PlaybackCurrentDelete
			plan.Result.Current = nil
		} else if current != nil {
			plan.CurrentAction = PlaybackCurrentPreserve
			plan.Result.Current = cloneCurrentPlayback(current)
		} else {
			plan.CurrentAction = PlaybackCurrentNone
			plan.Result.Current = nil
		}
	}
	return plan
}

func currentMatchesReport(current *CurrentPlayback, cmd PlaybackReportCommand) bool {
	if current == nil {
		return false
	}
	if current.ItemID != cmd.ItemID {
		return false
	}
	return !playSessionIDsConflict(current.PlaySessionID, cmd.PlaySessionID)
}

func playSessionIDsConflict(existing, reported string) bool {
	return existing != "" && reported != "" && existing != reported
}

func buildCurrentPlayback(cmd PlaybackReportCommand, startedAt time.Time, prior *CurrentPlayback) CurrentPlayback {
	var baseSnap PlaybackItemSnapshot
	var basePlay PlaybackPlayState
	priorRuntime := int64(0)
	priorPlaySession := ""
	priorMediaSource := ""
	if prior != nil {
		baseSnap = prior.ItemSnapshot
		basePlay = prior.PlayState
		priorRuntime = prior.RunTimeTicks
		priorPlaySession = prior.PlaySessionID
		priorMediaSource = prior.MediaSourceID
	}
	snap := mergePlaybackItemSnapshot(baseSnap, cmd.ItemSnapshot)
	if cmd.MetadataConfirmed {
		snap = clonePlaybackItemSnapshot(cmd.ItemSnapshot)
	}
	if snap.ID == "" {
		snap.ID = cmd.ItemID
	}
	play := mergePlaybackPlayState(basePlay, cmd.PlayState)
	runtime := snap.RunTimeTicks
	if !cmd.MetadataConfirmed {
		runtime = cmd.RunTimeTicks
		if runtime <= 0 {
			runtime = snap.RunTimeTicks
		}
		if runtime <= 0 {
			runtime = priorRuntime
		}
	}
	playSessionID := cmd.PlaySessionID
	if playSessionID == "" {
		playSessionID = priorPlaySession
	}
	mediaSourceID := cmd.MediaSourceID
	if mediaSourceID == "" && play.MediaSourceID != nil {
		mediaSourceID = strings.TrimSpace(*play.MediaSourceID)
	}
	if mediaSourceID == "" {
		mediaSourceID = priorMediaSource
	}
	if startedAt.IsZero() {
		startedAt = cmd.ReceivedAt
	}
	return CurrentPlayback{
		GatewayTokenHash: cmd.GatewayTokenHash,
		ItemID:           cmd.ItemID,
		PlaySessionID:    playSessionID,
		MediaSourceID:    mediaSourceID,
		ItemSnapshot:     snap,
		PlayState:        play,
		RunTimeTicks:     runtime,
		StartedAt:        startedAt.UTC(),
		LastReportedAt:   cmd.ReceivedAt,
	}
}

func buildDurableFromReport(cmd PlaybackReportCommand, session Session, existing *PlaybackState, policy PlaybackResumePolicy) PlaybackState {
	var state PlaybackState
	if existing != nil {
		state = *clonePlaybackState(existing)
	} else {
		state = PlaybackState{
			GatewayUserID:   session.GatewayUserID,
			SyntheticUserID: session.SyntheticUserID,
			ItemID:          cmd.ItemID,
		}
	}
	if state.GatewayUserID == "" {
		state.GatewayUserID = session.GatewayUserID
	}
	if state.SyntheticUserID == "" {
		state.SyntheticUserID = session.SyntheticUserID
	}
	state.ItemID = cmd.ItemID

	mergeSnapshotIntoDurable(&state, cmd.ItemSnapshot)
	if cmd.MetadataConfirmed {
		// Confirmed upstream metadata replaces stable catalog identity, including
		// authoritative absence. Personal playback/favorite/rating fields remain.
		state.ItemName = cmd.ItemSnapshot.Name
		state.ItemType = cmd.ItemSnapshot.Type
		state.SeriesID = cmd.ItemSnapshot.SeriesID
		state.SeriesName = cmd.ItemSnapshot.SeriesName
		state.SeasonID = cmd.ItemSnapshot.SeasonID
		state.IndexNumber = cmd.ItemSnapshot.IndexNumber
		state.ParentIndexNumber = cmd.ItemSnapshot.ParentIndexNumber
		state.RunTimeTicks = cmd.ItemSnapshot.RunTimeTicks
		state.OrphanedAt = nil
		if state.LastSeenAt == nil || cmd.ReceivedAt.After(*state.LastSeenAt) {
			seenAt := cmd.ReceivedAt.UTC()
			state.LastSeenAt = &seenAt
		}
		state.Fingerprint = snapshotFingerprint(cmd.ItemSnapshot)
	}

	if !cmd.MetadataConfirmed {
		if cmd.RunTimeTicks > 0 {
			state.RunTimeTicks = cmd.RunTimeTicks
		} else if cmd.ItemSnapshot.RunTimeTicks > 0 && state.RunTimeTicks <= 0 {
			state.RunTimeTicks = cmd.ItemSnapshot.RunTimeTicks
		}
	}

	if cmd.PlayState.PositionTicks != nil {
		pos := *cmd.PlayState.PositionTicks
		if pos < 0 {
			pos = 0
		}
		state.PlaybackPositionTicks = pos
	}
	if cmd.PlayedPercentage != nil {
		pct := *cmd.PlayedPercentage
		state.PlayedPercentage = &pct
	}
	wasPlayed := state.Played
	if cmd.Played != nil {
		state.Played = *cmd.Played
	}

	if cmd.Kind == PlaybackReportStopped {
		policy = policyForItemType(policy, state.ItemType)
		applyDurableStoppedState(&state, cmd.ReceivedAt, wasPlayed, policy, cmd.Played != nil && *cmd.Played)
	}

	state.UpdatedAt = cmd.ReceivedAt
	return state
}

func policyForItemType(policy PlaybackResumePolicy, itemType string) PlaybackResumePolicy {
	if strings.EqualFold(itemType, "AudioBook") || strings.EqualFold(itemType, "Book") {
		policy.MinDurationSeconds = 0
	}
	return policy
}

// applyDurableStoppedState finalizes durable state for Stopped reports.
// Percentage-based completion requires known runtime > 0. Explicit Played=true
// may complete even when runtime is unknown. Unknown runtime preserves resume.
func applyDurableStoppedState(state *PlaybackState, now time.Time, wasPlayed bool, policy PlaybackResumePolicy, explicitPlayedTrue bool) {
	if state == nil {
		return
	}
	// Already-played or explicit Played=true starts completed; percentage alone does not
	// complete when runtime is unknown.
	completed := explicitPlayedTrue || state.Played
	position := state.PlaybackPositionTicks
	if position < 0 {
		position = 0
	}
	runtime := state.RunTimeTicks

	if !completed && runtime > 0 && position > 0 {
		percentage := (float64(position) / float64(runtime)) * 100
		durationSeconds := float64(runtime) / float64(embyTicksPerSecond)
		switch {
		case percentage < policy.MinPct:
			position = 0
			state.PlayedPercentage = nil
		case percentage > policy.MaxPct || position >= runtime-embyTicksPerSecond:
			completed = true
		case policy.MinDurationSeconds > 0 && durationSeconds < policy.MinDurationSeconds:
			completed = true
		}
	}
	if !completed && runtime > 0 && state.PlayedPercentage != nil && *state.PlayedPercentage >= policy.MaxPct {
		completed = true
	}

	if completed {
		lastPlayed := now.UTC()
		state.LastPlayedDate = &lastPlayed
		state.Played = true
		state.PlaybackPositionTicks = 0
		state.PlayedPercentage = floatPtr(100)
		if !wasPlayed {
			state.PlayCount++
		}
		return
	}
	state.PlaybackPositionTicks = position
}

func mergeSnapshotIntoDurable(state *PlaybackState, snap PlaybackItemSnapshot) {
	if state == nil {
		return
	}
	if snap.Name != "" {
		state.ItemName = snap.Name
	}
	if snap.Type != "" {
		state.ItemType = snap.Type
	}
	if snap.SeriesID != "" {
		state.SeriesID = snap.SeriesID
	}
	if snap.SeriesName != "" {
		state.SeriesName = snap.SeriesName
	}
	if snap.SeasonID != "" {
		state.SeasonID = snap.SeasonID
	}
	if snap.IndexNumber != 0 {
		state.IndexNumber = snap.IndexNumber
	}
	if snap.ParentIndexNumber != 0 {
		state.ParentIndexNumber = snap.ParentIndexNumber
	}
	if snap.RunTimeTicks > 0 {
		state.RunTimeTicks = snap.RunTimeTicks
	}
	fp := snapshotFingerprint(snap)
	if fp == "" {
		return
	}
	if state.Fingerprint == "" || fingerprintsCompatible(state.Fingerprint, fp) {
		// Prefer richer stable facets: merge overlapping keys via compatibility, store new when richer.
		state.Fingerprint = mergeFingerprint(state.Fingerprint, fp)
	}
}

func snapshotFingerprint(snap PlaybackItemSnapshot) string {
	// Stable facets only (Type/SeriesId); Name is intentionally excluded.
	parts := make([]string, 0, 2)
	if snap.Type != "" {
		parts = append(parts, "type="+snap.Type)
	}
	if snap.SeriesID != "" {
		parts = append(parts, "seriesid="+snap.SeriesID)
	}
	return strings.Join(parts, "|")
}

func mergeFingerprint(existing, incoming string) string {
	if existing == "" {
		return incoming
	}
	if incoming == "" {
		return existing
	}
	parts := fingerprintParts(existing)
	for k, v := range fingerprintParts(incoming) {
		parts[k] = v
	}
	// Deterministic key order matching itemFingerprint facet priority.
	out := make([]string, 0, len(parts))
	for _, key := range []string{"type", "seriesid", "name"} {
		if v, ok := parts[key]; ok {
			out = append(out, key+"="+v)
			delete(parts, key)
		}
	}
	// Any remaining keys sorted by insertion order is unstable; append remaining deterministically.
	if len(parts) > 0 {
		keys := make([]string, 0, len(parts))
		for k := range parts {
			keys = append(keys, k)
		}
		// tiny insertion sort to avoid importing sort for rare path
		for i := 1; i < len(keys); i++ {
			for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
				keys[j], keys[j-1] = keys[j-1], keys[j]
			}
		}
		for _, k := range keys {
			out = append(out, k+"="+parts[k])
		}
	}
	return strings.Join(out, "|")
}

func buildPlaybackEvent(cmd PlaybackReportCommand, session Session, durable PlaybackState) *PlaybackEvent {
	ev := &PlaybackEvent{
		GatewayUserID:   session.GatewayUserID,
		SyntheticUserID: session.SyntheticUserID,
		ItemID:          cmd.ItemID,
		ItemName:        durable.ItemName,
		Event:           playbackReportEventName(cmd.Kind),
		RemoteIP:        cmd.RemoteIP,
		CreatedAt:       cmd.ReceivedAt,
	}
	if cmd.PlayState.PositionTicks != nil {
		ev.PositionTicks = *cmd.PlayState.PositionTicks
		if ev.PositionTicks < 0 {
			ev.PositionTicks = 0
		}
	} else {
		ev.PositionTicks = durable.PlaybackPositionTicks
	}
	if cmd.Played != nil {
		v := *cmd.Played
		ev.Played = &v
	}
	if cmd.PlayedPercentage != nil {
		v := *cmd.PlayedPercentage
		ev.PlayedPercentage = &v
	}
	return ev
}

func playbackReportEventName(kind PlaybackReportKind) string {
	switch kind {
	case PlaybackReportProgress:
		return "progress"
	case PlaybackReportStopped:
		return "stopped"
	default:
		return "playing"
	}
}

func mergePlaybackItemSnapshot(base, patch PlaybackItemSnapshot) PlaybackItemSnapshot {
	out := base
	if patch.ID != "" {
		out.ID = patch.ID
	}
	if patch.Name != "" {
		out.Name = patch.Name
	}
	if patch.Type != "" {
		out.Type = patch.Type
	}
	if patch.MediaType != "" {
		out.MediaType = patch.MediaType
	}
	if patch.SeriesID != "" {
		out.SeriesID = patch.SeriesID
	}
	if patch.SeriesName != "" {
		out.SeriesName = patch.SeriesName
	}
	if patch.SeasonID != "" {
		out.SeasonID = patch.SeasonID
	}
	if patch.ParentID != "" {
		out.ParentID = patch.ParentID
	}
	if patch.IndexNumber != 0 {
		out.IndexNumber = patch.IndexNumber
	}
	if patch.ParentIndexNumber != 0 {
		out.ParentIndexNumber = patch.ParentIndexNumber
	}
	if patch.RunTimeTicks > 0 {
		out.RunTimeTicks = patch.RunTimeTicks
	}
	if patch.ProductionYear != 0 {
		out.ProductionYear = patch.ProductionYear
	}
	if patch.PremiereDate != "" {
		out.PremiereDate = patch.PremiereDate
	}
	if patch.CommunityRating != 0 {
		out.CommunityRating = patch.CommunityRating
	}
	if patch.OfficialRating != "" {
		out.OfficialRating = patch.OfficialRating
	}
	if len(patch.ImageTags) > 0 {
		out.ImageTags = mergeStringMap(out.ImageTags, patch.ImageTags)
	}
	return out
}

func mergePlaybackPlayState(base, patch PlaybackPlayState) PlaybackPlayState {
	out := clonePlaybackPlayState(base)
	if patch.PositionTicks != nil {
		v := *patch.PositionTicks
		out.PositionTicks = &v
	}
	if patch.CanSeek != nil {
		v := *patch.CanSeek
		out.CanSeek = &v
	}
	if patch.IsPaused != nil {
		v := *patch.IsPaused
		out.IsPaused = &v
	}
	if patch.IsMuted != nil {
		v := *patch.IsMuted
		out.IsMuted = &v
	}
	if patch.VolumeLevel != nil {
		v := *patch.VolumeLevel
		out.VolumeLevel = &v
	}
	if patch.AudioStreamIndex != nil {
		v := *patch.AudioStreamIndex
		out.AudioStreamIndex = &v
	}
	if patch.SubtitleStreamIndex != nil {
		v := *patch.SubtitleStreamIndex
		out.SubtitleStreamIndex = &v
	}
	if patch.MediaSourceID != nil {
		v := *patch.MediaSourceID
		out.MediaSourceID = &v
	}
	if patch.PlayMethod != nil {
		v := *patch.PlayMethod
		out.PlayMethod = &v
	}
	if patch.PlaybackRate != nil {
		v := *patch.PlaybackRate
		out.PlaybackRate = &v
	}
	if patch.RepeatMode != nil {
		v := *patch.RepeatMode
		out.RepeatMode = &v
	}
	if patch.Shuffle != nil {
		v := *patch.Shuffle
		out.Shuffle = &v
	}
	if patch.SubtitleOffset != nil {
		v := *patch.SubtitleOffset
		out.SubtitleOffset = &v
	}
	return out
}

func mergeStringMap(base, patch map[string]string) map[string]string {
	if len(base) == 0 && len(patch) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range patch {
		out[k] = v
	}
	return out
}

func cloneCurrentPlayback(c *CurrentPlayback) *CurrentPlayback {
	if c == nil {
		return nil
	}
	copyVal := cloneCurrentPlaybackValue(*c)
	return &copyVal
}

func cloneCurrentPlaybackValue(c CurrentPlayback) CurrentPlayback {
	c.ItemSnapshot = clonePlaybackItemSnapshot(c.ItemSnapshot)
	c.PlayState = clonePlaybackPlayState(c.PlayState)
	return c
}

func clonePlaybackItemSnapshot(s PlaybackItemSnapshot) PlaybackItemSnapshot {
	if len(s.ImageTags) > 0 {
		tags := make(map[string]string, len(s.ImageTags))
		for k, v := range s.ImageTags {
			tags[k] = v
		}
		s.ImageTags = tags
	}
	return s
}

func clonePlaybackPlayState(ps PlaybackPlayState) PlaybackPlayState {
	out := PlaybackPlayState{}
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

func clonePlaybackState(s *PlaybackState) *PlaybackState {
	if s == nil {
		return nil
	}
	copyState := *s
	if s.PlayedPercentage != nil {
		v := *s.PlayedPercentage
		copyState.PlayedPercentage = &v
	}
	if s.LastPlayedDate != nil {
		t := *s.LastPlayedDate
		copyState.LastPlayedDate = &t
	}
	if s.Likes != nil {
		v := *s.Likes
		copyState.Likes = &v
	}
	if s.OrphanedAt != nil {
		t := *s.OrphanedAt
		copyState.OrphanedAt = &t
	}
	if s.LastSeenAt != nil {
		t := *s.LastSeenAt
		copyState.LastSeenAt = &t
	}
	return &copyState
}
