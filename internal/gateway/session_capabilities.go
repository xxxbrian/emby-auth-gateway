package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessioncaps"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"
)

const (
	defaultCapabilitiesJSON      = sessioncaps.DefaultJSON
	sessionCapabilitiesMaxBytes  = sessioncaps.MaxBytes
	sessionActivityTouchInterval = 30 * time.Second

	maxPlayableMediaTypes   = sessioncaps.MaxPlayableMediaTypes
	maxPlayableMediaTypeLen = sessioncaps.MaxPlayableMediaTypeLen
	maxSupportedCommands    = sessioncaps.MaxSupportedCommands
	maxSupportedCommandLen  = sessioncaps.MaxSupportedCommandLen
)

// ParseSessionCapabilities is the bounded canonical runtime validator for session
// capability documents. It delegates to internal/sessioncaps so migration and
// runtime share identical acceptance rules. Empty input defaults to {}; explicit
// null is rejected. Output RawJSON is compact, deterministic, and idempotent.
func ParseSessionCapabilities(raw string) (SessionCapabilities, error) {
	doc, err := sessioncaps.Parse(raw)
	if err != nil {
		return SessionCapabilities{}, err
	}
	return SessionCapabilities{
		RawJSON:              doc.RawJSON,
		PlayableMediaTypes:   doc.PlayableMediaTypes,
		SupportedCommands:    doc.SupportedCommands,
		SupportsMediaControl: doc.SupportsMediaControl,
		SupportsSync:         doc.SupportsSync,
	}, nil
}

