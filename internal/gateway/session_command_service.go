package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	sessionCommandMaxTextBytes          = 4096
	sessionCommandMaxLabelBytes         = 256
	sessionCommandMaxItemIDBytes        = 80
	sessionCommandMaxItemIDs            = 128
	sessionCommandMaxTimeoutMS    int64 = 24 * 60 * 60 * 1000
	sessionCommandMinPlaybackRate       = 0.25
	sessionCommandMaxPlaybackRate       = 4.0
)

var noArgumentGeneralCommands = foldedCommandSet(
	"MoveUp", "MoveDown", "MoveLeft", "MoveRight", "PageUp", "PageDown",
	"PreviousLetter", "NextLetter", "ToggleOsd", "ToggleContextMenu", "Select", "Back",
	"GoHome", "GoToSettings", "VolumeUp", "VolumeDown", "Mute", "Unmute", "ToggleMute",
	"ToggleFullscreen", "GoToSearch",
)

var typedGeneralCommands = foldedCommandSet(
	"SendString", "SetVolume", "SetAudioStreamIndex", "SetSubtitleStreamIndex",
	"DisplayContent", "DisplayMessage", "PlayTrailers", "SetPlaybackRate",
)

var playCommands = foldedCommandSet("PlayNow", "PlayNext", "PlayLast")

var playstateCommands = foldedCommandSet(
	"Stop", "Pause", "Unpause", "PlayPause", "NextTrack", "PreviousTrack",
	"Seek", "Rewind", "FastForward", "SeekRelative",
)

// SessionCommandService validates and queues local commands for a live session.
// A nil error means the command was accepted by the process-local queue, not executed.
type SessionCommandService struct {
	repository SessionRepository
	hub        SessionHub
	now        func() time.Time
}

func NewSessionCommandService(repository SessionRepository, hub SessionHub) *SessionCommandService {
	return &SessionCommandService{repository: repository, hub: hub, now: func() time.Time { return time.Now().UTC() }}
}

func (s *SessionCommandService) Send(ctx context.Context, caller Session, targetPublicID string, command SessionCommandEnvelope) error {
	switch command.Category {
	case SessionCommandGeneral:
		if command.General == nil || command.Play != nil || command.Playstate != nil {
			return ErrBadRequest
		}
		return s.SendGeneral(ctx, caller, targetPublicID, *command.General)
	case SessionCommandPlay:
		if command.Play == nil || command.General != nil || command.Playstate != nil {
			return ErrBadRequest
		}
		return s.SendPlay(ctx, caller, targetPublicID, *command.Play)
	case SessionCommandPlaystate:
		if command.Playstate == nil || command.General != nil || command.Play != nil {
			return ErrBadRequest
		}
		return s.SendPlaystate(ctx, caller, targetPublicID, *command.Playstate)
	default:
		return ErrBadRequest
	}
}

func (s *SessionCommandService) SendGeneral(ctx context.Context, caller Session, targetPublicID string, command GeneralCommand) error {
	target, identity, generation, err := s.resolveTarget(ctx, caller, targetPublicID)
	if err != nil {
		return err
	}
	envelope, err := generalCommandEnvelope(command, target.Capabilities)
	if err != nil {
		return err
	}
	return s.enqueue(identity, generation, envelope)
}

func (s *SessionCommandService) SendPlay(ctx context.Context, caller Session, targetPublicID string, command PlayCommand) error {
	target, identity, generation, err := s.resolveTarget(ctx, caller, targetPublicID)
	if err != nil {
		return err
	}
	envelope, err := playCommandEnvelope(command, target.Capabilities)
	if err != nil {
		return err
	}
	return s.enqueue(identity, generation, envelope)
}

func (s *SessionCommandService) SendPlaystate(ctx context.Context, caller Session, targetPublicID string, command PlaystateCommand) error {
	target, identity, generation, err := s.resolveTarget(ctx, caller, targetPublicID)
	if err != nil {
		return err
	}
	envelope, err := playstateCommandEnvelope(command, target.Capabilities)
	if err != nil {
		return err
	}
	return s.enqueue(identity, generation, envelope)
}

