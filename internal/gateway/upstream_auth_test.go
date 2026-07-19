package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xxxbrian/emby-auth-gateway/internal/observe"
)

func TestUpstreamAuthenticatorEnsureManagedSkipsHTTPAndCAS(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: managedRuntime("old-token")}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected HTTP") }))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(store, server.Client())
	runtime, err := auth.Ensure(context.Background())
	if err != nil || runtime.Source.BackendToken != "old-token" || store.casCalls != 0 {
		t.Fatalf("Ensure = %#v, %v; CAS=%d", runtime, err, store.casCalls)
	}
}

func TestUpstreamAuthenticatorPreContractRotatesWithFreshIdentity(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	var sawHeader, sawBody bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/AuthenticateByName" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		auth := ParseEmbyAuthHeader(r.Header.Get("X-Emby-Authorization"))
		sawHeader = auth.DeviceID == "NEW-DEVICE" && auth.Token == "" && auth.UserID == "" && r.Header.Get("User-Agent") == "agent"
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body) == `{"Pw":"password","Username":"backend"}` || string(body) == `{"Username":"backend","Pw":"password"}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"AccessToken":"new-token","ServerId":"server","User":{"Id":"new-user"}}`))
	}))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(store, server.Client())
	auth.deviceID = func() (string, error) { return "NEW-DEVICE", nil }
	auth.generation = func() (string, error) { return "new-generation", nil }
	auth.clock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("offset", 3600)) }
	runtime, err := auth.Ensure(context.Background())
	if err != nil || !sawHeader || !sawBody || store.casCalls != 1 || runtime.Source.AuthGenerationID != "new-generation" || runtime.Source.ClientIdentity.DeviceID != "NEW-DEVICE" || runtime.Source.TokenUpdatedAt.Location() != time.UTC {
		t.Fatalf("Ensure = %#v, %v header=%v body=%v CAS=%d", runtime, err, sawHeader, sawBody, store.casCalls)
	}
}

func TestUpstreamAuthenticatorRefreshSkipsChangedTokenAndRotatesMatchingToken(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: managedRuntime("current")}
	var logins int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			logins++
		}
		_, _ = w.Write([]byte(`{"AccessToken":"new","ServerId":"server","User":{"Id":"user"}}`))
	}))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(store, server.Client())
	auth.deviceID = func() (string, error) { return "NEW", nil }
	auth.generation = func() (string, error) { return "next", nil }
	if _, err := auth.Refresh(context.Background(), "stale"); err != nil || logins != 0 {
		t.Fatalf("changed Refresh err=%v logins=%d", err, logins)
	}
	if runtime, err := auth.Refresh(context.Background(), "current"); err != nil || logins != 1 || runtime.Source.BackendToken != "new" {
		t.Fatalf("matching Refresh = %#v, %v logins=%d", runtime, err, logins)
	}
}

func TestUpstreamAuthenticatorConcurrentEnsureUsesOneLoginAndCAS(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	started := make(chan struct{})
	allow := make(chan struct{})
	var once sync.Once
	var logins int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			logins++
		}
		once.Do(func() { close(started) })
		<-allow
		_, _ = w.Write([]byte(`{"AccessToken":"new","ServerId":"server","User":{"Id":"user"}}`))
	}))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(store, server.Client())
	auth.deviceID = func() (string, error) { return "NEW", nil }
	auth.generation = func() (string, error) { return "next", nil }
	errs := make(chan error, 2)
	for range 2 {
		go func() { _, err := auth.Ensure(context.Background()); errs <- err }()
	}
	<-started
	close(allow)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}
	if logins != 1 || store.casCalls != 1 {
		t.Fatalf("logins=%d CAS=%d", logins, store.casCalls)
	}
}

