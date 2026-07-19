package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
	"golang.org/x/sync/singleflight"
)

const (
	upstreamAuthTimeout       = 15 * time.Second
	upstreamCleanupTimeout    = 2 * time.Second
	upstreamAuthBodyLimit     = 1 << 20
	upstreamGeneratorAttempts = 4
)

var errManagedAuthUnauthorized = errors.New("managed upstream authentication unauthorized")

type upstreamAuthStore interface {
	LoadDefaultUpstreamRuntime(context.Context) (*UpstreamRuntime, error)
	CompareAndSwapUpstreamAuth(context.Context, UpstreamAuthUpdate) error
}

type upstreamAuthenticator struct {
	store          upstreamAuthStore
	client         *http.Client
	flights        singleflight.Group
	clock          func() time.Time
	deviceID       func() (string, error)
	generation     func() (string, error)
	authTimeout    time.Duration
	cleanupTimeout time.Duration
	emit           func(observe.Event)
}

func newUpstreamAuthenticator(store upstreamAuthStore, client *http.Client, emit ...func(observe.Event)) *upstreamAuthenticator {
	if client == nil {
		client = http.DefaultClient
	}
	cloned := *client
	cloned.Jar = nil
	cloned.CheckRedirect = upstreamRedirectPolicy(upstreamPurposeManagedAuth, "", "")
	var emitEvent func(observe.Event)
	if len(emit) != 0 {
		emitEvent = emit[0]
	}
	return &upstreamAuthenticator{
		store: store, client: &cloned, clock: func() time.Time { return time.Now().UTC() },
		deviceID: newUpstreamDeviceID, generation: newUpstreamGeneration, authTimeout: upstreamAuthTimeout, cleanupTimeout: upstreamCleanupTimeout,
		emit: emitEvent,
	}
}

func (a *upstreamAuthenticator) Ensure(ctx context.Context) (*UpstreamRuntime, error) {
	return a.run(ctx, "", false)
}

func (a *upstreamAuthenticator) Refresh(ctx context.Context, failedToken string) (*UpstreamRuntime, error) {
	if !isTrimmed(failedToken) {
		return nil, fmt.Errorf("%w: invalid failed upstream token", ErrBadRequest)
	}
	return a.run(ctx, failedToken, true)
}

func (a *upstreamAuthenticator) run(ctx context.Context, failedToken string, refresh bool) (*UpstreamRuntime, error) {
	retriedFlight := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		runtime, err := a.store.LoadDefaultUpstreamRuntime(ctx)
		if err != nil {
			return nil, err
		}
		if upstreamAuthSatisfied(runtime, failedToken, refresh) {
			return runtime, nil
		}
		result := a.flights.DoChan(runtime.Source.ID, func() (any, error) {
			return a.lead(ctx, failedToken, refresh)
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case shared := <-result:
			if shared.Err != nil {
				if isUpstreamAuthLeaderCanceled(shared.Err) && ctx.Err() == nil {
					if retriedFlight {
						return nil, shared.Err
					}
					retriedFlight = true
					continue
				}
				return nil, shared.Err
			}
			if authoritative, ok := shared.Val.(*UpstreamRuntime); ok && upstreamAuthSatisfied(authoritative, failedToken, refresh) {
				return authoritative, nil
			}
			// A different caller intent may have led the flight. Re-evaluate from
			// storage and start one new flight only when this caller still needs it.
			if retriedFlight {
				return nil, ErrUpstreamAuthConflict
			}
			retriedFlight = true
		}
	}
}

func upstreamAuthSatisfied(runtime *UpstreamRuntime, failedToken string, refresh bool) bool {
	if runtime == nil || runtime.Source.AuthGenerationID == "" {
		return false
	}
	if !refresh {
		return true
	}
	return runtime.Source.BackendToken != failedToken
}

