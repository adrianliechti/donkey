package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
)

func testPlatformConfig() config.PlatformConfig {
	return config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"u1@example.com", "ada@example.com"}}}
}

func adminTestContext() context.Context {
	ctx := context.WithValue(context.Background(), identityContextKey{}, &identity.Identity{Subject: "u1", Email: "u1@example.com"})
	return context.WithValue(ctx, platformContextKey{}, testPlatformConfig())
}

var testSessionKey = bytes.Repeat([]byte{7}, 32)

// A draining orchestrator answers POST /v1/deploys with 503 before it touches
// the cluster, which makes it the one state-changing route these tests can
// drive without one — handy for checking what the middleware in front of it
// does.
func drainingOrchestrator() *Server {
	s := &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey}, rt: planRuntime{}}
	s.draining.Store(true)
	return s
}

// sealedToken is a CLI token for one email, as the gateway mints it.
func sealedToken(t *testing.T, key []byte, email string) string {
	t.Helper()
	tok, err := identity.SealToken(key, identity.Identity{
		Subject: email, Email: email, Expires: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// asAdmin puts the credential the gateway forwards onto a request — how
// every real request arrives.
func asAdmin(t *testing.T, req *http.Request) {
	t.Helper()
	req.Header.Set("X-Access-Token", sealedToken(t, testSessionKey, "u1@example.com"))
}

// asForwarded is a request as donkey-server hands it to the orchestrator:
// the caller's token, and the server's own credential next to it.
func asForwarded(t *testing.T, req *http.Request) {
	t.Helper()
	asAdmin(t, req)
	req.Header.Set(orchestratorTokenHeader, orchestratorToken(testSessionKey))
}

// fakeOrchestrator stands in for donkey-orchestrator and keeps the headers
// of every request that reached it.
type fakeOrchestrator struct {
	*httptest.Server
	mu  sync.Mutex
	got []http.Header
}

func newFakeOrchestrator(t *testing.T, handler http.HandlerFunc) *fakeOrchestrator {
	t.Helper()
	f := &fakeOrchestrator{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.got = append(f.got, r.Header.Clone())
		f.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOrchestrator) calls() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.got...)
}

// apiServer is donkey-server, forwarding to the orchestrator at url.
func apiServer(url string) *Server {
	return &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey, OrchestratorURL: url}, rt: planRuntime{}}
}

func post(t *testing.T, s *Server, headers map[string]string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/v1/deploys", nil)
	asAdmin(t, req)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// Only a sealed token authorizes — X-Access-Token from the gateway or a
// Bearer from the CLI, both verified here and both checked against the admin
// rules. An identity header must never: app pods legitimately reach this
// Service (the AI proxy), and a header is just a string anything can type.
// What passes is handed to the orchestrator; what doesn't never reaches it.
func TestAuthPaths(t *testing.T) {
	orchestrator := newFakeOrchestrator(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	s := apiServer(orchestrator.URL)

	call := func(mutate func(*http.Request)) int {
		req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/v1/deploys", nil)
		mutate(req)
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call(func(r *http.Request) {}); got != http.StatusUnauthorized {
		t.Fatalf("anonymous request: want 401, got %d", got)
	}
	if got := call(func(r *http.Request) { r.Header.Set("Authorization", "Bearer garbage") }); got != http.StatusUnauthorized {
		t.Fatalf("garbage token: want 401, got %d", got)
	}
	// The regression that must never come back: a bare identity header was
	// once the whole authorization — one curl from any pod was admin.
	if got := call(func(r *http.Request) { r.Header.Set("X-Auth-Request-User", "u1") }); got != http.StatusUnauthorized {
		t.Fatalf("a typed identity header must be worthless: want 401, got %d", got)
	}
	if got := call(func(r *http.Request) { r.Header.Set("X-Access-Token", "garbage") }); got != http.StatusUnauthorized {
		t.Fatalf("a garbage access token: want 401, got %d", got)
	}
	// Knowing the orchestrator's header name buys nothing either.
	if got := call(func(r *http.Request) { r.Header.Set(orchestratorTokenHeader, orchestratorToken(testSessionKey)) }); got != http.StatusUnauthorized {
		t.Fatalf("a server credential without a sign-in: want 401, got %d", got)
	}
	if n := len(orchestrator.calls()); n != 0 {
		t.Fatalf("%d unauthenticated requests reached the orchestrator", n)
	}

	tok := sealedToken(t, testSessionKey, "ada@example.com")
	if got := call(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }); got != http.StatusAccepted {
		t.Fatalf("sealed CLI token: want past auth (forwarded), got %d", got)
	}
	if got := call(func(r *http.Request) {
		r.Header.Set("X-Access-Token", tok)
		r.Header.Set("X-Forwarded-User", "someone-else")
	}); got != http.StatusAccepted {
		t.Fatalf("gateway-forwarded token: want past auth (forwarded), got %d", got)
	}
	// Both arrive in the one shape the orchestrator verifies: the caller's
	// own token, the server's credential, and nothing a client typed.
	for i, h := range orchestrator.calls() {
		if h.Get("X-Access-Token") != tok || h.Get("Authorization") != "" {
			t.Errorf("call %d: caller token not forwarded as X-Access-Token: %v", i, h)
		}
		if h.Get(orchestratorTokenHeader) != orchestratorToken(testSessionKey) {
			t.Errorf("call %d: no server credential", i)
		}
		if h.Get("X-Forwarded-User") != "" {
			t.Errorf("call %d: a client's identity header reached the orchestrator", i)
		}
	}

	// Authentication also admits space members. Their resolved targets are
	// authorized by each operation, in the orchestrator.
	s.rt = planRuntime{platform: &config.PlatformConfig{Admins: config.AccessConfig{Users: []string{"bob@example.com"}}}}
	for _, set := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) },
		func(r *http.Request) { r.Header.Set("X-Access-Token", tok) },
	} {
		if got := call(set); got != http.StatusAccepted {
			t.Fatalf("signed-in space member: want forwarded, got %d", got)
		}
	}
}