func (s *SessionCommandService) resolveTarget(ctx context.Context, caller Session, targetPublicID string) (*Session, SessionConnectionIdentity, uint64, error) {
	if s == nil || s.repository == nil || s.hub == nil || caller.GatewayUserID == "" ||
		strings.TrimSpace(caller.GatewayUserID) != caller.GatewayUserID || targetPublicID == "" ||
		strings.TrimSpace(targetPublicID) != targetPublicID {
		return nil, SessionConnectionIdentity{}, 0, ErrBadRequest
	}
	target, err := s.repository.FindActiveSessionByPublicID(ctx, caller.GatewayUserID, targetPublicID, s.now())
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, SessionConnectionIdentity{}, 0, ErrNotFound
		}
		if errors.Is(err, ErrBadRequest) {
			return nil, SessionConnectionIdentity{}, 0, ErrBadRequest
		}
		return nil, SessionConnectionIdentity{}, 0, ErrStoreUnavailable
	}
	if target == nil || target.GatewayTokenHash == "" || target.PublicID != targetPublicID ||
		target.GatewayUserID != caller.GatewayUserID || !target.Active(s.now()) {
		return nil, SessionConnectionIdentity{}, 0, ErrNotFound
	}
	identity := SessionConnectionIdentity{
		TokenHash:     target.GatewayTokenHash,
		PublicID:      target.PublicID,
		GatewayUserID: target.GatewayUserID,
	}
	presence, ok := s.hub.Lookup(identity.TokenHash)
	if !ok || !presenceMatches(presence, identity) {
		return nil, SessionConnectionIdentity{}, 0, ErrNotFound
	}
	return target, identity, presence.Generation, nil
}

func (s *SessionCommandService) enqueue(identity SessionConnectionIdentity, generation uint64, envelope SessionOutboundEnvelope) error {
	result := s.hub.EnqueueGeneration(identity, generation, envelope)
	if errors.Is(result.Err, ErrStoreUnavailable) {
		return ErrStoreUnavailable
	}
	switch result.Status {
	case SessionCommandEnqueued:
		return nil
	case SessionCommandDisconnected:
		return ErrNotFound
	case SessionCommandQueueFull:
		return ErrStoreUnavailable
	default:
		if errors.Is(result.Err, ErrBadRequest) {
			return ErrBadRequest
		}
		return ErrStoreUnavailable
	}
}

func sessionCapabilitiesSupportGeneral(capabilities SessionCapabilities) bool {
	for _, command := range capabilities.SupportedCommands {
		folded := strings.ToLower(command)
		if noArgumentGeneralCommands[folded] != "" || typedGeneralCommands[folded] != "" {
			return true
		}
	}
	return false
}

func sessionCapabilitiesSupportPlaystate(capabilities SessionCapabilities) bool {
	return capabilities.SupportsMediaControl && len(playstateCommands) != 0
}

func sessionCapabilitiesSupportPlay(capabilities SessionCapabilities) bool {
	return capabilities.SupportsMediaControl && len(capabilities.PlayableMediaTypes) > 0 && len(playCommands) != 0
}

func sessionCapabilitiesSupportRemoteControl(capabilities SessionCapabilities) bool {
	return sessionCapabilitiesSupportGeneral(capabilities) ||
		sessionCapabilitiesSupportPlaystate(capabilities) ||
		sessionCapabilitiesSupportPlay(capabilities)
}

func presenceMatches(presence SessionHubPresence, identity SessionConnectionIdentity) bool {
	return presence.Generation != 0 && presence.PublicID == identity.PublicID && presence.GatewayUserID == identity.GatewayUserID
}

func generalCommandEnvelope(command GeneralCommand, capabilities SessionCapabilities) (SessionOutboundEnvelope, error) {
	if !validCommandName(command.Name) {
		return SessionOutboundEnvelope{}, ErrBadRequest
	}
	canonical, noArguments := noArgumentGeneralCommands[strings.ToLower(command.Name)]
	if !noArguments {
		canonical = typedGeneralCommands[strings.ToLower(command.Name)]
		if canonical == "" {
			return SessionOutboundEnvelope{}, ErrForbidden
		}
	}
	if !supportsCommand(capabilities.SupportedCommands, canonical) {
		return SessionOutboundEnvelope{}, ErrForbidden
	}
	arguments, err := generalCommandArguments(canonical, command, noArguments)
	if err != nil {
		return SessionOutboundEnvelope{}, err
	}
	data := struct {
		Name      string            `json:"Name"`
		Arguments map[string]string `json:"Arguments,omitempty"`
	}{Name: canonical, Arguments: arguments}
	return marshalCommandEnvelope("GeneralCommand", data)
}

