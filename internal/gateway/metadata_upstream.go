package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"github.com/xxxbrian/emby-auth-gateway/internal/routeclass"
)

// metadataUpstream is the deliberately narrow egress implementation for JSON
// metadata reads. It never copies client headers or request credentials.
type metadataUpstream struct {
	client  *http.Client
	refresh func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error)
	emit    func(observe.Event)
}

func newMetadataUpstream(client *http.Client, refresh func(context.Context, upstreamRequestSnapshot) (upstreamRequestSnapshot, bool, error), emit func(observe.Event)) *metadataUpstream {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.Jar = nil
	copy.CheckRedirect = RejectUpstreamRedirect
	return &metadataUpstream{client: &copy, refresh: refresh, emit: emit}
}

func (m *metadataUpstream) RoundTripMetadata(in metadataUpstreamRequest) (*http.Response, error) {
	started := time.Now()
	status := 0
	outcome := observe.OutcomeError
	errKind := "metadata_request"
	defer func() {
		if m != nil && m.emit != nil {
			m.emit(observe.Event{Kind: observe.KindUpstreamRequest, RouteClass: observe.RouteMetadata, Outcome: outcome, StatusClass: observe.StatusClassOf(status), ErrorKind: errKind, Direction: observe.DirectionUpstream, Method: requestMethod(in.Request), DurationMS: time.Since(started).Milliseconds()})
		}
	}()

	if err := validateMetadataRequest(in); err != nil {
		if errors.Is(err, ErrForbidden) {
			status = http.StatusForbidden
			outcome = observe.OutcomeDenied
			errKind = "metadata_forbidden"
		}
		return nil, err
	}
	var clientQuery url.Values
	if !in.Public {
		q, err := url.ParseQuery(in.Request.URL.RawQuery)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed metadata query", ErrBadRequest)
		}
		clientQuery = q
	}
	resp, snapshot, err := m.doAttempt(in, in.Snapshot, clientQuery)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			status = http.StatusForbidden
			outcome = observe.OutcomeDenied
			errKind = "metadata_forbidden"
		}
		return nil, err
	}
	status = resp.StatusCode
	if !in.Public && resp.StatusCode == http.StatusUnauthorized && m.refresh != nil {
		refreshed, confirmed, refreshErr := m.refresh(in.Request.Context(), snapshot)
		in.notifyRefreshResult(upstreamRefreshResult{Confirmed: confirmed, Err: refreshErr})
		if confirmed && refreshErr == nil {
			_ = resp.Body.Close()
			if in.SnapshotRef != nil {
				*in.SnapshotRef = refreshed
			}
			resp, _, err = m.doAttempt(in, refreshed, clientQuery)
			if err != nil {
				return nil, err
			}
			status = resp.StatusCode
		}
	}
	if status >= 200 && status < 300 {
		outcome = observe.OutcomeOK
	} else if status >= 400 && status < 500 {
		outcome = observe.OutcomeError
	}
	return resp, nil
}

func validateMetadataRequest(in metadataUpstreamRequest) error {
	if in.Request == nil || in.Request.URL == nil || (!in.Public && in.Session == nil) {
		return fmt.Errorf("%w: incomplete metadata request", ErrBadRequest)
	}
	if in.Request.Method != http.MethodGet && in.Request.Method != http.MethodHead {
		return fmt.Errorf("%w: metadata method not allowed", ErrBadRequest)
	}
	if in.Request.Body != nil && in.Request.Body != http.NoBody {
		return fmt.Errorf("%w: metadata body not allowed", ErrBadRequest)
	}
	if in.Public {
		if !in.Internal || !strings.EqualFold(in.Request.URL.Path, "/System/Info/Public") || in.Request.URL.RawQuery != "" || !validPublicSnapshot(in.Snapshot) {
			return fmt.Errorf("%w: public metadata request not allowed", ErrForbidden)
		}
		return nil
	}
	if in.Ownership != routeclass.MetadataProxy && !in.Internal {
		return fmt.Errorf("%w: metadata ownership not allowed", ErrForbidden)
	}
	if !relUserMatches(in.Request.URL.Path, in.Session.SyntheticUserID) {
		return fmt.Errorf("%w: metadata path user does not belong to session", ErrForbidden)
	}
	if in.Snapshot.baseURL == "" || in.Snapshot.userID == "" || in.Snapshot.token == "" {
		return fmt.Errorf("%w: incomplete upstream snapshot", ErrBadRequest)
	}
	return nil
}

func (m *metadataUpstream) doAttempt(in metadataUpstreamRequest, snapshot upstreamRequestSnapshot, clientQuery url.Values) (*http.Response, upstreamRequestSnapshot, error) {
	rel := in.Request.URL.Path
	rawQuery := ""
	if !in.Public {
		var err error
		stripMetadataSelectedToken(clientQuery, ExtractToken(in.Request))
		rawQuery, err = SanitizeMetadataQuery(clientQuery, in.Session.SyntheticUserID, snapshot.userID)
		if err != nil {
			return nil, snapshot, err
		}
		rel = strings.ReplaceAll(rel, in.Session.SyntheticUserID, snapshot.userID)
	}
	return m.do(in.Request.Context(), snapshot, in.Request.Method, rel, rawQuery, in.Public)
}

func (m *metadataUpstream) do(ctx context.Context, snapshot upstreamRequestSnapshot, method, rel, rawQuery string, public bool) (*http.Response, upstreamRequestSnapshot, error) {
	backend, err := backendURL(snapshot.baseURL, rel)
	if err != nil {
		return nil, snapshot, err
	}
	u, err := url.Parse(backend)
	if err != nil {
		return nil, snapshot, err
	}
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, snapshot, err
	}
	if public {
		rewritePublicHeaders(req.Header, snapshot.identity)
	} else {
		rewriteManagedMetadataHeaders(req.Header, snapshot)
	}
	req.Host = u.Host
	resp, err := m.client.Do(req)
	if err != nil {
		_ = closeResponseOnError(resp)
		return nil, snapshot, err
	}
	wrapResponseBodyOnce(resp)
	return resp, snapshot, nil
}

func rewriteManagedMetadataHeaders(header http.Header, snapshot upstreamRequestSnapshot) {
	identity := snapshot.identity.WithDefaults()
	header.Set("User-Agent", identity.UserAgent)
	header.Set("X-Emby-Token", snapshot.token)
	header.Set("X-Emby-Authorization", backendAuthHeader(identity, snapshot.userID, snapshot.token).String())
}

func stripMetadataSelectedToken(query url.Values, gatewayToken string) {
	if query == nil || gatewayToken == "" {
		return
	}
	for key, values := range query {
		kept := values[:0]
		for _, value := range values {
			if value == gatewayToken {
				continue
			}
			kept = append(kept, value)
		}
		if len(kept) == 0 {
			delete(query, key)
			continue
		}
		query[key] = kept
	}
}

func requestMethod(req *http.Request) string {
	if req == nil {
		return ""
	}
	return strings.ToUpper(req.Method)
}

var _ MetadataUpstream = (*metadataUpstream)(nil)
