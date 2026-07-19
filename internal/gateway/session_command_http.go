package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
	"github.com/xxxbrian/emby-auth-gateway/internal/sessionid"
)

const sessionCommandHTTPBodyLimit = 64 << 10

type sessionCommandHTTPInput struct {
	category SessionCommandCategory
	name     string
	command  SessionCommandEnvelope
}

type sessionCommandField struct {
	name string
	raw  json.RawMessage
}

type sessionCommandFields map[string]sessionCommandField

func isSessionCommandOperation(operation routeclass.Operation) bool {
	switch operation {
	case routeclass.OperationSessionGeneralCommand, routeclass.OperationSessionPlay, routeclass.OperationSessionPlaystate:
		return true
	default:
		return false
	}
}

func (s *Server) handleSessionCommandRequest(w http.ResponseWriter, r *http.Request, rel string, caller *Session, decision routeclass.Decision) {
	w.Header().Set("Cache-Control", "no-store")
	targetPublicID, pathCommand, err := sessionCommandPath(rel, decision.Operation)
	if err != nil {
		s.writeSessionCommandResult(w, r, rel, caller, decision, sessionCommandHTTPInput{category: commandCategoryForOperation(decision.Operation)}, err)
		return
	}
	input, err := parseSessionCommandHTTPRequest(w, r, caller, decision.Operation, pathCommand)
	if err == nil {
		if s.sessionCommands == nil {
			err = ErrStoreUnavailable
		} else {
			err = s.sessionCommands.Send(r.Context(), *caller, targetPublicID, input.command)
		}
	}
	if err != nil {
		s.writeSessionCommandResult(w, r, rel, caller, decision, input, err)
		return
	}

	s.auditSessionCommand(r, rel, caller, input, "session_command_delivered", "accepted", http.StatusOK)
	s.noteSession(caller, decision)
	s.touchSessionActivityBestEffort(r.Context(), caller, r)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) writeSessionCommandResult(w http.ResponseWriter, r *http.Request, rel string, caller *Session, decision routeclass.Decision, input sessionCommandHTTPInput, err error) {
	status, reason := sessionCommandHTTPError(err)
	s.auditSessionCommand(r, rel, caller, input, "session_command_denied", reason, status)
	if status >= 500 {
		s.emit(observe.Event{
			Kind:        observe.KindRequest,
			Outcome:     observe.OutcomeError,
			StatusClass: observe.Status5xx,
			RouteClass:  observe.RouteClassOf(decision),
			Method:      r.Method,
			ErrorKind:   "session_command_" + reason,
			UserID:      caller.GatewayUserID,
			Username:    caller.GatewayUsername,
			SessionID:   caller.GatewayTokenHash,
			Device:      caller.Device,
		})
	} else {
		s.emitSessionDeniedRequest(caller, r.Method, decision, status)
	}
	http.Error(w, http.StatusText(status), status)
}

func (s *Server) auditSessionCommand(r *http.Request, rel string, caller *Session, input sessionCommandHTTPInput, event, reason string, status int) {
	category := sessionCommandCategoryName(input.category)
	errorKind := category + "_" + reason
	if event == "session_command_delivered" && input.name != "" {
		errorKind = category + "_" + strings.ToLower(input.name)
	}
	s.audit(r.Context(), AuditLog{
		GatewayUserID:   caller.GatewayUserID,
		SyntheticUserID: caller.SyntheticUserID,
		Event:           event,
		Message:         category + " session command " + reason,
		RemoteIP:        remoteIP(r),
		Method:          r.Method,
		Path:            rel,
		Status:          status,
		ErrorKind:       errorKind,
	})
}

func sessionCommandHTTPError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrBadRequest):
		return http.StatusBadRequest, "bad_request"
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrStoreUnavailable):
		return http.StatusServiceUnavailable, "unavailable"
	default:
		return http.StatusServiceUnavailable, "unavailable"
	}
}

