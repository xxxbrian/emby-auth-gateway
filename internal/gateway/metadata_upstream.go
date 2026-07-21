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
	resp, snapshot, err := m.doAttempt(in, in.Snapshot)
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
			resp, _, err = m.doAttempt(in, refreshed)
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
	// PocketBase wraps empty GET/HEAD bodies in a rereadable closer. Accept only
	// declared zero-length bodies with no transfer encoding; never read Body.
	if requestDeclaresDisallowedBody(in.Request) {
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

// requestDeclaresDisallowedBody reports whether a GET/HEAD request declares a
// body that must be rejected before dial. Shared by metadata and media adapters.
// ContentLength == 0 with empty TransferEncoding is allowed even when Body is a
// non-nil wrapper (PocketBase empty-body path). Positive length, unknown length
// (ContentLength < 0), and any transfer encoding are rejected without reading Body.
func requestDeclaresDisallowedBody(r *http.Request) bool {
	if r == nil {
		return false
	}
	if len(r.TransferEncoding) > 0 {
		return true
	}
	return r.ContentLength != 0
}

func (m *metadataUpstream) doAttempt(in metadataUpstreamRequest, snapshot upstreamRequestSnapshot) (*http.Response, upstreamRequestSnapshot, error) {
	rel := in.Request.URL.Path
	rawQuery := ""
	if !in.Public {
		// Recompute policy from the original client request on every attempt
		// (including auth refresh retry) so path selection stays stable while
		// backend identity comes from the current snapshot.
		policy := metadataQueryPolicyForRequest(in.Request.Method, in.Request.URL.Path)
		var err error
		rawQuery, err = sanitizeMetadataRawQueryWithPolicy(
			in.Request.URL.RawQuery,
			in.Session.SyntheticUserID,
			snapshot.userID,
			ExtractToken(in.Request),
			policy,
		)
		if err != nil {
			return nil, snapshot, err
		}
		rel = projectUserPath(rel, in.Session.SyntheticUserID, snapshot.userID)
	}
	return m.do(in.Request.Context(), snapshot, in.Request.Method, rel, rawQuery, in.Public)
}

// metadataQueryPolicyForRequest selects the frozen metadata query policy for a
// metadata egress attempt. It is path/method selection only: it does not
// authorize routes or broaden routeclass.
func metadataQueryPolicyForRequest(method, path string) metadataQueryPolicy {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead:
	default:
		// Metadata adapter rejects non-GET/HEAD before dial; keep a safe default.
		return metadataQueryPolicyNonBaseItem
	}
	parts := responseProjectionPathParts(path)
	switch {
	case len(parts) == 2 && parts[0] == "system" && parts[1] == "info":
		return metadataQueryPolicySystemInfo
	case isPathBoundNeutralMetadataRoute(parts):
		return metadataQueryPolicyPathBoundNeutral
	case isUserBoundBaseItemMetadataRoute(parts):
		return metadataQueryPolicyPathBoundBaseItem
	case isGlobalBaseItemMetadataRoute(parts):
		return metadataQueryPolicyGlobalBaseItem
	default:
		// Exact /Items/{id}/Images, reserved opaque paths, and other non-BaseItem
		// metadata fall through here. Specific opaque/static segments are already
		// excluded from BaseItem classifiers before generic item/by-name matches.
		return metadataQueryPolicyNonBaseItem
	}
}

func isPathBoundNeutralMetadataRoute(parts []string) bool {
	return len(parts) == 3 && parts[0] == "users" && parts[1] != "" &&
		(parts[2] == "views" || parts[2] == "homesections")
}

// isUserBoundBaseItemMetadataRoute matches user-scoped BaseItem families that
// currently project as BaseItem / envelope / array under /Users/{self}/...
// Views and HomeSections are handled as path-bound neutral instead.
func isUserBoundBaseItemMetadataRoute(parts []string) bool {
	if len(parts) < 3 || parts[0] != "users" || parts[1] == "" {
		return false
	}
	switch {
	case len(parts) == 3 && (parts[2] == "items" || parts[2] == "suggestions"):
		return true
	case len(parts) == 4 && parts[2] == "items" && parts[3] != "":
		// Direct item, Resume, Latest, and other single-segment Items children.
		return true
	case len(parts) == 5 && parts[2] == "sections" && parts[3] != "" && parts[4] == "items":
		return true
	case len(parts) == 5 && parts[2] == "items" && parts[3] != "" &&
		(parts[4] == "intros" || parts[4] == "localtrailers" || parts[4] == "specialfeatures"):
		return true
	default:
		return false
	}
}

// isGlobalBaseItemMetadataRoute mirrors Phase 7 BaseItem projection families
// that are not user-path-bound (global Items lists, by-name, similar, theme media).
func isGlobalBaseItemMetadataRoute(parts []string) bool {
	if len(parts) > 0 && parts[0] == "users" {
		return false
	}
	return isBaseItemEnvelopeArrayRoute(parts) ||
		isAllThemeMediaRoute(parts) ||
		isDeclaredBaseItemArrayRoute(parts) ||
		isBaseItemEnvelopeRoute(parts) ||
		isDirectBaseItemRoute(parts)
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

func requestMethod(req *http.Request) string {
	if req == nil {
		return ""
	}
	return strings.ToUpper(req.Method)
}

var _ MetadataUpstream = (*metadataUpstream)(nil)
