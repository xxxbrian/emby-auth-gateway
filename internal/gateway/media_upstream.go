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
	"sort"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

const (
	negotiationRequestBodyLimit  = 1 << 20
	negotiationResponseBodyLimit = 2 << 20
)

var (
	ErrMediaRequestRejected       = errors.New("media request rejected")
	ErrNegotiationRequestRejected = errors.New("negotiation request rejected")
	ErrRequestBodyTooLarge        = errors.New("request body too large")
)

type mediaUpstream struct {
	client      *http.Client
	refresh     func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error)
	leases      MediaLeaseRegistry
	clock       func() time.Time
	closeStream func(context.Context, upstreamRequestSnapshot, PlaySessionID, LiveStreamID) error
	emit        func(observe.Event)
}

func newMediaUpstream(client *http.Client, refresh func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error), leases MediaLeaseRegistry, emit func(observe.Event)) *mediaUpstream {
	streaming := newStreamingHTTPClient(client)
	streaming.CheckRedirect = RejectUpstreamRedirect
	return &mediaUpstream{client: streaming, refresh: refresh, leases: leases, clock: func() time.Time { return time.Now().UTC() }, emit: emit}
}

func (m *mediaUpstream) RoundTripMedia(in mediaUpstreamRequest) (resp *http.Response, err error) {
	status := 0
	defer func() {
		if resp != nil {
			status = resp.StatusCode
		}
		m.emitPurpose(upstreamPurposeMedia, status, err)
	}()
	if err := validateMediaRequest(in); err != nil {
		return nil, err
	}
	selectors, _, err := collectNegotiationSelectors(nil, in.Request.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	if !in.Anonymous {
		if err := validateNegotiationSelectors(m.leases, in.Session.GatewayTokenHash, selectors, m.clock()); err != nil {
			return nil, err
		}
	}
	snapshot := in.Snapshot
	for attempt := 0; attempt < 2; attempt++ {
		attemptResp, attemptErr := m.mediaAttempt(in, snapshot)
		if attemptErr != nil {
			return nil, attemptErr
		}
		if attemptResp.StatusCode != http.StatusUnauthorized || attempt != 0 || m.refresh == nil {
			return attemptResp, nil
		}
		refreshed, confirmed, refreshErr := m.refresh(in.Request.Context(), snapshot)
		in.notifyRefreshResult(upstreamRefreshResult{Confirmed: confirmed, Err: refreshErr})
		if refreshErr != nil || !confirmed {
			return attemptResp, nil
		}
		_ = attemptResp.Body.Close()
		snapshot = refreshed
		if in.SnapshotRef != nil {
			*in.SnapshotRef = refreshed
		}
	}
	return nil, errors.New("media unauthorized retry exhausted")
}

func (m *mediaUpstream) mediaAttempt(in mediaUpstreamRequest, snapshot upstreamRequestSnapshot) (*http.Response, error) {
	rel := in.Request.URL.Path
	if !in.Anonymous {
		rel = projectUserPath(rel, in.Session.SyntheticUserID, snapshot.userID)
	}
	u, err := backendURL(snapshot.baseURL, rel)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	if in.Anonymous {
		parsed.RawQuery = stripMediaRedirectCredentials(in.Request.URL.RawQuery, "", "")
	} else {
		rawQuery, err := rewriteProxyRawQuery(in.Request.URL.RawQuery, in.Session, snapshot)
		if err != nil {
			if errors.Is(err, ErrForbidden) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: malformed query", ErrBadRequest)
		}
		parsed.RawQuery = rawQuery
	}
	req, err := http.NewRequestWithContext(in.Request.Context(), in.Request.Method, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	if in.Anonymous {
		copyAnonymousImageRequestHeaders(req.Header, in.Request.Header)
		sanitizeHopHeaders(req.Header)
		rewritePublicHeaders(req.Header, snapshot.identity)
	} else {
		copyForwardMediaHeaders(req.Header, in.Request.Header)
		sanitizeHopHeaders(req.Header)
		rewriteManagedHeaders(req.Header, snapshot)
	}
	client := *m.client
	if in.Anonymous {
		client.CheckRedirect = RejectUpstreamRedirect
	} else {
		client.CheckRedirect = upstreamRedirectPolicy(upstreamPurposeMedia, ExtractToken(in.Request), snapshot.token)
	}
	resp, err := client.Do(req)
	if err != nil {
		_ = closeResponseOnError(resp)
		return nil, err
	}
	wrapResponseBodyOnce(resp)
	return resp, nil
}

func validateMediaRequest(in mediaUpstreamRequest) error {
	if in.Request == nil || in.Request.URL == nil || (!in.Anonymous && in.Session == nil) {
		return fmt.Errorf("%w: incomplete request", ErrMediaRequestRejected)
	}
	if in.Request.Method != http.MethodGet && in.Request.Method != http.MethodHead {
		return fmt.Errorf("%w: method", ErrMediaRequestRejected)
	}
	if in.Request.Body != nil && in.Request.Body != http.NoBody {
		return fmt.Errorf("%w: body", ErrMediaRequestRejected)
	}
	d := routeclass.Classify(in.Request.Method, in.Request.URL.Path)
	if d.Ownership != routeclass.MediaProxy || d.Operation != routeclass.OperationMediaProxy || !d.MethodAllowed {
		return fmt.Errorf("%w: route ownership", ErrMediaRequestRejected)
	}
	if in.Anonymous {
		if !in.Internal || !validPublicSnapshot(in.Snapshot) {
			return fmt.Errorf("%w: anonymous snapshot", ErrMediaRequestRejected)
		}
	} else if !validSnapshotCredentials(in.Snapshot) {
		return fmt.Errorf("%w: snapshot", ErrMediaRequestRejected)
	}
	return nil
}

func copyForwardMediaHeaders(dst, src http.Header) {
	for name, values := range src {
		for _, value := range values {
			dst.Add(name, value)
		}
	}
	for _, name := range []string{"Authorization", "Proxy-Authorization", "X-Emby-Token", "X-MediaBrowser-Token", "X-Emby-Authorization", "X-MediaBrowser-Authorization"} {
		dst.Del(name)
	}
	stripResourceCookie(dst)
}

func rewriteManagedHeaders(header http.Header, snapshot upstreamRequestSnapshot) {
	identity := snapshot.identity.WithDefaults()
	header.Set("User-Agent", identity.UserAgent)
	header.Set("X-Emby-Token", snapshot.token)
	header.Set("X-Emby-Authorization", backendAuthHeader(identity, snapshot.userID, snapshot.token).String())
}

func rewritePublicHeaders(header http.Header, identity BackendClientIdentity) {
	identity = identity.WithDefaults()
	header.Set("User-Agent", identity.UserAgent)
	header.Set("X-Emby-Authorization", backendAuthHeader(identity, "", "").String())
}

func validPublicSnapshot(snapshot upstreamRequestSnapshot) bool {
	return isTrimmed(snapshot.baseURL) && isTrimmed(snapshot.identity.DeviceID)
}

func validSnapshotCredentials(snapshot upstreamRequestSnapshot) bool {
	return isTrimmed(snapshot.baseURL) && isTrimmed(snapshot.userID) && isTrimmed(snapshot.token) && isTrimmed(snapshot.identity.DeviceID)
}

func (m *mediaUpstream) RoundTripNegotiation(in negotiationUpstreamRequest) (result negotiationUpstreamResponse, err error) {
	status := 0
	defer func() {
		if result.Response != nil {
			status = result.Response.StatusCode
		}
		m.emitPurpose(upstreamPurposeNegotiation, status, err)
	}()
	if err := validateNegotiationRequest(in); err != nil {
		return negotiationUpstreamResponse{}, err
	}
	data, err := readBoundedRequestBody(in.Request)
	if err != nil {
		return negotiationUpstreamResponse{}, err
	}
	selectors, _, err := collectNegotiationSelectors(data, in.Request.URL.RawQuery)
	if err != nil {
		return negotiationUpstreamResponse{}, err
	}
	document, err := parseNegotiationDocument(data)
	if err != nil {
		return negotiationUpstreamResponse{}, err
	}
	if err := validateNegotiationSelectors(m.leases, in.Session.GatewayTokenHash, selectors, m.clock()); err != nil {
		return negotiationUpstreamResponse{}, err
	}
	snapshot := in.Snapshot
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		resp, err = m.negotiationAttempt(in, snapshot, document, len(data) != 0)
		if err != nil {
			return negotiationUpstreamResponse{}, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt != 0 || m.refresh == nil {
			break
		}
		if routeclass.Classify(in.Request.Method, in.Request.URL.Path).Operation == routeclass.OperationPlaybackInfo && isConcurrentPlaybackDenial(resp) {
			break
		}
		refreshed, confirmed, refreshErr := m.refresh(in.Request.Context(), snapshot)
		in.notifyRefreshResult(upstreamRefreshResult{Confirmed: confirmed, Err: refreshErr})
		if refreshErr != nil || !confirmed {
			break
		}
		_ = resp.Body.Close()
		snapshot = refreshed
		in.Snapshot = refreshed
		if in.SnapshotRef != nil {
			*in.SnapshotRef = refreshed
		}
	}
	in.Snapshot = snapshot
	operation := routeclass.Classify(in.Request.Method, in.Request.URL.Path).Operation
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && negotiationRegistersLease(operation) {
		registration, registrationErr := m.prepareNegotiationResponse(in, resp, operation)
		if registrationErr != nil {
			return negotiationUpstreamResponse{}, registrationErr
		}
		result.Registration = registration
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && negotiationReleasesLease(operation) && !selectors.empty() {
		_ = m.leases.Release(in.Session.GatewayTokenHash, selectors.PlaySessionIDs, selectors.LiveStreamIDs)
	}
	result.Response = resp
	return result, nil
}

func validateNegotiationRequest(in negotiationUpstreamRequest) error {
	if in.Request == nil || in.Request.URL == nil || in.Session == nil || !validSnapshotCredentials(in.Snapshot) {
		return fmt.Errorf("%w: incomplete request", ErrNegotiationRequestRejected)
	}
	d := routeclass.Classify(in.Request.Method, in.Request.URL.Path)
	switch d.Operation {
	case routeclass.OperationPlaybackInfo, routeclass.OperationLiveStreamOpen, routeclass.OperationLiveStreamMediaInfo, routeclass.OperationLiveStreamClose, routeclass.OperationActiveEncodingsDelete, routeclass.OperationActiveEncodingsDeleteCompat:
		if d.Ownership != routeclass.MediaProxy || !d.MethodAllowed {
			return fmt.Errorf("%w: method or ownership", ErrNegotiationRequestRejected)
		}
	default:
		return fmt.Errorf("%w: operation", ErrNegotiationRequestRejected)
	}
	if in.Request.Body != nil && in.Request.Body != http.NoBody && in.Request.ContentLength > negotiationRequestBodyLimit {
		return fmt.Errorf("%w: negotiation body", ErrRequestBodyTooLarge)
	}
	return nil
}

func readBoundedRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, negotiationRequestBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > negotiationRequestBodyLimit {
		return nil, fmt.Errorf("%w: negotiation body", ErrRequestBodyTooLarge)
	}
	return data, nil
}

func (m *mediaUpstream) negotiationAttempt(in negotiationUpstreamRequest, snapshot upstreamRequestSnapshot, document *negotiationDocument, hasBody bool) (*http.Response, error) {
	rel := projectUserPath(in.Request.URL.Path, in.Session.SyntheticUserID, snapshot.userID)
	u, err := backendURL(snapshot.baseURL, rel)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	rawQuery, err := rewriteProxyRawQuery(in.Request.URL.RawQuery, in.Session, snapshot)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: malformed query", ErrBadRequest)
	}
	parsed.RawQuery = rawQuery
	var body []byte
	if hasBody {
		body, err = rewriteNegotiationDocument(document, snapshot)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(in.Request.Context(), in.Request.Method, parsed.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType := in.Request.Header.Get("Content-Type"); contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else if len(body) != 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	sanitizeHopHeaders(req.Header)
	rewriteManagedHeaders(req.Header, snapshot)
	resp, err := m.client.Do(req)
	if err != nil {
		_ = closeResponseOnError(resp)
		return nil, err
	}
	wrapResponseBodyOnce(resp)
	return resp, nil
}

type negotiationDocument struct {
	members []negotiationMember
}

type negotiationMember struct {
	key      string
	rawKey   []byte
	rawValue []byte
}

func parseNegotiationDocument(data []byte) (*negotiationDocument, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var members []negotiationMember
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%w: negotiation body must be an object", ErrBadRequest)
	}
	seenIdentity := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: malformed negotiation body", ErrBadRequest)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("%w: malformed negotiation key", ErrBadRequest)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%w: malformed negotiation value", ErrBadRequest)
		}
		folded := strings.ToLower(key)
		if isNegotiationIdentityKey(folded) {
			if _, exists := seenIdentity[folded]; exists {
				return nil, fmt.Errorf("%w: duplicate negotiation identity", ErrBadRequest)
			}
			seenIdentity[folded] = struct{}{}
		}
		if err := rejectNestedNegotiationIdentity(raw); err != nil {
			return nil, err
		}
		rawKey, _ := json.Marshal(key)
		members = append(members, negotiationMember{key: key, rawKey: rawKey, rawValue: append([]byte(nil), raw...)})
	}
	if _, err := decoder.Token(); err != nil || ensureJSONEOF(decoder) != nil {
		return nil, fmt.Errorf("%w: malformed negotiation body", ErrBadRequest)
	}
	return &negotiationDocument{members: members}, nil
}