func defaultSessionCapabilities() SessionCapabilities {
	caps, err := ParseSessionCapabilities(defaultCapabilitiesJSON)
	if err != nil {
		// "{}" is always valid JSON.
		return SessionCapabilities{RawJSON: defaultCapabilitiesJSON}
	}
	return caps
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneSession(s *Session) *Session {
	if s == nil {
		return nil
	}
	copySession := *s
	copySession.Capabilities = SessionCapabilities{
		RawJSON:              s.Capabilities.RawJSON,
		PlayableMediaTypes:   cloneStrings(s.Capabilities.PlayableMediaTypes),
		SupportedCommands:    cloneStrings(s.Capabilities.SupportedCommands),
		SupportsMediaControl: s.Capabilities.SupportsMediaControl,
		SupportsSync:         s.Capabilities.SupportsSync,
	}
	if s.RevokedAt != nil {
		t := *s.RevokedAt
		copySession.RevokedAt = &t
	}
	return &copySession
}

// repairSessionAggregate hydrates or repairs a session aggregate in place.
//
// PublicID == "" means a genuinely missing profile (rollback-forward hole):
// synthesize PublicID, default empty capabilities to {}, and fill zero activity
// from CreatedAt/now. When PublicID is present the profile row is treated as
// persisted: invalid PublicID, empty/invalid capabilities, or zero activity are
// operational integrity errors (parity with pbstore sessionFromRecords).
func repairSessionAggregate(s *Session, now time.Time) error {
	if s == nil {
		return fmt.Errorf("nil session")
	}
	if s.PublicID == "" {
		id, err := sessionid.New()
		if err != nil {
			return err
		}
		s.PublicID = id
		raw := s.Capabilities.RawJSON
		if raw == "" {
			raw = defaultCapabilitiesJSON
		}
		caps, err := ParseSessionCapabilities(raw)
		if err != nil {
			return err
		}
		s.Capabilities = caps
		if s.LastActivityAt.IsZero() {
			if !s.CreatedAt.IsZero() {
				s.LastActivityAt = s.CreatedAt
			} else {
				s.LastActivityAt = now
			}
		}
		return nil
	}

	if !sessionid.Valid(s.PublicID) {
		return fmt.Errorf("invalid public session id %q", s.PublicID)
	}
	if s.Capabilities.RawJSON == "" {
		return fmt.Errorf("session profile integrity: empty capabilities_json")
	}
	caps, err := ParseSessionCapabilities(s.Capabilities.RawJSON)
	if err != nil {
		return fmt.Errorf("session profile integrity: capabilities_json: %w", err)
	}
	s.Capabilities = caps
	if s.LastActivityAt.IsZero() {
		return fmt.Errorf("session profile integrity: missing or zero last_activity_at")
	}
	return nil
}

func prepareNewSession(session Session, now time.Time) (Session, error) {
	if session.GatewayTokenHash == "" {
		return Session{}, fmt.Errorf("gateway token hash is required")
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = now
	}
	if session.PublicID == "" {
		id, err := sessionid.New()
		if err != nil {
			return Session{}, err
		}
		session.PublicID = id
	} else if !sessionid.Valid(session.PublicID) {
		return Session{}, fmt.Errorf("invalid public session id %q", session.PublicID)
	}
	raw := session.Capabilities.RawJSON
	if raw == "" {
		raw = defaultCapabilitiesJSON
	}
	caps, err := ParseSessionCapabilities(raw)
	if err != nil {
		return Session{}, err
	}
	session.Capabilities = caps
	if session.LastActivityAt.IsZero() {
		session.LastActivityAt = session.CreatedAt
	}
	return session, nil
}

// --- HTTP capability handlers and activity touch ---

func (s *Server) touchSessionActivityBestEffort(ctx context.Context, session *Session, r *http.Request) {
	if s == nil || session == nil || s.sessions == nil {
		return
	}
	now := time.Now().UTC()
	_, err := s.sessions.TouchSessionActivity(ctx, session.GatewayTokenHash, now, sessionActivityTouchInterval)
	if err == nil || errors.Is(err, ErrNotFound) {
		return
	}
	s.audit(ctx, AuditLog{
		GatewayUserID:   session.GatewayUserID,
		SyntheticUserID: session.SyntheticUserID,
		Event:           "session_activity_touch_failed",
		Message:         "session activity touch failed",
		RemoteIP:        remoteIP(r),
		Method:          r.Method,
		Path:            r.URL.Path,
		Status:          http.StatusInternalServerError,
		ErrorKind:       "session_activity_touch",
	})
	s.emit(observe.Event{
		Kind:       observe.KindReliability,
		Outcome:    observe.OutcomeError,
		RouteClass: observe.RouteOther,
		ErrorKind:  "session_activity_touch",
		UserID:     session.GatewayUserID,
		Username:   session.GatewayUsername,
		SessionID:  session.GatewayTokenHash,
		Device:     session.Device,
	})
}

func (s *Server) handleSessionCapabilitiesSlim(w http.ResponseWriter, r *http.Request, session *Session) {
	// Query-only: form body must not supply or override capability fields.
	query := r.URL.Query()
	if err := validateOptionalSessionID(query.Get("Id"), "", session.PublicID); err != nil {
		writeSessionIDError(w, err)
		return
	}

	media, err := parseCapabilityStringList(query["PlayableMediaTypes"], maxPlayableMediaTypes, maxPlayableMediaTypeLen)
	if err != nil {
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}
	commands, err := parseCapabilityStringList(query["SupportedCommands"], maxSupportedCommands, maxSupportedCommandLen)
	if err != nil {
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}
	supportsMedia, err := parseOptionalBoolForm(query, "SupportsMediaControl")
	if err != nil {
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}
	supportsSync, err := parseOptionalBoolForm(query, "SupportsSync")
	if err != nil {
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}

	merged, err := mergeSessionCapabilities(session.Capabilities, capabilityPatch{
		PlayableMediaTypes:   media,
		SupportedCommands:    commands,
		SupportsMediaControl: supportsMedia,
		SupportsSync:         supportsSync,
		SetMedia:             query["PlayableMediaTypes"] != nil,
		SetCommands:          query["SupportedCommands"] != nil,
		SetMediaControl:      query["SupportsMediaControl"] != nil,
		SetSync:              query["SupportsSync"] != nil,
	})
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeEmptySessionCapabilitiesError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}

	now := time.Now().UTC()
	if _, err := s.sessions.UpdateSessionCapabilities(r.Context(), session.GatewayTokenHash, merged, now); err != nil {
		s.audit(r.Context(), AuditLog{
			GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID,
			Event: "session_capabilities_save_failed", Message: "session capabilities save failed",
			RemoteIP: remoteIP(r), Method: r.Method, Path: r.URL.Path, Status: http.StatusInternalServerError,
		})
		writeEmptySessionCapabilitiesError(w, http.StatusInternalServerError, "capabilities unavailable")
		return
	}
	writeEmptySessionCapabilitiesOK(w)
}

