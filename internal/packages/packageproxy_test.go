package packages

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportTrustsConfiguredPrivateCA(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	caFile := filepath.Join(t.TempDir(), "ca.crt")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw})
	if err := os.WriteFile(caFile, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	configured, err := transport(caFile)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Transport: configured}).Get(upstream.URL)
	if err != nil {
		t.Fatalf("package transport rejected its configured private CA: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("package upstream status = %d", response.StatusCode)
	}
}

func TestHandlerRoutesPackagesAndScopesCurrentCredentials(t *testing.T) {
	var requests []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("jfrog-user:rotated-token"))
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %q, want current upstream credential", got)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})

	cfg := Config{
		NPMUpstream:   "https://packages.example/npmjs",
		PyPIUpstream:  "https://packages.example/index/simple",
		GoUpstream:    "https://packages.example/proxy",
		MavenUpstream: "https://packages.example/maven2",
		NuGetUpstream: "https://packages.example/v3/index.json",
	}
	credentials := func(host string) (string, string, bool, error) {
		if host != "packages.example" {
			t.Fatalf("credentials requested for unexpected host %q", host)
		}
		return "jfrog-user", "rotated-token", true, nil
	}
	proxy, err := handler(cfg, credentials, transport)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/npm/@scope%2Fpackage?version=1",
		"/pypi/example/",
		"/go/example.com/module/@v/list",
		"/maven/org/example/library/maven-metadata.xml",
		"/nuget/index.json",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer must-not-pass")
		res := httptest.NewRecorder()
		proxy.ServeHTTP(res, req)
		if res.Code != http.StatusNoContent {
			t.Fatalf("GET %s: status = %d, body = %s", path, res.Code, res.Body.String())
		}
	}

	want := []string{
		"/npmjs/@scope%2Fpackage?version=1",
		"/index/simple/example/?",
		"/proxy/example.com/module/@v/list?",
		"/maven2/org/example/library/maven-metadata.xml?",
		"/v3/index.json?",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("upstream requests:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(want, "\n"))
	}
}

func TestHandlerDoesNotForwardBuildAuthorizationWithoutConfiguredCredentials(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("build Authorization leaked upstream: %q", got)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})

	cfg := Config{NPMUpstream: "https://packages.example", PyPIUpstream: "https://packages.example", GoUpstream: "https://packages.example", MavenUpstream: "https://packages.example", NuGetUpstream: "https://packages.example/v3/index.json"}
	proxy, err := handler(cfg, func(string) (string, string, bool, error) {
		return "", "", false, nil
	}, transport)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/npm/package", nil)
	req.Header.Set("Authorization", "Bearer tenant-controlled")
	res := httptest.NewRecorder()
	proxy.ServeHTTP(res, req)
	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
}

func TestHandlerRefusesWritesAndPathsOutsidePackageRoutes(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("refused request reached upstream")
		return nil, nil
	})

	cfg := Config{NPMUpstream: "https://packages.example", PyPIUpstream: "https://packages.example", GoUpstream: "https://packages.example", MavenUpstream: "https://packages.example", NuGetUpstream: "https://packages.example/v3/index.json"}
	proxy, err := handler(cfg, func(string) (string, string, bool, error) {
		return "", "", false, nil
	}, transport)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodPut, "/npm/package", http.StatusMethodNotAllowed},
		{http.MethodGet, "/npm/../../api/security/users", http.StatusForbidden},
		{http.MethodGet, "/npm/%252e%252e/api/security/users", http.StatusForbidden},
		{http.MethodGet, "/pypi/-/files/packages/example.whl", http.StatusForbidden},
		{http.MethodGet, "/pypi/-/unknown/example.whl", http.StatusForbidden},
		{http.MethodGet, "/https://attacker.example/package", http.StatusNotFound},
	} {
		req := httptest.NewRequest(tc.method, tc.path, io.NopCloser(strings.NewReader("payload")))
		res := httptest.NewRecorder()
		proxy.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Errorf("%s %s: status = %d, want %d", tc.method, tc.path, res.Code, tc.status)
		}
	}
}

