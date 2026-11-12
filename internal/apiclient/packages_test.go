package apiclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/server"
)

// A package manager's request leaves as the signed-in user's, names the
// local address it arrived on, and carries nothing the package manager was
// configured to send a registry.
func TestPackagesProxySignsInAndNamesTheLocalAddress(t *testing.T) {
	var got *http.Request
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		fmt.Fprint(w, "index")
	}))
	defer api.Close()
	client, err := New(api.URL, "sealed-token", "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := client.PackagesProxy()
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(proxy)
	defer local.Close()

	req, _ := http.NewRequest(http.MethodGet, local.URL+"/npm/@scope%2fname?write=true", nil)
	req.Header.Set("Authorization", "Bearer npm-token-for-the-public-registry")
	req.Header.Set("Cookie", "session=1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if body, _ := io.ReadAll(res.Body); string(body) != "index" {
		t.Fatalf("body %q", body)
	}
	if uri := got.URL.RequestURI(); uri != "/v1/packages/npm/@scope%2fname?write=true" {
		t.Errorf("the server should see the path as it was escaped, got %q", uri)
	}
	if auth := got.Header.Get("Authorization"); auth != "Bearer sealed-token" {
		t.Errorf("Authorization = %q, want donkey's sign-in", auth)
	}
	if address := got.Header.Get(server.PackagesAddressHeader); address != strings.TrimPrefix(local.URL, "http://") {
		t.Errorf("%s = %q, want the local address %s", server.PackagesAddressHeader, address, local.URL)
	}
	if got.Header.Get("Cookie") != "" {
		t.Error("a cookie travelled on to the server")
	}
}

// The check before a package manager starts: an expired login and a server
// that predates the mirror each say what they are, instead of surfacing as
// a registry error in the middle of an install.
func TestPackagesCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"offered":        {http.StatusOK, `{"ok":true}`, ""},
		"expired login":  {http.StatusUnauthorized, `{"error":"not signed in — run ` + "`donkey login`" + `"}`, "not signed in"},
		"older server":   {http.StatusOK, `<!doctype html><title>donkey</title>`, "needs an update"},
		"something else": {http.StatusOK, `{"ok":false}`, "needs an update"},
	} {
		t.Run(name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/packages/" || r.Header.Get("Authorization") != "Bearer sealed-token" {
					t.Errorf("unexpected %s %s %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer api.Close()
			client, err := New(api.URL, "sealed-token", "")
			if err != nil {
				t.Fatal(err)
			}
			err = client.Packages(context.Background())
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// The server answers an untidy path with a redirect to the tidy one under
// its own route. The local address has no such route, so the package
// manager is sent to the same path there.
func TestPackagesProxyKeepsRedirectsOnTheLocalAddress(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/packages/maven//org/apache/maven/apache-maven-3.9.9-bin.zip" {
			w.Header().Set("Location", "/v1/packages/maven/org/apache/maven/apache-maven-3.9.9-bin.zip")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		fmt.Fprint(w, r.URL.Path)
	}))
	defer api.Close()
	client, err := New(api.URL, "sealed-token", "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := client.PackagesProxy()
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(proxy)
	defer local.Close()

	res, err := http.Get(local.URL + "/maven//org/apache/maven/apache-maven-3.9.9-bin.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if body, _ := io.ReadAll(res.Body); string(body) != "/v1/packages/maven/org/apache/maven/apache-maven-3.9.9-bin.zip" {
		t.Fatalf("after the redirect the server was asked for %q", body)
	}
}
