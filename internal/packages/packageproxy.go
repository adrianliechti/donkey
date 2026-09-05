// Package packages exposes read-only package registries to BuildKit without
// handing their credentials to app Dockerfiles.
package packages

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"golang.org/x/net/html"
)

type Config struct {
	Listen           string
	NPMUpstream      string
	PyPIUpstream     string
	GoUpstream       string
	MavenUpstream    string
	NuGetUpstream    string
	ResourceHosts    []string
	CredentialHosts  []string
	Username         string
	Password         string
	DockerConfigDir  string
	DockerConfigHost string
	CAFile           string
	checkUpstreams   []string
}

func ConfigFromEnv() (*Config, error) {
	npm, pypi := os.Getenv("DONKEY_PACKAGES_NPM_URL"), os.Getenv("DONKEY_PACKAGES_PYPI_URL")
	goUpstream, maven := os.Getenv("DONKEY_PACKAGES_GO_URL"), os.Getenv("DONKEY_PACKAGES_MAVEN_URL")
	nuget := os.Getenv("DONKEY_PACKAGES_NUGET_URL")
	packages := config.InstallPackages{
		NPM:              npm,
		PyPI:             pypi,
		Go:               goUpstream,
		Maven:            maven,
		NuGet:            nuget,
		ResourceHosts:    splitCommaSeparated(os.Getenv("DONKEY_PACKAGES_RESOURCE_HOSTS")),
		CredentialHosts:  splitCommaSeparated(os.Getenv("DONKEY_PACKAGES_CREDENTIAL_HOSTS")),
		Username:         os.Getenv("DONKEY_PACKAGES_USERNAME"),
		Password:         os.Getenv("DONKEY_PACKAGES_PASSWORD"),
		DockerConfigDir:  os.Getenv("DONKEY_PACKAGES_DOCKER_CONFIG_DIR"),
		DockerConfigHost: os.Getenv("DONKEY_PACKAGES_DOCKER_CONFIG_HOST"),
	}
	if err := packages.ValidateCredentials(); err != nil {
		return nil, err
	}
	return &Config{
		Listen:           envOr("DONKEY_LISTEN", ":8090"),
		NPMUpstream:      packages.NPMUpstream(),
		PyPIUpstream:     packages.PyPIUpstream(),
		GoUpstream:       packages.GoUpstream(),
		MavenUpstream:    packages.MavenUpstream(),
		NuGetUpstream:    packages.NuGetUpstream(),
		ResourceHosts:    packages.ResourceHosts,
		CredentialHosts:  packages.CredentialHosts,
		Username:         packages.Username,
		Password:         packages.Password,
		DockerConfigDir:  packages.DockerConfigDir,
		DockerConfigHost: packages.DockerConfigHost,
		CAFile:           os.Getenv("DONKEY_PACKAGES_CA_FILE"),
		checkUpstreams: nonDefaultUpstreams(
			npm, config.DefaultNPMUpstream,
			pypi, config.DefaultPyPIUpstream,
			goUpstream, config.DefaultGoUpstream,
			maven, config.DefaultMavenUpstream,
			nuget, config.DefaultNuGetUpstream,
		),
	}, nil
}

func (c Config) credentials(host string) (string, string, bool, error) {
	for _, configured := range c.CredentialHosts {
		if strings.EqualFold(host, configured) {
			if c.DockerConfigDir != "" {
				username, password, err := c.dockerCredentials()
				if err != nil {
					return "", "", false, err
				}
				return username, password, true, nil
			}
			return c.Username, c.Password, true, nil
		}
	}
	return "", "", false, nil
}

func (c Config) dockerCredentials() (string, string, error) {
	username, password, found, err := config.DockerConfigDirCredentials(c.DockerConfigDir, c.DockerConfigHost)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", fmt.Errorf("Docker config has no credentials for %s", c.DockerConfigHost)
	}
	if username == "" || password == "" {
		return "", "", fmt.Errorf("Docker config credentials for %s are incomplete", c.DockerConfigHost)
	}
	return username, password, nil
}

