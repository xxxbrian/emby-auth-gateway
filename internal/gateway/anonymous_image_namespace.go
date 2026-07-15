package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
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

const anonymousImageNamespaceMaxAge = 2 * time.Hour

func (e *AnonymousImageNamespaceError) Error() string { return e.Err.Error() }
func (e *AnonymousImageNamespaceError) Unwrap() error { return e.Err }

func IsAnonymousImageNamespaceTransient(err error) bool {
	var namespaceErr *AnonymousImageNamespaceError
	return errors.As(err, &namespaceErr) && namespaceErr.Kind == AnonymousImageNamespaceTransientError
}

type AnonymousImageOrigin struct {
	BaseURL         string
	ClientIdentity  BackendClientIdentity
	BackendServerID string
}

type anonymousImageNamespaceSnapshot struct {
	origin      AnonymousImageOrigin
	fingerprint string
	generation  uint64
	validatedAt time.Time
	available   bool
	diagnostic  string
}

type anonymousImageNamespaceState struct {
	mu         sync.RWMutex
	validateMu sync.Mutex
	snapshot   anonymousImageNamespaceSnapshot
}

func (s *Server) anonymousImageConfigEnabled() bool {
	return s.cfg.AnonymousImageConfigured || s.cfg.AnonymousImageServerRecordID != "" || s.cfg.AnonymousImageBackendServerID != ""
}

// ValidateAnonymousImageNamespace strictly validates the configured anonymous
// image ingress. It never sends a user, backend-account token, or cookie.
func (s *Server) ValidateAnonymousImageNamespace(ctx context.Context) error {
	s.anonymousImages.validateMu.Lock()
	defer s.anonymousImages.validateMu.Unlock()
	if !s.anonymousImageConfigEnabled() {
		s.setAnonymousImageNamespaceSnapshot(anonymousImageNamespaceSnapshot{diagnostic: "disabled"})
		return nil
	}
	selectedID := strings.TrimSpace(s.cfg.AnonymousImageServerRecordID)
	expectedID := strings.TrimSpace(s.cfg.AnonymousImageBackendServerID)
	if selectedID == "" || expectedID == "" {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceStaticError, "anonymous image configuration requires both server record and backend server IDs")
	}
	servers, err := s.store.ListEnabledServers(ctx)
	if err != nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceTransientError, "anonymous image namespace store unavailable")
	}
	fingerprint := anonymousImageNamespaceFingerprint(servers)
	var selected *EmbyServer
	for i := range servers {
		server := &servers[i]
		storedID := server.BackendServerID
		if strings.TrimSpace(storedID) != "" && storedID != expectedID {
			return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceMismatchError, "anonymous image namespace stored backend server ID mismatch")
		}
		if _, err := validAnonymousImageBaseURL(server.BaseURL); err != nil {
			return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceStaticError, "anonymous image namespace has an invalid enabled server base URL")
		}
		if server.ID == selectedID {
			selected = server
		}
	}
	if selected == nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceStaticError, "anonymous image selected server is absent or disabled")
	}
	for _, server := range servers {
		if err := s.probeAnonymousImageServer(ctx, server, expectedID); err != nil {
			s.setAnonymousImageNamespaceSnapshot(anonymousImageNamespaceSnapshot{fingerprint: fingerprint, diagnostic: "probe unavailable"})
			return err
		}
	}
	s.setAnonymousImageNamespaceSnapshot(anonymousImageNamespaceSnapshot{
		origin:      AnonymousImageOrigin{BaseURL: selected.BaseURL, ClientIdentity: selected.ClientIdentity.WithDefaults(), BackendServerID: expectedID},
		fingerprint: fingerprint,
		validatedAt: s.anonymousImageNow().UTC(),
		available:   true,
		diagnostic:  "available",
	})
	return nil
}

func validAnonymousImageBaseURL(value string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) {
		return nil, errors.New("invalid base URL")
	}
	return u, nil
}