func TestUpstreamAuthenticatorCanceledWaiterAndLeaderRetry(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	logins := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		logins++
		attempt := logins
		mu.Unlock()
		if attempt == 1 {
			close(firstStarted)
			select {
			case <-r.Context().Done():
			case <-releaseFirst:
			}
			return
		}
		_, _ = w.Write([]byte(`{"AccessToken":"new","ServerId":"server","User":{"Id":"user"}}`))
	}))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(store, server.Client())
	auth.deviceID = func() (string, error) { return "NEW", nil }
	auth.generation = func() (string, error) { return "next", nil }
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() { _, err := auth.Ensure(leaderCtx); leaderDone <- err }()
	<-firstStarted
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() { _, err := auth.Ensure(waiterCtx); waiterDone <- err }()
	cancelWaiter()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}
	liveDone := make(chan error, 1)
	go func() { _, err := auth.Ensure(context.Background()); liveDone <- err }()
	cancelLeader()
	close(releaseFirst)
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v", err)
	}
	if err := <-liveDone; err != nil {
		t.Fatalf("live waiter retry error = %v", err)
	}
	mu.Lock()
	gotLogins := logins
	mu.Unlock()
	if gotLogins != 2 || store.casCalls != 1 {
		t.Fatalf("logins=%d CAS=%d", gotLogins, store.casCalls)
	}
}

func TestUpstreamAuthenticatorRejectsCollisionAndDoesNotLeakSecrets(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: managedRuntime("old-token")}
	store.runtime.Source.AuthGenerationID = ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"AccessToken":"old-token","ServerId":"server","User":{"Id":"user"}}`))
	}))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(store, server.Client())
	auth.deviceID = func() (string, error) { return "NEW", nil }
	auth.generation = func() (string, error) { return "next", nil }
	_, err := auth.Ensure(context.Background())
	if err == nil || store.casCalls != 0 || strings.Contains(err.Error(), "old-token") || strings.Contains(err.Error(), "backend") || strings.Contains(err.Error(), "password") {
		t.Fatalf("collision error = %v CAS=%d", err, store.casCalls)
	}
}

func TestUpstreamAuthenticatorChildTimeoutDoesNotRetryFlight(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	var mu sync.Mutex
	logins := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		logins++
		mu.Unlock()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	store.runtime.Endpoint.BaseURL = "http://upstream.test"
	auth := newUpstreamAuthenticator(store, client)
	auth.authTimeout = 10 * time.Millisecond
	auth.deviceID = func() (string, error) { return "NEW", nil }
	auth.generation = func() (string, error) { return "next", nil }
	if _, err := auth.Ensure(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	mu.Lock()
	got := logins
	mu.Unlock()
	if got != 1 {
		t.Fatalf("logins=%d, want 1", got)
	}
}

func TestUpstreamAuthenticatorClientDoesNotShareCookies(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	var sawCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie")
		http.SetCookie(w, &http.Cookie{Name: "upstream", Value: "cookie"})
		_, _ = w.Write([]byte(`{"AccessToken":"new","ServerId":"server","User":{"Id":"user"}}`))
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: "ambient", Value: "cookie"}})
	client := &http.Client{Jar: jar}
	store.runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(store, client)
	auth.deviceID = func() (string, error) { return "NEW", nil }
	auth.generation = func() (string, error) { return "next", nil }
	if _, err := auth.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sawCookie != "" || len(jar.Cookies(u)) != 1 || jar.Cookies(u)[0].Name != "ambient" {
		t.Fatalf("cookie isolation failed request=%q cookies=%#v", sawCookie, jar.Cookies(u))
	}
}

func TestUpstreamAuthenticatorAcceptsNewerWinnerAfterCASSuccess(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	store.afterCAS = func(runtime *UpstreamRuntime) {
		runtime.Source.AuthGenerationID = "winner-generation"
		runtime.Source.ClientIdentity.DeviceID = "WINNER"
		runtime.Source.BackendUserID = "winner-user"
		runtime.Source.BackendToken = "winner-token"
		now := time.Now().UTC()
		runtime.Source.TokenUpdatedAt, runtime.Source.LastLoginAt = &now, &now
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"AccessToken":"new","ServerId":"server","User":{"Id":"user"}}`))
	}))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(store, server.Client())
	auth.deviceID = func() (string, error) { return "NEW", nil }
	auth.generation = func() (string, error) { return "submitted", nil }
	runtime, err := auth.Ensure(context.Background())
	if err != nil || runtime.Source.AuthGenerationID != "winner-generation" || runtime.Source.BackendToken != "winner-token" {
		t.Fatalf("newer winner = %#v, %v", runtime, err)
	}
}