func (s *Server) handleSessionCapabilitiesFull(w http.ResponseWriter, r *http.Request, session *Session) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sessionCapabilitiesMaxBytes+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) || errors.Is(err, io.ErrUnexpectedEOF) {
			writeEmptySessionCapabilitiesError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		// MaxBytesReader returns error containing "http: request body too large"
		if strings.Contains(err.Error(), "request body too large") {
			writeEmptySessionCapabilitiesError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}
	if len(body) > sessionCapabilitiesMaxBytes {
		writeEmptySessionCapabilitiesError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}

	fields, err := sessioncaps.DecodeObjectFields(string(body))
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeEmptySessionCapabilitiesError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}

	queryID := r.URL.Query().Get("Id")
	bodyID := ""
	if rawID, ok := fields["Id"]; ok {
		trimmed := bytes.TrimSpace(rawID)
		if string(trimmed) != "null" {
			var id string
			if err := json.Unmarshal(trimmed, &id); err != nil {
				writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
				return
			}
			bodyID = id
		}
	}
	if err := validateOptionalSessionID(queryID, bodyID, session.PublicID); err != nil {
		writeSessionIDError(w, err)
		return
	}
	// Id is not part of stored ClientCapabilities document.
	delete(fields, "Id")

	// Full replace: missing known fields default to [] / false in the stored document.
	if _, ok := fields["PlayableMediaTypes"]; !ok {
		fields["PlayableMediaTypes"] = json.RawMessage("[]")
	}
	if _, ok := fields["SupportedCommands"]; !ok {
		fields["SupportedCommands"] = json.RawMessage("[]")
	}
	if _, ok := fields["SupportsMediaControl"]; !ok {
		fields["SupportsMediaControl"] = json.RawMessage("false")
	}
	if _, ok := fields["SupportsSync"]; !ok {
		fields["SupportsSync"] = json.RawMessage("false")
	}

	encoded, err := sessioncaps.MarshalObjectFields(fields)
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeEmptySessionCapabilitiesError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}

	caps, err := ParseSessionCapabilities(encoded)
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeEmptySessionCapabilitiesError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
		return
	}
	now := time.Now().UTC()
	if _, err := s.sessions.UpdateSessionCapabilities(r.Context(), session.GatewayTokenHash, caps, now); err != nil {
		s.audit(r.Context(), AuditLog{
			GatewayUserID: session.GatewayUserID, SyntheticUserID: session.SyntheticUserID,
			Event: "session_capabilities_save_failed", Message: "session capabilities save failed",
			RemoteIP: remoteIP(r), Method: r.Method, Path: r.URL.Path, Status: http.StatusInternalServerError,
		})
		writeEmptySessionCapabilitiesError(w, http.StatusInternalServerError, "capabilities unavailable")
		return
	}
	writeEmptySessionCapabilitiesOK(w)
}

type capabilityPatch struct {
	PlayableMediaTypes   []string
	SupportedCommands    []string
	SupportsMediaControl bool
	SupportsSync         bool
	SetMedia             bool
	SetCommands          bool
	SetMediaControl      bool
	SetSync              bool
}

