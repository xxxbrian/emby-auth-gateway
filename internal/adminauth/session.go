// Package adminauth provides opaque in-memory superuser admin sessions.
package adminauth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// CookieSecure is used when the request is TLS (or X-Forwarded-Proto=https).
	CookieSecure = "__Secure-eag_admin_session"
	// CookieDev is used for local non-TLS development/tests.
	CookieDev = "eag_admin_session"

	CookiePath = "/admin"

	AbsoluteTTL = 8 * time.Hour
	IdleTTL     = 30 * time.Minute
	ReauthTTL   = 5 * time.Minute

	DefaultMaxSessions = 100

	CSRFHeader = "X-CSRF-Token"
	ReauthHeader = "X-Admin-Reauth"
)

var (
	ErrSessionFull    = errors.New("admin session capacity full")
	ErrSessionMissing = errors.New("admin session not found")
	ErrSessionExpired = errors.New("admin session expired")
	ErrCSRFMismatch   = errors.New("csrf token mismatch")
	ErrReauthInvalid  = errors.New("reauth ticket invalid or expired")
)

// Claims are superuser identity fields stored with the session.
type Claims struct {
	SuperuserID string
	Email       string
}

// Session is an opaque admin browser session (JWT held server-side only).
type Session struct {
	ID          string
	Token       string // PocketBase superuser JWT
	CSRF        string
	SuperuserID string
	Email       string
	Created     time.Time
	LastSeen    time.Time
}