func sessionCommandPath(rel string, operation routeclass.Operation) (string, string, error) {
	parts := strings.Split(strings.Trim(rel, "/"), "/")
	if len(parts) < 3 || !strings.EqualFold(parts[0], "Sessions") || !sessionid.Valid(parts[1]) {
		return "", "", ErrBadRequest
	}
	switch operation {
	case routeclass.OperationSessionGeneralCommand:
		if len(parts) == 3 && strings.EqualFold(parts[2], "Command") {
			return parts[1], "", nil
		}
		if len(parts) == 4 && strings.EqualFold(parts[2], "Command") && validCommandName(parts[3]) {
			return parts[1], parts[3], nil
		}
	case routeclass.OperationSessionPlay:
		if len(parts) == 3 && strings.EqualFold(parts[2], "Playing") {
			return parts[1], "", nil
		}
	case routeclass.OperationSessionPlaystate:
		if len(parts) == 4 && strings.EqualFold(parts[2], "Playing") && validCommandName(parts[3]) {
			return parts[1], parts[3], nil
		}
	}
	return "", "", ErrBadRequest
}

func parseSessionCommandHTTPRequest(w http.ResponseWriter, r *http.Request, caller *Session, operation routeclass.Operation, pathCommand string) (sessionCommandHTTPInput, error) {
	input := sessionCommandHTTPInput{category: commandCategoryForOperation(operation)}
	fields, err := readSessionCommandFields(w, r)
	if err != nil {
		return input, ErrBadRequest
	}
	if err := consumeSessionCommandIdentity(fields, caller); err != nil {
		return input, err
	}

	switch operation {
	case routeclass.OperationSessionGeneralCommand:
		command, err := parseGeneralCommandFields(fields, pathCommand)
		input.name = canonicalGeneralCommandName(command.Name)
		input.command = SessionCommandEnvelope{Category: SessionCommandGeneral, General: &command}
		return input, err
	case routeclass.OperationSessionPlay:
		command, err := parsePlayCommandFields(fields)
		input.name = canonicalCommandName(playCommands, command.Command)
		input.command = SessionCommandEnvelope{Category: SessionCommandPlay, Play: &command}
		return input, err
	case routeclass.OperationSessionPlaystate:
		command, err := parsePlaystateCommandFields(fields, pathCommand)
		input.name = canonicalCommandName(playstateCommands, command.Name)
		input.command = SessionCommandEnvelope{Category: SessionCommandPlaystate, Playstate: &command}
		return input, err
	default:
		return input, ErrBadRequest
	}
}

func readSessionCommandFields(w http.ResponseWriter, r *http.Request) (sessionCommandFields, error) {
	fields := sessionCommandFields{}
	query, err := parseRawQuery(r.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	if err := addSessionCommandValues(fields, query, true); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, sessionCommandHTTPBodyLimit))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fields, nil
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	switch contentType {
	case "", "application/json", "text/json":
		bodyFields, err := decodeSessionCommandObject(body)
		if err != nil {
			return nil, err
		}
		for _, field := range bodyFields {
			if err := addSessionCommandField(fields, field.name, field.raw); err != nil {
				return nil, err
			}
		}
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		if err := addSessionCommandValues(fields, values, false); err != nil {
			return nil, err
		}
	default:
		return nil, ErrBadRequest
	}
	return fields, nil
}

func decodeSessionCommandObject(data []byte) (sessionCommandFields, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrBadRequest
	}
	fields := sessionCommandFields{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, ErrBadRequest
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		if err := addSessionCommandField(fields, key, raw); err != nil {
			return nil, err
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrBadRequest
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return fields, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrBadRequest
	}
	return nil
}

func addSessionCommandValues(fields sessionCommandFields, values url.Values, query bool) error {
	for name, entries := range values {
		if query && isSessionCommandAuthQuery(name) {
			continue
		}
		if len(entries) != 1 {
			if strings.EqualFold(name, "ItemIds") && len(entries) > 0 {
				raw, _ := json.Marshal(entries)
				if err := addSessionCommandField(fields, name, raw); err != nil {
					return err
				}
				continue
			}
			return ErrBadRequest
		}
		raw, _ := json.Marshal(entries[0])
		if err := addSessionCommandField(fields, name, raw); err != nil {
			return err
		}
	}
	return nil
}

func isSessionCommandAuthQuery(name string) bool {
	return name == genericQueryAuthKey || isStrictQueryAuthKey(name)
}

func addSessionCommandField(fields sessionCommandFields, name string, raw json.RawMessage) error {
	if name == "" || len(name) > maxSupportedCommandLen {
		return ErrBadRequest
	}
	folded := strings.ToLower(name)
	if _, exists := fields[folded]; exists {
		return ErrBadRequest
	}
	fields[folded] = sessionCommandField{name: name, raw: append(json.RawMessage(nil), raw...)}
	return nil
}