func generalCommandArguments(name string, command GeneralCommand, noArguments bool) (map[string]string, error) {
	if noArguments {
		if hasGeneralArguments(command) {
			return nil, ErrBadRequest
		}
		return nil, nil
	}
	arguments := make(map[string]string, 3)
	switch name {
	case "SendString":
		if !boundedText(command.Text, 1, sessionCommandMaxTextBytes) || hasGeneralArgumentsExcept(command, "text") {
			return nil, ErrBadRequest
		}
		arguments["String"] = command.Text
	case "SetVolume":
		if command.Volume == nil || *command.Volume < 0 || *command.Volume > 100 || hasGeneralArgumentsExcept(command, "volume") {
			return nil, ErrBadRequest
		}
		arguments["Volume"] = strconv.Itoa(*command.Volume)
	case "SetAudioStreamIndex":
		if command.Index == nil || *command.Index < 0 || hasGeneralArgumentsExcept(command, "index") {
			return nil, ErrBadRequest
		}
		arguments["Index"] = strconv.Itoa(*command.Index)
	case "SetSubtitleStreamIndex":
		if command.Index == nil || *command.Index < -1 || hasGeneralArgumentsExcept(command, "index") {
			return nil, ErrBadRequest
		}
		arguments["Index"] = strconv.Itoa(*command.Index)
	case "DisplayContent":
		if !safeLabel(command.ItemType, 1, sessionCommandMaxLabelBytes) || !safeItemID(command.ItemID) ||
			!boundedText(command.ItemName, 0, sessionCommandMaxLabelBytes) || hasGeneralArgumentsExcept(command, "content") {
			return nil, ErrBadRequest
		}
		arguments["ItemType"] = command.ItemType
		arguments["ItemId"] = command.ItemID
		if command.ItemName != "" {
			arguments["ItemName"] = command.ItemName
		}
	case "DisplayMessage":
		if !boundedText(command.Header, 0, sessionCommandMaxLabelBytes) || !boundedText(command.Text, 1, sessionCommandMaxTextBytes) ||
			(command.TimeoutMS != nil && (*command.TimeoutMS < 0 || *command.TimeoutMS > sessionCommandMaxTimeoutMS)) || hasGeneralArgumentsExcept(command, "message") {
			return nil, ErrBadRequest
		}
		if command.Header != "" {
			arguments["Header"] = command.Header
		}
		arguments["Text"] = command.Text
		if command.TimeoutMS != nil {
			arguments["TimeoutMs"] = strconv.FormatInt(*command.TimeoutMS, 10)
		}
	case "PlayTrailers":
		if !safeItemID(command.ItemID) || hasGeneralArgumentsExcept(command, "item") {
			return nil, ErrBadRequest
		}
		arguments["ItemId"] = command.ItemID
	case "SetPlaybackRate":
		if command.PlaybackRate == nil || !finiteInRange(*command.PlaybackRate, sessionCommandMinPlaybackRate, sessionCommandMaxPlaybackRate) || hasGeneralArgumentsExcept(command, "rate") {
			return nil, ErrBadRequest
		}
		arguments["PlaybackRate"] = strconv.FormatFloat(*command.PlaybackRate, 'f', -1, 64)
	default:
		return nil, ErrForbidden
	}
	return arguments, nil
}

func playCommandEnvelope(command PlayCommand, capabilities SessionCapabilities) (SessionOutboundEnvelope, error) {
	if !validCommandName(command.Command) {
		return SessionOutboundEnvelope{}, ErrBadRequest
	}
	canonical := playCommands[strings.ToLower(command.Command)]
	if canonical == "" {
		return SessionOutboundEnvelope{}, ErrForbidden
	}
	if !capabilities.SupportsMediaControl || len(capabilities.PlayableMediaTypes) == 0 {
		return SessionOutboundEnvelope{}, ErrForbidden
	}
	if len(command.ItemIDs) == 0 || len(command.ItemIDs) > sessionCommandMaxItemIDs {
		return SessionOutboundEnvelope{}, ErrBadRequest
	}
	for _, id := range command.ItemIDs {
		if !safeItemID(id) {
			return SessionOutboundEnvelope{}, ErrBadRequest
		}
	}
	if command.StartPositionTicks != nil && *command.StartPositionTicks < 0 {
		return SessionOutboundEnvelope{}, ErrBadRequest
	}
	if (command.MediaSourceID != "" && !safeItemID(command.MediaSourceID)) ||
		(command.AudioStreamIndex != nil && *command.AudioStreamIndex < 0) ||
		(command.SubtitleStreamIndex != nil && *command.SubtitleStreamIndex < -1) ||
		(command.StartIndex != nil && (*command.StartIndex < 0 || *command.StartIndex >= len(command.ItemIDs))) {
		return SessionOutboundEnvelope{}, ErrBadRequest
	}
	data := struct {
		ItemIDs             []string `json:"ItemIds"`
		PlayCommand         string   `json:"PlayCommand"`
		StartPositionTicks  *int64   `json:"StartPositionTicks,omitempty"`
		MediaSourceID       string   `json:"MediaSourceId,omitempty"`
		AudioStreamIndex    *int     `json:"AudioStreamIndex,omitempty"`
		SubtitleStreamIndex *int     `json:"SubtitleStreamIndex,omitempty"`
		StartIndex          *int     `json:"StartIndex,omitempty"`
	}{command.ItemIDs, canonical, command.StartPositionTicks, command.MediaSourceID, command.AudioStreamIndex, command.SubtitleStreamIndex, command.StartIndex}
	return marshalCommandEnvelope("Play", data)
}