func TestExtractUpstreamAccessTokenToleratesTrailingFailure(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"AccessToken":"token","User":`),
		append([]byte(`{"AccessToken":"token","x":"`), make([]byte, upstreamAuthBodyLimit)...),
	} {
		if token := extractUpstreamAccessToken(data); token != "token" {
			t.Fatalf("token = %q", token)
		}
	}
}

func TestUpstreamAuthenticatorRedirectDoesNotLeakLocation(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/AuthenticateByName" {
			t.Fatal("redirect was followed")
		}
		http.Redirect(w, r, "/next?username=sentinel-user&password=sentinel-password&token=sentinel-token", http.StatusFound)
	}))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := configuredAuth(store, server.Client())
	_, err := auth.Ensure(context.Background())
	if !errors.Is(err, ErrUpstreamRedirectRejected) {
		t.Fatalf("redirect error = %v", err)
	}
	for _, secret := range []string{"sentinel-user", "sentinel-password", "sentinel-token", "/next"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("redirect error leaked %q: %v", secret, err)
		}
	}
}

func TestManagedAuthUpstreamExactOperationsAndTelemetry(t *testing.T) {
	runtime := preContractRuntime()
	runtime.Source.AuthGenerationID = "old-generation"
	runtime.Source.BackendUserID = "backend-user"
	runtime.Source.BackendToken = "sentinel-token"
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/Users/AuthenticateByName":
			_, _ = w.Write([]byte(`{"AccessToken":"generated-token","ServerId":"server","User":{"Id":"generated-user"}}`))
		case "/System/Info":
			if r.Header.Get("X-Emby-Token") != "sentinel-token" {
				t.Fatalf("probe token = %q", r.Header.Get("X-Emby-Token"))
			}
			_, _ = w.Write([]byte(`{"Id":"server","ServerName":"name","Version":"1.2.3"}`))
		case "/Sessions/Logout":
			if r.Header.Get("X-Emby-Token") != "generated-token" {
				t.Fatalf("logout token = %q", r.Header.Get("X-Emby-Token"))
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	runtime.Endpoint.BaseURL = server.URL
	var events []observe.Event
	auth := newUpstreamAuthenticator(&fakeUpstreamAuthStore{runtime: runtime}, server.Client(), func(event observe.Event) { events = append(events, event) })
	auth.deviceID = func() (string, error) { return "NEW-DEVICE", nil }
	auth.generation = func() (string, error) { return "new-generation", nil }
	auth.clock = func() time.Time { return time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC) }

	login, err := auth.Login(managedAuthLoginRequest{Context: context.Background(), Runtime: *runtime})
	if err != nil || login.BackendToken != "generated-token" || login.DeviceID != "NEW-DEVICE" {
		t.Fatalf("Login = %#v, %v", login, err)
	}
	probe, err := auth.Probe(managedAuthProbeRequest{Context: context.Background(), Snapshot: *runtime})
	if err != nil || probe.ServerID != "server" || probe.ServerName != "name" || probe.ServerVersion != "1.2.3" {
		t.Fatalf("Probe = %#v, %v", probe, err)
	}
	logoutSnapshot := upstreamRequestSnapshot{baseURL: server.URL, userID: login.BackendUserID, token: login.BackendToken, identity: runtime.Source.ClientIdentity}
	logoutSnapshot.identity.DeviceID = login.DeviceID
	if err := auth.Logout(managedAuthLogoutRequest{Context: context.Background(), Snapshot: logoutSnapshot}); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	wantRequests := []string{"POST /Users/AuthenticateByName", "GET /System/Info", "POST /Sessions/Logout"}
	if strings.Join(requests, "|") != strings.Join(wantRequests, "|") {
		t.Fatalf("requests = %#v", requests)
	}
	wantPurposes := []string{"managed_auth_login", "managed_auth_probe", "managed_auth_logout"}
	if len(events) != len(wantPurposes) {
		t.Fatalf("events = %#v", events)
	}
	for i, event := range events {
		if event.Kind != observe.KindUpstreamRequest || event.RouteClass != observe.RouteAuth || event.Direction != observe.DirectionUpstream || event.Outcome != observe.OutcomeOK || event.ErrorKind != wantPurposes[i] {
			t.Fatalf("event[%d] = %#v", i, event)
		}
		serialized := fmt.Sprintf("%#v", event)
		for _, secret := range []string{"backend", "password", "sentinel-token", "generated-token", "generated-user"} {
			if strings.Contains(serialized, secret) {
				t.Fatalf("telemetry leaked %q: %s", secret, serialized)
			}
		}
	}
}

