package gateway

import (
	"time"
)

// sessionInfoDTO projects a gateway Session into Emby SessionInfo JSON.
// current is nil for idle sessions (login and sessions without now-playing).
// userData is gateway-local UserData for the authenticated user/item; nil when idle.
// NowPlayingItem is omitted while idle (Phase 3 shape).
func sessionInfoDTO(session *Session, serverID string, current *CurrentPlayback, userData map[string]any) map[string]any {
	if session == nil {
		return map[string]any{}
	}
	media := session.Capabilities.PlayableMediaTypes
	if media == nil {
		media = []string{}
	}
	commands := session.Capabilities.SupportedCommands
	if commands == nil {
		commands = []string{}
	}
	activity := session.LastActivityAt
	if activity.IsZero() {
		activity = session.CreatedAt
	}
	if activity.IsZero() {
		activity = time.Now().UTC()
	}
	out := map[string]any{
		"Id":                    session.PublicID,
		"ServerId":              serverID,
		"UserId":                session.SyntheticUserID,
		"UserName":              session.GatewayUsername,
		"Client":                session.Client,
		"DeviceName":            session.Device,
		"DeviceId":              session.DeviceID,
		"ApplicationVersion":    session.Version,
		"SupportedCommands":     commands,
		"PlayableMediaTypes":    media,
		"AdditionalUsers":       []any{},
		"SupportsRemoteControl": false,
		"LastActivityDate":      activity.UTC().Format(time.RFC3339Nano),
		"PlayState":             idlePlayStateDTO(),
	}
	if current == nil {
		return out
	}
	out["PlayState"] = activePlayStateDTO(current.PlayState, current.MediaSourceID)
	out["NowPlayingItem"] = nowPlayingItemDTO(current.ItemSnapshot, current.ItemID, userData)
	return out
}

func idlePlayStateDTO() map[string]any {
	// Phase 3 idle shape: bool triad only (no PositionTicks).
	return map[string]any{
		"CanSeek":  false,
		"IsPaused": false,
		"IsMuted":  false,
	}
}

// activePlayStateDTO always includes PositionTicks/CanSeek/IsPaused/IsMuted.
// Optional fields appear only when reported (no nulls). Position is never
// auto-advanced from LastReportedAt.
func activePlayStateDTO(ps PlaybackPlayState, mediaSourceID string) map[string]any {
	pos := int64(0)
	if ps.PositionTicks != nil {
		pos = *ps.PositionTicks
		if pos < 0 {
			pos = 0
		}
	}
	out := map[string]any{
		"PositionTicks": pos,
		"CanSeek":       boolOrFalse(ps.CanSeek),
		"IsPaused":      boolOrFalse(ps.IsPaused),
		"IsMuted":       boolOrFalse(ps.IsMuted),
	}
	if ps.VolumeLevel != nil {
		out["VolumeLevel"] = *ps.VolumeLevel
	}
	if ps.AudioStreamIndex != nil {
		out["AudioStreamIndex"] = *ps.AudioStreamIndex
	}
	if ps.SubtitleStreamIndex != nil {
		out["SubtitleStreamIndex"] = *ps.SubtitleStreamIndex
	}
	ms := ""
	if ps.MediaSourceID != nil {
		ms = *ps.MediaSourceID
	}
	if ms == "" {
		ms = mediaSourceID
	}
	if ms != "" {
		out["MediaSourceId"] = ms
	}
	if ps.PlayMethod != nil && *ps.PlayMethod != "" {
		out["PlayMethod"] = *ps.PlayMethod
	}
	if ps.PlaybackRate != nil {
		out["PlaybackRate"] = *ps.PlaybackRate
	}
	if ps.RepeatMode != nil && *ps.RepeatMode != "" {
		out["RepeatMode"] = *ps.RepeatMode
	}
	if ps.Shuffle != nil {
		out["Shuffle"] = *ps.Shuffle
	}
	if ps.SubtitleOffset != nil {
		out["SubtitleOffset"] = *ps.SubtitleOffset
	}
	return out
}

func boolOrFalse(v *bool) bool {
	if v == nil {
		return false
	}
	return *v
}