func playstateCommandEnvelope(command PlaystateCommand, capabilities SessionCapabilities) (SessionOutboundEnvelope, error) {
	if !validCommandName(command.Name) {
		return SessionOutboundEnvelope{}, ErrBadRequest
	}
	canonical := playstateCommands[strings.ToLower(command.Name)]
	if canonical == "" {
		return SessionOutboundEnvelope{}, ErrForbidden
	}
	if !capabilities.SupportsMediaControl {
		return SessionOutboundEnvelope{}, ErrForbidden
	}
	switch canonical {
	case "Seek":
		if command.SeekPositionTicks == nil || *command.SeekPositionTicks < 0 {
			return SessionOutboundEnvelope{}, ErrBadRequest
		}
	case "SeekRelative":
		if command.SeekPositionTicks == nil || *command.SeekPositionTicks == 0 {
			return SessionOutboundEnvelope{}, ErrBadRequest
		}
	default:
		if command.SeekPositionTicks != nil {
			return SessionOutboundEnvelope{}, ErrBadRequest
		}
	}
	data := struct {
		Command           string `json:"Command"`
		SeekPositionTicks *int64 `json:"SeekPositionTicks,omitempty"`
	}{canonical, command.SeekPositionTicks}
	return marshalCommandEnvelope("Playstate", data)
}

func marshalCommandEnvelope(messageType string, data any) (SessionOutboundEnvelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return SessionOutboundEnvelope{}, ErrBadRequest
	}
	return SessionOutboundEnvelope{MessageType: messageType, Data: raw}, nil
}

func foldedCommandSet(commands ...string) map[string]string {
	set := make(map[string]string, len(commands))
	for _, command := range commands {
		set[strings.ToLower(command)] = command
	}
	return set
}

func supportsCommand(commands []string, canonical string) bool {
	for _, command := range commands {
		if strings.EqualFold(command, canonical) {
			return true
		}
	}
	return false
}

func validCommandName(value string) bool {
	if !safeLabel(value, 1, maxSupportedCommandLen) {
		return false
	}
	for _, r := range value {
		if r < 'A' || r > 'Z' && r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func boundedText(value string, minBytes, maxBytes int) bool {
	return utf8.ValidString(value) && len(value) >= minBytes && len(value) <= maxBytes
}

func safeLabel(value string, minBytes, maxBytes int) bool {
	return boundedText(value, minBytes, maxBytes) && strings.TrimSpace(value) == value
}

func safeItemID(value string) bool {
	if !safeLabel(value, 1, sessionCommandMaxItemIDBytes) {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f || r == '/' || r == '\\' || r == '?' || r == '#' {
			return false
		}
	}
	return true
}

func finiteInRange(value, min, max float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= min && value <= max
}

func hasGeneralArguments(command GeneralCommand) bool {
	return command.Text != "" || command.Volume != nil || command.Index != nil || command.ItemType != "" ||
		command.ItemID != "" || command.ItemName != "" || command.Header != "" || command.TimeoutMS != nil || command.PlaybackRate != nil
}

func hasGeneralArgumentsExcept(command GeneralCommand, allowed string) bool {
	switch allowed {
	case "text":
		command.Text = ""
	case "volume":
		command.Volume = nil
	case "index":
		command.Index = nil
	case "content":
		command.ItemType, command.ItemID, command.ItemName = "", "", ""
	case "message":
		command.Header, command.Text, command.TimeoutMS = "", "", nil
	case "item":
		command.ItemID = ""
	case "rate":
		command.PlaybackRate = nil
	}
	return hasGeneralArguments(command)
}