// Public is the non-secret session view returned to the client.
type Public struct {
	SuperuserID string    `json:"superuser_id"`
	Email       string    `json:"email"`
	CSRF        string    `json:"csrf"`
	Created     time.Time `json:"created"`
	LastSeen    time.Time `json:"last_seen"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Store is an in-memory opaque session store.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
	reauth   map[string]*reauthTicket
	max      int
	now      func() time.Time
}

type reauthTicket struct {
	SessionID string
	Expires   time.Time
}

// NewStore creates a session store with the given max capacity.
func NewStore(maxSessions int) *Store {
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessions
	}
	return &Store{
		sessions: make(map[string]*Session),
		reauth:   make(map[string]*reauthTicket),
		max:      maxSessions,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Create stores a new session for the given PB JWT and claims.
func (s *Store) Create(jwt string, claims Claims) (*Session, error) {
	jwt = strings.TrimSpace(jwt)
	if jwt == "" {
		return nil, errors.New("token is required")
	}
	if strings.TrimSpace(claims.SuperuserID) == "" {
		return nil, errors.New("superuser id is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if len(s.sessions) >= s.max {
		return nil, ErrSessionFull
	}

	id, err := randomID(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomID(32)
	if err != nil {
		return nil, err
	}
	now := s.now()
	sess := &Session{
		ID:          id,
		Token:       jwt,
		CSRF:        csrf,
		SuperuserID: claims.SuperuserID,
		Email:       claims.Email,
		Created:     now,
		LastSeen:    now,
	}
	s.sessions[id] = sess
	return cloneSession(sess), nil
}

// Get returns a live session by id (does not touch last_seen).
func (s *Store) Get(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		// Opportunistic cleanup of other expired sessions.
		s.expireLocked()
		return nil, ErrSessionMissing
	}
	if s.isExpiredLocked(sess) {
		delete(s.sessions, id)
		s.expireLocked()
		return nil, ErrSessionExpired
	}
	return cloneSession(sess), nil
}

// Touch updates last_seen and returns the session.
func (s *Store) Touch(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		s.expireLocked()
		return nil, ErrSessionMissing
	}
	if s.isExpiredLocked(sess) {
		delete(s.sessions, id)
		s.expireLocked()
		return nil, ErrSessionExpired
	}
	sess.LastSeen = s.now()
	return cloneSession(sess), nil
}

// Delete removes a session.
func (s *Store) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	// Drop reauth tickets bound to this session.
	for tid, t := range s.reauth {
		if t.SessionID == id {
			delete(s.reauth, tid)
		}
	}
}

// ValidateCSRF checks the CSRF token for a session.
func (s *Store) ValidateCSRF(id, csrf string) error {
	sess, err := s.Get(id)
	if err != nil {
		return err
	}
	if csrf == "" || csrf != sess.CSRF {
		return ErrCSRFMismatch
	}
	return nil
}

// IssueReauth creates a short-lived reauth ticket bound to the session.
func (s *Store) IssueReauth(sessionID string) (ticket string, expires time.Time, err error) {
	if _, err := s.Get(sessionID); err != nil {
		return "", time.Time{}, err
	}
	ticket, err = randomID(32)
	if err != nil {
		return "", time.Time{}, err
	}
	exp := s.now().Add(ReauthTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reauth[ticket] = &reauthTicket{SessionID: sessionID, Expires: exp}
	return ticket, exp, nil
}

// ConsumeReauth validates and consumes a reauth ticket for the given session.
func (s *Store) ConsumeReauth(sessionID, ticket string) error {
	ticket = strings.TrimSpace(ticket)
	if ticket == "" {
		return ErrReauthInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	t, ok := s.reauth[ticket]
	if !ok || t.SessionID != sessionID || !t.Expires.After(s.now()) {
		delete(s.reauth, ticket)
		return ErrReauthInvalid
	}
	delete(s.reauth, ticket)
	return nil
}

// PublicView returns the client-facing session info.
func (s *Session) PublicView() Public {
	if s == nil {
		return Public{}
	}
	abs := s.Created.Add(AbsoluteTTL)
	idle := s.LastSeen.Add(IdleTTL)
	exp := abs
	if idle.Before(exp) {
		exp = idle
	}
	return Public{
		SuperuserID: s.SuperuserID,
		Email:       s.Email,
		CSRF:        s.CSRF,
		Created:     s.Created,
		LastSeen:    s.LastSeen,
		ExpiresAt:   exp,
	}
}

// CookieName returns the cookie name for the request transport.
func CookieName(r *http.Request) string {
	if isSecureRequest(r) {
		return CookieSecure
	}
	return CookieDev
}

// ReadSessionID extracts the admin session id from the request cookie.
func ReadSessionID(r *http.Request) string {
	if r == nil {
		return ""
	}
	// Prefer secure name, then dev name.
	for _, name := range []string{CookieSecure, CookieDev} {
		if c, err := r.Cookie(name); err == nil && c != nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// SetSessionCookie writes the session cookie.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string, maxAge int) {
	secure := isSecureRequest(r)
	name := CookieDev
	if secure {
		name = CookieSecure
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    sessionID,
		Path:     CookiePath,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
}

// ClearSessionCookie clears both possible cookie names.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	secure := isSecureRequest(r)
	for _, name := range []string{CookieSecure, CookieDev} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     CookiePath,
			HttpOnly: true,
			Secure:   secure || name == CookieSecure,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   -1,
		})
	}
}

func isSecureRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Store) expireLocked() {
	now := s.now()
	for id, sess := range s.sessions {
		if s.isExpiredAt(sess, now) {
			delete(s.sessions, id)
		}
	}
	for id, t := range s.reauth {
		if !t.Expires.After(now) {
			delete(s.reauth, id)
		}
	}
}

func (s *Store) isExpiredLocked(sess *Session) bool {
	return s.isExpiredAt(sess, s.now())
}

func (s *Store) isExpiredAt(sess *Session, now time.Time) bool {
	if now.Sub(sess.Created) > AbsoluteTTL {
		return true
	}
	if now.Sub(sess.LastSeen) > IdleTTL {
		return true
	}
	return false
}

func cloneSession(s *Session) *Session {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}

func randomID(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Len returns the number of live sessions (for tests/metrics).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	return len(s.sessions)
}