func isNegotiationIdentityKey(folded string) bool {
	switch folded {
	case "userid", "deviceid", "sessionid", "controllinguserid":
		return true
	default:
		return false
	}
}

func rejectNestedNegotiationIdentity(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, _ := keyToken.(string)
				if depth >= 0 && isNegotiationIdentityKey(strings.ToLower(key)) {
					return fmt.Errorf("%w: nested negotiation identity", ErrBadRequest)
				}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return nil
		}
	}
	if err := walk(0); err != nil {
		return err
	}
	return nil
}

func rewriteNegotiationDocument(document *negotiationDocument, snapshot upstreamRequestSnapshot) ([]byte, error) {
	if document == nil {
		return nil, nil
	}
	var out bytes.Buffer
	out.WriteByte('{')
	wrote := false
	for _, member := range document.members {
		folded := strings.ToLower(member.key)
		if folded == "sessionid" || folded == "controllinguserid" {
			continue
		}
		if wrote {
			out.WriteByte(',')
		}
		wrote = true
		switch folded {
		case "userid":
			out.WriteString(`"UserId":`)
			encoded, _ := json.Marshal(snapshot.userID)
			out.Write(encoded)
		case "deviceid":
			out.WriteString(`"DeviceId":`)
			encoded, _ := json.Marshal(snapshot.identity.DeviceID)
			out.Write(encoded)
		default:
			out.Write(member.rawKey)
			out.WriteByte(':')
			out.Write(member.rawValue)
		}
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

type negotiationSelectorSet struct {
	PlaySessionIDs []PlaySessionID
	LiveStreamIDs  []LiveStreamID
}

func (s negotiationSelectorSet) empty() bool {
	return len(s.PlaySessionIDs) == 0 && len(s.LiveStreamIDs) == 0
}

func collectNegotiationSelectors(body []byte, rawQuery string) (negotiationSelectorSet, any, error) {
	play := make(map[string]struct{})
	live := make(map[string]struct{})
	query, err := parseRawQuery(rawQuery)
	if err != nil {
		return negotiationSelectorSet{}, nil, fmt.Errorf("%w: malformed query", ErrBadRequest)
	}
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if left == right {
			return keys[i] < keys[j]
		}
		return left < right
	})
	for _, key := range keys {
		var target map[string]struct{}
		switch strings.ToLower(key) {
		case "playsessionid":
			target = play
		case "livestreamid":
			target = live
		default:
			continue
		}
		for _, value := range query[key] {
			if err := addNegotiationSelector(target, value); err != nil {
				return negotiationSelectorSet{}, nil, err
			}
		}
	}

	var document any
	if len(body) != 0 {
		scanner := json.NewDecoder(bytes.NewReader(body))
		scanner.UseNumber()
		if err := scanNegotiationJSONValue(scanner, "", play, live); err != nil {
			return negotiationSelectorSet{}, nil, err
		}
		if err := ensureJSONEOF(scanner); err != nil {
			return negotiationSelectorSet{}, nil, fmt.Errorf("%w: malformed JSON", ErrBadRequest)
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			return negotiationSelectorSet{}, nil, fmt.Errorf("%w: malformed JSON", ErrBadRequest)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return negotiationSelectorSet{}, nil, fmt.Errorf("%w: malformed JSON", ErrBadRequest)
		}
	}
	return negotiationSelectorSet{PlaySessionIDs: sortedPlaySessionIDs(play), LiveStreamIDs: sortedLiveStreamIDs(live)}, document, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("extra JSON value")
		}
		return err
	}
	return nil
}