func TestHandlerKeepsMetadataAndRedirectsOnTheInternalProxy(t *testing.T) {
	const upstreamURL = "https://packages.example"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"dist":{"tarball":"` + upstreamURL + `/npmjs/package/-/package.tgz"}}`
		header := http.Header{}
		header.Set("Content-Type", "application/json")
		header.Set("ETag", "upstream-body")
		header.Set("Location", upstreamURL+"/npmjs/package/-/package.tgz")
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})

	cfg := Config{NPMUpstream: upstreamURL + "/npmjs", PyPIUpstream: upstreamURL, GoUpstream: upstreamURL, MavenUpstream: upstreamURL, NuGetUpstream: upstreamURL + "/v3/index.json"}
	proxy, err := handler(cfg, func(string) (string, string, bool, error) {
		return "", "", false, nil
	}, transport)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://donkey-packages/npm/package", nil)
	res := httptest.NewRecorder()
	proxy.ServeHTTP(res, req)
	if got, want := res.Header().Get("Location"), "http://donkey-packages/npm/package/-/package.tgz"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if body := res.Body.String(); strings.Contains(body, upstreamURL) || !strings.Contains(body, "http://donkey-packages/npm/") {
		t.Errorf("metadata escaped the internal proxy: %s", body)
	}
	if got := res.Header().Get("ETag"); got != "" {
		t.Errorf("rewritten response retained upstream ETag %q", got)
	}
}

func TestHandlerKeepsNuGetV3ResourcesOnTheInternalProxy(t *testing.T) {
	const upstreamURL = "https://api.nuget.example"
	var paths []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.EscapedPath())
		body := `{"version":"3.0.0","resources":[{"@id":"` + upstreamURL + `/v3-flatcontainer/","@type":"PackageBaseAddress/3.0.0"}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})
	cfg := Config{
		NPMUpstream: upstreamURL, PyPIUpstream: upstreamURL, GoUpstream: upstreamURL,
		MavenUpstream: upstreamURL, NuGetUpstream: upstreamURL + "/v3/index.json",
	}
	proxy, err := handler(cfg, func(string) (string, string, bool, error) {
		return "", "", false, nil
	}, transport)
	if err != nil {
		t.Fatal(err)
	}

	indexReq := httptest.NewRequest(http.MethodGet, "http://donkey-packages/nuget/index.json", nil)
	indexRes := httptest.NewRecorder()
	proxy.ServeHTTP(indexRes, indexReq)
	if body := indexRes.Body.String(); strings.Contains(body, upstreamURL) || !strings.Contains(body, "http://donkey-packages/nuget/v3-flatcontainer/") {
		t.Fatalf("NuGet service index escaped the internal proxy: %s", body)
	}

	resourceReq := httptest.NewRequest(http.MethodGet, "http://donkey-packages/nuget/v3-flatcontainer/example/index.json", nil)
	resourceRes := httptest.NewRecorder()
	proxy.ServeHTTP(resourceRes, resourceReq)
	if resourceRes.Code != http.StatusOK {
		t.Fatalf("resource status = %d", resourceRes.Code)
	}
	if got, want := strings.Join(paths, "\n"), "/v3/index.json\n/v3-flatcontainer/example/index.json"; got != want {
		t.Fatalf("NuGet upstream paths = %q, want %q", got, want)
	}
}

func TestHandlerOmitsNuGetSigningResourcesOverHTTP(t *testing.T) {
	const upstream = "https://api.nuget.example"
	const signing = upstream + "/v3-index/repository-signatures/5.0.0/index.json"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"resources":[{"@id":"` + signing + `","@type":"RepositorySignatures/5.0.0"},{"@id":"` + upstream + `/v3-flatcontainer/","@type":"PackageBaseAddress/3.0.0"},{"@id":"` + signing + `","@type":["RepositorySignatures/4.9.0","RepositorySignatures/5.0.0"]}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	cfg := Config{NPMUpstream: upstream, PyPIUpstream: upstream, GoUpstream: upstream, MavenUpstream: upstream, NuGetUpstream: upstream + "/v3/index.json"}
	proxy, err := handler(cfg, func(string) (string, string, bool, error) { return "", "", false, nil }, transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			rec := httptest.NewRecorder()
			proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, scheme+"://donkey-packages/nuget/index.json", nil))
			var index struct {
				Resources []struct {
					ID string `json:"@id"`
				} `json:"resources"`
			}
			want := 1
			if scheme == "https" {
				want = 3
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &index); err != nil || len(index.Resources) != want {
				t.Fatalf("invalid service index: %s", rec.Body.String())
			}
			if scheme == "http" && !strings.HasSuffix(index.Resources[0].ID, "/v3-flatcontainer/") {
				t.Fatalf("package resource was removed: %s", rec.Body.String())
			}
			for _, resource := range index.Resources {
				if !strings.HasPrefix(resource.ID, scheme+"://donkey-packages/nuget/") {
					t.Fatalf("package or unconfigured resource escaped the proxy: %s", resource.ID)
				}
			}
		})
	}
}

