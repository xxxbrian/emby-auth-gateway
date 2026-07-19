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
		rel = strings.ReplaceAll(rel, in.Session.SyntheticUserID, snapshot.userID)
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
		query, err := parseRawQuery(in.Request.URL.RawQuery)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed query", ErrBadRequest)
		}
		rewriteProxyQueryValues(query, ExtractToken(in.Request), in.Session, snapshot)
		parsed.RawQuery = query.Encode()
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

func (m *mediaUpstream) RoundTripNegotiation(in negotiationUpstreamRequest) (resp *http.Response, err error) {
	status := 0
	defer func() {
		if resp != nil {
			status = resp.StatusCode
		}
		m.emitPurpose(upstreamPurposeNegotiation, status, err)
	}()
	if err := validateNegotiationRequest(in); err != nil {
		return nil, err
	}
	data, err := readBoundedRequestBody(in.Request)
	if err != nil {
		return nil, err
	}
	selectors, document, err := collectNegotiationSelectors(data, in.Request.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	if err := validateNegotiationSelectors(m.leases, in.Session.GatewayTokenHash, selectors, m.clock()); err != nil {
		return nil, err
	}
	snapshot := in.Snapshot
	for attempt := 0; attempt < 2; attempt++ {
		resp, err = m.negotiationAttempt(in, snapshot, document, len(data) != 0)
		if err != nil {
			return nil, err
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
		if err := m.registerNegotiationResponse(in, resp, operation); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && negotiationReleasesLease(operation) && !selectors.empty() {
		_ = m.leases.Release(in.Session.GatewayTokenHash, selectors.PlaySessionIDs, selectors.LiveStreamIDs)
	}
	return resp, nil
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

func (m *mediaUpstream) negotiationAttempt(in negotiationUpstreamRequest, snapshot upstreamRequestSnapshot, document any, hasBody bool) (*http.Response, error) {
	rel := strings.ReplaceAll(in.Request.URL.Path, in.Session.SyntheticUserID, snapshot.userID)
	u, err := backendURL(snapshot.baseURL, rel)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	query, err := parseRawQuery(in.Request.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed query", ErrBadRequest)
	}
	rewriteProxyQueryValues(query, ExtractToken(in.Request), in.Session, snapshot)
	parsed.RawQuery = query.Encode()
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

func rewriteNegotiationDocument(document any, snapshot upstreamRequestSnapshot) ([]byte, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("%w: negotiation body", ErrBadRequest)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: negotiation body", ErrBadRequest)
	}
	rewriteNegotiationValue(value, snapshot)
	return json.Marshal(value)
}

func rewriteNegotiationValue(value any, snapshot upstreamRequestSnapshot) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			rewriteNegotiationValue(item, snapshot)
		}
	case map[string]any:
		userAlias := false
		deviceAlias := false
		for key, child := range v {
			switch strings.ToLower(key) {
			case "sessionid", "controllinguserid":
				delete(v, key)
			case "userid":
				delete(v, key)
				userAlias = true
			case "deviceid":
				delete(v, key)
				deviceAlias = true
			default:
				rewriteNegotiationValue(child, snapshot)
			}
		}
		if userAlias {
			v["UserId"] = snapshot.userID
		}
		if deviceAlias {
			v["DeviceId"] = snapshot.identity.DeviceID
		}
	}
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

func (m *mediaUpstream) registerNegotiationResponse(in negotiationUpstreamRequest, resp *http.Response, operation routeclass.Operation) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, negotiationResponseBodyLimit+1))
	_ = resp.Body.Close()
	if err != nil || len(data) > negotiationResponseBodyLimit {
		return fmt.Errorf("%w: response body", ErrBadRequest)
	}
	selectors, _, selectorErr := collectNegotiationSelectors(data, "")
	if selectorErr != nil {
		return selectorErr
	}
	if selectors.empty() {
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return nil
	}
	if m.leases == nil {
		m.closeReturnedLiveStreams(in, selectors)
		return fmt.Errorf("%w: lease registry unavailable", ErrStoreUnavailable)
	}
	err = m.leases.RegisterAll(in.Session.GatewayTokenHash, selectors.PlaySessionIDs, selectors.LiveStreamIDs)
	if err != nil {
		m.closeReturnedLiveStreams(in, selectors)
		if errors.Is(err, ErrStoreUnavailable) {
			return ErrStoreUnavailable
		}
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return nil
}

func (m *mediaUpstream) closeReturnedLiveStreams(in negotiationUpstreamRequest, selectors negotiationSelectorSet) {
	if len(selectors.LiveStreamIDs) == 0 || m.closeStream == nil {
		return
	}
	var play PlaySessionID
	if len(selectors.PlaySessionIDs) != 0 {
		play = selectors.PlaySessionIDs[0]
	}
	for _, live := range selectors.LiveStreamIDs {
		_ = m.closeStream(in.Request.Context(), in.Snapshot, play, live)
	}
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