func scanNegotiationJSONValue(decoder *json.Decoder, selector string, play, live map[string]struct{}) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: malformed JSON", ErrBadRequest)
	}
	if selector != "" {
		if token == nil {
			return nil
		}
		text, ok := token.(string)
		if !ok {
			return fmt.Errorf("%w: %s must be a string", ErrBadRequest, selector)
		}
		if strings.EqualFold(selector, "PlaySessionId") {
			return addNegotiationSelector(play, text)
		}
		return addNegotiationSelector(live, text)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("%w: malformed JSON", ErrBadRequest)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%w: malformed JSON object", ErrBadRequest)
			}
			kind := ""
			switch strings.ToLower(key) {
			case "playsessionid":
				kind = "PlaySessionId"
			case "livestreamid":
				kind = "LiveStreamId"
			}
			if err := scanNegotiationJSONValue(decoder, kind, play, live); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("%w: malformed JSON object", ErrBadRequest)
		}
	case '[':
		for decoder.More() {
			if err := scanNegotiationJSONValue(decoder, "", play, live); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("%w: malformed JSON array", ErrBadRequest)
		}
	default:
		return fmt.Errorf("%w: malformed JSON", ErrBadRequest)
	}
	return nil
}

func addNegotiationSelector(target map[string]struct{}, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if len(value) > mediaLeaseIdentifierMaxBytes {
		return fmt.Errorf("%w: invalid negotiation identifier", ErrBadRequest)
	}
	target[value] = struct{}{}
	return nil
}

