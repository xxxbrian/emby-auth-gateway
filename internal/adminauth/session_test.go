package adminauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionCreateGetTouchDelete(t *testing.T) {
	s := NewStore(10)
	fixed := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }

	sess, err := s.Create("jwt-token", Claims{SuperuserID: "su1", Email: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" || sess.CSRF == "" || sess.Token != "jwt-token" {
		t.Fatalf("bad session: %+v", sess)
	}

	got, err := s.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "a@example.com" {
		t.Fatalf("email=%q", got.Email)
	}

	fixed = fixed.Add(time.Minute)
	touched, err := s.Touch(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !touched.LastSeen.Equal(fixed) {
		t.Fatalf("last_seen=%v want %v", touched.LastSeen, fixed)
	}

	s.Delete(sess.ID)
	if _, err := s.Get(sess.ID); err != ErrSessionMissing {
		t.Fatalf("err=%v want missing", err)
	}
}

func TestSessionAbsoluteAndIdleExpiry(t *testing.T) {
	s := NewStore(10)
	start := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	now := start
	s.now = func() time.Time { return now }

	sess, err := s.Create("jwt", Claims{SuperuserID: "su1", Email: "a@x"})
	if err != nil {
		t.Fatal(err)
	}

	// Idle expiry.
	now = start.Add(IdleTTL + time.Second)
	if _, err := s.Get(sess.ID); err != ErrSessionExpired {
		t.Fatalf("idle err=%v", err)
	}

	// Absolute expiry (with touches).
	sess, err = s.Create("jwt2", Claims{SuperuserID: "su1", Email: "a@x"})
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(IdleTTL + time.Second) // recreate at "now"
	// recreate properly
	s = NewStore(10)
	now = start
	s.now = func() time.Time { return now }
	sess, err = s.Create("jwt3", Claims{SuperuserID: "su1", Email: "a@x"})
	if err != nil {
		t.Fatal(err)
	}
	// Keep touching within idle window until absolute TTL.
	for i := 0; i < 20; i++ {
		now = now.Add(IdleTTL / 2)
		if now.Sub(start) > AbsoluteTTL {
			break
		}
		if _, err := s.Touch(sess.ID); err != nil {
			t.Fatalf("touch %d: %v", i, err)
		}
	}
	now = start.Add(AbsoluteTTL + time.Second)
	if _, err := s.Get(sess.ID); err != ErrSessionExpired {
		t.Fatalf("absolute err=%v", err)
	}
}

func TestSessionCapacity(t *testing.T) {
	s := NewStore(2)
	if _, err := s.Create("a", Claims{SuperuserID: "1", Email: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("b", Claims{SuperuserID: "2", Email: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("c", Claims{SuperuserID: "3", Email: "c"}); err != ErrSessionFull {
		t.Fatalf("err=%v want full", err)
	}
}

func TestCSRFValidation(t *testing.T) {
	s := NewStore(5)
	sess, err := s.Create("jwt", Claims{SuperuserID: "su", Email: "e"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateCSRF(sess.ID, sess.CSRF); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateCSRF(sess.ID, "wrong"); err != ErrCSRFMismatch {
		t.Fatalf("err=%v", err)
	}
	if err := s.ValidateCSRF("missing", "x"); err != ErrSessionMissing {
		t.Fatalf("err=%v", err)
	}
}

func TestReauthTicket(t *testing.T) {
	s := NewStore(5)
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	sess, err := s.Create("jwt", Claims{SuperuserID: "su", Email: "e"})
	if err != nil {
		t.Fatal(err)
	}
	ticket, exp, err := s.IssueReauth(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ticket == "" || !exp.Equal(now.Add(ReauthTTL)) {
		t.Fatalf("ticket=%q exp=%v", ticket, exp)
	}
	if err := s.ConsumeReauth(sess.ID, ticket); err != nil {
		t.Fatal(err)
	}
	// Single use.
	if err := s.ConsumeReauth(sess.ID, ticket); err != ErrReauthInvalid {
		t.Fatalf("reuse err=%v", err)
	}

	ticket, _, err = s.IssueReauth(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(ReauthTTL + time.Second)
	if err := s.ConsumeReauth(sess.ID, ticket); err != ErrReauthInvalid {
		t.Fatalf("expired err=%v", err)
	}
}

func TestCookieNameSecureVsDev(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://localhost/admin", nil)
	if CookieName(req) != CookieDev {
		t.Fatalf("dev name=%q", CookieName(req))
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	if CookieName(req) != CookieSecure {
		t.Fatalf("secure name=%q", CookieName(req))
	}
}

func TestRateLimiter(t *testing.T) {
	r := NewRateLimiter(2, time.Minute)
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	if !r.Allow("ip1") || !r.Allow("ip1") {
		t.Fatal("first two should allow")
	}
	if r.Allow("ip1") {
		t.Fatal("third should deny")
	}
	if !r.Allow("ip2") {
		t.Fatal("other key should allow")
	}
	now = now.Add(time.Minute)
	if !r.Allow("ip1") {
		t.Fatal("new window should allow")
	}
}