func (c Config) validateCredentialSource() error {
	if c.DockerConfigDir == "" {
		return nil
	}
	_, _, err := c.dockerCredentials()
	return err
}

type credentials func(host string) (username, password string, ok bool, err error)

type proxyRoute struct {
	prefix       string
	upstream     *url.URL
	serviceIndex bool
	pypi         bool
	allowed      map[string]bool
}

type proxyRequest struct {
	route    proxyRoute
	rest     string
	target   *url.URL
	public   string
	username string
	password string
	hasAuth  bool
}

type proxyRequestKey struct{}

func handler(cfg Config, current credentials, transport http.RoundTripper) (http.Handler, error) {
	routes := make([]proxyRoute, 0, 5)
	for _, route := range []struct {
		prefix       string
		raw          string
		serviceIndex bool
	}{
		{prefix: "/npm/", raw: cfg.NPMUpstream},
		{prefix: "/pypi/", raw: cfg.PyPIUpstream},
		{prefix: "/go/", raw: cfg.GoUpstream},
		{prefix: "/maven/", raw: cfg.MavenUpstream},
		{prefix: "/nuget/", raw: cfg.NuGetUpstream, serviceIndex: true},
	} {
		upstream, err := url.Parse(route.raw)
		if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.User != nil {
			return nil, fmt.Errorf("package upstream %s URL %q is invalid", strings.Trim(route.prefix, "/"), route.raw)
		}
		allowed := map[string]bool{originKey(upstream): true}
		for _, host := range cfg.ResourceHosts {
			allowed["https://"+strings.ToLower(host)] = true
		}
		if route.prefix == "/pypi/" && strings.EqualFold(upstream.Host, "pypi.org") {
			allowed["https://files.pythonhosted.org"] = true
		}
		routes = append(routes, proxyRoute{
			prefix: route.prefix, upstream: upstream, serviceIndex: route.serviceIndex,
			pypi: route.prefix == "/pypi/", allowed: allowed,
		})
	}

	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			request := pr.In.Context().Value(proxyRequestKey{}).(proxyRequest)
			pr.Out.URL = request.target
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery
			pr.Out.Host = request.target.Host
			// Nothing supplied by a build may become an upstream credential.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Set("Accept-Encoding", "identity")
			if request.hasAuth {
				pr.Out.SetBasicAuth(request.username, request.password)
			}
		},
		ModifyResponse: func(response *http.Response) error {
			request := response.Request.Context().Value(proxyRequestKey{}).(proxyRequest)

			if location := response.Header.Get("Location"); location != "" {
				if rewritten, ok := rewriteReference(location, response.Request.URL, request); ok {
					response.Header.Set("Location", rewritten)
				}
			}
			if response.Request.Method == http.MethodHead {
				return nil
			}
			contentType := response.Header.Get("Content-Type")
			if !strings.Contains(contentType, "json") && !strings.Contains(contentType, "text") && !strings.Contains(contentType, "html") {
				return nil
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				return err
			}
			_ = response.Body.Close()
			body = rewriteBody(body, contentType, request)
			response.Body = io.NopCloser(bytes.NewReader(body))
			response.ContentLength = int64(len(body))
			response.Header.Set("Content-Length", fmt.Sprint(len(body)))
			response.Header.Del("Content-MD5")
			response.Header.Del("ETag")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "package upstream request failed: "+err.Error(), http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "the package proxy is read-only", http.StatusMethodNotAllowed)
			return
		}

		route, rest, found := matchRoute(routes, r.URL.EscapedPath())
		if !found {
			http.NotFound(w, r)
			return
		}
		if !staysClean(rest) {
			http.Error(w, "package path escapes its configured upstream", http.StatusForbidden)
			return
		}

		target, ok := targetURL(route, rest)
		if !ok {
			http.Error(w, "package path targets an unsupported upstream", http.StatusForbidden)
			return
		}

		username, password, hasAuth, err := current(target.Host)
		if err != nil {
			http.Error(w, "package upstream credentials are unavailable: "+err.Error(), http.StatusBadGateway)
			return
		}
		if hasAuth && (username == "" || password == "") {
			http.Error(w, "package upstream credentials are incomplete", http.StatusBadGateway)
			return
		}

		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		request := proxyRequest{
			route: route, rest: rest, target: target,
			public:   scheme + "://" + r.Host + strings.TrimSuffix(route.prefix, "/"),
			username: username, password: password, hasAuth: hasAuth,
		}
		ctx := context.WithValue(r.Context(), proxyRequestKey{}, request)
		proxy.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func matchRoute(routes []proxyRoute, escapedPath string) (proxyRoute, string, bool) {
	for _, route := range routes {
		root := strings.TrimSuffix(route.prefix, "/")
		if escapedPath == root || escapedPath == route.prefix {
			return route, "", true
		}
		if strings.HasPrefix(escapedPath, route.prefix) {
			return route, strings.TrimPrefix(escapedPath, route.prefix), true
		}
	}
	return proxyRoute{}, "", false
}

