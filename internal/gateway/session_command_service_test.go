package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

type commandSessionRepository struct {
	SessionRepository
	find func(context.Context, string, string, time.Time) (*Session, error)
}

func (r *commandSessionRepository) FindActiveSessionByPublicID(ctx context.Context, gatewayUserID, publicID string, now time.Time) (*Session, error) {
	return r.find(ctx, gatewayUserID, publicID, now)
}

func commandTarget(token, user, publicID string, capabilities SessionCapabilities) *Session {
	return &Session{
		GatewayTokenHash: token,
		GatewayUserID:    user,
		PublicID:         publicID,
		ExpiresAt:        time.Now().Add(time.Hour),
		Capabilities:     capabilities,
	}
}

func commandServiceTarget(t *testing.T, capabilities SessionCapabilities) (*SessionCommandService, SessionHub, SessionConnectionIdentity, SessionHubRegistration) {
	t.Helper()
	target := commandTarget("target-token-hash", "user-1", "session-target", capabilities)
	repository := &commandSessionRepository{find: func(_ context.Context, userID, publicID string, _ time.Time) (*Session, error) {
		if userID != target.GatewayUserID || publicID != target.PublicID {
			return nil, ErrNotFound
		}
		return cloneSession(target), nil
	}}
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity(target.GatewayTokenHash, target.GatewayUserID, target.PublicID)
	registration, _ := registerSession(t, hub, identity)
	return NewSessionCommandService(repository, hub), hub, identity, registration
}

func dequeueCommand(t *testing.T, hub SessionHub, identity SessionConnectionIdentity, registration SessionHubRegistration) SessionOutboundEnvelope {
	t.Helper()
	envelope, ok := hub.Dequeue(identity, registration)
	if !ok {
		t.Fatal("command was not queued")
	}
	if envelope.MessageID != "" {
		t.Fatalf("MessageID = %q, want empty", envelope.MessageID)
	}
	if strings.Contains(string(envelope.Data), identity.TokenHash) {
		t.Fatalf("token hash leaked in Data: %s", envelope.Data)
	}
	return envelope
}

func TestSessionCommandServiceGeneralExactEnvelopes(t *testing.T) {
	volume, audio, subtitle := 35, 2, -1
	timeout := int64(2500)
	rate := 1.25
	tests := []struct {
		name    string
		command GeneralCommand
		want    string
	}{
		{"no arguments canonicalized", GeneralCommand{Name: "gOhOmE"}, `{"Name":"GoHome"}`},
		{"send string", GeneralCommand{Name: "SendString", Text: "hello"}, `{"Name":"SendString","Arguments":{"String":"hello"}}`},
		{"set volume", GeneralCommand{Name: "SetVolume", Volume: &volume}, `{"Name":"SetVolume","Arguments":{"Volume":"35"}}`},
		{"set audio", GeneralCommand{Name: "SetAudioStreamIndex", Index: &audio}, `{"Name":"SetAudioStreamIndex","Arguments":{"Index":"2"}}`},
		{"set subtitle", GeneralCommand{Name: "SetSubtitleStreamIndex", Index: &subtitle}, `{"Name":"SetSubtitleStreamIndex","Arguments":{"Index":"-1"}}`},
		{"display content", GeneralCommand{Name: "DisplayContent", ItemType: "Movie", ItemID: "item-1", ItemName: "Film"}, `{"Name":"DisplayContent","Arguments":{"ItemId":"item-1","ItemName":"Film","ItemType":"Movie"}}`},
		{"display message", GeneralCommand{Name: "DisplayMessage", Header: "Notice", Text: "hello", TimeoutMS: &timeout}, `{"Name":"DisplayMessage","Arguments":{"Header":"Notice","Text":"hello","TimeoutMs":"2500"}}`},
		{"play trailers", GeneralCommand{Name: "PlayTrailers", ItemID: "item-1"}, `{"Name":"PlayTrailers","Arguments":{"ItemId":"item-1"}}`},
		{"playback rate", GeneralCommand{Name: "SetPlaybackRate", PlaybackRate: &rate}, `{"Name":"SetPlaybackRate","Arguments":{"PlaybackRate":"1.25"}}`},
	}
	commands := make([]string, 0, len(tests))
	for _, tt := range tests {
		commands = append(commands, strings.ToUpper(tt.command.Name))
	}
	service, hub, identity, registration := commandServiceTarget(t, SessionCapabilities{SupportedCommands: commands})
	caller := Session{GatewayUserID: "user-1"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := service.SendGeneral(context.Background(), caller, identity.PublicID, tt.command); err != nil {
				t.Fatal(err)
			}
			envelope := dequeueCommand(t, hub, identity, registration)
			if envelope.MessageType != "GeneralCommand" || string(envelope.Data) != tt.want {
				t.Fatalf("envelope = %#v, want Data %s", envelope, tt.want)
			}
		})
	}
}

