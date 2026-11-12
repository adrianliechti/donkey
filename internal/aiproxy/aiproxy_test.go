package aiproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The proxy attaches the platform's own upstream key, so the path it forwards
// has to stay inside the upstream's base and inside the inference surface.
// ServeMux cleans the *escaped* path, so percent-encoded dot segments reach
// this function still encoded and only decode into ".." here — which is what
// makes the containment check, not the mux, the control that matters.
func TestAIUpstreamPath(t *testing.T) {
	cases := []struct {
		path string
		want string // "" means: must be refused
	}{
		// The inference surface apps and the console actually call.
		{"/v1/chat/completions", "chat/completions"},
		{"/v1/models", "models"},
		{"/v1/models/gpt-4o", "models/gpt-4o"},
		{"/v1/embeddings", "embeddings"},
		{"/v1/responses", "responses"},
		{"/v1/audio/transcriptions", "audio/transcriptions"},
		{"/v1/images/generations", "images/generations"},
		{"/v1/moderations", "moderations"},
		{"/v1/completions", "completions"},
		{"/v1/models/", "models/"},

		// Base-path escapes: URL.Path is already decoded by the time it gets
		// here, and url.JoinPath would resolve these out of the base.
		{"/v1/../../admin/keys", ""},
		{"/v1/../admin", ""},
		{"/v1/chat/../../../admin/keys", ""},
		{"/v1//etc/passwd", ""},
		{"/v1/./chat/completions", ""},

		// Escaped twice (and three times): this process sees a path that is
		// clean and forwards it as written — the dot segments appear at the
		// hop that decodes next, with the platform's key already attached.
		{"/v1/chat/%2e%2e/%2e%2e/admin", ""},
		{"/v1/chat/%252e%252e/admin", ""},
		{"/v1/chat/%25252e%25252e/admin", ""},
		{"/v1/chat/%2f%2e%2e/admin", ""},
		{"/v1/chat/%zz", ""},

		// What is refused is a segment that can still become "..", not
		// escaping as such — an escape that decodes into an ordinary name
		// (a namespaced model id, say) rides through as it arrived.
		{"/v1/models/meta-llama%2FLlama-3-8B", "models/meta-llama%2FLlama-3-8B"},

		// The account's own state is not the model.
		{"/v1/files", ""},
		{"/v1/files/file-abc123", ""},
		{"/v1/fine_tuning/jobs", ""},
		{"/v1/assistants", ""},
		{"/v1/batches", ""},
		{"/v1/uploads", ""},
		{"/v1/vector_stores", ""},
		{"/v1/organization/invites", ""},
		{"/v1/", ""},
	}
	for _, c := range cases {
		got, ok := aiUpstreamPath(c.path)
		if c.want == "" {
			if ok {
				t.Errorf("%s: proxied as %q, want refused", c.path, got)
			}
			continue
		}
		if !ok {
			t.Errorf("%s: refused, want %q", c.path, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.path, got, c.want)
		}
	}
}

func TestOpenAIProxyRejectsStoredStateAndKeepsSessionPrivate(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("wrong upstream credential")
		}
		for _, h := range []string{"Cookie", "X-Access-Token", "X-Api-Key", "Api-Key", "OpenAI-Project", "OpenAI-Organization"} {
			if r.Header.Get(h) != "" {
				t.Errorf("leaked %s", h)
			}
		}
		w.Header().Set("Set-Cookie", "upstream=private")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	handler, err := Handler(upstream.URL+"/provider/v1", "upstream-secret", func(context.Context) ([]string, error) { return []string{"space-token"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/models", 200},
		{"POST", "/v1/chat/completions", 200},
		{"POST", "/v1/responses", 200},
		{"GET", "/v1/responses/other-space-response", 403},
		{"POST", "/v1/responses/other-space-response/cancel", 403},
		{"GET", "/v1/chat/completions", 403},
		{"POST", "/v1/chat/completions/other-space-chat", 403},
		{"POST", "/v1/models", 403},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer space-token")
		for _, h := range []string{"Cookie", "X-Access-Token", "X-Api-Key", "Api-Key", "OpenAI-Project", "OpenAI-Organization"} {
			req.Header.Set(h, "private")
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != c.status || out.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s %s: %d %s", c.method, c.path, out.Code, out.Body.String())
		}
	}
	if calls != 3 {
		t.Fatalf("stored-state requests reached upstream: %d", calls)
	}
}

func TestOpenAIProxyRejectsUnsafeUpstream(t *testing.T) {
	for _, raw := range []string{"relative", "file:///tmp", "https://user:secret@host/v1", "https://host/v1?key=secret", "https://host/v1#secret"} {
		if _, err := Handler(raw, "secret", nil); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid URL accepted or disclosed: %q %v", raw, err)
		}
	}
}