func sortedPlaySessionIDs(values map[string]struct{}) []PlaySessionID {
	items := make([]string, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Strings(items)
	result := make([]PlaySessionID, len(items))
	for i, value := range items {
		result[i] = PlaySessionID(value)
	}
	return result
}

func sortedLiveStreamIDs(values map[string]struct{}) []LiveStreamID {
	items := make([]string, 0, len(values))
	for value := range values {
		items = append(items, value)
	}
	sort.Strings(items)
	result := make([]LiveStreamID, len(items))
	for i, value := range items {
		result[i] = LiveStreamID(value)
	}
	return result
}

func validateNegotiationSelectors(registry MediaLeaseRegistry, owner string, selectors negotiationSelectorSet, now time.Time) error {
	if selectors.empty() {
		return nil
	}
	if registry == nil {
		return ErrStoreUnavailable
	}
	return registry.ValidateAll(owner, selectors.PlaySessionIDs, selectors.LiveStreamIDs, now)
}

func (m *mediaUpstream) prepareNegotiationResponse(in negotiationUpstreamRequest, resp *http.Response, operation routeclass.Operation) (*negotiationLeaseRegistration, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, negotiationResponseBodyLimit+1))
	_ = resp.Body.Close()
	if err != nil || len(data) > negotiationResponseBodyLimit {
		return nil, fmt.Errorf("%w: response body", ErrBadRequest)
	}
	resp.Body = &onceReadCloser{reader: bytes.NewReader(data), closer: io.NopCloser(bytes.NewReader(nil))}
	selectors, _, selectorErr := collectNegotiationSelectors(data, "")
	if selectorErr != nil {
		return nil, selectorErr
	}
	if selectors.empty() {
		return nil, nil
	}
	return newNegotiationLeaseRegistration(m.leases, in.Session.GatewayTokenHash, selectors, operation, in.Request.Context(), in.Snapshot, m.closeStream), nil
}