func (a *upstreamAuthenticator) lead(ctx context.Context, failedToken string, refresh bool) (*UpstreamRuntime, error) {
	runtime, err := a.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil {
		return nil, a.leaderError(ctx, err)
	}
	if upstreamAuthSatisfied(runtime, failedToken, refresh) {
		return runtime, nil
	}
	loginCtx, cancel := context.WithTimeout(ctx, a.authTimeout)
	update, loginErr := a.Login(managedAuthLoginRequest{Context: loginCtx, Runtime: *runtime})
	cancel()
	if loginErr != nil {
		a.cleanupInvocation(update, runtime)
		return nil, a.leaderError(ctx, loginErr)
	}
	if update.BackendToken == runtime.Source.BackendToken {
		return nil, errors.New("upstream authentication token collision")
	}
	if err := ctx.Err(); err != nil {
		a.cleanupInvocation(update, runtime)
		return nil, a.leaderError(ctx, err)
	}
	if err := a.store.CompareAndSwapUpstreamAuth(ctx, update); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), a.cleanupTimeout)
		defer cancel()
		current, reloadErr := a.store.LoadDefaultUpstreamRuntime(cleanupCtx)
		if reloadErr == nil && upstreamAuthOwnershipMatches(current, update) {
			return current, nil
		}
		if errors.Is(err, ErrUpstreamAuthConflict) && reloadErr == nil {
			a.cleanupInvocationWithCurrent(cleanupCtx, update, runtime, current)
			return current, nil
		}
		if reloadErr == nil {
			a.cleanupInvocationWithCurrent(cleanupCtx, update, runtime, current)
		}
		return nil, a.leaderError(ctx, err)
	}
	authoritative, err := a.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil {
		return nil, a.leaderError(ctx, err)
	}
	if upstreamAuthOwnershipMatches(authoritative, update) {
		a.retireOld(runtime)
		return authoritative, nil
	}
	if authoritative.Source.AuthGenerationID != update.ExpectedGenerationID && authoritative.Source.AuthGenerationID != update.GenerationID {
		return authoritative, nil
	}
	if authoritative.Source.AuthGenerationID == update.ExpectedGenerationID {
		return nil, errors.New("upstream authentication persistence mismatch")
	}
	return nil, errors.New("upstream authentication persistence mismatch")
}

type upstreamAuthLeaderCanceled struct{ cause error }

func (e *upstreamAuthLeaderCanceled) Error() string { return e.cause.Error() }
func (e *upstreamAuthLeaderCanceled) Unwrap() error { return e.cause }

func isUpstreamAuthLeaderCanceled(err error) bool {
	var marker *upstreamAuthLeaderCanceled
	return errors.As(err, &marker)
}

func (a *upstreamAuthenticator) leaderError(ctx context.Context, err error) error {
	if parentErr := ctx.Err(); parentErr != nil {
		return &upstreamAuthLeaderCanceled{cause: parentErr}
	}
	return err
}

func (a *upstreamAuthenticator) freshIdentifiers(source UpstreamSource) (string, string, error) {
	for range upstreamGeneratorAttempts {
		deviceID, err := a.deviceID()
		if err != nil {
			return "", "", err
		}
		generation, err := a.generation()
		if err != nil {
			return "", "", err
		}
		if deviceID != source.ClientIdentity.DeviceID && generation != source.AuthGenerationID {
			return deviceID, generation, nil
		}
	}
	return "", "", errors.New("upstream authentication identifier collision")
}

type upstreamLoginResult struct {
	Token  string
	UserID string
}

func (a *upstreamAuthenticator) Login(in managedAuthLoginRequest) (UpstreamAuthUpdate, error) {
	var update UpstreamAuthUpdate
	if err := in.Context.Err(); err != nil {
		return update, err
	}
	deviceID, generation, err := a.freshIdentifiers(in.Runtime.Source)
	if err != nil {
		return update, err
	}
	update = UpstreamAuthUpdate{SourceID: in.Runtime.Source.ID, ExpectedGenerationID: in.Runtime.Source.AuthGenerationID, GenerationID: generation, DeviceID: deviceID, AuthenticatedAt: a.clock().UTC()}
	result, err := a.login(in.Context, &in.Runtime, deviceID)
	update.BackendUserID = result.UserID
	update.BackendToken = result.Token
	return update, err
}

