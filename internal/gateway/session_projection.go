package gateway

import (
	"encoding/json"
	"time"
)

// sessionInfoDTO projects a gateway Session into Emby SessionInfo JSON.
// current is nil for idle sessions (login and sessions without now-playing).
// userData is gateway-local UserData for the authenticated user/item; nil when idle.
// NowPlayingItem is omitted while idle (Phase 3 shape).
func sessionInfoDTO(session *Session, serverID string, current *CurrentPlayback, userData map[string]any) map[string]any {
	return sessionInfoMapDTOWithRemoteControl(session, serverID, current, userData, false)
}

func sessionInfoDTOWithRemoteControl(session *Session, serverID string, current *CurrentPlayback, userData map[string]any, supportsRemoteControl bool) map[string]any {
	return sessionInfoMapDTOWithRemoteControl(session, serverID, current, userData, supportsRemoteControl)
}

func sessionInfoMapDTOWithRemoteControl(session *Session, serverID string, current *CurrentPlayback, userData map[string]any, supportsRemoteControl bool) map[string]any {
	if session == nil {
		return map[string]any{}
	}
	typed := sessionInfoWireDTO(session, serverID, current, userDataMapToWireDTO(userData), supportsRemoteControl)
	data, _ := json.Marshal(typed)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func sessionInfoWireDTO(session *Session, serverID string, current *CurrentPlayback, userData *embyUserItemData, supportsRemoteControl bool) embySessionInfo {
	if session == nil {
		return embySessionInfo{}
	}
	activity := session.LastActivityAt
	if activity.IsZero() {
		activity = session.CreatedAt
	}
	if activity.IsZero() {
		activity = time.Now().UTC()
	}
	out := embySessionInfo{
		ID:                    session.PublicID,
		ServerID:              serverID,
		UserID:                session.SyntheticUserID,
		UserName:              session.GatewayUsername,
		Client:                session.Client,
		DeviceName:            session.Device,
		DeviceID:              session.DeviceID,
		ApplicationVersion:    session.Version,
		SupportedCommands:     nonNilStrings(session.Capabilities.SupportedCommands),
		PlayableMediaTypes:    nonNilStrings(session.Capabilities.PlayableMediaTypes),
		AdditionalUsers:       nonNilRawMessages{},
		SupportsRemoteControl: supportsRemoteControl,
		LastActivityDate:      activity.UTC().Format(time.RFC3339Nano),
		PlayState:             idlePlayStateWireDTO(),
	}
	if current == nil {
		return out
	}
	out.PlayState = activePlayStateWireDTO(current.PlayState, current.MediaSourceID)
	out.NowPlayingItem = nowPlayingItemWireDTO(current.ItemSnapshot, current.ItemID, userData)
	return out
}

func sessionSupportsRemoteControl(session *Session, live func(SessionConnectionIdentity) bool) bool {
	if session == nil || live == nil || session.GatewayTokenHash == "" || session.GatewayUserID == "" || session.PublicID == "" {
		return false
	}
	if !live(SessionConnectionIdentity{
		TokenHash:     session.GatewayTokenHash,
		PublicID:      session.PublicID,
		GatewayUserID: session.GatewayUserID,
	}) {
		return false
	}
	return sessionCapabilitiesSupportRemoteControl(session.Capabilities)
}

func idlePlayStateDTO() map[string]any {
	// Phase 3 idle shape: bool triad only (no PositionTicks).
	return map[string]any{
		"CanSeek":  false,
		"IsPaused": false,
		"IsMuted":  false,
	}
}

func idlePlayStateWireDTO() embyPlayState {
	return embyPlayState{}
}

// activePlayStateDTO always includes PositionTicks/CanSeek/IsPaused/IsMuted.
// Optional fields appear only when reported (no nulls). Position is never
// auto-advanced from LastReportedAt.
func activePlayStateDTO(ps PlaybackPlayState, mediaSourceID string) map[string]any {
	typed := activePlayStateWireDTO(ps, mediaSourceID)
	data, _ := json.Marshal(typed)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

func activePlayStateWireDTO(ps PlaybackPlayState, mediaSourceID string) embyPlayState {
	pos := int64(0)
	if ps.PositionTicks != nil {
		pos = *ps.PositionTicks
		if pos < 0 {
			pos = 0
		}
	}
	out := embyPlayState{
		PositionTicks: &pos,
		CanSeek:       boolOrFalse(ps.CanSeek),
		IsPaused:      boolOrFalse(ps.IsPaused),
		IsMuted:       boolOrFalse(ps.IsMuted),
	}
	if ps.VolumeLevel != nil {
		out.VolumeLevel = intPointer(*ps.VolumeLevel)
	}
	if ps.AudioStreamIndex != nil {
		out.AudioStreamIndex = intPointer(*ps.AudioStreamIndex)
	}
	if ps.SubtitleStreamIndex != nil {
		out.SubtitleStreamIndex = intPointer(*ps.SubtitleStreamIndex)
	}
	ms := ""
	if ps.MediaSourceID != nil {
		ms = *ps.MediaSourceID
	}
	if ms == "" {
		ms = mediaSourceID
	}
	if ms != "" {
		out.MediaSourceID = stringPointerValue(ms)
	}
	if ps.PlayMethod != nil && *ps.PlayMethod != "" {
		out.PlayMethod = stringPointerValue(*ps.PlayMethod)
	}
	if ps.PlaybackRate != nil {
		out.PlaybackRate = float64Pointer(*ps.PlaybackRate)
	}
	if ps.RepeatMode != nil && *ps.RepeatMode != "" {
		out.RepeatMode = stringPointerValue(*ps.RepeatMode)
	}
	if ps.Shuffle != nil {
		out.Shuffle = boolPointer(*ps.Shuffle)
	}
	if ps.SubtitleOffset != nil {
		out.SubtitleOffset = float64Pointer(*ps.SubtitleOffset)
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
	raw := nowPlayingItemWireDTO(snap, itemID, userDataMapToWireDTO(userData))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func nowPlayingItemWireDTO(snap PlaybackItemSnapshot, itemID string, userData *embyUserItemData) json.RawMessage {
	id := snap.ID
	if id == "" {
		id = itemID
	}
	snap.ID = id
	raw, _ := json.Marshal(snap)
	return mergeNowPlayingItemUserData(raw, userData)
}

func mergeNowPlayingItemUserData(raw json.RawMessage, userData *embyUserItemData) json.RawMessage {
	doc, err := parseRawJSONObject(raw, "UserData")
	if err != nil {
		return nil
	}
	if userData != nil {
		encoded, err := json.Marshal(userData)
		if err != nil || doc.SetSemantic("UserData", encoded) != nil {
			return nil
		}
	}
	encoded, err := doc.MarshalJSON()
	if err != nil {
		return nil
	}
	return encoded
}

func boolPointer(value bool) *bool { return &value }

func float64Pointer(value float64) *float64 { return &value }

func stringPointerValue(value string) *string { return &value }

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