func staysClean(path string) bool {
	if path == "" {
		return true
	}
	for {
		if !fs.ValidPath(strings.TrimSuffix(path, "/")) {
			return false
		}
		decoded, err := url.PathUnescape(path)
		if err != nil {
			return false
		}
		if decoded == path {
			return true
		}
		path = decoded
	}
}

func joinURL(base *url.URL, rest string) *url.URL {
	joined := *base
	escaped := strings.TrimRight(base.EscapedPath(), "/")
	if rest != "" {
		escaped += "/" + strings.TrimLeft(rest, "/")
	}
	joined.Path, _ = url.PathUnescape(escaped)
	joined.RawPath = escaped
	return &joined
}

const resourceMarker = "-/resource/"

// Package indexes can point at metadata or artifacts outside their configured
// base path. Those targets use an encoded origin and are accepted only when
// the origin is the configured upstream or an explicitly allowed resource
// host; an index cannot turn donkey-packages into an arbitrary-host forwarder.
func targetURL(route proxyRoute, rest string) (*url.URL, bool) {
	if strings.HasPrefix(rest, resourceMarker) {
		encoded := strings.TrimPrefix(rest, resourceMarker)
		token, resourcePath, _ := strings.Cut(encoded, "/")
		rawOrigin, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			return nil, false
		}
		origin, err := url.Parse(string(rawOrigin))
		if err != nil || origin.User != nil || origin.Host == "" ||
			(origin.Scheme != "http" && origin.Scheme != "https") ||
			origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" ||
			!route.allowed[originKey(origin)] {
			return nil, false
		}
		return joinURL(origin, resourcePath), true
	}
	if strings.HasPrefix(rest, "-/") {
		return nil, false
	}
	if !route.serviceIndex || (rest != "" && rest != "index.json") {
		base := route.upstream
		if route.serviceIndex {
			origin := *route.upstream
			origin.Path, origin.RawPath, origin.RawQuery, origin.Fragment = "", "", "", ""
			base = &origin
		}
		return joinURL(base, rest), true
	}
	target := *route.upstream
	return &target, true
}

