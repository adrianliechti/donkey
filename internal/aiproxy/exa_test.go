package aiproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExaProxyCustomBaseCredentialsAndBody(t *testing.T) {
	const body = `{"query":"donkey","contents":{"text":true}}`
	for _, operation := range []string{"search", "contents"} {
		for _, auth := range []string{"x-api-key", "Authorization"} {
			t.Run(operation+"/"+auth, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/gateway/exa/"+operation || r.URL.RawQuery != "test=1" {
						t.Errorf("upstream URL: %s", r.URL)
					}
					if r.Header.Get("x-api-key") != "upstream-secret" {
						t.Error("key not swapped")
					}
					for _, h := range []string{"Authorization", "Cookie", "X-Access-Token"} {
						if r.Header.Get(h) != "" {
							t.Errorf("forwarded %s", h)
						}
					}
					got, _ := io.ReadAll(r.Body)
					if string(got) != body {
						t.Errorf("changed body: %s", got)
					}
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Set-Cookie", "upstream-cookie=secret")
					w.WriteHeader(http.StatusTooManyRequests)
					w.Write([]byte(`{"error":"rate limit"}`))
				}))
				defer upstream.Close()
				handler, err := ExaHandler(upstream.URL+"/gateway/exa/", "upstream-secret", func(context.Context) ([]string, error) { return []string{"space-token"}, nil })
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, "/exa/"+operation+"?test=1", strings.NewReader(body))
				value := "space-token"
				if auth == "Authorization" {
					value = "Bearer " + value
				}
				req.Header.Set(auth, value)
				req.Header.Set("Cookie", "admin=secret")
				req.Header.Set("X-Access-Token", "admin-secret")
				out := httptest.NewRecorder()
				handler.ServeHTTP(out, req)
				if out.Code != http.StatusTooManyRequests || out.Body.String() != `{"error":"rate limit"}` || out.Header().Get("Set-Cookie") != "" {
					t.Fatalf("response: %+v", out)
				}
			})
		}
	}
}

func TestExaProxyRefusesInvalidAccess(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer upstream.Close()
	handler, err := ExaHandler(upstream.URL, "secret", func(context.Context) ([]string, error) { return []string{"space-token"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path, token string
		status              int
	}{
		{"POST", "/exa/search", "", 401},
		{"POST", "/exa/search", "openai-token", 401},
		{"POST", "/exa/search", "upstream-secret", 401},
		{"GET", "/exa/search", "space-token", 405},
		{"POST", "/exa/keys", "space-token", 403},
		{"POST", "/exa/search/../keys", "space-token", 403},
		{"POST", "/exa/%252e%252e/search", "space-token", 403},
		{"POST", "/exa/contents/extra", "space-token", 403},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("x-api-key", c.token)
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != c.status {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, out.Code, c.status)
		}
	}
	if calls != 0 {
		t.Fatalf("unauthorized requests reached upstream: %d", calls)
	}

}

func TestExaDisabledAndInvalidConfig(t *testing.T) {
	handler, err := ExaHandler("", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, httptest.NewRequest("POST", "/exa/search", nil))
	if out.Code != 503 {
		t.Fatalf("disabled proxy: %d", out.Code)
	}
	for _, raw := range []string{"relative", "https://u:secret@host", "https://host?key=secret", "https://host#search", "file:///tmp"} {
		if _, err := ExaHandler(raw, "secret", nil); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestProxyTokenCacheRefreshRevokes(t *testing.T) {
	active := true
	tokens := &proxyTokens{refresh: func() (map[string]bool, error) { return map[string]bool{"space-token": active}, nil }}
	if ok, err := tokens.valid("space-token"); err != nil || !ok {
		t.Fatal("initial token rejected")
	}
	active = false
	tokens.until = time.Time{}
	if ok, err := tokens.valid("space-token"); err != nil || ok {
		t.Fatal("token survived refresh")
	}
}

// A base without a path — Exa's default, or an OpenAI-compatible server at
// its root — still yields absolute request paths upstream.
func TestProxiesToBaseWithoutPath(t *testing.T) {
	var requested string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.RequestURI
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	tokens := func(context.Context) ([]string, error) { return []string{"space-token"}, nil }
	exa, err := ExaHandler(upstream.URL, "secret", tokens)
	if err != nil {
		t.Fatal(err)
	}
	openai, err := Handler(upstream.URL, "secret", tokens)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		handler            http.Handler
		method, path, want string
	}{
		{exa, "POST", "/exa/search", "/search"},
		{openai, "GET", "/v1/models", "/models"},
	} {
		requested = ""
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer space-token")
		out := httptest.NewRecorder()
		c.handler.ServeHTTP(out, req)
		if out.Code != 200 || requested != c.want {
			t.Errorf("%s %s: %d, upstream saw %q, want %q", c.method, c.path, out.Code, requested, c.want)
		}
	}
}
