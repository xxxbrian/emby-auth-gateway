package gateway

import (
	"fmt"
	"math"
	"strings"
)

// Typed input bounds for prepared playback commands (schema-aligned where applicable).
const (
	playbackRemoteIPMaxBytes       = 128
	playbackEventNameMaxBytes      = 64
	playbackSnapshotNameMaxBytes   = 512
	playbackSnapshotTypeMaxBytes   = 80
	playbackSnapshotSeriesMaxBytes = 255
	playbackSnapshotDateMaxBytes   = 64
	playbackSnapshotRatingMaxBytes = 32
	playbackPlayMethodMaxBytes     = 64
	playbackRepeatModeMaxBytes     = 64
	playbackMaxImageTags           = 32
	playbackImageTagKeyMaxBytes    = 64
	playbackImageTagValueMaxBytes  = 255
)

// PreparePlaybackReportCommand returns a canonical validated copy of cmd.
// Repositories must call it before any session/current/durable lookup so lookup
// keys cannot diverge from reducer normalization.
//
// Rules:
//   - trim token/item/play/media IDs and relevant string optionals
//   - reconcile ItemID and ItemSnapshot.Id (derive when only one present; reject
//     nonempty conflict; canonical snapshot Id equals ItemID when item present)
//   - missing item remains a valid no-op command (not an error)
//   - validate kind/time/token, finite/nonnegative numerics, pointer values, policy
//   - zero policy fields receive defaults (including MinDurationSeconds zero);
//     explicit configured values are kept; impossible/nonfinite policy is rejected
func PreparePlaybackReportCommand(cmd PlaybackReportCommand) (PlaybackReportCommand, error) {
	// Own every mutable nested value before normalization or validation. Both
	// successful and rejected preparation must leave caller state untouched.
	out := clonePlaybackReportCommand(cmd)
	out.GatewayTokenHash = strings.TrimSpace(out.GatewayTokenHash)
	out.ItemID = strings.TrimSpace(out.ItemID)
	out.PlaySessionID = strings.TrimSpace(out.PlaySessionID)
	out.MediaSourceID = strings.TrimSpace(out.MediaSourceID)
	out.EventName = strings.TrimSpace(out.EventName)
	out.RemoteIP = strings.TrimSpace(out.RemoteIP)
	if !out.ReceivedAt.IsZero() {
		out.ReceivedAt = out.ReceivedAt.UTC()
	}

	if out.GatewayTokenHash == "" {
		return PlaybackReportCommand{}, fmt.Errorf("%w: gateway token hash required", ErrBadRequest)
	}
	if out.ReceivedAt.IsZero() {
		return PlaybackReportCommand{}, fmt.Errorf("%w: received_at required", ErrBadRequest)
	}
	switch out.Kind {
	case PlaybackReportPlaying, PlaybackReportProgress, PlaybackReportStopped, PlaybackReportPing:
	default:
		return PlaybackReportCommand{}, fmt.Errorf("%w: invalid playback report kind", ErrBadRequest)
	}

	// Reconcile ItemID / snapshot Id.
	snapID := strings.TrimSpace(out.ItemSnapshot.ID)
	out.ItemSnapshot.ID = snapID
	switch {
	case out.ItemID == "" && snapID == "":
		// missing item: no-op contract
	case out.ItemID == "":
		out.ItemID = snapID
	case snapID == "":
		out.ItemSnapshot.ID = out.ItemID
	case out.ItemID != snapID:
		return PlaybackReportCommand{}, fmt.Errorf(
			"%w: item id %q conflicts with item snapshot Id %q",
			ErrBadRequest, out.ItemID, snapID,
		)
	}
	if out.ItemID != "" {
		if err := validateCurrentPlaybackItemID(out.ItemID); err != nil {
			return PlaybackReportCommand{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
		}
		out.ItemSnapshot.ID = out.ItemID
	}

	if err := validateCurrentPlaybackOptionalText("play_session_id", out.PlaySessionID, currentPlaybackPlaySessionIDMaxBytes); err != nil {
		return PlaybackReportCommand{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if err := validateCurrentPlaybackOptionalText("media_source_id", out.MediaSourceID, currentPlaybackMediaSourceIDMaxBytes); err != nil {
		return PlaybackReportCommand{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if err := validateCurrentPlaybackOptionalText("remote_ip", out.RemoteIP, playbackRemoteIPMaxBytes); err != nil {
		return PlaybackReportCommand{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if err := validateCurrentPlaybackOptionalText("event_name", out.EventName, playbackEventNameMaxBytes); err != nil {
		return PlaybackReportCommand{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}

	if err := validatePreparedItemSnapshot(&out.ItemSnapshot, out.ItemID); err != nil {
		return PlaybackReportCommand{}, err
	}
	if err := validatePreparedPlayState(&out.PlayState); err != nil {
		return PlaybackReportCommand{}, err
	}

	// Apply EventName after validating play-state shape so Pause/Unpause is canonical.
	out.PlayState = applyEventNameToPlayState(out.PlayState, out.EventName)
	if out.MediaSourceID == "" && out.PlayState.MediaSourceID != nil {
		ms := strings.TrimSpace(*out.PlayState.MediaSourceID)
		out.MediaSourceID = ms
		if ms != "" {
			v := ms
			out.PlayState.MediaSourceID = &v
		}
		if err := validateCurrentPlaybackOptionalText("media_source_id", out.MediaSourceID, currentPlaybackMediaSourceIDMaxBytes); err != nil {
			return PlaybackReportCommand{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
		}
	}

	if out.RunTimeTicks < 0 {
		return PlaybackReportCommand{}, fmt.Errorf("%w: run_time_ticks must be nonnegative", ErrBadRequest)
	}
	if out.PlayedPercentage != nil {
		if err := requireFiniteNonNegative("played_percentage", *out.PlayedPercentage); err != nil {
			return PlaybackReportCommand{}, err
		}
	}

	policy, err := normalizePlaybackResumePolicy(out.Policy)
	if err != nil {
		return PlaybackReportCommand{}, err
	}
	out.Policy = policy

	return out, nil
}

func clonePlaybackReportCommand(cmd PlaybackReportCommand) PlaybackReportCommand {
	out := cmd
	out.ItemSnapshot = clonePlaybackItemSnapshot(cmd.ItemSnapshot)
	out.PlayState = clonePlaybackPlayState(cmd.PlayState)
	if cmd.Played != nil {
		v := *cmd.Played
		out.Played = &v
	}
	if cmd.PlayedPercentage != nil {
		v := *cmd.PlayedPercentage
		out.PlayedPercentage = &v
	}
	return out
}

// normalizePlaybackResumePolicy fills zero fields with defaults and rejects
// impossible or nonfinite thresholds. Explicit nonzero values are preserved.
// MinDurationSeconds zero receives the default (cannot encode "disable" via zero
// on the command; item-type overrides may still set zero later in the reducer).
func normalizePlaybackResumePolicy(p PlaybackResumePolicy) (PlaybackResumePolicy, error) {
	if err := requireFinite("policy.min_pct", p.MinPct); err != nil {
		return PlaybackResumePolicy{}, err
	}
	if err := requireFinite("policy.max_pct", p.MaxPct); err != nil {
		return PlaybackResumePolicy{}, err
	}
	if err := requireFinite("policy.min_duration_seconds", p.MinDurationSeconds); err != nil {
		return PlaybackResumePolicy{}, err
	}
	if p.MinPct < 0 || p.MaxPct < 0 || p.MinDurationSeconds < 0 {
		return PlaybackResumePolicy{}, fmt.Errorf("%w: policy thresholds must be nonnegative", ErrBadRequest)
	}
	if p.MinPct == 0 {
		p.MinPct = defaultMinResumePct
	}
	if p.MaxPct == 0 {
		p.MaxPct = defaultMaxResumePct
	}
	if p.MinDurationSeconds == 0 {
		p.MinDurationSeconds = defaultMinResumeDurationSeconds
	}
	if p.MinPct > p.MaxPct {
		return PlaybackResumePolicy{}, fmt.Errorf("%w: policy min_pct %v exceeds max_pct %v", ErrBadRequest, p.MinPct, p.MaxPct)
	}
	if p.MaxPct > 100 {
		return PlaybackResumePolicy{}, fmt.Errorf("%w: policy max_pct %v exceeds 100", ErrBadRequest, p.MaxPct)
	}
	return p, nil
}

func validatePreparedItemSnapshot(snap *PlaybackItemSnapshot, itemID string) error {
	if snap == nil {
		return fmt.Errorf("%w: item snapshot is nil", ErrBadRequest)
	}
	if err := validatePlaybackItemSnapshotFields(*snap, itemID); err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	return nil
}

func validatePreparedPlayState(ps *PlaybackPlayState) error {
	if ps == nil {
		return fmt.Errorf("%w: play state is nil", ErrBadRequest)
	}
	if ps.MediaSourceID != nil {
		*ps.MediaSourceID = strings.TrimSpace(*ps.MediaSourceID)
	}
	if ps.PlayMethod != nil {
		*ps.PlayMethod = strings.TrimSpace(*ps.PlayMethod)
	}
	if ps.RepeatMode != nil {
		*ps.RepeatMode = strings.TrimSpace(*ps.RepeatMode)
	}
	if err := validatePlaybackPlayStateFields(*ps); err != nil {
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	return nil
}

func requireFinite(field string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%w: %s is not a finite number", ErrBadRequest, field)
	}
	return nil
}

func requireFiniteNonNegative(field string, v float64) error {
	if err := requireFinite(field, v); err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("%w: %s must be nonnegative", ErrBadRequest, field)
	}
	return nil
}

// ValidatePlaybackMutationPlan is the shared storage-neutral guard used before
// any repository mutation. It checks action/pointer coherence, typed current
// integrity, durable/event identifiers and numerics, and cross-object
// item/user/token consistency against the prepared command and session.
func ValidatePlaybackMutationPlan(plan PlaybackMutationPlan, cmd PlaybackReportCommand, session Session) error {
	if session.GatewayTokenHash == "" || session.GatewayTokenHash != cmd.GatewayTokenHash {
		return fmt.Errorf("playback plan integrity: session token hash mismatch")
	}
	if plan.Result.GatewayUserID != "" && plan.Result.GatewayUserID != session.GatewayUserID {
		return fmt.Errorf("playback plan integrity: result gateway user mismatch")
	}
	if plan.Result.SyntheticUserID != "" && plan.Result.SyntheticUserID != session.SyntheticUserID {
		return fmt.Errorf("playback plan integrity: result synthetic user mismatch")
	}

	switch plan.CurrentAction {
	case PlaybackCurrentNone, PlaybackCurrentPreserve:
		if plan.Current != nil {
			return fmt.Errorf("playback plan integrity: current pointer must be nil for action %d", plan.CurrentAction)
		}
	case PlaybackCurrentUpsert:
		if plan.Current == nil {
			return fmt.Errorf("playback plan integrity: upsert requires current")
		}
		if err := ValidateCurrentPlayback(plan.Current, cmd.GatewayTokenHash); err != nil {
			return err
		}
	case PlaybackCurrentDelete:
		if plan.Current != nil {
			return fmt.Errorf("playback plan integrity: delete requires nil current")
		}
	default:
		return fmt.Errorf("playback plan integrity: unknown current action %d", plan.CurrentAction)
	}

	if plan.WriteDurable {
		if plan.Durable == nil {
			return fmt.Errorf("playback plan integrity: WriteDurable requires durable")
		}
		if err := validatePlanDurableState(*plan.Durable, session, cmd); err != nil {
			return err
		}
	} else if plan.Durable != nil {
		return fmt.Errorf("playback plan integrity: durable set without WriteDurable")
	}

	if plan.Event != nil {
		if cmd.Kind == PlaybackReportPing {
			return fmt.Errorf("playback plan integrity: ping must not write event")
		}
		if err := validatePlanPlaybackEvent(*plan.Event, session, cmd); err != nil {
			return err
		}
		if plan.WriteDurable && plan.Durable != nil && plan.Event.ItemID != plan.Durable.ItemID {
			return fmt.Errorf("playback plan integrity: event item_id %q != durable item_id %q", plan.Event.ItemID, plan.Durable.ItemID)
		}
	}

	if plan.ActivityAt != nil {
		if plan.ActivityAt.IsZero() {
			return fmt.Errorf("playback plan integrity: activity_at is zero")
		}
	}

	// Result.Current coherence with action.
	switch plan.CurrentAction {
	case PlaybackCurrentUpsert:
		if plan.Result.Current == nil {
			return fmt.Errorf("playback plan integrity: upsert result missing current")
		}
		if plan.Result.Current.ItemID != plan.Current.ItemID {
			return fmt.Errorf("playback plan integrity: result current item mismatch")
		}
		if err := ValidateCurrentPlayback(plan.Result.Current, cmd.GatewayTokenHash); err != nil {
			return err
		}
	case PlaybackCurrentPreserve:
		if plan.Result.Current == nil {
			return fmt.Errorf("playback plan integrity: preserve result missing current")
		}
		if err := ValidateCurrentPlayback(plan.Result.Current, cmd.GatewayTokenHash); err != nil {
			return err
		}
	case PlaybackCurrentDelete:
		if plan.Result.Current != nil {
			return fmt.Errorf("playback plan integrity: delete result must omit current")
		}
	}

	if plan.WriteDurable && plan.Result.Durable == nil {
		return fmt.Errorf("playback plan integrity: write durable result missing durable")
	}
	if !plan.WriteDurable && plan.Result.Durable != nil {
		return fmt.Errorf("playback plan integrity: result durable without write")
	}
	return nil
}

func validatePlanDurableState(state PlaybackState, session Session, cmd PlaybackReportCommand) error {
	if state.GatewayUserID == "" || state.GatewayUserID != session.GatewayUserID {
		return fmt.Errorf("playback plan integrity: durable gateway user mismatch")
	}
	if state.SyntheticUserID != "" && state.SyntheticUserID != session.SyntheticUserID {
		return fmt.Errorf("playback plan integrity: durable synthetic user mismatch")
	}
	if err := validateCurrentPlaybackItemID(state.ItemID); err != nil {
		return fmt.Errorf("playback plan integrity: durable: %v", err)
	}
	if cmd.ItemID != "" && state.ItemID != cmd.ItemID {
		return fmt.Errorf("playback plan integrity: durable item_id %q != command item_id %q", state.ItemID, cmd.ItemID)
	}
	if state.RunTimeTicks < 0 || state.PlaybackPositionTicks < 0 || state.PlayCount < 0 {
		return fmt.Errorf("playback plan integrity: durable numeric field is negative")
	}
	if state.PlayedPercentage != nil {
		if err := requireFiniteNonNegative("durable.played_percentage", *state.PlayedPercentage); err != nil {
			return fmt.Errorf("playback plan integrity: %v", err)
		}
	}
	if state.UpdatedAt.IsZero() {
		return fmt.Errorf("playback plan integrity: durable updated_at is zero")
	}
	if state.LastPlayedDate != nil && state.LastPlayedDate.IsZero() {
		return fmt.Errorf("playback plan integrity: durable last_played_date is zero pointer")
	}
	return nil
}

func validatePlanPlaybackEvent(ev PlaybackEvent, session Session, cmd PlaybackReportCommand) error {
	if ev.GatewayUserID == "" || ev.GatewayUserID != session.GatewayUserID {
		return fmt.Errorf("playback plan integrity: event gateway user mismatch")
	}
	if ev.SyntheticUserID != "" && ev.SyntheticUserID != session.SyntheticUserID {
		return fmt.Errorf("playback plan integrity: event synthetic user mismatch")
	}
	if err := validateCurrentPlaybackItemID(ev.ItemID); err != nil {
		return fmt.Errorf("playback plan integrity: event: %v", err)
	}
	if cmd.ItemID != "" && ev.ItemID != cmd.ItemID {
		return fmt.Errorf("playback plan integrity: event item_id %q != command item_id %q", ev.ItemID, cmd.ItemID)
	}
	if ev.Event == "" {
		return fmt.Errorf("playback plan integrity: event name is empty")
	}
	if ev.PositionTicks < 0 {
		return fmt.Errorf("playback plan integrity: event position_ticks is negative")
	}
	if ev.CreatedAt.IsZero() {
		return fmt.Errorf("playback plan integrity: event created_at is zero")
	}
	if ev.PlayedPercentage != nil {
		if err := requireFiniteNonNegative("event.played_percentage", *ev.PlayedPercentage); err != nil {
			return fmt.Errorf("playback plan integrity: %v", err)
		}
	}
	return nil
}
