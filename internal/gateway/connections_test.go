package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestBrowserProxyPreservesAPIPathQueryAndEscaping(t *testing.T) {
	var uri, prefix string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri, prefix = r.RequestURI, r.Header.Get("X-Forwarded-Prefix")
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	for _, test := range []struct{ path, want string }{
		{"/_donkey/api/api/items/a%2Fb?q=1", "/api/items/a%2Fb?q=1"},
		{"/%5Fdonkey/api/api/items/a%2Fb?q=1", "/api/items/a%2Fb?q=1"},
		{"/_donkey/api", "/"},
		{"/_donkey/api/health", "/health"},
	} {
		req := httptest.NewRequest(http.MethodGet, "https://web.example.com"+test.path, nil)
		testServer().proxy(route{Backend: target, Path: "/_donkey/api", StripPrefix: true}, nil, false, "").ServeHTTP(httptest.NewRecorder(), req)
		if uri != test.want || prefix != "/_donkey/api" {
			t.Fatalf("proxy received %s (%s), want %s", uri, prefix, test.want)
		}
	}
}

func TestBrowserProxyKeepsRedirectsOnAPIRoute(t *testing.T) {
	for _, test := range []struct{ location, want string }{
		{"/login?next=a%2Fb#form", "/_donkey/api/login?next=a%2Fb#form"},
		{"/items/", "/_donkey/api/items/"},
		{"/", "/_donkey/api/"},
		{"/_donkey/api/items/", "/_donkey/api/items/"},
		{"/_donkey/api?query=1", "/_donkey/api?query=1"},
		{"/_donkey/api-other/", "/_donkey/api/_donkey/api-other/"},
		{"https://identity.example.com/login", "https://identity.example.com/login"},
		{"//identity.example.com/login", "//identity.example.com/login"},
		{"next", "next"},
	} {
		t.Run(test.location, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", test.location)
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			defer backend.Close()
			target, _ := url.Parse(backend.URL)
			for _, strip := range []bool{true, false} {
				req := httptest.NewRequest(http.MethodGet, "https://web.example.com/_donkey/api/start", nil)
				rec := httptest.NewRecorder()
				testServer().proxy(route{Backend: target, Path: "/_donkey/api", StripPrefix: strip}, nil, false, "").ServeHTTP(rec, req)
				want := test.location
				if strip {
					want = test.want
				}
				if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != want {
					t.Fatalf("strip=%v: %d %s, want %s", strip, rec.Code, rec.Header().Get("Location"), want)
				}
			}
		})
	}
}
