package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
)

// A draining server answers POST /v1/deploys with 503 before it touches the
// cluster, which makes it the one state-changing route these tests can drive
// without one — handy for checking what the middleware in front of it does.
func drainingServer() *Server {
	s := &Server{cfg: &Config{Domain: "example.com"}}
	s.draining.Store(true)
	return s
}

func post(t *testing.T, s *Server, headers map[string]string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/v1/deploys", nil)
	// The identity the gateway forwards after its login — how every real
	// request arrives.
	req.Header.Set("X-Auth-Request-User", "u1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
}

// Identity comes from the gateway's headers or a sealed CLI token — nothing
// else. The token path enforces the admin rules itself: it bypasses the
// gateway, so nobody has checked them yet.
func TestAuthPaths(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	s := &Server{cfg: &Config{Domain: "example.com", SessionKey: key}}
	s.draining.Store(true) // deploys answer 503 before touching a cluster

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
	if got := call(func(r *http.Request) { r.Header.Set("X-Auth-Request-User", "u1") }); got != http.StatusServiceUnavailable {
		t.Fatalf("gateway identity: want past auth (503 drain), got %d", got)
	}

	tok, err := identity.SealToken(key, identity.Identity{Subject: "u1", Email: "ada@example.com", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if got := call(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }); got != http.StatusServiceUnavailable {
		t.Fatalf("sealed token: want past auth (503 drain), got %d", got)
	}

	s.cfg.Admins = &config.AccessConfig{Users: []string{"bob@example.com"}}
	if got := call(func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }); got != http.StatusUnauthorized {
		t.Fatalf("token outside the admin rules: want 401, got %d", got)
	}
}

// The CLI is not a browser: it sends no Origin and no Sec-Fetch-Site, and must
// never be caught by the CSRF middleware. A page on another origin must be.
func TestCrossOriginProtection(t *testing.T) {
	s := drainingServer()

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
}

// Draining must refuse new deploys rather than accept work it cannot finish,
// and say so on readiness so the gateway stops sending any.
func TestDrainingRefusesDeploys(t *testing.T) {
	s := drainingServer()
	if got := post(t, s, nil); got != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while draining, got %d", got)
	}

	ready := httptest.NewRecorder()
	s.routes().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "https://donkey.example.com/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected readyz to fail while draining, got %d", ready.Code)
	}

	// Liveness must keep answering: a pod deliberately finishing its deploys
	// must not be killed by the kubelet.
	live := httptest.NewRecorder()
	s.routes().ServeHTTP(live, httptest.NewRequest(http.MethodGet, "https://donkey.example.com/healthz", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("expected healthz to stay ok while draining, got %d", live.Code)
	}
}