func TestHandlerKeepsNuGetPackageBytes(t *testing.T) {
	const upstream = "https://api.nuget.example"
	// Include an upstream URL to catch accidental rewriting inside an archive.
	const archive = "PK\x03\x04\x00.signature.p7s\x00" + upstream + "/signed-content"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/octet-stream"}, "ETag": {"signed-package"}}, Body: io.NopCloser(strings.NewReader(archive)), Request: r}, nil
	})
	cfg := Config{NPMUpstream: upstream, PyPIUpstream: upstream, GoUpstream: upstream, MavenUpstream: upstream, NuGetUpstream: upstream + "/v3/index.json"}
	proxy, err := handler(cfg, func(string) (string, string, bool, error) { return "", "", false, nil }, transport)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://donkey-packages/nuget/v3-flatcontainer/example/example.nupkg", nil))
	if rec.Body.String() != archive || rec.Header().Get("ETag") != "signed-package" {
		t.Fatal("package bytes or integrity headers changed")
	}
}

func TestHandlerKeepsEnterprisePyPIArtifactsOnTheInternalProxy(t *testing.T) {
	const upstreamURL = "https://pypi.mirror.example.com/index/simple"
	var requests []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("reader:token"))
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %q, want scoped mirror credential", got)
		}
		body := `<a href="../../artifacts/example.whl">example</a>`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})
	cfg := Config{
		NPMUpstream: "https://registry.npmjs.org", PyPIUpstream: upstreamURL,
		GoUpstream: "https://proxy.golang.org", MavenUpstream: "https://repo.maven.apache.org/maven2",
		NuGetUpstream: "https://api.nuget.org/v3/index.json",
	}
	proxy, err := handler(cfg, func(host string) (string, string, bool, error) {
		return "reader", "token", host == "pypi.mirror.example.com", nil
	}, transport)
	if err != nil {
		t.Fatal(err)
	}

	indexReq := httptest.NewRequest(http.MethodGet, "http://donkey-packages/pypi/example/", nil)
	indexRes := httptest.NewRecorder()
	proxy.ServeHTTP(indexRes, indexReq)
	artifactURL := firstHref(t, indexRes.Body.String())
	if !strings.HasPrefix(artifactURL, "http://donkey-packages/pypi/-/resource/") {
		t.Fatalf("PyPI artifact escaped the internal proxy: %s", indexRes.Body.String())
	}

	artifactReq := httptest.NewRequest(http.MethodGet, artifactURL, nil)
	artifactRes := httptest.NewRecorder()
	proxy.ServeHTTP(artifactRes, artifactReq)
	if got, want := strings.Join(requests, "\n"), upstreamURL+"/example/\nhttps://pypi.mirror.example.com/index/artifacts/example.whl"; got != want {
		t.Fatalf("PyPI upstream requests = %q, want %q", got, want)
	}
}

func TestHandlerKeepsPublicPyPIFilesOnTheInternalProxy(t *testing.T) {
	var targetHost string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		targetHost = r.URL.Host
		if r.Host != r.URL.Host {
			t.Errorf("Host header = %q, target host = %q", r.Host, r.URL.Host)
		}
		if r.URL.Host == "files.pythonhosted.org" && r.Header.Get("Authorization") != "" {
			t.Error("PyPI index credential leaked to files.pythonhosted.org")
		}
		body := `<a href="https://files.pythonhosted.org/packages/example.whl">example</a>`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})
	cfg := Config{
		NPMUpstream: "https://registry.npmjs.org", PyPIUpstream: "https://pypi.org/simple",
		GoUpstream: "https://proxy.golang.org", MavenUpstream: "https://repo.maven.apache.org/maven2",
		NuGetUpstream: "https://api.nuget.org/v3/index.json",
	}
	proxy, err := handler(cfg, func(host string) (string, string, bool, error) {
		return "reader", "token", host == "pypi.org", nil
	}, transport)
	if err != nil {
		t.Fatal(err)
	}

	indexReq := httptest.NewRequest(http.MethodGet, "http://donkey-packages/pypi/example/", nil)
	indexRes := httptest.NewRecorder()
	proxy.ServeHTTP(indexRes, indexReq)
	artifactURL := firstHref(t, indexRes.Body.String())
	if !strings.HasPrefix(artifactURL, "http://donkey-packages/pypi/-/resource/") {
		t.Fatalf("public PyPI artifact escaped the internal proxy: %s", indexRes.Body.String())
	}

	artifactReq := httptest.NewRequest(http.MethodGet, artifactURL, nil)
	artifactRes := httptest.NewRecorder()
	proxy.ServeHTTP(artifactRes, artifactReq)
	if targetHost != "files.pythonhosted.org" {
		t.Fatalf("public PyPI artifact host = %q, want files.pythonhosted.org", targetHost)
	}
}