func TestManagedAuthUpstreamRejectsRedirectsForEveryOperation(t *testing.T) {
	redirected := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			redirected = true
			return
		}
		http.Redirect(w, r, "/redirected?token=sentinel", http.StatusFound)
	}))
	defer server.Close()
	runtime := managedRuntime("token")
	runtime.Endpoint.BaseURL = server.URL
	auth := configuredAuth(&fakeUpstreamAuthStore{runtime: runtime}, server.Client())

	operations := []struct {
		name string
		run  func() error
	}{
		{"login", func() error {
			_, err := auth.Login(managedAuthLoginRequest{Context: context.Background(), Runtime: *runtime})
			return err
		}},
		{"probe", func() error {
			_, err := auth.Probe(managedAuthProbeRequest{Context: context.Background(), Snapshot: *runtime})
			return err
		}},
		{"logout", func() error {
			return auth.Logout(managedAuthLogoutRequest{Context: context.Background(), Snapshot: upstreamRequestSnapshot{baseURL: server.URL, token: "generated", identity: runtime.Source.ClientIdentity}})
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, ErrUpstreamRedirectRejected) {
				t.Fatalf("redirect error = %v", err)
			}
		})
	}
	if redirected {
		t.Fatal("managed authentication followed a redirect")
	}
}

func TestManagedAuthProbeBoundsBodyAndHonorsCancellation(t *testing.T) {
	runtime := managedRuntime("token")
	runtime.Endpoint.BaseURL = "http://upstream.test"
	closed := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: r, Body: &closeTrackingReader{Reader: strings.NewReader(strings.Repeat("x", upstreamAuthBodyLimit+1)), closed: &closed}}, nil
	})}
	auth := newUpstreamAuthenticator(&fakeUpstreamAuthStore{runtime: runtime}, client)
	if _, err := auth.Probe(managedAuthProbeRequest{Context: context.Background(), Snapshot: *runtime}); err == nil || !strings.Contains(err.Error(), "too large") || !closed {
		t.Fatalf("oversized probe error=%v closed=%v", err, closed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := auth.Probe(managedAuthProbeRequest{Context: ctx, Snapshot: *runtime}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe error = %v", err)
	}
}

func TestManagedAuthProbeReportsLiteralUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/System/Info" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	runtime := managedRuntime("failed-token")
	runtime.Endpoint.BaseURL = server.URL
	auth := newUpstreamAuthenticator(&fakeUpstreamAuthStore{runtime: runtime}, server.Client())
	if _, err := auth.Probe(managedAuthProbeRequest{Context: context.Background(), Snapshot: *runtime}); !errors.Is(err, errManagedAuthUnauthorized) {
		t.Fatalf("probe error = %v", err)
	}
}

func TestManagedAuthWrapsSuccessfulResponseBodyOnce(t *testing.T) {
	body := &adapterCloseCountingBody{Reader: strings.NewReader("ok")}
	auth := newUpstreamAuthenticator(nil, &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})})
	req, err := http.NewRequest(http.MethodGet, "http://backend.test/System/Info", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := auth.doManagedAuth(req, "probe")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	_ = resp.Body.Close()
	if body.closes != 1 {
		t.Fatalf("managed auth response closes=%d, want 1", body.closes)
	}
}