// The CLI is not a browser: it sends no Origin and no Sec-Fetch-Site, and must
// never be caught by the CSRF middleware. A page on another origin must be.
func TestCrossOriginProtection(t *testing.T) {
	orchestrator := newFakeOrchestrator(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	s := apiServer(orchestrator.URL)

	if got := post(t, s, nil); got == http.StatusForbidden {
		t.Fatal("the CLI (no browser headers) was refused by the CSRF middleware")
	}
	if got := post(t, s, map[string]string{"Sec-Fetch-Site": "same-origin"}); got == http.StatusForbidden {
		t.Fatal("the console (same-origin) was refused by the CSRF middleware")
	}
	if got := post(t, s, map[string]string{
		"Sec-Fetch-Site": "cross-site",
		"Origin":         "https://evil.example",
	}); got != http.StatusForbidden {
		t.Fatalf("expected a cross-site request to be refused, got %d", got)
	}
	if n := len(orchestrator.calls()); n != 2 {
		t.Fatalf("want the two same-site deploys forwarded and the cross-site one stopped, got %d calls", n)
	}
}

// Draining must refuse new deploys rather than accept work it cannot finish,
// and say so on readiness so donkey-server stops sending any.
func TestDrainingRefusesDeploys(t *testing.T) {
	s := drainingOrchestrator()
	req := httptest.NewRequest(http.MethodPost, "http://donkey-orchestrator/v1/deploys", nil)
	asForwarded(t, req)
	rec := httptest.NewRecorder()
	s.orchestratorRoutes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while draining, got %d", rec.Code)
	}

	ready := httptest.NewRecorder()
	s.orchestratorRoutes().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "http://donkey-orchestrator/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz to fail while draining, got %d", ready.Code)
	}

	// Liveness must keep answering: a pod deliberately finishing its deploys
	// must not be killed by the kubelet.
	live := httptest.NewRecorder()
	s.orchestratorRoutes().ServeHTTP(live, httptest.NewRequest(http.MethodGet, "http://donkey-orchestrator/healthz", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("expected healthz to stay ok while draining, got %d", live.Code)
	}
}

// The API itself stops quickly: its readiness goes the moment it is told to
// stop, and nothing waits for a deploy.
func TestServerStopsWithoutWaitingForDeploys(t *testing.T) {
	s := apiServer("")
	s.cfg.Listen = "127.0.0.1:0"
	// A deploy the orchestrator is running is not this process's business.
	s.deploying.Add(1)
	defer s.deploying.Done()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("donkey-server waited for a deploy before stopping")
	}
	ready := httptest.NewRecorder()
	s.routes().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "https://donkey.example.com/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("a stopping server must leave rotation, readyz = %d", ready.Code)
	}
}