func TestHandlerRoutesAllowedNuGetResourceHostsAndScopesTheirCredentials(t *testing.T) {
	const indexHost = "nuget.mirror.example.com"
	const resourceHost = "nuget-content.example.com"
	var requests []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("reader:token"))
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization to %s = %q, want scoped credential", r.URL.Host, got)
		}
		body := `{"version":"3.0.0","resources":[{"@id":"https://` + resourceHost + `/flat/","@type":"PackageBaseAddress/3.0.0"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	cfg := Config{
		NPMUpstream: "https://registry.npmjs.org", PyPIUpstream: "https://pypi.org/simple",
		GoUpstream: "https://proxy.golang.org", MavenUpstream: "https://repo.maven.apache.org/maven2",
		NuGetUpstream: "https://" + indexHost + "/v3/index.json", ResourceHosts: []string{resourceHost},
	}
	proxy, err := handler(cfg, func(host string) (string, string, bool, error) {
		return "reader", "token", host == indexHost || host == resourceHost, nil
	}, transport)
	if err != nil {
		t.Fatal(err)
	}

	indexRes := httptest.NewRecorder()
	proxy.ServeHTTP(indexRes, httptest.NewRequest(http.MethodGet, "http://donkey-packages/nuget/index.json", nil))
	var resource string
	if body := indexRes.Body.String(); strings.Contains(body, resourceHost) {
		t.Fatalf("NuGet resource host escaped the proxy: %s", body)
	} else {
		_, rest, ok := strings.Cut(body, `"@id":"`)
		if !ok {
			t.Fatalf("NuGet response has no @id: %s", body)
		}
		resource, _, _ = strings.Cut(rest, `"`)
	}
	proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, resource+"example/index.json", nil))
	if got, want := strings.Join(requests, "\n"), "https://"+indexHost+"/v3/index.json\nhttps://"+resourceHost+"/flat/example/index.json"; got != want {
		t.Fatalf("NuGet requests = %q, want %q", got, want)
	}
}

func TestDockerConfigCredentialsRotateAndNeverFallBackToAnonymous(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".dockerconfigjson")
	writeDockerConfig := func(host, password string) {
		t.Helper()
		auth := base64.StdEncoding.EncodeToString([]byte("reader:" + password))
		contents := `{"auths":{"` + host + `":{"auth":"` + auth + `"}}}`
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const dockerConfigHost = "oci.mirror.example"
	writeDockerConfig(dockerConfigHost, "first")
	cfg := Config{
		NPMUpstream: "https://packages.example/npm", PyPIUpstream: "https://packages.example/pypi",
		GoUpstream: "https://packages.example/go", MavenUpstream: "https://packages.example/maven",
		NuGetUpstream:    "https://packages.example/nuget/index.json",
		CredentialHosts:  []string{"packages.example", "artifacts.example"},
		DockerConfigDir:  dir,
		DockerConfigHost: dockerConfigHost,
	}
	var authorizations []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})
	proxy, err := handler(cfg, cfg.credentials, transport)
	if err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		res := httptest.NewRecorder()
		proxy.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://donkey-packages/npm/example", nil))
		return res
	}
	if res := request(); res.Code != http.StatusNoContent {
		t.Fatalf("first request = %d: %s", res.Code, res.Body.String())
	}
	writeDockerConfig(dockerConfigHost, "rotated")
	if res := request(); res.Code != http.StatusNoContent {
		t.Fatalf("rotated request = %d: %s", res.Code, res.Body.String())
	}
	writeDockerConfig("unrelated.example", "wrong")
	if res := request(); res.Code != http.StatusBadGateway || !strings.Contains(res.Body.String(), "no credentials") {
		t.Fatalf("missing credential request = %d: %s", res.Code, res.Body.String())
	}
	if err := cfg.validateCredentialSource(); err == nil {
		t.Fatal("missing Docker credential left the package proxy ready")
	}
	writeDockerConfig(dockerConfigHost, "shared")
	username, password, ok, err := cfg.credentials("artifacts.example")
	if err != nil || !ok || username != "reader" || password != "shared" {
		t.Fatalf("shared source credential for second allowed target = %q, %q, %v, %v", username, password, ok, err)
	}
	if _, _, ok, err := cfg.credentials(dockerConfigHost); err != nil || ok {
		t.Fatal("Docker config source host implicitly became an allowed target")
	}
	want := []string{
		"Basic " + base64.StdEncoding.EncodeToString([]byte("reader:first")),
		"Basic " + base64.StdEncoding.EncodeToString([]byte("reader:rotated")),
	}
	if strings.Join(authorizations, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Authorization headers = %v, want %v", authorizations, want)
	}
}