func (a *upstreamAuthenticator) login(ctx context.Context, runtime *UpstreamRuntime, deviceID string) (upstreamLoginResult, error) {
	var result upstreamLoginResult
	u, err := backendURL(runtime.Endpoint.BaseURL, "/Users/AuthenticateByName")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]string{"Username": runtime.Source.BackendUsername, "Pw": runtime.Source.BackendPassword})
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	identity := runtime.Source.ClientIdentity
	identity.DeviceID = deviceID
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("X-Emby-Authorization", backendAuthHeader(identity, "", "").String())
	resp, err := a.doManagedAuth(req, "login")
	if err != nil {
		_ = closeResponseOnError(resp)
		return result, fmt.Errorf("upstream authentication request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, upstreamAuthBodyLimit+1))
	withinLimit := data
	if len(withinLimit) > upstreamAuthBodyLimit {
		withinLimit = withinLimit[:upstreamAuthBodyLimit]
	}
	result.Token = extractUpstreamAccessToken(withinLimit)
	if err != nil {
		return result, errors.New("upstream authentication response read failed")
	}
	if len(data) > upstreamAuthBodyLimit {
		return result, errors.New("upstream authentication response too large")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return result, fmt.Errorf("upstream authentication status %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"AccessToken"`
		ServerID    string `json:"ServerId"`
		User        struct {
			ID string `json:"Id"`
		} `json:"User"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return result, errors.New("upstream authentication response malformed")
	}
	result.Token = payload.AccessToken
	result.UserID = payload.User.ID
	if !isTrimmed(result.Token) || !isTrimmed(result.UserID) || !isTrimmed(payload.ServerID) || payload.ServerID != runtime.Source.ServerID {
		return result, errors.New("upstream authentication response invalid")
	}
	return result, nil
}

func (a *upstreamAuthenticator) Probe(in managedAuthProbeRequest) (UpstreamServerInfoUpdate, error) {
	var update UpstreamServerInfoUpdate
	if err := in.Context.Err(); err != nil {
		return update, err
	}
	runtime := in.Snapshot
	u, err := backendURL(runtime.Endpoint.BaseURL, "/System/Info")
	if err != nil {
		return update, err
	}
	req, err := http.NewRequestWithContext(in.Context, http.MethodGet, u, nil)
	if err != nil {
		return update, err
	}
	rewriteManagedAuthHeaders(req.Header, runtime.Source.ClientIdentity, runtime.Source.BackendUserID, runtime.Source.BackendToken)
	resp, err := a.doManagedAuth(req, "probe")
	if err != nil {
		_ = closeResponseOnError(resp)
		return update, fmt.Errorf("upstream authentication probe failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := readManagedAuthBody(resp.Body)
	if err != nil {
		return update, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if resp.StatusCode == http.StatusUnauthorized {
			return update, errManagedAuthUnauthorized
		}
		return update, fmt.Errorf("upstream authentication probe status %d", resp.StatusCode)
	}
	var payload struct {
		ID         string `json:"Id"`
		ServerName string `json:"ServerName"`
		Version    string `json:"Version"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return update, errors.New("upstream authentication probe response malformed")
	}
	update = UpstreamServerInfoUpdate{SourceID: runtime.Source.ID, ServerID: payload.ID, ServerName: payload.ServerName, ServerVersion: payload.Version, CheckedAt: a.clock().UTC()}
	if payload.ID != runtime.Source.ServerID || ValidateUpstreamServerInfoUpdate(update) != nil {
		return UpstreamServerInfoUpdate{}, errors.New("upstream authentication probe response invalid")
	}
	return update, nil
}

func readManagedAuthBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, upstreamAuthBodyLimit+1))
	if err != nil {
		return data, errors.New("upstream authentication response read failed")
	}
	if len(data) > upstreamAuthBodyLimit {
		return data, errors.New("upstream authentication response too large")
	}
	return data, nil
}

func extractUpstreamAccessToken(data []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return ""
		}
		name, ok := key.(string)
		if !ok {
			return ""
		}
		if name == "AccessToken" {
			var token string
			if decoder.Decode(&token) == nil {
				return token
			}
			return ""
		}
		var ignored json.RawMessage
		if decoder.Decode(&ignored) != nil {
			return ""
		}
	}
	return ""
}

func upstreamAuthOwnershipMatches(runtime *UpstreamRuntime, update UpstreamAuthUpdate) bool {
	return runtime != nil && runtime.Source.ID == update.SourceID && runtime.Source.AuthGenerationID == update.GenerationID && runtime.Source.ClientIdentity.DeviceID == update.DeviceID && runtime.Source.BackendUserID == update.BackendUserID && runtime.Source.BackendToken == update.BackendToken
}