func (m *mediaUpstream) closeNegotiatedStream(ctx context.Context, snapshot upstreamRequestSnapshot, play PlaySessionID, live LiveStreamID) (err error) {
	status := 0
	defer func() { m.emitPurpose(upstreamPurposeNegotiation, status, err) }()
	u, err := backendURL(snapshot.baseURL, "/LiveStreams/Close")
	if err != nil {
		return err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return err
	}
	query := parsed.Query()
	if play != "" {
		query.Set("PlaySessionId", string(play))
	}
	if live != "" {
		query.Set("LiveStreamId", string(live))
	}
	parsed.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), nil)
	if err != nil {
		return err
	}
	rewriteManagedHeaders(req.Header, snapshot)
	resp, err := m.client.Do(req)
	if err != nil {
		_ = closeResponseOnError(resp)
		return err
	}
	wrapResponseBodyOnce(resp)
	defer resp.Body.Close()
	status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("live stream cleanup status %d", resp.StatusCode)
	}
	return nil
}

func negotiationRegistersLease(operation routeclass.Operation) bool {
	return operation == routeclass.OperationPlaybackInfo || operation == routeclass.OperationLiveStreamOpen || operation == routeclass.OperationLiveStreamMediaInfo
}

func negotiationReleasesLease(operation routeclass.Operation) bool {
	return operation == routeclass.OperationLiveStreamClose || operation == routeclass.OperationActiveEncodingsDelete || operation == routeclass.OperationActiveEncodingsDeleteCompat
}

var _ MediaUpstream = (*mediaUpstream)(nil)

func (m *mediaUpstream) emitPurpose(purpose upstreamPurpose, status int, err error) {
	if m == nil || m.emit == nil {
		return
	}
	outcome := observe.OutcomeError
	if err == nil && status >= 200 && status < 300 {
		outcome = observe.OutcomeOK
	}
	m.emit(observe.Event{Kind: observe.KindUpstreamRequest, RouteClass: observe.RouteMedia, Outcome: outcome, StatusClass: observe.StatusClassOf(status), ErrorKind: purpose.String(), Direction: observe.DirectionUpstream})
}