func TestReadinessRejectsPrivateUpstreamAuthenticationFailures(t *testing.T) {
	cfg := &Config{CredentialHosts: []string{"packages.example"}, Username: "reader", Password: "wrong", checkUpstreams: []string{"https://packages.example/npm"}}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if username, password, ok := r.BasicAuth(); !ok || username != "reader" || password != "wrong" {
			t.Fatalf("readiness did not send configured credentials: %q/%q/%v", username, password, ok)
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})
	ready := &readinessCheck{cfg: cfg, transport: transport}
	if err := ready.status(t.Context()); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("readiness error = %v, want rejected credentials", err)
	}
}

func firstHref(t *testing.T, body string) string {
	t.Helper()
	_, rest, ok := strings.Cut(body, `href="`)
	if !ok {
		t.Fatalf("response has no href: %s", body)
	}
	href, _, ok := strings.Cut(rest, `"`)
	if !ok {
		t.Fatalf("response has an unterminated href: %s", body)
	}
	return href
}

func TestConfigFromEnvUsesPublicDefaultsWithoutCredentials(t *testing.T) {
	for _, name := range []string{
		"DONKEY_PACKAGES_NPM_URL", "DONKEY_PACKAGES_PYPI_URL", "DONKEY_PACKAGES_GO_URL",
		"DONKEY_PACKAGES_MAVEN_URL", "DONKEY_PACKAGES_NUGET_URL",
		"DONKEY_PACKAGES_DEBIAN_URL", "DONKEY_PACKAGES_ALPINE_URL",
		"DONKEY_PACKAGES_RESOURCE_HOSTS", "DONKEY_PACKAGES_CREDENTIAL_HOSTS",
		"DONKEY_PACKAGES_USERNAME", "DONKEY_PACKAGES_PASSWORD", "DONKEY_PACKAGES_DOCKER_CONFIG_DIR",
		"DONKEY_PACKAGES_DOCKER_CONFIG_HOST",
	} {
		t.Setenv(name, "")
	}
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DebianUpstream != "" || cfg.AlpineUpstream != "" || cfg.NPMUpstream != "https://registry.npmjs.org" || cfg.PyPIUpstream != "https://pypi.org/simple" || cfg.GoUpstream != "https://proxy.golang.org" || cfg.MavenUpstream != "https://repo.maven.apache.org/maven2" || cfg.NuGetUpstream != "https://api.nuget.org/v3/index.json" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestConfigFromEnvScopesSharedCredentialsToExplicitHosts(t *testing.T) {
	t.Setenv("DONKEY_PACKAGES_NPM_URL", "https://npm.mirror.example.com/npm")
	t.Setenv("DONKEY_PACKAGES_PYPI_URL", "https://pypi.mirror.example.com/index/simple")
	t.Setenv("DONKEY_PACKAGES_DEBIAN_URL", "https://debian.mirror.example.com/debian")
	t.Setenv("DONKEY_PACKAGES_ALPINE_URL", "https://alpine.mirror.example.com/alpinelinux")
	t.Setenv("DONKEY_PACKAGES_CREDENTIAL_HOSTS", "npm.mirror.example.com, pypi.mirror.example.com, debian.mirror.example.com, alpine.mirror.example.com")
	t.Setenv("DONKEY_PACKAGES_USERNAME", "reader")
	t.Setenv("DONKEY_PACKAGES_PASSWORD", "token")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"npm.mirror.example.com", "pypi.mirror.example.com", "debian.mirror.example.com", "alpine.mirror.example.com"} {
		username, password, ok, err := cfg.credentials(host)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || username != "reader" || password != "token" {
			t.Errorf("credentials(%q) = %q, %q, %v", host, username, password, ok)
		}
	}
	if _, _, ok, err := cfg.credentials("registry.npmjs.org"); err != nil || ok {
		t.Fatal("shared credentials escaped to an unconfigured host")
	}
}