// nowPlayingItemDTO projects an allowlisted item snapshot. Always includes Id.
// Only nonempty/present fields are emitted; no internal/raw fields.
func nowPlayingItemDTO(snap PlaybackItemSnapshot, itemID string, userData map[string]any) map[string]any {
	id := snap.ID
	if id == "" {
		id = itemID
	}
	out := map[string]any{
		"Id": id,
	}
	if snap.Name != "" {
		out["Name"] = snap.Name
	}
	if snap.Type != "" {
		out["Type"] = snap.Type
	}
	if snap.MediaType != "" {
		out["MediaType"] = snap.MediaType
	}
	if snap.SeriesID != "" {
		out["SeriesId"] = snap.SeriesID
	}
	if snap.SeriesName != "" {
		out["SeriesName"] = snap.SeriesName
	}
	if snap.SeasonID != "" {
		out["SeasonId"] = snap.SeasonID
	}
	if snap.ParentID != "" {
		out["ParentId"] = snap.ParentID
	}
	if snap.IndexNumber != 0 {
		out["IndexNumber"] = snap.IndexNumber
	}
	if snap.ParentIndexNumber != 0 {
		out["ParentIndexNumber"] = snap.ParentIndexNumber
	}
	if snap.RunTimeTicks > 0 {
		out["RunTimeTicks"] = snap.RunTimeTicks
	}
	if snap.ProductionYear != 0 {
		out["ProductionYear"] = snap.ProductionYear
	}
	if snap.PremiereDate != "" {
		out["PremiereDate"] = snap.PremiereDate
	}
	if snap.CommunityRating != 0 {
		out["CommunityRating"] = snap.CommunityRating
	}
	if snap.OfficialRating != "" {
		out["OfficialRating"] = snap.OfficialRating
	}
	if len(snap.ImageTags) > 0 {
		tags := make(map[string]string, len(snap.ImageTags))
		for k, v := range snap.ImageTags {
			tags[k] = v
		}
		out["ImageTags"] = tags
	}
	if userData != nil {
		out["UserData"] = userData
	}
	return out
}

// filterLocalSessions applies exact case-sensitive Id/PublicID and DeviceId filters
// and ControllableByUserId foreign-empty semantics. Unknown query params are ignored.
// sessions is assumed already sorted by the repository.
func filterLocalSessions(sessions []Session, current *Session, q map[string][]string) []Session {
	if current == nil {
		return nil
	}
	// ControllableByUserId: foreign identity yields empty list. Matching current
	// synthetic user (or Me) does not further filter; remote control remains false.
	if vals := q["ControllableByUserId"]; len(vals) > 0 {
		for _, v := range vals {
			if v == "" {
				continue
			}
			if !equalFoldMeOrID(v, current.SyntheticUserID) {
				return []Session{}
			}
		}
	}

	out := sessions
	if ids := q["Id"]; len(ids) > 0 {
		want := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if id != "" {
				want[id] = struct{}{}
			}
		}
		if len(want) > 0 {
			filtered := make([]Session, 0, len(out))
			for _, s := range out {
				if _, ok := want[s.PublicID]; ok {
					filtered = append(filtered, s)
				}
			}
			out = filtered
		}
	}
	if devices := q["DeviceId"]; len(devices) > 0 {
		want := make(map[string]struct{}, len(devices))
		for _, d := range devices {
			if d != "" {
				want[d] = struct{}{}
			}
		}
		if len(want) > 0 {
			filtered := make([]Session, 0, len(out))
			for _, s := range out {
				if _, ok := want[s.DeviceID]; ok {
					filtered = append(filtered, s)
				}
			}
			out = filtered
		}
	}
	if out == nil {
		return []Session{}
	}
	return out
}

func equalFoldMeOrID(value, syntheticID string) bool {
	if value == syntheticID {
		return true
	}
	// Emby sometimes uses "Me"; treat as current user only case-insensitively for Me.
	if len(value) == 2 && (value[0] == 'M' || value[0] == 'm') && (value[1] == 'e' || value[1] == 'E') {
		return true
	}
	return false
}