func TestSessionCommandServicePlayExactEnvelope(t *testing.T) {
	startTicks := int64(50)
	audio, subtitle, startIndex := 1, -1, 1
	service, hub, identity, registration := commandServiceTarget(t, SessionCapabilities{
		SupportsMediaControl: true,
		PlayableMediaTypes:   []string{"Video"},
	})
	err := service.SendPlay(context.Background(), Session{GatewayUserID: "user-1"}, identity.PublicID, PlayCommand{
		Command: "playnext", ItemIDs: []string{"one", "two"}, StartPositionTicks: &startTicks,
		MediaSourceID: "source-1", AudioStreamIndex: &audio, SubtitleStreamIndex: &subtitle, StartIndex: &startIndex,
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope := dequeueCommand(t, hub, identity, registration)
	want := `{"ItemIds":["one","two"],"PlayCommand":"PlayNext","StartPositionTicks":50,"MediaSourceId":"source-1","AudioStreamIndex":1,"SubtitleStreamIndex":-1,"StartIndex":1}`
	if envelope.MessageType != "Play" || string(envelope.Data) != want {
		t.Fatalf("envelope = %#v, want Data %s", envelope, want)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := data["Id"]; ok {
		t.Fatal("target ID appeared in Play Data")
	}
}

func TestSessionCommandServicePlaystateExactEnvelopes(t *testing.T) {
	service, hub, identity, registration := commandServiceTarget(t, SessionCapabilities{SupportsMediaControl: true})
	caller := Session{GatewayUserID: "user-1"}
	seek := int64(100)
	relative := int64(-50)
	tests := []struct {
		command PlaystateCommand
		want    string
	}{
		{PlaystateCommand{Name: "pAuSe"}, `{"Command":"Pause"}`},
		{PlaystateCommand{Name: "Seek", SeekPositionTicks: &seek}, `{"Command":"Seek","SeekPositionTicks":100}`},
		{PlaystateCommand{Name: "SeekRelative", SeekPositionTicks: &relative}, `{"Command":"SeekRelative","SeekPositionTicks":-50}`},
	}
	for _, tt := range tests {
		if err := service.SendPlaystate(context.Background(), caller, identity.PublicID, tt.command); err != nil {
			t.Fatal(err)
		}
		envelope := dequeueCommand(t, hub, identity, registration)
		if envelope.MessageType != "Playstate" || string(envelope.Data) != tt.want {
			t.Fatalf("envelope = %#v, want Data %s", envelope, tt.want)
		}
	}
}

func TestSessionCommandServiceCapabilityAndAllowlistDenials(t *testing.T) {
	volume := 20
	tests := []struct {
		name string
		caps SessionCapabilities
		send func(*SessionCommandService, Session, string) error
	}{
		{"general unsupported", SessionCapabilities{SupportedCommands: []string{"Pause"}}, func(s *SessionCommandService, c Session, id string) error {
			return s.SendGeneral(context.Background(), c, id, GeneralCommand{Name: "GoHome"})
		}},
		{"general unknown", SessionCapabilities{SupportedCommands: []string{"SendKey"}}, func(s *SessionCommandService, c Session, id string) error {
			return s.SendGeneral(context.Background(), c, id, GeneralCommand{Name: "SendKey", Text: "x"})
		}},
		{"play no control", SessionCapabilities{PlayableMediaTypes: []string{"Video"}}, func(s *SessionCommandService, c Session, id string) error {
			return s.SendPlay(context.Background(), c, id, PlayCommand{Command: "PlayNow", ItemIDs: []string{"one"}})
		}},
		{"play no media types", SessionCapabilities{SupportsMediaControl: true}, func(s *SessionCommandService, c Session, id string) error {
			return s.SendPlay(context.Background(), c, id, PlayCommand{Command: "PlayNow", ItemIDs: []string{"one"}})
		}},
		{"play mutation", SessionCapabilities{SupportsMediaControl: true, PlayableMediaTypes: []string{"Video"}}, func(s *SessionCommandService, c Session, id string) error {
			return s.SendPlay(context.Background(), c, id, PlayCommand{Command: "SetShuffleQueue", ItemIDs: []string{"one"}})
		}},
		{"playstate no control", SessionCapabilities{}, func(s *SessionCommandService, c Session, id string) error {
			return s.SendPlaystate(context.Background(), c, id, PlaystateCommand{Name: "Pause"})
		}},
		{"playstate unknown", SessionCapabilities{SupportsMediaControl: true}, func(s *SessionCommandService, c Session, id string) error {
			return s.SendPlaystate(context.Background(), c, id, PlaystateCommand{Name: "SetSubtitleOffset"})
		}},
		{"typed unsupported by target", SessionCapabilities{SupportedCommands: []string{"GoHome"}}, func(s *SessionCommandService, c Session, id string) error {
			return s.SendGeneral(context.Background(), c, id, GeneralCommand{Name: "SetVolume", Volume: &volume})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, _, identity, _ := commandServiceTarget(t, tt.caps)
			if err := tt.send(service, Session{GatewayUserID: "user-1"}, identity.PublicID); !errors.Is(err, ErrForbidden) {
				t.Fatalf("error = %v, want ErrForbidden", err)
			}
		})
	}
}

func TestSessionCommandServiceMalformedAndBounds(t *testing.T) {
	minusTwo, tooLoud := -2, 101
	negative := int64(-1)
	zero := int64(0)
	nan := math.NaN()
	service, _, identity, _ := commandServiceTarget(t, SessionCapabilities{
		SupportedCommands:    []string{"GoHome", "SendString", "SetVolume", "SetSubtitleStreamIndex", "SetPlaybackRate"},
		SupportsMediaControl: true,
		PlayableMediaTypes:   []string{"Video"},
	})
	caller := Session{GatewayUserID: "user-1"}
	tests := []struct {
		name string
		send func() error
	}{
		{"empty command", func() error {
			return service.SendGeneral(context.Background(), caller, identity.PublicID, GeneralCommand{})
		}},
		{"no-arg arguments", func() error {
			return service.SendGeneral(context.Background(), caller, identity.PublicID, GeneralCommand{Name: "GoHome", Text: "x"})
		}},
		{"empty string", func() error {
			return service.SendGeneral(context.Background(), caller, identity.PublicID, GeneralCommand{Name: "SendString"})
		}},
		{"long string", func() error {
			return service.SendGeneral(context.Background(), caller, identity.PublicID, GeneralCommand{Name: "SendString", Text: strings.Repeat("x", sessionCommandMaxTextBytes+1)})
		}},
		{"volume", func() error {
			return service.SendGeneral(context.Background(), caller, identity.PublicID, GeneralCommand{Name: "SetVolume", Volume: &tooLoud})
		}},
		{"subtitle index", func() error {
			return service.SendGeneral(context.Background(), caller, identity.PublicID, GeneralCommand{Name: "SetSubtitleStreamIndex", Index: &minusTwo})
		}},
		{"nonfinite rate", func() error {
			return service.SendGeneral(context.Background(), caller, identity.PublicID, GeneralCommand{Name: "SetPlaybackRate", PlaybackRate: &nan})
		}},
		{"empty items", func() error {
			return service.SendPlay(context.Background(), caller, identity.PublicID, PlayCommand{Command: "PlayNow"})
		}},
		{"too many items", func() error {
			return service.SendPlay(context.Background(), caller, identity.PublicID, PlayCommand{Command: "PlayNow", ItemIDs: make([]string, sessionCommandMaxItemIDs+1)})
		}},
		{"unsafe item", func() error {
			return service.SendPlay(context.Background(), caller, identity.PublicID, PlayCommand{Command: "PlayNow", ItemIDs: []string{"bad/id"}})
		}},
		{"negative start", func() error {
			return service.SendPlay(context.Background(), caller, identity.PublicID, PlayCommand{Command: "PlayNow", ItemIDs: []string{"one"}, StartPositionTicks: &negative})
		}},
		{"missing seek", func() error {
			return service.SendPlaystate(context.Background(), caller, identity.PublicID, PlaystateCommand{Name: "Seek"})
		}},
		{"zero relative seek", func() error {
			return service.SendPlaystate(context.Background(), caller, identity.PublicID, PlaystateCommand{Name: "SeekRelative", SeekPositionTicks: &zero})
		}},
		{"position on pause", func() error {
			return service.SendPlaystate(context.Background(), caller, identity.PublicID, PlaystateCommand{Name: "Pause", SeekPositionTicks: &zero})
		}},
		{"malformed target", func() error {
			badRepository := &commandSessionRepository{find: func(context.Context, string, string, time.Time) (*Session, error) { return nil, ErrBadRequest }}
			badService := NewSessionCommandService(badRepository, NewProcessLocalSessionHub())
			return badService.SendGeneral(context.Background(), caller, "bad", GeneralCommand{Name: "GoHome"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.send(); !errors.Is(err, ErrBadRequest) {
				t.Fatalf("error = %v, want ErrBadRequest", err)
			}
		})
	}
}

func TestSessionCommandServiceTargetAndPresenceErrors(t *testing.T) {
	now := time.Now()
	base := commandTarget("token", "user-1", "public", SessionCapabilities{SupportedCommands: []string{"GoHome"}})
	revoked := now
	tests := []struct {
		name   string
		caller Session
		find   func(context.Context, string, string, time.Time) (*Session, error)
		live   bool
		want   error
	}{
		{"absent", Session{GatewayUserID: "user-1"}, func(context.Context, string, string, time.Time) (*Session, error) { return nil, ErrNotFound }, false, ErrNotFound},
		{"foreign result", Session{GatewayUserID: "user-1"}, func(context.Context, string, string, time.Time) (*Session, error) {
			s := *base
			s.GatewayUserID = "user-2"
			return &s, nil
		}, true, ErrNotFound},
		{"inactive", Session{GatewayUserID: "user-1"}, func(context.Context, string, string, time.Time) (*Session, error) {
			s := *base
			s.RevokedAt = &revoked
			return &s, nil
		}, true, ErrNotFound},
		{"store", Session{GatewayUserID: "user-1"}, func(context.Context, string, string, time.Time) (*Session, error) { return nil, errors.New("db") }, false, ErrStoreUnavailable},
		{"offline", Session{GatewayUserID: "user-1"}, func(context.Context, string, string, time.Time) (*Session, error) { return cloneSession(base), nil }, false, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repository := &commandSessionRepository{find: tt.find}
			hub := NewProcessLocalSessionHub()
			if tt.live {
				registerSession(t, hub, sessionIdentity(base.GatewayTokenHash, base.GatewayUserID, base.PublicID))
			}
			service := NewSessionCommandService(repository, hub)
			if err := service.SendGeneral(context.Background(), tt.caller, base.PublicID, GeneralCommand{Name: "GoHome"}); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

type reconnectingCommandHub struct {
	SessionHub
	identity        SessionConnectionIdentity
	replacement     SessionHubRegistration
	replacementDone bool
	failed          bool
}

type closedCommandHub struct{ SessionHub }

func (h closedCommandHub) Enqueue(SessionConnectionIdentity, SessionOutboundEnvelope) SessionCommandEnqueueResult {
	return SessionCommandEnqueueResult{Status: SessionCommandDisconnected, Err: ErrStoreUnavailable}
}

func (h closedCommandHub) EnqueueGeneration(SessionConnectionIdentity, uint64, SessionOutboundEnvelope) SessionCommandEnqueueResult {
	return SessionCommandEnqueueResult{Status: SessionCommandDisconnected, Err: ErrStoreUnavailable}
}

func (h *reconnectingCommandHub) EnqueueGeneration(identity SessionConnectionIdentity, generation uint64, envelope SessionOutboundEnvelope) SessionCommandEnqueueResult {
	if !h.replacementDone {
		h.replacementDone = true
		registration, err := h.SessionHub.Replace(h.identity, newFakeSessionHubConnection())
		h.replacement = registration
		h.failed = err != nil
	}
	return h.SessionHub.EnqueueGeneration(identity, generation, envelope)
}

func TestSessionCommandServiceRejectsReconnectGeneration(t *testing.T) {
	target := commandTarget("token", "user", "public", SessionCapabilities{SupportedCommands: []string{"GoHome"}})
	repository := &commandSessionRepository{find: func(context.Context, string, string, time.Time) (*Session, error) { return cloneSession(target), nil }}
	baseHub := NewProcessLocalSessionHub()
	identity := sessionIdentity(target.GatewayTokenHash, target.GatewayUserID, target.PublicID)
	registerSession(t, baseHub, identity)
	hub := &reconnectingCommandHub{SessionHub: baseHub, identity: identity}
	service := NewSessionCommandService(repository, hub)
	if err := service.SendGeneral(context.Background(), Session{GatewayUserID: "user"}, "public", GeneralCommand{Name: "GoHome"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if hub.failed {
		t.Fatal("replacement registration failed")
	}
	if _, ok := baseHub.Dequeue(identity, hub.replacement); ok {
		t.Fatal("command was delivered to replacement generation")
	}
}

func TestSessionCommandServiceMapsClosedHubUnavailable(t *testing.T) {
	target := commandTarget("token", "user", "public", SessionCapabilities{SupportedCommands: []string{"GoHome"}})
	repository := &commandSessionRepository{find: func(context.Context, string, string, time.Time) (*Session, error) { return cloneSession(target), nil }}
	baseHub := NewProcessLocalSessionHub()
	identity := sessionIdentity(target.GatewayTokenHash, target.GatewayUserID, target.PublicID)
	registerSession(t, baseHub, identity)
	service := NewSessionCommandService(repository, closedCommandHub{SessionHub: baseHub})
	if err := service.SendGeneral(context.Background(), Session{GatewayUserID: "user"}, "public", GeneralCommand{Name: "GoHome"}); !errors.Is(err, ErrStoreUnavailable) {
		t.Fatalf("error = %v, want ErrStoreUnavailable", err)
	}
}

func TestSessionCommandServiceQueueFullFIFOAndTwoUserIsolation(t *testing.T) {
	t.Run("queue full", func(t *testing.T) {
		service, hub, identity, _ := commandServiceTarget(t, SessionCapabilities{SupportedCommands: []string{"GoHome"}})
		for i := 0; i < sessionOutboundQueueCapacity; i++ {
			if result := hub.Enqueue(identity, commandEnvelope("fill")); result.Status != SessionCommandEnqueued {
				t.Fatalf("fill %d = %#v", i, result)
			}
		}
		if err := service.SendGeneral(context.Background(), Session{GatewayUserID: "user-1"}, identity.PublicID, GeneralCommand{Name: "GoHome"}); !errors.Is(err, ErrStoreUnavailable) {
			t.Fatalf("error = %v, want ErrStoreUnavailable", err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		service, hub, identity, registration := commandServiceTarget(t, SessionCapabilities{SupportedCommands: []string{"GoHome", "VolumeUp"}})
		caller := Session{GatewayUserID: "user-1"}
		for _, name := range []string{"GoHome", "VolumeUp"} {
			if err := service.SendGeneral(context.Background(), caller, identity.PublicID, GeneralCommand{Name: name}); err != nil {
				t.Fatal(err)
			}
		}
		first := dequeueCommand(t, hub, identity, registration)
		second := dequeueCommand(t, hub, identity, registration)
		if string(first.Data) != `{"Name":"GoHome"}` || string(second.Data) != `{"Name":"VolumeUp"}` {
			t.Fatalf("FIFO = %s then %s", first.Data, second.Data)
		}
	})

	t.Run("two users", func(t *testing.T) {
		one := commandTarget("one-token", "one", "shared", SessionCapabilities{SupportedCommands: []string{"GoHome"}})
		two := commandTarget("two-token", "two", "shared", SessionCapabilities{SupportedCommands: []string{"GoHome"}})
		repository := &commandSessionRepository{find: func(_ context.Context, userID, publicID string, _ time.Time) (*Session, error) {
			if publicID != "shared" {
				return nil, ErrNotFound
			}
			switch userID {
			case "one":
				return cloneSession(one), nil
			case "two":
				return cloneSession(two), nil
			default:
				return nil, ErrNotFound
			}
		}}
		hub := NewProcessLocalSessionHub()
		oneID := sessionIdentity(one.GatewayTokenHash, one.GatewayUserID, one.PublicID)
		twoID := sessionIdentity(two.GatewayTokenHash, two.GatewayUserID, two.PublicID)
		oneReg, _ := registerSession(t, hub, oneID)
		twoReg, _ := registerSession(t, hub, twoID)
		service := NewSessionCommandService(repository, hub)
		if err := service.SendGeneral(context.Background(), Session{GatewayUserID: "one"}, "shared", GeneralCommand{Name: "GoHome"}); err != nil {
			t.Fatal(err)
		}
		if _, ok := hub.Dequeue(twoID, twoReg); ok {
			t.Fatal("user two received user one's command")
		}
		if _, ok := hub.Dequeue(oneID, oneReg); !ok {
			t.Fatal("user one did not receive command")
		}
	})
}

func TestSessionCommandServiceClosedUnion(t *testing.T) {
	service, _, identity, _ := commandServiceTarget(t, SessionCapabilities{SupportedCommands: []string{"GoHome"}})
	caller := Session{GatewayUserID: "user-1"}
	if err := service.Send(context.Background(), caller, identity.PublicID, SessionCommandEnvelope{
		Category: SessionCommandGeneral,
		General:  &GeneralCommand{Name: "GoHome"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.Send(context.Background(), caller, identity.PublicID, SessionCommandEnvelope{
		Category: SessionCommandGeneral,
		General:  &GeneralCommand{Name: "GoHome"},
		Play:     &PlayCommand{},
	}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("mixed union error = %v", err)
	}
}
