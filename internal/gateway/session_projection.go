package gateway

import (
	"time"
)

// sessionInfoDTO projects a gateway Session into Emby SessionInfo JSON.
// NowPlayingItem is omitted while idle (Phase 3).
func sessionInfoDTO(session *Session, serverID string) map[string]any {
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
	return map[string]any{
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
		"PlayState": map[string]any{
			"CanSeek":  false,
			"IsPaused": false,
			"IsMuted":  false,
		},
	}
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
