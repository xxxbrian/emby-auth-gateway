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

	"golang.org/x/sync/singleflight"
)

const (
	upstreamAuthTimeout       = 15 * time.Second
	upstreamCleanupTimeout    = 2 * time.Second
	upstreamAuthBodyLimit     = 1 << 20
	upstreamGeneratorAttempts = 4
)

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
}

func newUpstreamAuthenticator(store upstreamAuthStore, client *http.Client) *upstreamAuthenticator {
	if client == nil {
		client = http.DefaultClient
	}
	cloned := *client
	cloned.Jar = nil
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &upstreamAuthenticator{
		store: store, client: &cloned, clock: func() time.Time { return time.Now().UTC() },
		deviceID: newUpstreamDeviceID, generation: newUpstreamGeneration, authTimeout: upstreamAuthTimeout, cleanupTimeout: upstreamCleanupTimeout,
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
	deviceID, generation, err := a.freshIdentifiers(runtime.Source)
	if err != nil {
		return nil, err
	}
	loginCtx, cancel := context.WithTimeout(ctx, a.authTimeout)
	result, loginErr := a.login(loginCtx, runtime, deviceID)
	cancel()
	if loginErr != nil {
		a.cleanupInvocation(result, runtime, deviceID)
		return nil, a.leaderError(ctx, loginErr)
	}
	if result.Token == runtime.Source.BackendToken {
		return nil, errors.New("upstream authentication token collision")
	}
	if err := ctx.Err(); err != nil {
		a.cleanupInvocation(result, runtime, deviceID)
		return nil, a.leaderError(ctx, err)
	}
	update := UpstreamAuthUpdate{SourceID: runtime.Source.ID, ExpectedGenerationID: runtime.Source.AuthGenerationID, GenerationID: generation, DeviceID: deviceID, BackendUserID: result.UserID, BackendToken: result.Token, AuthenticatedAt: a.clock().UTC()}
	if err := a.store.CompareAndSwapUpstreamAuth(ctx, update); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), a.cleanupTimeout)
		defer cancel()
		current, reloadErr := a.store.LoadDefaultUpstreamRuntime(cleanupCtx)
		if reloadErr == nil && upstreamAuthOwnershipMatches(current, update) {
			return current, nil
		}
		if errors.Is(err, ErrUpstreamAuthConflict) && reloadErr == nil {
			a.cleanupInvocationWithCurrent(cleanupCtx, result, runtime, deviceID, current)
			return current, nil
		}
		if reloadErr == nil {
			a.cleanupInvocationWithCurrent(cleanupCtx, result, runtime, deviceID, current)
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
	resp, err := a.client.Do(req)
	if err != nil {
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

func (a *upstreamAuthenticator) cleanupInvocation(result upstreamLoginResult, runtime *UpstreamRuntime, deviceID string) {
	if result.Token == "" || runtime == nil || result.Token == runtime.Source.BackendToken {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.cleanupTimeout)
	defer cancel()
	current, err := a.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil || current.Source.BackendToken == result.Token {
		return
	}
	a.cleanupInvocationWithCurrent(ctx, result, runtime, deviceID, current)
}

func (a *upstreamAuthenticator) cleanupInvocationWithCurrent(ctx context.Context, result upstreamLoginResult, runtime *UpstreamRuntime, deviceID string, current *UpstreamRuntime) {
	if result.Token == "" || runtime == nil || current == nil || result.Token == runtime.Source.BackendToken || current.Source.BackendToken == result.Token {
		return
	}
	a.logout(ctx, runtime.Endpoint, runtime.Source.ClientIdentity, deviceID, result.UserID, result.Token)
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
	a.logout(ctx, runtime.Endpoint, runtime.Source.ClientIdentity, runtime.Source.ClientIdentity.DeviceID, runtime.Source.BackendUserID, runtime.Source.BackendToken)
}

func (a *upstreamAuthenticator) logout(ctx context.Context, endpoint UpstreamEndpoint, identity BackendClientIdentity, deviceID, userID, token string) {
	if !isTrimmed(token) {
		return
	}
	u, err := backendURL(endpoint.BaseURL, "/Sessions/Logout")
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return
	}
	identity.DeviceID = deviceID
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("X-Emby-Authorization", backendAuthHeader(identity, userID, token).String())
	resp, err := a.client.Do(req)
	if err == nil && resp != nil {
		resp.Body.Close()
	}
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