type closeTrackingReader struct {
	io.Reader
	closed *bool
}

func (r *closeTrackingReader) Close() error {
	*r.closed = true
	return nil
}

func TestUpstreamAuthenticatorFailureTokensAreGuardedlyLoggedOut(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"malformed", http.StatusOK, `{"AccessToken":"invoke","ServerId":`},
		{"non-2xx", http.StatusUnauthorized, `{"AccessToken":"invoke"}`},
		{"oversized", http.StatusOK, `{"AccessToken":"invoke","x":"` + strings.Repeat("x", upstreamAuthBodyLimit)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
			store.runtime.Source.BackendToken = "old"
			var logoutToken, logoutDevice, logoutUser string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/Sessions/Logout" {
					logoutToken = r.Header.Get("X-Emby-Token")
					auth := ParseEmbyAuthHeader(r.Header.Get("X-Emby-Authorization"))
					logoutDevice, logoutUser = auth.DeviceID, auth.UserID
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			store.runtime.Endpoint.BaseURL = server.URL
			auth := configuredAuth(store, server.Client())
			if _, err := auth.Ensure(context.Background()); err == nil {
				t.Fatal("expected authentication failure")
			}
			if logoutToken != "invoke" || logoutDevice != "NEW" || (tc.name == "malformed" && logoutUser != "") {
				t.Fatalf("logout token=%q device=%q user=%q", logoutToken, logoutDevice, logoutUser)
			}
		})
	}
}

func TestUpstreamAuthenticatorCASConflictAndAmbiguousOwnership(t *testing.T) {
	t.Run("conflict cleans loser", func(t *testing.T) {
		store := &fakeUpstreamAuthStore{runtime: preContractRuntime(), casErr: ErrUpstreamAuthConflict}
		store.casHook = func(runtime *UpstreamRuntime) {
			now := time.Now().UTC()
			runtime.Source.AuthGenerationID = "winner-generation"
			runtime.Source.BackendToken = "winner"
			runtime.Source.BackendUserID = "winner-user"
			runtime.Source.ClientIdentity.DeviceID = "WINNER"
			runtime.Source.TokenUpdatedAt, runtime.Source.LastLoginAt = &now, &now
		}
		var logout string
		server := lifecycleServer(t, &logout)
		defer server.Close()
		store.runtime.Endpoint.BaseURL = server.URL
		if runtime, err := configuredAuth(store, server.Client()).Ensure(context.Background()); err != nil || runtime.Source.BackendToken != "winner" || logout != "new" {
			t.Fatalf("conflict runtime=%#v err=%v logout=%q", runtime, err, logout)
		}
	})
	t.Run("ambiguous persisted tuple transfers", func(t *testing.T) {
		store := &fakeUpstreamAuthStore{runtime: preContractRuntime(), casErr: errors.New("ambiguous"), persistOnCASErr: true}
		var logout string
		server := lifecycleServer(t, &logout)
		defer server.Close()
		store.runtime.Endpoint.BaseURL = server.URL
		if runtime, err := configuredAuth(store, server.Client()).Ensure(context.Background()); err != nil || runtime.Source.BackendToken != "new" || logout != "" {
			t.Fatalf("ambiguous runtime=%#v err=%v logout=%q", runtime, err, logout)
		}
	})
}

func TestUpstreamAuthenticatorRetiresOnlyManagedOldIdentity(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: managedRuntime("old")}
	store.runtime.Source.ClientIdentity.DeviceID = "OLD-DEVICE"
	var logoutToken, logoutDevice, logoutUser string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Sessions/Logout" {
			logoutToken = r.Header.Get("X-Emby-Token")
			auth := ParseEmbyAuthHeader(r.Header.Get("X-Emby-Authorization"))
			logoutDevice, logoutUser = auth.DeviceID, auth.UserID
			return
		}
		_, _ = w.Write([]byte(`{"AccessToken":"new","ServerId":"server","User":{"Id":"new-user"}}`))
	}))
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	auth := configuredAuth(store, server.Client())
	if _, err := auth.Refresh(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	if logoutToken != "old" || logoutDevice != "OLD-DEVICE" || logoutUser != "user" {
		t.Fatalf("old logout token=%q device=%q user=%q", logoutToken, logoutDevice, logoutUser)
	}
}