func TestConfigFromEnvMapsDockerCredentialSourceToAllowedTarget(t *testing.T) {
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("reader:token"))
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{"oci.mirror.example.com":{"auth":"`+auth+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DONKEY_PACKAGES_NPM_URL", "https://npm.mirror.example.com/npm")
	t.Setenv("DONKEY_PACKAGES_CREDENTIAL_HOSTS", "npm.mirror.example.com")
	t.Setenv("DONKEY_PACKAGES_DOCKER_CONFIG_DIR", dir)
	t.Setenv("DONKEY_PACKAGES_DOCKER_CONFIG_HOST", "oci.mirror.example.com")
	t.Setenv("DONKEY_PACKAGES_USERNAME", "")
	t.Setenv("DONKEY_PACKAGES_PASSWORD", "")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	username, password, ok, err := cfg.credentials("npm.mirror.example.com")
	if err != nil || !ok || username != "reader" || password != "token" {
		t.Fatalf("mapped Docker credential = %q, %q, %v, %v", username, password, ok, err)
	}
	if _, _, ok, err := cfg.credentials("oci.mirror.example.com"); err != nil || ok {
		t.Fatal("Docker credential source was treated as an allowed target")
	}
}

func TestOSPackageRoutesPreserveIndexesAndScopeCredentials(t *testing.T) {
	cfg := Config{
		NPMUpstream: config.DefaultNPMUpstream, PyPIUpstream: config.DefaultPyPIUpstream,
		GoUpstream: config.DefaultGoUpstream, MavenUpstream: config.DefaultMavenUpstream, NuGetUpstream: config.DefaultNuGetUpstream,
		DebianUpstream: "https://debian.mirror.example/debian", AlpineUpstream: "https://alpine.mirror.example/alpinelinux",
		CredentialHosts: []string{"debian.mirror.example", "alpine.mirror.example"}, Username: "reader", Password: "private-token",
	}
	for _, tc := range []struct{ path, target, body string }{
		{"/debian/dists/bookworm/InRelease", "https://debian.mirror.example/debian/dists/bookworm/InRelease", "-----BEGIN PGP SIGNED MESSAGE-----\nOrigin: https://debian.mirror.example/debian\n"},
		{"/debian/pool/main/libj/libjansson/libjansson4.deb", "https://debian.mirror.example/debian/pool/main/libj/libjansson/libjansson4.deb", "\x00\xffhttps://debian.mirror.example/debian"},
		{"/alpine/v3.21/main/x86_64/APKINDEX.tar.gz", "https://alpine.mirror.example/alpinelinux/v3.21/main/x86_64/APKINDEX.tar.gz", "\x1f\x8bhttps://alpine.mirror.example/alpinelinux"},
		{"/alpine/edge/community/aarch64/example.apk", "https://alpine.mirror.example/alpinelinux/edge/community/aarch64/example.apk", "signed-apk"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tc.target {
					t.Fatalf("target = %s, want %s", r.URL, tc.target)
				}
				u, p, ok := r.BasicAuth()
				if !ok || u != "reader" || p != "private-token" {
					t.Fatal("missing proxy-owned upstream credential")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/plain"}, "ETag": {"signed-index"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			proxy, err := handler(cfg, cfg.credentials, transport)
			if err != nil {
				t.Fatal(err)
			}
			res := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer build-supplied")
			proxy.ServeHTTP(res, req)
			if res.Code != http.StatusOK || res.Body.String() != tc.body || res.Header().Get("ETag") != "signed-index" {
				t.Fatalf("archive bytes/headers changed: status=%d body=%q etag=%q", res.Code, res.Body.String(), res.Header().Get("ETag"))
			}
		})
	}
	cfg.DebianUpstream, cfg.AlpineUpstream = "", ""
	proxy, err := handler(cfg, cfg.credentials, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("disabled mirror contacted upstream")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/debian/dists/bookworm/InRelease", "/alpine/v3.21/main/x86_64/APKINDEX.tar.gz"} {
		res := httptest.NewRecorder()
		proxy.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusNotFound {
			t.Errorf("disabled %s: status=%d", path, res.Code)
		}
	}
}