func consumeSessionCommandIdentity(fields sessionCommandFields, caller *Session) error {
	if _, ok := takeSessionCommandField(fields, "Id"); ok {
		return ErrBadRequest
	}
	if field, ok := takeSessionCommandField(fields, "ControllingUserId"); ok {
		value, err := sessionCommandString(field.raw)
		if err != nil || value == "" || value != caller.SyntheticUserID {
			return ErrBadRequest
		}
	}
	return nil
}

func parseGeneralCommandFields(fields sessionCommandFields, pathCommand string) (GeneralCommand, error) {
	if arguments, ok := takeSessionCommandField(fields, "Arguments"); ok {
		nested, err := decodeSessionCommandObject(arguments.raw)
		if err != nil {
			return GeneralCommand{}, ErrBadRequest
		}
		for _, field := range nested {
			if err := addSessionCommandField(fields, field.name, field.raw); err != nil {
				return GeneralCommand{}, ErrBadRequest
			}
		}
	}

	name := pathCommand
	if field, ok := takeSessionCommandField(fields, "Name"); ok {
		if pathCommand != "" {
			return GeneralCommand{}, ErrBadRequest
		}
		var err error
		name, err = sessionCommandString(field.raw)
		if err != nil {
			return GeneralCommand{}, ErrBadRequest
		}
	}
	command := GeneralCommand{Name: name}
	if err := assignGeneralCommandFields(fields, &command); err != nil {
		return GeneralCommand{}, err
	}
	if len(fields) != 0 {
		return GeneralCommand{}, ErrBadRequest
	}
	return command, nil
}

func assignGeneralCommandFields(fields sessionCommandFields, command *GeneralCommand) error {
	text, textSet, err := takeAliasedString(fields, "Text", "String")
	if err != nil {
		return err
	}
	if textSet {
		command.Text = text
	}
	if value, ok, err := takeOptionalInt(fields, "Volume"); err != nil {
		return err
	} else if ok {
		command.Volume = &value
	}
	if value, ok, err := takeOptionalInt(fields, "Index"); err != nil {
		return err
	} else if ok {
		command.Index = &value
	}
	for name, destination := range map[string]*string{
		"ItemType": &command.ItemType,
		"ItemId":   &command.ItemID,
		"ItemName": &command.ItemName,
		"Header":   &command.Header,
	} {
		if field, ok := takeSessionCommandField(fields, name); ok {
			value, err := sessionCommandString(field.raw)
			if err != nil {
				return ErrBadRequest
			}
			*destination = value
		}
	}
	if value, ok, err := takeOptionalInt64(fields, "TimeoutMs"); err != nil {
		return err
	} else if ok {
		command.TimeoutMS = &value
	}
	if value, ok, err := takeOptionalFloat64(fields, "PlaybackRate"); err != nil {
		return err
	} else if ok {
		command.PlaybackRate = &value
	}
	return nil
}

func parsePlayCommandFields(fields sessionCommandFields) (PlayCommand, error) {
	command := PlayCommand{}
	if field, ok := takeSessionCommandField(fields, "PlayCommand"); ok {
		value, err := sessionCommandString(field.raw)
		if err != nil {
			return command, ErrBadRequest
		}
		command.Command = value
	}
	if field, ok := takeSessionCommandField(fields, "ItemIds"); ok {
		items, err := sessionCommandStringList(field.raw)
		if err != nil {
			return command, ErrBadRequest
		}
		command.ItemIDs = items
	}
	if value, ok, err := takeOptionalInt64(fields, "StartPositionTicks"); err != nil {
		return command, err
	} else if ok {
		command.StartPositionTicks = &value
	}
	if field, ok := takeSessionCommandField(fields, "MediaSourceId"); ok {
		value, err := sessionCommandString(field.raw)
		if err != nil {
			return command, ErrBadRequest
		}
		command.MediaSourceID = value
	}
	if value, ok, err := takeOptionalInt(fields, "AudioStreamIndex"); err != nil {
		return command, err
	} else if ok {
		command.AudioStreamIndex = &value
	}
	if value, ok, err := takeOptionalInt(fields, "SubtitleStreamIndex"); err != nil {
		return command, err
	} else if ok {
		command.SubtitleStreamIndex = &value
	}
	if value, ok, err := takeOptionalInt(fields, "StartIndex"); err != nil {
		return command, err
	} else if ok {
		command.StartIndex = &value
	}
	if len(fields) != 0 {
		return command, ErrBadRequest
	}
	return command, nil
}