func originKey(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func pathBelow(base, target *url.URL) (string, bool) {
	root := strings.TrimRight(base.EscapedPath(), "/")
	path := target.EscapedPath()
	if path == root {
		return "", true
	}
	if strings.HasPrefix(path, root+"/") {
		return strings.TrimPrefix(path, root+"/"), true
	}
	return "", false
}

func appendURLSuffix(base, escapedPath, rawQuery, fragment string) string {
	value := strings.TrimRight(base, "/")
	if escapedPath != "" {
		value += "/" + strings.TrimLeft(escapedPath, "/")
	}
	if rawQuery != "" {
		value += "?" + rawQuery
	}
	if fragment != "" {
		value += "#" + fragment
	}
	return value
}

func resourceOriginURL(origin string, request proxyRequest) string {
	token := base64.RawURLEncoding.EncodeToString([]byte(origin))
	return request.public + "/-/resource/" + token
}

func internalURL(target *url.URL, request proxyRequest) (string, bool) {
	if target == nil || target.User != nil || target.Host == "" ||
		(target.Scheme != "http" && target.Scheme != "https") ||
		!request.route.allowed[originKey(target)] {
		return "", false
	}
	if sameOrigin(target, request.route.upstream) {
		if request.route.serviceIndex {
			if target.EscapedPath() == request.route.upstream.EscapedPath() {
				return appendURLSuffix(request.public+"/index.json", "", target.RawQuery, target.Fragment), true
			}
			return appendURLSuffix(request.public, target.EscapedPath(), target.RawQuery, target.Fragment), true
		}
		if rest, ok := pathBelow(request.route.upstream, target); ok {
			return appendURLSuffix(request.public, rest, target.RawQuery, target.Fragment), true
		}
	}
	origin := originKey(target)
	return appendURLSuffix(resourceOriginURL(origin, request), target.EscapedPath(), target.RawQuery, target.Fragment), true
}

func rewriteReference(value string, base *url.URL, request proxyRequest) (string, bool) {
	reference, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	target := base.ResolveReference(reference)
	if rewritten, ok := internalURL(target, request); ok {
		return rewritten, true
	}
	// Keep an unconfigured redirect or package link inside the proxy too.
	// targetURL will reject its encoded origin with 403, which is safer and
	// much clearer in a restricted build than silently bypassing the proxy.
	if target.User != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return "", false
	}
	return appendURLSuffix(resourceOriginURL(originKey(target), request), target.EscapedPath(), target.RawQuery, target.Fragment), true
}

func rewriteBody(body []byte, contentType string, request proxyRequest) []byte {
	if request.route.pypi && strings.Contains(contentType, "html") {
		if rewritten, err := rewritePyPIHTML(body, request); err == nil {
			return rewritten
		}
	}
	if strings.Contains(contentType, "json") {
		var value any
		if json.Unmarshal(body, &value) == nil {
			rewriteJSONURLs(&value, "", request)
			if rewritten, err := json.Marshal(value); err == nil {
				return rewritten
			}
		}
	}
	return []byte(rewriteTextURLs(string(body), request))
}

func rewritePyPIHTML(body []byte, request proxyRequest) ([]byte, error) {
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	var out bytes.Buffer
	for {
		typ := tokenizer.Next()
		if typ == html.ErrorToken {
			if err := tokenizer.Err(); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return out.Bytes(), nil
		}
		token := tokenizer.Token()
		if (typ == html.StartTagToken || typ == html.SelfClosingTagToken) && strings.EqualFold(token.Data, "a") {
			for i := range token.Attr {
				if strings.EqualFold(token.Attr[i].Key, "href") {
					if value, ok := rewriteReference(token.Attr[i].Val, request.target, request); ok {
						token.Attr[i].Val = value
					}
				}
			}
		}
		out.WriteString(token.String())
	}
}

func rewriteJSONURLs(value *any, key string, request proxyRequest) {
	switch current := (*value).(type) {
	case map[string]any:
		for childKey, child := range current {
			rewriteJSONURLs(&child, childKey, request)
			current[childKey] = child
		}
	case []any:
		if request.route.serviceIndex && key == "resources" && strings.HasPrefix(request.public, "http://") {
			// This optional NuGet resource requires HTTPS. An HTTP mirror must
			// not advertise it. Package bytes and their signatures stay intact.
			resources := current[:0]
			for _, resource := range current {
				if !repositorySignatures(resource) {
					resources = append(resources, resource)
				}
			}
			current = resources
			*value = current
		}
		for i := range current {
			rewriteJSONURLs(&current[i], key, request)
		}
	case string:
		candidate, err := url.Parse(current)
		if err != nil {
			return
		}
		if !candidate.IsAbs() {
			if !request.route.pypi || !strings.EqualFold(key, "url") {
				return
			}
			candidate = request.target.ResolveReference(candidate)
		}
		if rewritten, ok := internalURL(candidate, request); ok {
			*value = rewritten
		} else if request.route.serviceIndex ||
			(request.route.pypi && strings.EqualFold(key, "url")) ||
			(request.route.prefix == "/npm/" && strings.EqualFold(key, "tarball")) {
			*value = appendURLSuffix(resourceOriginURL(originKey(candidate), request), candidate.EscapedPath(), candidate.RawQuery, candidate.Fragment)
		}
	}
}