func TestUpstreamAuthenticatorGeneratorCollisionStopsBeforeLogin(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	auth := newUpstreamAuthenticator(store, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected login")
		return nil, nil
	})})
	auth.deviceID = func() (string, error) { return "OLD", nil }
	auth.generation = func() (string, error) { return "next", nil }
	if _, err := auth.Ensure(context.Background()); err == nil || store.casCalls != 0 {
		t.Fatalf("collision error=%v CAS=%d", err, store.casCalls)
	}
}

func TestUpstreamAuthenticatorCASLoserEqualWinnerSkipsLogout(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime(), casErr: ErrUpstreamAuthConflict}
	store.casHook = makeManagedWinner("winner", "WINNER")
	var logout string
	server := lifecycleServer(t, &logout)
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	// The login token deliberately equals the winner token.
	auth := configuredAuth(store, server.Client())
	auth.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/Users/AuthenticateByName" {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"AccessToken":"winner","ServerId":"server","User":{"Id":"user"}}`)), Header: make(http.Header), Request: r}, nil
		}
		return server.Client().Transport.RoundTrip(r)
	})}
	if runtime, err := auth.Ensure(context.Background()); err != nil || runtime.Source.BackendToken != "winner" || logout != "" {
		t.Fatalf("runtime=%#v err=%v logout=%q", runtime, err, logout)
	}
}

func TestUpstreamAuthenticatorCASReconciliationLoadFailureFailsClosed(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime(), casErr: ErrStoreUnavailable}
	store.casHook = func(*UpstreamRuntime) { store.loadErr = ErrStoreUnavailable }
	var logout string
	server := lifecycleServer(t, &logout)
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	_, err := configuredAuth(store, server.Client()).Ensure(context.Background())
	if !errors.Is(err, ErrStoreUnavailable) || logout != "" {
		t.Fatalf("error=%v logout=%q", err, logout)
	}
}

func TestUpstreamAuthenticatorPreContractStaleTokenIsNotRetired(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	store.runtime.Source.BackendToken = "stale"
	store.runtime.Source.BackendUserID = "stale-user"
	store.runtime.Source.ClientIdentity.DeviceID = "STALE-DEVICE"
	var logout string
	server := lifecycleServer(t, &logout)
	defer server.Close()
	store.runtime.Endpoint.BaseURL = server.URL
	if _, err := configuredAuth(store, server.Client()).Ensure(context.Background()); err != nil || logout != "" {
		t.Fatalf("error=%v logout=%q", err, logout)
	}
}

func TestUpstreamAuthenticatorCleanupFailureAndTimeoutKeepPrimaryError(t *testing.T) {
	for _, timeout := range []time.Duration{upstreamCleanupTimeout, time.Nanosecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
			store.runtime.Source.BackendToken = "old"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/Sessions/Logout" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(`{"AccessToken":"invoke","ServerId":`))
			}))
			defer server.Close()
			store.runtime.Endpoint.BaseURL = server.URL
			auth := configuredAuth(store, server.Client())
			auth.cleanupTimeout = timeout
			if _, err := auth.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("primary error = %v", err)
			}
		})
	}
}

func TestUpstreamAuthenticatorTerminalReadErrorAfterTokenCleansWithoutCAS(t *testing.T) {
	store := &fakeUpstreamAuthStore{runtime: preContractRuntime()}
	store.runtime.Source.BackendToken = "old"
	var logout string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/Sessions/Logout" {
			logout = r.Header.Get("X-Emby-Token")
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: &tokenThenErrorReader{data: []byte(`{"AccessToken":"invoke","ServerId":"server"}`), err: errors.New("read failure")}, Header: make(http.Header), Request: r}, nil
	})}
	store.runtime.Endpoint.BaseURL = "http://upstream.test"
	if _, err := configuredAuth(store, client).Ensure(context.Background()); err == nil || store.casCalls != 0 || logout != "invoke" {
		t.Fatalf("error=%v CAS=%d logout=%q", err, store.casCalls, logout)
	}
}