func mergeSessionCapabilities(existing SessionCapabilities, patch capabilityPatch) (SessionCapabilities, error) {
	rawJSON := existing.RawJSON
	if rawJSON == "" {
		rawJSON = defaultCapabilitiesJSON
	}
	fields, err := sessioncaps.DecodeObjectFields(rawJSON)
	if err != nil {
		// Start fresh if stored JSON is corrupt; Parse will still validate.
		fields = map[string]json.RawMessage{}
	}
	// Preserve unknown fields and DeviceProfile; update known fields when set.
	media := existing.PlayableMediaTypes
	if media == nil {
		media = []string{}
	}
	commands := existing.SupportedCommands
	if commands == nil {
		commands = []string{}
	}
	supportsMedia := existing.SupportsMediaControl
	supportsSync := existing.SupportsSync
	if patch.SetMedia {
		media = patch.PlayableMediaTypes
	}
	if patch.SetCommands {
		commands = patch.SupportedCommands
	}
	if patch.SetMediaControl {
		supportsMedia = patch.SupportsMediaControl
	}
	if patch.SetSync {
		supportsSync = patch.SupportsSync
	}
	mediaRaw, err := json.Marshal(media)
	if err != nil {
		return SessionCapabilities{}, err
	}
	cmdRaw, err := json.Marshal(commands)
	if err != nil {
		return SessionCapabilities{}, err
	}
	mediaControlRaw, err := json.Marshal(supportsMedia)
	if err != nil {
		return SessionCapabilities{}, err
	}
	syncRaw, err := json.Marshal(supportsSync)
	if err != nil {
		return SessionCapabilities{}, err
	}
	fields["PlayableMediaTypes"] = mediaRaw
	fields["SupportedCommands"] = cmdRaw
	fields["SupportsMediaControl"] = mediaControlRaw
	fields["SupportsSync"] = syncRaw
	encoded, err := sessioncaps.MarshalObjectFields(fields)
	if err != nil {
		return SessionCapabilities{}, err
	}
	return ParseSessionCapabilities(encoded)
}

func parseCapabilityStringList(values []string, maxItems, maxLen int) ([]string, error) {
	var parts []string
	for _, v := range values {
		if strings.Contains(v, ",") {
			for _, p := range strings.Split(v, ",") {
				parts = append(parts, p)
			}
		} else {
			parts = append(parts, v)
		}
	}
	return sessioncaps.NormalizeStringList(parts, maxItems, maxLen)
}

func parseOptionalBoolForm(form url.Values, key string) (bool, error) {
	if form[key] == nil {
		return false, nil
	}
	v := strings.TrimSpace(form.Get(key))
	if v == "" {
		return false, nil
	}
	switch strings.ToLower(v) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("invalid bool")
	}
}

var (
	errSessionIDMalformed = errors.New("session id malformed")
	errSessionIDConflict  = errors.New("session id conflict")
	errSessionIDForeign   = errors.New("session id foreign")
)

func validateOptionalSessionID(queryID, bodyID, currentPublicID string) error {
	queryID = strings.TrimSpace(queryID)
	bodyID = strings.TrimSpace(bodyID)
	if queryID != "" && bodyID != "" && queryID != bodyID {
		return errSessionIDConflict
	}
	id := queryID
	if id == "" {
		id = bodyID
	}
	if id == "" {
		return nil
	}
	if !sessionid.Valid(id) {
		return errSessionIDMalformed
	}
	if id != currentPublicID {
		return errSessionIDForeign
	}
	return nil
}

func writeSessionIDError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSessionIDForeign):
		writeEmptySessionCapabilitiesError(w, http.StatusNotFound, "not found")
	case errors.Is(err, errSessionIDConflict), errors.Is(err, errSessionIDMalformed):
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
	default:
		writeEmptySessionCapabilitiesError(w, http.StatusBadRequest, "bad request")
	}
}

func writeEmptySessionCapabilitiesOK(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

func writeEmptySessionCapabilitiesError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, msg, status)
}
