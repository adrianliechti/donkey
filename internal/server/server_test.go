package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A draining server answers POST /v1/deploys with 503 before it touches the
// cluster, which makes it the one state-changing route these tests can drive
// without one — handy for checking what the middleware in front of it does.
func drainingServer() *Server {
	s := &Server{cfg: &Config{APIToken: "dk_test", BaseDomain: "example.com"}}
	s.draining.Store(true)
	return s
}

func post(t *testing.T, s *Server, headers map[string]string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/v1/deploys", nil)
	req.Header.Set("Authorization", "Bearer dk_test")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec.Code
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
