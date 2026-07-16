package gateway

import (
	"context"
	"errors"
	"sync"
	"time"
)

type AnonymousImageNamespaceErrorKind uint8

const (
	AnonymousImageNamespaceStaticError AnonymousImageNamespaceErrorKind = iota + 1
	AnonymousImageNamespaceMismatchError
	AnonymousImageNamespaceTransientError
)

type AnonymousImageNamespaceError struct {
	Kind AnonymousImageNamespaceErrorKind
	Err  error
}

func (e *AnonymousImageNamespaceError) Error() string { return e.Err.Error() }
func (e *AnonymousImageNamespaceError) Unwrap() error { return e.Err }
func IsAnonymousImageNamespaceTransient(err error) bool {
	var e *AnonymousImageNamespaceError
	return errors.As(err, &e) && e.Kind == AnonymousImageNamespaceTransientError
}

const anonymousImageNamespaceMaxAge = 2 * time.Hour

type AnonymousImageOrigin struct {
	BaseURL         string
	ClientIdentity  BackendClientIdentity
	BackendServerID string
}
type anonymousImageNamespaceKey struct{ sourceID, serverID, endpointID, baseURL, userAgent, client, device, deviceID, version string }
type anonymousImageNamespaceSnapshot struct {
	origin      AnonymousImageOrigin
	key         anonymousImageNamespaceKey
	validatedAt time.Time
	available   bool
	diagnostic  string
}
type anonymousImageNamespaceState struct {
	mu         sync.RWMutex
	validateMu sync.Mutex
	snapshot   anonymousImageNamespaceSnapshot
}

func anonymousImageNamespaceKeyFor(runtime *UpstreamRuntime) (anonymousImageNamespaceKey, error) {
	if runtime == nil || ValidateUpstreamRuntime(*runtime) != nil {
		return anonymousImageNamespaceKey{}, errors.New("invalid singleton upstream topology")
	}
	i := runtime.Source.ClientIdentity
	return anonymousImageNamespaceKey{runtime.Source.ID, runtime.Source.ServerID, runtime.Endpoint.ID, runtime.Endpoint.BaseURL, i.UserAgent, i.Client, i.Device, i.DeviceID, i.Version}, nil
}
func anonymousImageOriginFor(runtime *UpstreamRuntime) AnonymousImageOrigin {
	return AnonymousImageOrigin{BaseURL: runtime.Endpoint.BaseURL, ClientIdentity: runtime.Source.ClientIdentity, BackendServerID: runtime.Source.ServerID}
}

func (s *Server) ValidateAnonymousImageNamespace(ctx context.Context) error {
	s.anonymousImages.validateMu.Lock()
	defer s.anonymousImages.validateMu.Unlock()
	runtime, err := s.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceStaticError, "anonymous image singleton topology unavailable")
	}
	key, err := anonymousImageNamespaceKeyFor(runtime)
	if err != nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceStaticError, "anonymous image singleton topology invalid")
	}
	if _, err := s.probeUpstreamPublic(ctx, runtime); err != nil {
		kind := AnonymousImageNamespaceTransientError
		if errors.Is(err, ErrUpstreamServerInfoConflict) {
			kind = AnonymousImageNamespaceMismatchError
		}
		return s.setAnonymousImageNamespaceFailure(kind, "anonymous image active endpoint probe unavailable")
	}
	current, err := s.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceTransientError, "anonymous image singleton changed during validation")
	}
	currentKey, err := anonymousImageNamespaceKeyFor(current)
	if err != nil || currentKey != key {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceTransientError, "anonymous image singleton changed during validation")
	}
	s.setAnonymousImageNamespaceSnapshot(anonymousImageNamespaceSnapshot{origin: anonymousImageOriginFor(current), key: key, validatedAt: s.anonymousImageNow().UTC(), available: true, diagnostic: "available"})
	return nil
}
func (s *Server) setAnonymousImageNamespaceFailure(kind AnonymousImageNamespaceErrorKind, message string) error {
	s.setAnonymousImageNamespaceSnapshot(anonymousImageNamespaceSnapshot{diagnostic: message})
	return &AnonymousImageNamespaceError{Kind: kind, Err: errors.New(message)}
}
func (s *Server) setAnonymousImageNamespaceSnapshot(snapshot anonymousImageNamespaceSnapshot) {
	s.anonymousImages.mu.Lock()
	s.anonymousImages.snapshot = snapshot
	s.anonymousImages.mu.Unlock()
}
func (s *Server) ValidatedAnonymousImageOrigin(ctx context.Context) (AnonymousImageOrigin, bool) {
	s.anonymousImages.mu.RLock()
	snapshot := s.anonymousImages.snapshot
	s.anonymousImages.mu.RUnlock()
	if !snapshot.available || s.anonymousImageNow().Sub(snapshot.validatedAt) > anonymousImageNamespaceMaxAge {
		return AnonymousImageOrigin{}, false
	}
	runtime, err := s.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil {
		return AnonymousImageOrigin{}, false
	}
	key, err := anonymousImageNamespaceKeyFor(runtime)
	if err != nil || key != snapshot.key {
		return AnonymousImageOrigin{}, false
	}
	return snapshot.origin, true
}
func (s *Server) AnonymousImageNamespaceDiagnostic() string {
	s.anonymousImages.mu.RLock()
	defer s.anonymousImages.mu.RUnlock()
	return s.anonymousImages.snapshot.diagnostic
}
func (s *Server) AnonymousImageNamespaceValidatedAt() time.Time {
	s.anonymousImages.mu.RLock()
	defer s.anonymousImages.mu.RUnlock()
	return s.anonymousImages.snapshot.validatedAt
}
func (s *Server) AnonymousImageNamespaceFingerprintChanged(ctx context.Context) bool {
	runtime, err := s.store.LoadDefaultUpstreamRuntime(ctx)
	if err != nil {
		return true
	}
	key, err := anonymousImageNamespaceKeyFor(runtime)
	s.anonymousImages.mu.RLock()
	snapshot := s.anonymousImages.snapshot
	s.anonymousImages.mu.RUnlock()
	return err != nil || key != snapshot.key
}
func anonymousImageNamespaceFingerprint(_ []EmbyServer) string { return "" }
