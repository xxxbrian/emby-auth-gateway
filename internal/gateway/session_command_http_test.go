package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	commandCallerToken = "command-caller-token"
	commandTargetToken = "command-target-token"
	commandCallerID    = "session-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commandTargetID    = "session-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type sessionCommandHTTPHarness struct {
	server       *Server
	httpServer   *httptest.Server
	store        *MemoryStore
	hub          SessionHub
	identity     SessionConnectionIdentity
	registration SessionHubRegistration
	upstream     *countingRoundTripper
}

func newSessionCommandHTTPHarness(t *testing.T, targetCapabilities SessionCapabilities, online bool) *sessionCommandHTTPHarness {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	caller := &Session{
		GatewayTokenHash: HashToken(commandCallerToken), GatewayUserID: "user-1", GatewayUsername: "alice",
		SyntheticUserID: "gateway-user", DeviceID: "caller-device", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		PublicID: commandCallerID, Capabilities: defaultSessionCapabilities(), LastActivityAt: now,
	}
	target := &Session{
		GatewayTokenHash: HashToken(commandTargetToken), GatewayUserID: "user-1", GatewayUsername: "alice",
		SyntheticUserID: "gateway-user", DeviceID: "target-device", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		PublicID: commandTargetID, Capabilities: targetCapabilities, LastActivityAt: now,
	}
	store := NewMemoryStore()
	store.Sessions[caller.GatewayTokenHash] = caller
	store.Sessions[target.GatewayTokenHash] = target
	hub := NewProcessLocalSessionHub()
	identity := sessionIdentity(target.GatewayTokenHash, target.GatewayUserID, target.PublicID)
	var registration SessionHubRegistration
	if online {
		registration, _ = registerSession(t, hub, identity)
	}
	upstream := &countingRoundTripper{}
	server := newServerWithSessionHub(Config{GatewayBasePath: "/emby", HTTPClient: &http.Client{Transport: upstream}}, &noUpstreamLoadStore{MemoryStore: store}, hub)
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		server.CloseWebSockets()
		httpServer.Close()
	})
	return &sessionCommandHTTPHarness{server: server, httpServer: httpServer, store: store, hub: hub, identity: identity, registration: registration, upstream: upstream}
}

