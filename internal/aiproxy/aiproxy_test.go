package aiproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMachineProxyForwardsRequestsUnderConfiguredBase(t *testing.T) {
	privateHeaders := []string{"Cookie", "X-Access-Token", "X-Forwarded-Access-Token", "X-Auth-Request-Access-Token", "X-Api-Key", "Api-Key", "OpenAI-Project", "OpenAI-Organization"}
	const prefix = "/provider/v1"
	type request struct{ method, uri, body string }
	requests := make(chan request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("incorrect upstream credential")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests <- request{r.Method, r.URL.RequestURI(), string(data)}
		for _, h := range privateHeaders {
			if r.Header.Get(h) != "" {
				t.Errorf("leaked %s", h)
			}
		}
		w.Header().Set("Set-Cookie", "upstream=private")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	handler, err := Handler(upstream.URL+prefix, "upstream-secret", func(context.Context) ([]string, error) { return []string{"machine-token"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, suffix string }{
		{"OPTIONS", "/"}, {"GET", "/models/org%2Fmodel?limit=5"}, {"POST", "/chat/completions"},
		{"GET", "/files"}, {"DELETE", "/files/file-1"}, {"PUT", "/vendor/entry"}, {"PATCH", "/vendor/entry"},
	} {
		body := ""
		if tc.method == "POST" || tc.method == "PUT" || tc.method == "PATCH" {
			body = `{"example":true}`
		}
		req := httptest.NewRequest(tc.method, "/v1"+tc.suffix, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer machine-token")
		for _, h := range privateHeaders {
			req.Header.Set(h, "private")
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != http.StatusOK || out.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s %s: %d %s", tc.method, tc.suffix, out.Code, out.Body.String())
		}
		if got := <-requests; got != (request{tc.method, prefix + tc.suffix, body}) {
			t.Fatalf("incorrect upstream request: %+v", got)
		}
	}
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/v1/models", "", http.StatusUnauthorized},
		{"/v1/files", "old-space-token", http.StatusUnauthorized},
		{"/outside/models", "machine-token", http.StatusBadRequest},
		{"/v1/chat/%252e%252e/admin", "machine-token", http.StatusBadRequest},
		{"/v1/responses/..;/files", "machine-token", http.StatusBadRequest},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != tc.status || len(requests) != 0 {
			t.Fatalf("expected local rejection for %s, got %d", tc.path, out.Code)
		}
	}
}

func TestOpenAIProxyRejectsUnsafeUpstream(t *testing.T) {
	for _, raw := range []string{"relative", "file:///tmp", "https://user:secret@host/v1", "https://host/v1?key=secret", "https://host/v1#secret"} {
		if _, err := Handler(raw, "secret", nil); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid URL accepted or disclosed: %q %v", raw, err)
		}
	}
}