func publicUpstreams() Config {
	return Config{
		NPMUpstream: config.DefaultNPMUpstream, PyPIUpstream: config.DefaultPyPIUpstream, GoUpstream: config.DefaultGoUpstream,
		MavenUpstream: config.DefaultMavenUpstream, NuGetUpstream: config.DefaultNuGetUpstream,
	}
}

// Maven and Go publish a checksum for every file, and Gradle reads its
// module metadata in the order it was written ("formatVersion" first). Their
// content has no links to keep inside the proxy, so not one byte of it
// changes — not even where it is JSON or text that names the upstream.
func TestHandlerKeepsMavenAndGoContentByteForByte(t *testing.T) {
	bodies := map[string][2]string{
		"/maven2/com/example/lib/1.0/lib-1.0.module": {"application/vnd.org.gradle.module+json",
			"{\n  \"formatVersion\": \"1.1\",\n  \"component\": { \"url\": \"https://repo.maven.apache.org/maven2/com/example\" }\n}\n"},
		"/maven2/com/example/lib/1.0/lib-1.0.pom": {"text/xml",
			"<project><url>https://repo.maven.apache.org/maven2</url></project>"},
		"/example.com/lib/@v/v1.0.0.mod":  {"text/plain; charset=UTF-8", "module example.com/lib // see https://proxy.golang.org\n"},
		"/example.com/lib/@v/v1.0.0.info": {"application/json", `{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`},
	}
	proxy, err := handler(publicUpstreams(), func(string) (string, string, bool, error) { return "", "", false, nil },
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, ok := bodies[r.URL.Path]
			if !ok {
				t.Errorf("unexpected upstream request %s", r.URL)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{body[0]}, "Etag": []string{`"abc"`}},
				Body: io.NopCloser(strings.NewReader(body[1])), Request: r}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"/maven/com/example/lib/1.0/lib-1.0.module": bodies["/maven2/com/example/lib/1.0/lib-1.0.module"][1],
		"/maven/com/example/lib/1.0/lib-1.0.pom":    bodies["/maven2/com/example/lib/1.0/lib-1.0.pom"][1],
		"/go/example.com/lib/@v/v1.0.0.mod":         bodies["/example.com/lib/@v/v1.0.0.mod"][1],
		"/go/example.com/lib/@v/v1.0.0.info":        bodies["/example.com/lib/@v/v1.0.0.info"][1],
	} {
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://donkey-packages"+path, nil))
		if rec.Body.String() != want {
			t.Errorf("%s was changed:\n got %q\nwant %q", path, rec.Body.String(), want)
		}
		if rec.Header().Get("Etag") != `"abc"` {
			t.Errorf("%s lost the upstream's ETag", path)
		}
	}
}

// NuGet keeps its registration and vulnerability indexes compressed and
// sends them that way whatever it is asked for. Their links are rewritten
// all the same: left as they are, a client follows them straight to the
// public host a restricted network does not reach.
func TestHandlerRewritesDocumentsTheUpstreamSendsCompressed(t *testing.T) {
	proxy, err := handler(publicUpstreams(), func(string) (string, string, bool, error) { return "", "", false, nil },
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if encoding := r.Header.Get("Accept-Encoding"); encoding != "identity" {
				t.Errorf("Accept-Encoding = %q", encoding)
			}
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			_, _ = writer.Write([]byte(`[{"@name":"base","@id":"https://api.nuget.org/v3-vulnerabilities/2026/vulnerability.base.json"},` +
				`{"url":"https://github.com/advisories/GHSA-5crp-9r3c-p9vr","severity":2}]`))
			_ = writer.Close()
			return &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Encoding": []string{"gzip"}},
				Body:   io.NopCloser(&compressed), Request: r}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://donkey-packages/nuget/v3/vulnerabilities/index.json", nil))
	if want := `"@id":"http://donkey-packages/nuget/v3-vulnerabilities/2026/vulnerability.base.json"`; !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("body %q, want it to contain %s", rec.Body.String(), want)
	}
	if encoding := rec.Header().Get("Content-Encoding"); encoding != "" {
		t.Errorf("the rewritten document is plain but still labelled %q", encoding)
	}
	// An answer with those headers and no body — the client's copy is
	// current — has nothing to rewrite and passes as it is.
	empty, err := handler(publicUpstreams(), func(string) (string, string, bool, error) { return "", "", false, nil },
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNotModified,
				Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Encoding": []string{"gzip"}},
				Body:   http.NoBody, Request: r}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	unchanged := httptest.NewRecorder()
	empty.ServeHTTP(unchanged, httptest.NewRequest(http.MethodGet, "http://donkey-packages/nuget/v3/vulnerabilities/index.json", nil))
	if unchanged.Code != http.StatusNotModified {
		t.Errorf("an empty compressed answer: status %d, want it passed on as 304", unchanged.Code)
	}
	// The advisory a warning points a person to is not a package address.
	if want := `"url":"https://github.com/advisories/GHSA-5crp-9r3c-p9vr"`; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body %q, want the advisory link left as %s", rec.Body.String(), want)
	}
}

