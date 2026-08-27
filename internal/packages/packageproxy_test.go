package packages

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
	if cfg.NPMUpstream != "https://registry.npmjs.org" || cfg.PyPIUpstream != "https://pypi.org/simple" || cfg.GoUpstream != "https://proxy.golang.org" || cfg.MavenUpstream != "https://repo.maven.apache.org/maven2" || cfg.NuGetUpstream != "https://api.nuget.org/v3/index.json" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestConfigFromEnvScopesSharedCredentialsToExplicitHosts(t *testing.T) {
	t.Setenv("DONKEY_PACKAGES_NPM_URL", "https://npm.mirror.example.com/npm")
	t.Setenv("DONKEY_PACKAGES_PYPI_URL", "https://pypi.mirror.example.com/index/simple")
	t.Setenv("DONKEY_PACKAGES_CREDENTIAL_HOSTS", "npm.mirror.example.com, pypi.mirror.example.com")
	t.Setenv("DONKEY_PACKAGES_USERNAME", "reader")
	t.Setenv("DONKEY_PACKAGES_PASSWORD", "token")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"npm.mirror.example.com", "pypi.mirror.example.com"} {
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