func parsePlaystateCommandFields(fields sessionCommandFields, pathCommand string) (PlaystateCommand, error) {
	command := PlaystateCommand{Name: pathCommand}
	if _, ok := takeSessionCommandField(fields, "Command"); ok {
		return command, ErrBadRequest
	}
	if value, ok, err := takeOptionalInt64(fields, "SeekPositionTicks"); err != nil {
		return command, err
	} else if ok {
		command.SeekPositionTicks = &value
	}
	if len(fields) != 0 {
		return command, ErrBadRequest
	}
	return command, nil
}

func takeAliasedString(fields sessionCommandFields, names ...string) (string, bool, error) {
	var result string
	found := false
	for _, name := range names {
		field, ok := takeSessionCommandField(fields, name)
		if !ok {
			continue
		}
		if found {
			return "", false, ErrBadRequest
		}
		value, err := sessionCommandString(field.raw)
		if err != nil {
			return "", false, ErrBadRequest
		}
		result, found = value, true
	}
	return result, found, nil
}

func takeOptionalInt(fields sessionCommandFields, name string) (int, bool, error) {
	value, ok, err := takeOptionalInt64(fields, name)
	if err != nil || !ok {
		return 0, ok, err
	}
	converted := int(value)
	if int64(converted) != value {
		return 0, false, ErrBadRequest
	}
	return converted, true, nil
}

func takeOptionalInt64(fields sessionCommandFields, name string) (int64, bool, error) {
	field, ok := takeSessionCommandField(fields, name)
	if !ok {
		return 0, false, nil
	}
	value, err := sessionCommandInt64(field.raw)
	if err != nil {
		return 0, false, ErrBadRequest
	}
	return value, true, nil
}

func takeOptionalFloat64(fields sessionCommandFields, name string) (float64, bool, error) {
	field, ok := takeSessionCommandField(fields, name)
	if !ok {
		return 0, false, nil
	}
	value, err := sessionCommandFloat64(field.raw)
	if err != nil {
		return 0, false, ErrBadRequest
	}
	return value, true, nil
}

func takeSessionCommandField(fields sessionCommandFields, name string) (sessionCommandField, bool) {
	field, ok := fields[strings.ToLower(name)]
	if ok {
		delete(fields, strings.ToLower(name))
	}
	return field, ok
}

func sessionCommandString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func sessionCommandInt64(raw json.RawMessage) (int64, error) {
	if value, err := sessionCommandString(raw); err == nil {
		return strconv.ParseInt(value, 10, 64)
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, err
	}
	return number.Int64()
}

func sessionCommandFloat64(raw json.RawMessage) (float64, error) {
	if value, err := sessionCommandString(raw); err == nil {
		return strconv.ParseFloat(value, 64)
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, err
	}
	return number.Float64()
}

func sessionCommandStringList(raw json.RawMessage) ([]string, error) {
	var values []string
	if err := json.Unmarshal(raw, &values); err == nil {
		return splitSessionCommandItems(values), nil
	}
	value, err := sessionCommandString(raw)
	if err != nil {
		return nil, err
	}
	return splitSessionCommandItems([]string{value}), nil
}

func splitSessionCommandItems(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strings.Split(value, ",")...)
	}
	return result
}

func commandCategoryForOperation(operation routeclass.Operation) SessionCommandCategory {
	switch operation {
	case routeclass.OperationSessionGeneralCommand:
		return SessionCommandGeneral
	case routeclass.OperationSessionPlay:
		return SessionCommandPlay
	case routeclass.OperationSessionPlaystate:
		return SessionCommandPlaystate
	default:
		return 0
	}
}

func sessionCommandCategoryName(category SessionCommandCategory) string {
	switch category {
	case SessionCommandGeneral:
		return "general"
	case SessionCommandPlay:
		return "play"
	case SessionCommandPlaystate:
		return "playstate"
	default:
		return "command"
	}
}

func canonicalGeneralCommandName(name string) string {
	if canonical := canonicalCommandName(noArgumentGeneralCommands, name); canonical != "" {
		return canonical
	}
	return canonicalCommandName(typedGeneralCommands, name)
}

func canonicalCommandName(commands map[string]string, name string) string {
	return commands[strings.ToLower(name)]
}