func (a *upstreamAuthenticator) cleanupInvocation(update UpstreamAuthUpdate, runtime *UpstreamRuntime) {
	if update.BackendToken == "" || runtime == nil || update.BackendToken == runtime.Source.BackendToken {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.cleanupTimeout)
	defer cancel()
	current, err := a.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil || current.Source.BackendToken == update.BackendToken {
		return
	}
	a.cleanupInvocationWithCurrent(ctx, update, runtime, current)
}

func (a *upstreamAuthenticator) cleanupInvocationWithCurrent(ctx context.Context, update UpstreamAuthUpdate, runtime *UpstreamRuntime, current *UpstreamRuntime) {
	if update.BackendToken == "" || runtime == nil || current == nil || update.BackendToken == runtime.Source.BackendToken || current.Source.BackendToken == update.BackendToken {
		return
	}
	_ = a.Logout(managedAuthLogoutRequest{Context: ctx, Snapshot: upstreamRequestSnapshot{baseURL: runtime.Endpoint.BaseURL, userID: update.BackendUserID, token: update.BackendToken, identity: BackendClientIdentity{UserAgent: runtime.Source.ClientIdentity.UserAgent, Client: runtime.Source.ClientIdentity.Client, Device: runtime.Source.ClientIdentity.Device, DeviceID: update.DeviceID, Version: runtime.Source.ClientIdentity.Version}}})
}

func (a *upstreamAuthenticator) retireOld(runtime *UpstreamRuntime) {
	if runtime == nil || runtime.Source.AuthGenerationID == "" || !isTrimmed(runtime.Source.BackendToken) || !isTrimmed(runtime.Source.BackendUserID) || !isTrimmed(runtime.Source.ClientIdentity.DeviceID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.cleanupTimeout)
	defer cancel()
	current, err := a.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil || current.Source.BackendToken == runtime.Source.BackendToken {
		return
	}
	_ = a.Logout(managedAuthLogoutRequest{Context: ctx, Snapshot: upstreamRequestSnapshot{baseURL: runtime.Endpoint.BaseURL, userID: runtime.Source.BackendUserID, token: runtime.Source.BackendToken, identity: runtime.Source.ClientIdentity}})
}

func (a *upstreamAuthenticator) Logout(in managedAuthLogoutRequest) error {
	if err := in.Context.Err(); err != nil {
		return err
	}
	if !isTrimmed(in.Snapshot.token) {
		return fmt.Errorf("%w: invalid managed authentication logout", ErrBadRequest)
	}
	u, err := backendURL(in.Snapshot.baseURL, "/Sessions/Logout")
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(in.Context, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	rewriteManagedAuthHeaders(req.Header, in.Snapshot.identity, in.Snapshot.userID, in.Snapshot.token)
	resp, err := a.doManagedAuth(req, "logout")
	if err != nil {
		_ = closeResponseOnError(resp)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("upstream authentication logout status %d", resp.StatusCode)
	}
	return nil
}

func rewriteManagedAuthHeaders(header http.Header, identity BackendClientIdentity, userID, token string) {
	header.Set("User-Agent", identity.UserAgent)
	header.Set("X-Emby-Token", token)
	header.Set("X-Emby-Authorization", backendAuthHeader(identity, userID, token).String())
}

func (a *upstreamAuthenticator) doManagedAuth(req *http.Request, operation string) (*http.Response, error) {
	started := time.Now()
	resp, err := a.client.Do(req)
	if err == nil {
		wrapResponseBodyOnce(resp)
	}
	if errors.Is(err, ErrUpstreamRedirectRejected) {
		err = ErrUpstreamRedirectRejected
	}
	if a.emit != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		outcome := observe.OutcomeOK
		if err != nil || status < http.StatusOK || status >= http.StatusMultipleChoices {
			outcome = observe.OutcomeError
		}
		a.emit(observe.Event{Kind: observe.KindUpstreamRequest, RouteClass: observe.RouteAuth, Outcome: outcome, StatusClass: observe.StatusClassOf(status), ErrorKind: upstreamPurposeManagedAuth.String() + "_" + operation, Direction: observe.DirectionUpstream, Method: requestMethod(req), DurationMS: time.Since(started).Milliseconds()})
	}
	return resp, err
}

func newUpstreamDeviceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08X-%04X-%04X-%04X-%012X", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func newUpstreamGeneration() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

var _ ManagedAuthUpstream = (*upstreamAuthenticator)(nil)
