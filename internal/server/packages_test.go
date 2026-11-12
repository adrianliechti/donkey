package server

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/packages"
)

func packagesServer(mirror string) *Server {
	return &Server{cfg: &Config{Domain: "example.com", SessionKey: testSessionKey, PackagesURL: mirror}, rt: planRuntime{}}
}

// packagesCall is a request as `donkey packages` sends it: signed in, and
// naming the local address the package manager talks to.
func packagesCall(t *testing.T, s *Server, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/packages/"+path, nil)
	req.Header.Set(PackagesAddressHeader, "127.0.0.1:4874")
	asAdmin(t, req)
	for name, values := range header {
		req.Header[name] = values
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

// The mirror writes links to the host it is asked as, so it has to be asked
// as the developer's local address — with the path exactly as the package
// manager escaped it, and with nothing of the sign-in left on the request.
func TestPackagesReachTheMirrorAsTheLocalAddress(t *testing.T) {
	var got *http.Request
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
	}))
	defer mirror.Close()

	rec := packagesCall(t, packagesServer(mirror.URL), "npm/@scope%2fname?write=true", http.Header{
		"Authorization":       {"Bearer what-the-package-manager-sent"},
		"Cookie":              {"session=sealed"},
		"X-Auth-Request-User": {"forged"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got.Host != "127.0.0.1:4874" {
		t.Errorf("the mirror should be asked as the local address, got host %q", got.Host)
	}
	if uri := got.URL.RequestURI(); uri != "/npm/@scope%2fname?write=true" {
		t.Errorf("the mirror should see the path as it was escaped, got %q", uri)
	}
	for _, name := range []string{"Authorization", "X-Access-Token", "Cookie", "X-Auth-Request-User", PackagesAddressHeader} {
		if value := got.Header.Get(name); value != "" {
			t.Errorf("%s must stop at the API, the mirror got %q", name, value)
		}
	}
}

// The route carries the installation's upstream credential to whoever may
// use it: a verified sign-in, asking for a language registry, from a package
// manager on its own machine.
func TestPackagesNeedASignInAndALocalAddress(t *testing.T) {
	reached := false
	mirror := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer mirror.Close()
	s := packagesServer(mirror.URL)

	anonymous := httptest.NewRequest(http.MethodGet, "/v1/packages/npm/left-pad", nil)
	anonymous.Header.Set(PackagesAddressHeader, "127.0.0.1:4874")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, anonymous)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without a sign-in: status %d, want 401", rec.Code)
	}

	for _, address := range []string{"", "127.0.0.1", "registry.example.com:443", "10.0.0.7:4874", "127.0.0.1:0"} {
		rec := packagesCall(t, s, "npm/left-pad", http.Header{PackagesAddressHeader: {address}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("address %q: status %d, want 400", address, rec.Code)
		}
	}
	for _, address := range []string{"localhost:4874", "[::1]:4874"} {
		if rec := packagesCall(t, s, "npm/left-pad", http.Header{PackagesAddressHeader: {address}}); rec.Code != http.StatusOK {
			t.Errorf("address %q: status %d, want 200", address, rec.Code)
		}
	}
	reached = false
	for _, path := range []string{"debian/dists/trixie/Release", "alpine/v3.22/main", "healthz"} {
		if rec := packagesCall(t, s, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%q: status %d, want 404", path, rec.Code)
		}
	}
	// What the CLI asks first: the mirror is offered and the sign-in is good.
	if rec := packagesCall(t, s, "", nil); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"ok":true}` {
		t.Errorf("status check: %d %s", rec.Code, rec.Body)
	}
	if reached {
		t.Error("a refused request must not travel on to the mirror")
	}
}

// Indexes leave the cluster compressed when the caller can read that;
// archives, which are compressed already, pass through as they are.
func TestPackagesCompressIndexesNotArchives(t *testing.T) {
	index := `{"name":"left-pad","versions":{}}`
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tgz") {
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, "archive")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.npm.install-v1+json")
		fmt.Fprint(w, index)
	}))
	defer mirror.Close()
	s := packagesServer(mirror.URL)
	accepts := http.Header{"Accept-Encoding": {"gzip, deflate, br"}}

	rec := packagesCall(t, s, "npm/left-pad", accepts)
	if encoding := rec.Header().Get("Content-Encoding"); encoding != "gzip" {
		t.Fatalf("index encoding %q, want gzip", encoding)
	}
	plain, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(plain); string(body) != index {
		t.Errorf("index after decompression = %q", body)
	}

	if rec := packagesCall(t, s, "npm/left-pad/-/left-pad-1.3.0.tgz", accepts); rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "archive" {
		t.Errorf("an archive must pass unchanged, got %q %q", rec.Header().Get("Content-Encoding"), rec.Body)
	}
	if rec := packagesCall(t, s, "npm/left-pad", nil); rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != index {
		t.Errorf("a caller that didn't ask gets it plain, got %q %q", rec.Header().Get("Content-Encoding"), rec.Body)
	}
}

// End to end with the real mirror: the tarball link in npm's metadata comes
// back pointing at the developer's local address, which is the only place
// their package manager can fetch it from.
func TestPackagesLinksPointAtTheLocalAddress(t *testing.T) {
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":"left-pad","versions":{"1.3.0":{"dist":{"tarball":"%s/left-pad/-/left-pad-1.3.0.tgz"}}}}`, upstream.URL)
	}))
	defer upstream.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- packages.Run(ctx, &packages.Config{
			Listen:      address,
			NPMUpstream: upstream.URL, PyPIUpstream: upstream.URL, GoUpstream: upstream.URL,
			MavenUpstream: upstream.URL, NuGetUpstream: upstream.URL,
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stopping the mirror: %v", err)
		}
	})
	mirror := "http://" + address
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		res, err := http.Get(mirror + "/healthz")
		if err == nil {
			res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the mirror did not start: %v", err)
		}
	}

	rec := packagesCall(t, packagesServer(mirror), "npm/left-pad", nil)
	if want := `"tarball":"http://127.0.0.1:4874/npm/left-pad/-/left-pad-1.3.0.tgz"`; rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("status %d, body %s — want %s", rec.Code, rec.Body, want)
	}
	// Naming a language registry and then climbing out of it reaches
	// nothing: the mirror refuses the path as it does for a build.
	if rec := packagesCall(t, packagesServer(mirror), "npm/%2e%2e/debian/dists/trixie/Release", nil); rec.Code != http.StatusForbidden {
		t.Errorf("escaping the registry: status %d %s, want 403", rec.Code, rec.Body)
	}
}
