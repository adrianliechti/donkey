package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The console calls /ai/v1 on its own origin; donkey-ai serves plain /v1,
// the same path an app uses. The "/ai" belongs to this origin only, so it
// must not survive the hop.
func TestAIForwardStripsTheOriginPrefix(t *testing.T) {
	var got *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
	}))
	defer upstream.Close()

	req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/ai/v1/chat/completions", nil)
	// The space's binding token — what donkey-ai checks, and the only
	// credential that belongs on this hop.
	req.Header.Set("Authorization", "Bearer space-token")
	// The console rides a route the gateway marks for token forwarding, so
	// the admin credential is on the request too. It must stop here.
	req.Header.Set("X-Access-Token", "sealed-admin-token")
	aiForwardHandler(upstream.URL).ServeHTTP(httptest.NewRecorder(), req)

	if got.URL.Path != "/v1/chat/completions" {
		t.Errorf("donkey-ai should see /v1/chat/completions, got %q", got.URL.Path)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer space-token" {
		t.Errorf("the binding token must ride through untouched, got %q", auth)
	}
	if tok := got.Header.Get("X-Access-Token"); tok != "" {
		t.Errorf("the admin credential must not travel past the API that verifies it, got %q", tok)
	}
}

// Without an endpoint configured, an OpenAI client must get an error it can
// parse — not the console's index.html, which is what falling through to the
// SPA would hand it.
func TestAIForwardWithoutAnEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/ai/v1/chat/completions", nil)
	aiForwardHandler("").ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("an OpenAI client parses JSON, got %q", ct)
	}
}