func repositorySignatures(value any) bool {
	resource, ok := value.(map[string]any)
	if !ok {
		return false
	}
	switch kinds := resource["@type"].(type) {
	case string:
		return strings.HasPrefix(kinds, "RepositorySignatures/")
	case []any:
		for _, kind := range kinds {
			if name, ok := kind.(string); ok && strings.HasPrefix(name, "RepositorySignatures/") {
				return true
			}
		}
	}
	return false
}

func rewriteTextURLs(value string, request proxyRequest) string {
	upstream := strings.TrimRight(request.route.upstream.String(), "/")
	if request.route.serviceIndex {
		value = strings.ReplaceAll(value, upstream, request.public+"/index.json")
	} else {
		value = strings.ReplaceAll(value, upstream, request.public)
	}
	for origin := range request.route.allowed {
		if strings.HasPrefix(value, origin) || strings.Contains(value, origin) {
			value = strings.ReplaceAll(value, origin, resourceOriginURL(origin, request))
		}
	}
	return value
}

func transport(caFile string) (*http.Transport, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile == "" {
		return transport, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading package upstream certificate authority: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s doesn't contain a usable certificate", caFile)
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return transport, nil
}

type readinessCheck struct {
	cfg       *Config
	transport http.RoundTripper
	mu        sync.Mutex
	checked   time.Time
	err       error
}

func (r *readinessCheck) status(ctx context.Context) error {
	r.mu.Lock()
	if !r.checked.IsZero() && time.Since(r.checked) < 10*time.Second {
		err := r.err
		r.mu.Unlock()
		return err
	}
	r.mu.Unlock()

	err := r.check(ctx)
	r.mu.Lock()
	r.checked, r.err = time.Now(), err
	r.mu.Unlock()
	return err
}

func (r *readinessCheck) check(ctx context.Context) error {
	if err := r.cfg.validateCredentialSource(); err != nil {
		return err
	}
	if len(r.cfg.checkUpstreams) == 0 {
		return nil
	}
	results := make(chan error, len(r.cfg.checkUpstreams))
	for _, raw := range r.cfg.checkUpstreams {
		go func() { results <- r.probe(ctx, raw) }()
	}
	for range r.cfg.checkUpstreams {
		if err := <-results; err != nil {
			return err
		}
	}
	return nil
}

func (r *readinessCheck) probe(parent context.Context, raw string) error {
	target, err := url.Parse(raw)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "donkey-packages-readiness")
	username, password, ok, err := r.cfg.credentials(target.Host)
	if err != nil {
		return err
	}
	if ok {
		req.SetBasicAuth(username, password)
	}
	response, err := r.transport.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("package upstream %s is unavailable: %w", target.Host, err)
	}
	_ = response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return fmt.Errorf("package upstream %s rejected its configured credentials (HTTP %d)", target.Host, response.StatusCode)
	}
	if response.StatusCode >= 500 {
		return fmt.Errorf("package upstream %s is unavailable (HTTP %d)", target.Host, response.StatusCode)
	}
	return nil
}

func Run(ctx context.Context, cfg *Config) error {
	transport, err := transport(cfg.CAFile)
	if err != nil {
		return err
	}
	ready := &readinessCheck{cfg: cfg, transport: transport}
	proxy, err := handler(*cfg, cfg.credentials, transport)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/npm/", proxy)
	mux.Handle("/pypi/", proxy)
	mux.Handle("/go/", proxy)
	mux.Handle("/maven/", proxy)
	mux.Handle("/nuget/", proxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, req *http.Request) {
		if err := ready.status(req.Context()); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 1)
	go func() {
		log.Printf("donkey-packages listening on %s", cfg.Listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func splitCommaSeparated(raw string) []string {
	var values []string
	for value := range strings.SplitSeq(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func nonDefaultUpstreams(values ...string) []string {
	var result []string
	for i := 0; i+1 < len(values); i += 2 {
		if values[i] != "" && values[i] != values[i+1] {
			result = append(result, values[i])
		}
	}
	return result
}