func commandCapabilities(t *testing.T, commands []string, media []string, supportsMedia bool) SessionCapabilities {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"SupportedCommands": commands, "PlayableMediaTypes": media, "SupportsMediaControl": supportsMedia, "SupportsSync": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := ParseSessionCapabilities(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return capabilities
}

func (h *sessionCommandHTTPHarness) request(t *testing.T, method, path, contentType string, body io.Reader) *http.Response {
	t.Helper()
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	req := mustRequest(t, method, h.httpServer.URL+"/emby"+path+separator+"api_key="+url.QueryEscape(commandCallerToken), body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return do(t, req)
}

func assertEmptyCommandResponse(t *testing.T, response *http.Response, status int) {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status/cache = %d/%q, want %d/no-store body=%q", response.StatusCode, response.Header.Get("Cache-Control"), status, body)
	}
	if status == http.StatusOK && len(body) != 0 {
		t.Fatalf("accepted body = %q, want empty", body)
	}
}

func TestSessionCommandHTTPGeneralPlayAndPlaystateCompatibility(t *testing.T) {
	caps := commandCapabilities(t,
		[]string{"GOHOME", "SENDSTRING", "SETVOLUME", "DISPLAYMESSAGE"},
		[]string{"Video"}, true,
	)
	h := newSessionCommandHTTPHarness(t, caps, true)

	tests := []struct {
		name        string
		path        string
		contentType string
		body        string
		messageType string
		data        string
	}{
		{"general body", "/Sessions/" + commandTargetID + "/Command", "application/json", `{"Name":"SendString","Arguments":{"String":"hello"}}`, "GeneralCommand", `{"Name":"SendString","Arguments":{"String":"hello"}}`},
		{"general path query", "/Sessions/" + commandTargetID + "/Command/SetVolume?Volume=35", "", "", "GeneralCommand", `{"Name":"SetVolume","Arguments":{"Volume":"35"}}`},
		{"play json", "/Sessions/" + commandTargetID + "/Playing", "application/json", `{"PlayCommand":"PlayNext","ItemIds":["one","two"],"StartPositionTicks":50,"MediaSourceId":"source","AudioStreamIndex":1,"SubtitleStreamIndex":-1,"StartIndex":1,"ControllingUserId":"gateway-user"}`, "Play", `{"ItemIds":["one","two"],"PlayCommand":"PlayNext","StartPositionTicks":50,"MediaSourceId":"source","AudioStreamIndex":1,"SubtitleStreamIndex":-1,"StartIndex":1}`},
		{"playstate query", "/Sessions/" + commandTargetID + "/Playing/Seek?SeekPositionTicks=99&ControllingUserId=gateway-user", "", "", "Playstate", `{"Command":"Seek","SeekPositionTicks":99}`},
		{"playstate body", "/Sessions/" + commandTargetID + "/Playing/SeekRelative", "application/json", `{"SeekPositionTicks":-25}`, "Playstate", `{"Command":"SeekRelative","SeekPositionTicks":-25}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := h.request(t, http.MethodPost, tt.path, tt.contentType, strings.NewReader(tt.body))
			assertEmptyCommandResponse(t, response, http.StatusOK)
			envelope := dequeueCommand(t, h.hub, h.identity, h.registration)
			if envelope.MessageType != tt.messageType || string(envelope.Data) != tt.data || envelope.MessageID != "" {
				t.Fatalf("envelope = %#v, want %s %s", envelope, tt.messageType, tt.data)
			}
			encoded, err := encodeSessionEnvelope(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte(commandTargetID)) || bytes.Contains(encoded, []byte(h.identity.TokenHash)) {
				t.Fatalf("target identity leaked in envelope: %s", encoded)
			}
		})
	}
	if h.upstream.hits != 0 {
		t.Fatalf("upstream hits = %d, want 0", h.upstream.hits)
	}
	if len(h.store.AuditLogs) != len(tests) {
		t.Fatalf("audits = %#v", h.store.AuditLogs)
	}
	for _, audit := range h.store.AuditLogs {
		if audit.Event != "session_command_delivered" || audit.Status != http.StatusOK || strings.Contains(audit.Message, "hello") || strings.Contains(audit.Message, "one") || strings.Contains(audit.ErrorKind, h.identity.TokenHash) {
			t.Fatalf("unsafe delivered audit = %#v", audit)
		}
	}
}

func TestSessionCommandHTTPAllPlaystateCommands(t *testing.T) {
	h := newSessionCommandHTTPHarness(t, commandCapabilities(t, nil, nil, true), true)
	seek := map[string]string{"Seek": "100", "SeekRelative": "-50"}
	commands := []string{"Stop", "Pause", "Unpause", "PlayPause", "NextTrack", "PreviousTrack", "Seek", "Rewind", "FastForward", "SeekRelative"}
	for _, command := range commands {
		path := "/Sessions/" + commandTargetID + "/Playing/" + command
		if value := seek[command]; value != "" {
			path += "?SeekPositionTicks=" + url.QueryEscape(value)
		}
		response := h.request(t, http.MethodPost, path, "", nil)
		assertEmptyCommandResponse(t, response, http.StatusOK)
		envelope := dequeueCommand(t, h.hub, h.identity, h.registration)
		if envelope.MessageType != "Playstate" || !strings.Contains(string(envelope.Data), `"Command":"`+command+`"`) {
			t.Fatalf("%s envelope = %#v", command, envelope)
		}
	}
}

func TestSessionCommandHTTPPlayCommandsBoundsAndCapabilities(t *testing.T) {
	t.Run("all play commands", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, commandCapabilities(t, nil, []string{"Audio", "Video"}, true), true)
		for _, command := range []string{"PlayNow", "PlayNext", "PlayLast"} {
			body := `{"PlayCommand":"` + command + `","ItemIds":["item-1"]}`
			response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Playing", "application/json", strings.NewReader(body))
			assertEmptyCommandResponse(t, response, http.StatusOK)
			envelope := dequeueCommand(t, h.hub, h.identity, h.registration)
			if envelope.MessageType != "Play" || !strings.Contains(string(envelope.Data), `"PlayCommand":"`+command+`"`) {
				t.Fatalf("%s envelope = %#v", command, envelope)
			}
		}
	})

	t.Run("form compatibility", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, commandCapabilities(t, nil, []string{"Video"}, true), true)
		form := url.Values{"PlayCommand": {"PlayNow"}, "ItemIds": {"one,two"}, "StartPositionTicks": {"10"}}
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Playing", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
		assertEmptyCommandResponse(t, response, http.StatusOK)
		envelope := dequeueCommand(t, h.hub, h.identity, h.registration)
		if string(envelope.Data) != `{"ItemIds":["one","two"],"PlayCommand":"PlayNow","StartPositionTicks":10}` {
			t.Fatalf("form envelope = %#v", envelope)
		}
	})

	t.Run("item bounds", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, commandCapabilities(t, nil, []string{"Video"}, true), true)
		items := make([]string, sessionCommandMaxItemIDs+1)
		for i := range items {
			items[i] = "item"
		}
		body, _ := json.Marshal(map[string]any{"PlayCommand": "PlayNow", "ItemIds": items})
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Playing", "application/json", bytes.NewReader(body))
		assertEmptyCommandResponse(t, response, http.StatusBadRequest)
	})

	t.Run("media capability", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, commandCapabilities(t, nil, nil, true), true)
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Playing", "application/json", strings.NewReader(`{"PlayCommand":"PlayNow","ItemIds":["item-1"]}`))
		assertEmptyCommandResponse(t, response, http.StatusForbidden)
	})
}

func TestSessionCommandHTTPStatusAuditAndZeroEgressMatrix(t *testing.T) {
	caps := commandCapabilities(t, []string{"GoHome", "SetVolume"}, []string{"Video"}, true)
	tests := []struct {
		name        string
		online      bool
		path        string
		contentType string
		body        string
		status      int
		event       string
	}{
		{"malformed trailing", true, "/Sessions/" + commandTargetID + "/Command", "application/json", `{"Name":"GoHome"}{}`, 400, "session_command_denied"},
		{"duplicate field", true, "/Sessions/" + commandTargetID + "/Command", "application/json", `{"Name":"GoHome","name":"GoHome"}`, 400, "session_command_denied"},
		{"duplicate target identity", true, "/Sessions/" + commandTargetID + "/Command", "application/json", `{"Name":"GoHome","Id":"` + commandTargetID + `"}`, 400, "session_command_denied"},
		{"arbitrary argument", true, "/Sessions/" + commandTargetID + "/Command", "application/json", `{"Name":"GoHome","Arguments":{"Arbitrary":"x"}}`, 400, "session_command_denied"},
		{"unsupported capability", true, "/Sessions/" + commandTargetID + "/Command/SendString?String=x", "", "", 403, "session_command_denied"},
		{"absent", true, "/Sessions/session-cccccccccccccccccccccccccccccccc/Command/GoHome", "", "", 404, "session_command_denied"},
		{"offline", false, "/Sessions/" + commandTargetID + "/Command/GoHome", "", "", 404, "session_command_denied"},
		{"wrong method", true, "/Sessions/" + commandTargetID + "/Command/GoHome", "", "", 405, "session_method_not_allowed"},
		{"system denied", true, "/Sessions/" + commandTargetID + "/System/DisplayContent", "", "", 403, "session_access_denied"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSessionCommandHTTPHarness(t, caps, tt.online)
			method := http.MethodPost
			if tt.name == "wrong method" {
				method = http.MethodGet
			}
			response := h.request(t, method, tt.path, tt.contentType, strings.NewReader(tt.body))
			assertEmptyCommandResponse(t, response, tt.status)
			if h.upstream.hits != 0 {
				t.Fatalf("upstream hits = %d", h.upstream.hits)
			}
			if !hasAuditEvent(h.store, tt.event) {
				t.Fatalf("missing %s audit in %#v", tt.event, h.store.AuditLogs)
			}
			if tt.event == "session_command_denied" {
				audit := h.store.AuditLogs[len(h.store.AuditLogs)-1]
				if strings.Contains(audit.Message, commandTargetID) || strings.Contains(audit.Message, "Arbitrary") || strings.Contains(audit.ErrorKind, commandTargetID) {
					t.Fatalf("denial audit leaked request details: %#v", audit)
				}
			}
		})
	}
}

func TestSessionCommandHTTPBodyLimitAndQueueSaturation(t *testing.T) {
	caps := commandCapabilities(t, []string{"GoHome"}, nil, false)
	t.Run("body limit", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, caps, true)
		body := `{"Name":"GoHome","Padding":"` + strings.Repeat("x", sessionCommandHTTPBodyLimit) + `"}`
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Command", "application/json", strings.NewReader(body))
		assertEmptyCommandResponse(t, response, http.StatusBadRequest)
	})
	t.Run("queue full", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, caps, true)
		for i := 0; i < sessionOutboundQueueCapacity; i++ {
			if result := h.hub.Enqueue(h.identity, commandEnvelope("fill")); result.Status != SessionCommandEnqueued {
				t.Fatalf("fill %d = %#v", i, result)
			}
		}
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Command/GoHome", "", nil)
		assertEmptyCommandResponse(t, response, http.StatusServiceUnavailable)
		if audit := h.store.AuditLogs[len(h.store.AuditLogs)-1]; audit.Event != "session_command_denied" || audit.ErrorKind != "general_unavailable" {
			t.Fatalf("queue audit = %#v", audit)
		}
	})
}

func TestSessionCommandHTTPTargetLifecycleAndCallerActivity(t *testing.T) {
	caps := commandCapabilities(t, []string{"GoHome"}, nil, false)

	t.Run("foreign", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, caps, true)
		h.store.mu.Lock()
		h.store.Sessions[h.identity.TokenHash].GatewayUserID = "user-2"
		h.store.mu.Unlock()
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Command/GoHome", "", nil)
		assertEmptyCommandResponse(t, response, http.StatusNotFound)
	})

	t.Run("inactive", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, caps, true)
		revoked := time.Now().UTC()
		h.store.mu.Lock()
		h.store.Sessions[h.identity.TokenHash].RevokedAt = &revoked
		h.store.mu.Unlock()
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Command/GoHome", "", nil)
		assertEmptyCommandResponse(t, response, http.StatusNotFound)
	})

	t.Run("reconnect generation", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, caps, true)
		reconnecting := &reconnectingCommandHub{SessionHub: h.hub, identity: h.identity}
		h.server.sessionHub = reconnecting
		h.server.sessionCommands = NewSessionCommandService(h.store, reconnecting)
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Command/GoHome", "", nil)
		assertEmptyCommandResponse(t, response, http.StatusNotFound)
	})

	t.Run("caller only activity", func(t *testing.T) {
		h := newSessionCommandHTTPHarness(t, caps, true)
		h.store.mu.RLock()
		callerBefore := h.store.Sessions[HashToken(commandCallerToken)].LastActivityAt
		targetBefore := h.store.Sessions[HashToken(commandTargetToken)].LastActivityAt
		h.store.mu.RUnlock()
		response := h.request(t, http.MethodPost, "/Sessions/"+commandTargetID+"/Command/GoHome", "", nil)
		assertEmptyCommandResponse(t, response, http.StatusOK)
		h.store.mu.RLock()
		callerAfter := h.store.Sessions[HashToken(commandCallerToken)].LastActivityAt
		targetAfter := h.store.Sessions[HashToken(commandTargetToken)].LastActivityAt
		h.store.mu.RUnlock()
		if !callerAfter.After(callerBefore) || !targetAfter.Equal(targetBefore) {
			t.Fatalf("activity caller %s -> %s, target %s -> %s", callerBefore, callerAfter, targetBefore, targetAfter)
		}
	})
}

func TestSessionCommandHTTPRealWebSocketExactFIFO(t *testing.T) {
	server, httpServer, store, upstream, _ := newWebSocketTransportServer(t, false)
	setWebSocketTestCapabilities(t, store, HashToken(websocketTestToken), []string{"GoHome", "SendString"}, []string{"Video"}, true)
	conn, _ := dialTestWebSocket(t, httpServer, nil)
	waitForWebSocketPresence(t, server.sessionHub)

	requests := []struct {
		path string
		body string
	}{
		{"/Sessions/" + websocketTestPublicID + "/Command/GoHome", ""},
		{"/Sessions/" + websocketTestPublicID + "/Command", `{"Name":"SendString","Arguments":{"String":"hello"}}`},
		{"/Sessions/" + websocketTestPublicID + "/Playing", `{"PlayCommand":"PlayNow","ItemIds":["item-1"]}`},
	}
	for _, request := range requests {
		httpRequest := mustRequest(t, http.MethodPost, httpServer.URL+"/emby"+request.path+"?api_key="+websocketTestToken, strings.NewReader(request.body))
		if request.body != "" {
			httpRequest.Header.Set("Content-Type", "application/json")
		}
		response := do(t, httpRequest)
		assertEmptyCommandResponse(t, response, http.StatusOK)
	}

	wants := []struct{ messageType, data string }{
		{"GeneralCommand", `{"Name":"GoHome"}`},
		{"GeneralCommand", `{"Name":"SendString","Arguments":{"String":"hello"}}`},
		{"Play", `{"ItemIds":["item-1"],"PlayCommand":"PlayNow"}`},
	}
	for _, want := range wants {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		messageType, payload, err := conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if messageType != websocket.MessageText {
			t.Fatalf("websocket type = %v", messageType)
		}
		envelope, err := decodeSessionEnvelope(payload)
		if err != nil {
			t.Fatal(err)
		}
		if envelope.MessageType != want.messageType || string(envelope.Data) != want.data || envelope.MessageID != "" {
			t.Fatalf("envelope = %#v, want %s %s", envelope, want.messageType, want.data)
		}
		if bytes.Contains(payload, []byte(websocketTestPublicID)) || bytes.Contains(payload, []byte(HashToken(websocketTestToken))) {
			t.Fatalf("identity leaked in websocket payload: %s", payload)
		}
	}
	if upstream.hits != 0 {
		t.Fatalf("upstream hits = %d", upstream.hits)
	}
}

func TestSessionCommandProjectionCaseInsensitiveCapabilityAttachDetach(t *testing.T) {
	server, httpServer, store, _, _ := newWebSocketTransportServer(t, false)
	setWebSocketTestCapabilities(t, store, HashToken(websocketTestToken), []string{"gOhOmE"}, nil, false)
	assertProjection := func(want bool) {
		response := do(t, mustRequest(t, http.MethodGet, httpServer.URL+"/emby/Sessions?api_key="+websocketTestToken, nil))
		var sessions []map[string]any
		decodeJSON(t, response.Body, &sessions)
		_ = response.Body.Close()
		got, _ := sessionByID(t, sessions, websocketTestPublicID)["SupportsRemoteControl"].(bool)
		if got != want {
			t.Fatalf("SupportsRemoteControl = %v, want %v", got, want)
		}
	}
	assertProjection(false)
	conn, _ := dialTestWebSocket(t, httpServer, nil)
	waitForWebSocketPresence(t, server.sessionHub)
	assertProjection(true)
	writeWebSocketEnvelope(t, conn, "SessionsStart", "0,60000")
	initial := sessionByID(t, readSessionsEnvelope(t, conn), websocketTestPublicID)
	if supports, _ := initial["SupportsRemoteControl"].(bool); !supports {
		t.Fatalf("subscription initial SupportsRemoteControl = %#v", initial["SupportsRemoteControl"])
	}

	updateCapabilities := func(commands []string, want bool) {
		body, _ := json.Marshal(map[string]any{"SupportedCommands": commands})
		req := mustRequest(t, http.MethodPost, httpServer.URL+"/emby/Sessions/Capabilities/Full?api_key="+websocketTestToken, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		response := do(t, req)
		assertEmptyCommandResponse(t, response, http.StatusOK)
		projected := sessionByID(t, readSessionsEnvelope(t, conn), websocketTestPublicID)
		got, _ := projected["SupportsRemoteControl"].(bool)
		if got != want {
			t.Fatalf("subscription SupportsRemoteControl = %v, want %v", got, want)
		}
		assertProjection(want)
	}
	updateCapabilities([]string{"FutureCommand"}, false)
	updateCapabilities([]string{"gOhOmE"}, true)
	if err := conn.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.sessionHub.(*observingSessionHub).unregistered:
	case <-time.After(2 * time.Second):
		t.Fatal("websocket did not detach")
	}
	assertProjection(false)
}