// Gradle finds plugins in its plugin portal, whose answers lead on to the
// portal's file host and back to Maven Central. Beside the public Central
// all of that stays on the proxy. An enterprise upstream is expected to
// carry the plugins itself, so what is asked of the portal there is asked of
// that upstream.
func TestHandlerServesTheGradlePluginPortalBesidePublicMavenCentral(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		header := http.Header{}
		switch r.URL.String() {
		case "https://plugins.gradle.org/m2/com/example/plugin/1.0/plugin-1.0.jar":
			header.Set("Location", "https://plugins-artifacts.gradle.org/com.example/plugin/1.0/abc/plugin-1.0.jar")
		case "https://plugins.gradle.org/m2/org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.pom":
			header.Set("Location", "https://repo.maven.apache.org/maven2/org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.pom")
		case "https://plugins-artifacts.gradle.org/com.example/plugin/1.0/abc/plugin-1.0.jar":
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("jar")), Request: r}, nil
		case "https://artifactory.example.com/maven-remote/com/example/plugin/1.0/plugin-1.0.jar":
			return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("enterprise jar")), Request: r}, nil
		default:
			t.Errorf("unexpected upstream request %s", r.URL)
		}
		return &http.Response{StatusCode: http.StatusSeeOther, Header: header, Body: http.NoBody, Request: r}, nil
	})
	none := func(string) (string, string, bool, error) { return "", "", false, nil }
	proxy, err := handler(publicUpstreams(), none, transport)
	if err != nil {
		t.Fatal(err)
	}
	get := func(proxy http.Handler, url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		return rec
	}
	portal := "http://donkey-packages" + GradlePluginPortalPath

	jar := get(proxy, portal+"com/example/plugin/1.0/plugin-1.0.jar")
	files := "http://donkey-packages/maven/-/resource/" + base64.RawURLEncoding.EncodeToString([]byte("https://plugins-artifacts.gradle.org"))
	if want := files + "/com.example/plugin/1.0/abc/plugin-1.0.jar"; jar.Code != http.StatusSeeOther || jar.Header().Get("Location") != want {
		t.Fatalf("plugin jar: %d to %q, want a redirect to %s", jar.Code, jar.Header().Get("Location"), want)
	}
	if rec := get(proxy, jar.Header().Get("Location")); rec.Code != http.StatusOK || rec.Body.String() != "jar" {
		t.Fatalf("plugin file host: %d %s", rec.Code, rec.Body)
	}
	central := get(proxy, portal+"org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.pom")
	if want := "http://donkey-packages/maven/org/apache/commons/commons-lang3/3.14.0/commons-lang3-3.14.0.pom"; central.Header().Get("Location") != want {
		t.Fatalf("an artifact the portal hands to Central: redirect to %q, want %s", central.Header().Get("Location"), want)
	}

	enterprise := publicUpstreams()
	enterprise.MavenUpstream = "https://artifactory.example.com/maven-remote"
	proxy, err = handler(enterprise, none, transport)
	if err != nil {
		t.Fatal(err)
	}
	if rec := get(proxy, portal+"com/example/plugin/1.0/plugin-1.0.jar"); rec.Code != http.StatusOK || rec.Body.String() != "enterprise jar" {
		t.Fatalf("the portal beside an enterprise upstream: %d %s, want the upstream's own copy", rec.Code, rec.Body)
	}
	// The portal's file host is still nobody's to reach from there.
	if rec := get(proxy, files+"/com.example/plugin/1.0/abc/plugin-1.0.jar"); rec.Code != http.StatusForbidden {
		t.Fatalf("the portal's file host beside an enterprise upstream: %d, want 403", rec.Code)
	}
}