func (s *Server) probeAnonymousImageServer(ctx context.Context, server EmbyServer, expectedID string) error {
	u, err := backendURL(server.BaseURL, "/System/Info/Public")
	if err != nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceStaticError, "anonymous image namespace has an invalid enabled server base URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceTransientError, "anonymous image namespace probe unavailable")
	}
	identity := server.ClientIdentity.WithDefaults()
	req.Header.Set("User-Agent", identity.UserAgent)
	req.Header.Set("X-Emby-Authorization", backendAuthHeader(identity, "", "").String())
	client := *s.client
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceTransientError, "anonymous image namespace probe unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceTransientError, "anonymous image namespace probe unavailable")
	}
	var body struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceTransientError, "anonymous image namespace probe unavailable")
	}
	if strings.TrimSpace(body.ID) == "" {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceStaticError, "anonymous image namespace probe returned no backend server ID")
	}
	if body.ID != expectedID {
		return s.setAnonymousImageNamespaceFailure(AnonymousImageNamespaceMismatchError, "anonymous image namespace live backend server ID mismatch")
	}
	return nil
}

func (s *Server) setAnonymousImageNamespaceFailure(kind AnonymousImageNamespaceErrorKind, message string) error {
	s.setAnonymousImageNamespaceSnapshot(anonymousImageNamespaceSnapshot{diagnostic: message})
	return &AnonymousImageNamespaceError{Kind: kind, Err: errors.New(message)}
}

func (s *Server) setAnonymousImageNamespaceSnapshot(snapshot anonymousImageNamespaceSnapshot) {
	s.anonymousImages.mu.Lock()
	snapshot.generation = s.anonymousImages.snapshot.generation + 1
	s.anonymousImages.snapshot = snapshot
	s.anonymousImages.mu.Unlock()
}

// ValidatedAnonymousImageOrigin returns an origin only while the enabled-server
// fingerprint still matches the strict validation snapshot.
func (s *Server) ValidatedAnonymousImageOrigin(ctx context.Context) (AnonymousImageOrigin, bool) {
	s.anonymousImages.mu.RLock()
	snapshot := s.anonymousImages.snapshot
	s.anonymousImages.mu.RUnlock()
	if !snapshot.available {
		return AnonymousImageOrigin{}, false
	}
	if s.anonymousImageNow().Sub(snapshot.validatedAt) > anonymousImageNamespaceMaxAge {
		s.invalidateAnonymousImageNamespace(snapshot.generation, snapshot.fingerprint, "validation expired")
		return AnonymousImageOrigin{}, false
	}
	servers, err := s.store.ListEnabledServers(ctx)
	if err != nil || anonymousImageNamespaceFingerprint(servers) != snapshot.fingerprint {
		s.invalidateAnonymousImageNamespace(snapshot.generation, snapshot.fingerprint, "configuration changed")
		return AnonymousImageOrigin{}, false
	}
	return snapshot.origin, true
}

func (s *Server) invalidateAnonymousImageNamespace(generation uint64, fingerprint, diagnostic string) {
	s.anonymousImages.mu.Lock()
	if s.anonymousImages.snapshot.generation == generation && s.anonymousImages.snapshot.fingerprint == fingerprint {
		s.anonymousImages.snapshot.available = false
		s.anonymousImages.snapshot.diagnostic = diagnostic
	}
	s.anonymousImages.mu.Unlock()
}

func (s *Server) AnonymousImageNamespaceFingerprintChanged(ctx context.Context) bool {
	s.anonymousImages.mu.RLock()
	snapshot := s.anonymousImages.snapshot
	s.anonymousImages.mu.RUnlock()
	servers, err := s.store.ListEnabledServers(ctx)
	return err != nil || snapshot.fingerprint != anonymousImageNamespaceFingerprint(servers)
}

func anonymousImageNamespaceFingerprint(servers []EmbyServer) string {
	entries := make([]string, 0, len(servers))
	for _, server := range servers {
		identity := server.ClientIdentity.WithDefaults()
		entries = append(entries, strings.Join([]string{server.ID, server.BaseURL, server.BackendServerID, identity.UserAgent, identity.Client, identity.Device, identity.DeviceID, identity.Version}, "\x00"))
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\x01")))
	return hex.EncodeToString(sum[:])
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