func makeManagedWinner(token, device string) func(*UpstreamRuntime) {
	return func(runtime *UpstreamRuntime) {
		now := time.Now().UTC()
		runtime.Source.AuthGenerationID = "winner-generation"
		runtime.Source.BackendToken = token
		runtime.Source.BackendUserID = "winner-user"
		runtime.Source.ClientIdentity.DeviceID = device
		runtime.Source.TokenUpdatedAt, runtime.Source.LastLoginAt = &now, &now
	}
}

type tokenThenErrorReader struct {
	data []byte
	err  error
}

func (r *tokenThenErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func (r *tokenThenErrorReader) Close() error { return nil }

func configuredAuth(store *fakeUpstreamAuthStore, client *http.Client) *upstreamAuthenticator {
	auth := newUpstreamAuthenticator(store, client)
	auth.deviceID = func() (string, error) { return "NEW", nil }
	auth.generation = func() (string, error) { return "next", nil }
	return auth
}

func lifecycleServer(t *testing.T, logout *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Sessions/Logout" {
			*logout = r.Header.Get("X-Emby-Token")
			return
		}
		_, _ = w.Write([]byte(`{"AccessToken":"new","ServerId":"server","User":{"Id":"invoke-user"}}`))
	}))
}

type fakeUpstreamAuthStore struct {
	mu              sync.Mutex
	runtime         *UpstreamRuntime
	casCalls        int
	casErr          error
	afterCAS        func(*UpstreamRuntime)
	casHook         func(*UpstreamRuntime)
	persistOnCASErr bool
	loadErr         error
}

func (s *fakeUpstreamAuthStore) LoadDefaultUpstreamRuntime(ctx context.Context) (*UpstreamRuntime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	copy := *s.runtime
	copy.Source = cloneUpstreamSource(s.runtime.Source)
	return &copy, nil
}

func (s *fakeUpstreamAuthStore) CompareAndSwapUpstreamAuth(ctx context.Context, update UpstreamAuthUpdate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.casCalls++
	if s.runtime.Source.AuthGenerationID != update.ExpectedGenerationID {
		return ErrUpstreamAuthConflict
	}
	if s.casErr != nil && !s.persistOnCASErr {
		if s.casHook != nil {
			s.casHook(s.runtime)
		}
		return s.casErr
	}
	at := update.AuthenticatedAt.UTC()
	s.runtime.Source.AuthGenerationID = update.GenerationID
	s.runtime.Source.ClientIdentity.DeviceID = update.DeviceID
	s.runtime.Source.BackendUserID = update.BackendUserID
	s.runtime.Source.BackendToken = update.BackendToken
	s.runtime.Source.TokenUpdatedAt = &at
	s.runtime.Source.LastLoginAt = &at
	s.runtime.Source.LastLoginError = ""
	if s.afterCAS != nil {
		s.afterCAS(s.runtime)
	}
	return s.casErr
}

func preContractRuntime() *UpstreamRuntime {
	return &UpstreamRuntime{Source: UpstreamSource{ID: "source", Key: "default", ServerID: "server", BackendUsername: "backend", BackendPassword: "password", ClientIdentity: BackendClientIdentity{UserAgent: "agent", Client: "client", Device: "device", DeviceID: "OLD", Version: "1"}}, Endpoint: UpstreamEndpoint{ID: "endpoint", SourceID: "source", Key: "primary", BaseURL: "http://invalid", Active: true}}
}

func managedRuntime(token string) *UpstreamRuntime {
	runtime := preContractRuntime()
	now := time.Now().UTC()
	runtime.Source.AuthGenerationID = "generation"
	runtime.Source.BackendUserID = "user"
	runtime.Source.BackendToken = token
	runtime.Source.TokenUpdatedAt = &now
	runtime.Source.LastLoginAt = &now
	return runtime
}

var _ upstreamAuthStore = (*fakeUpstreamAuthStore)(nil)
