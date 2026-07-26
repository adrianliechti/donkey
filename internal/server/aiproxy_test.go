package server

import "testing"

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
		{"/ai/v1/chat/completions", "chat/completions"},
		{"/ai/v1/models", "models"},
		{"/ai/v1/models/gpt-4o", "models/gpt-4o"},
		{"/ai/v1/embeddings", "embeddings"},
		{"/ai/v1/responses", "responses"},
		{"/ai/v1/audio/transcriptions", "audio/transcriptions"},
		{"/ai/v1/images/generations", "images/generations"},
		{"/ai/v1/moderations", "moderations"},
		{"/ai/v1/completions", "completions"},
		{"/ai/v1/models/", "models/"},

		// Base-path escapes: URL.Path is already decoded by the time it gets
		// here, and url.JoinPath would resolve these out of the base.
		{"/ai/v1/../../admin/keys", ""},
		{"/ai/v1/../admin", ""},
		{"/ai/v1/chat/../../../admin/keys", ""},
		{"/ai/v1//etc/passwd", ""},
		{"/ai/v1/./chat/completions", ""},

		// The account's own state is not the model.
		{"/ai/v1/files", ""},
		{"/ai/v1/files/file-abc123", ""},
		{"/ai/v1/fine_tuning/jobs", ""},
		{"/ai/v1/assistants", ""},
		{"/ai/v1/batches", ""},
		{"/ai/v1/uploads", ""},
		{"/ai/v1/vector_stores", ""},
		{"/ai/v1/organization/invites", ""},
		{"/ai/v1/", ""},
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
