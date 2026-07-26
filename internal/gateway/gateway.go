// Package gateway is donkey's login gateway — the auth proxy every protected
// app's traffic goes through. The shared Gateway (any Gateway API
// implementation) terminates TLS and routes protected hostnames here; this
// proxy runs the OIDC login itself, seals what it learned into a cookie,
// checks the app's access rules (users, groups) and forwards the request to
// the app's Service.
//
// Keeping login in donkey's own binary — instead of a proxy vendor's CRD —
// is a deliberate trade: the Gateway API has no standard auth story, so this
// is the only auth donkey can promise on every Gateway implementation, and
// the only one that can read the ID token's claims (email, groups) with
// every IdP.
package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/k8s"
	"github.com/adrianliechti/donkey/internal/k8s/manifests"
)

type Config struct {
	// Listen serves app traffic; HealthListen serves the kubelet's probes.
	// They are separate ports because /healthz on the traffic port would
	// shadow every app's own /healthz.
	Listen       string
	HealthListen string

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string

	// Domain turns logins central: every hostname under it shares one
	// callback (https://<Domain>/oauth2/callback) and one domain-wide
	// session cookie — a single redirect URI to register at the IdP, and
	// signing in once covers every app. Hostnames outside the base domain
	// (custom `domain:` apps) keep the per-host flow.
	Domain string

	// SessionKey seals the session cookie (32 bytes, hex). Sessions live in
	// the cookie, not in memory, so replicas need nothing but the same key.
	SessionKey []byte
}

func FromEnv() (*Config, error) {
	c := &Config{
		Listen:           envOr("DONKEY_LISTEN", ":8090"),
		HealthListen:     envOr("DONKEY_HEALTH_LISTEN", ":8091"),
		OIDCIssuer:       os.Getenv("DONKEY_OIDC_ISSUER"),
		OIDCClientID:     os.Getenv("DONKEY_OIDC_CLIENT_ID"),
		OIDCClientSecret: os.Getenv("DONKEY_OIDC_CLIENT_SECRET"),
		Domain:           os.Getenv("DONKEY_DOMAIN"),
	}
	// No issuer is a legal installation: the gateway still routes every
	// app, it just has no login to offer — and it fails closed if a route
	// demands one anyway.
	key, err := hex.DecodeString(os.Getenv("DONKEY_SESSION_KEY"))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("DONKEY_SESSION_KEY must be 32 bytes of hex — `donkey server install` generates it")
	}
	c.SessionKey = key
	return c, nil
}

// authConfigured says the installation can actually run a login round trip.
func (c *Config) authConfigured() bool {
	return c.OIDCIssuer != "" && c.OIDCClientID != "" && c.OIDCClientSecret != ""
}

// ssoHost reports whether host shares the installation's session domain —
// its logins ride the central callback on the base domain and a cookie the
// whole domain sees. Without a base domain (an older installation), every
// host keeps its own flow.
func (c *Config) ssoHost(host string) bool {
	if c.Domain == "" {
		return false
	}
	return host == c.Domain || strings.HasSuffix(host, "."+c.Domain)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// route is one hostname (or one path prefix of a hostname) this gateway
// serves, read from the HTTPRoutes donkey rendered.
type route struct {
	Host string
	// Path is the route's prefix; "/" for an app's own address.
	Path    string
	Backend *url.URL
	// Login says visitors must sign in before this route forwards.
	Login  bool
	Access *config.AccessConfig
}

type Server struct {
	cfg    *Config
	k8s    *k8s.Client
	sealer *sealer
	auth   *authenticator

	mu     sync.RWMutex
	routes map[string][]route // host -> routes, most specific path first
	// lastRefresh rate-limits the on-miss refresh: a fresh deploy's hostname
	// must work immediately, an unknown-host scan must not hammer the API.
	lastRefresh time.Time
}

func New(cfg *Config) (*Server, error) {
	client, err := k8s.NewInCluster()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:    cfg,
		k8s:    client,
		sealer: newSealer(cfg.SessionKey),
		auth:   newAuthenticator(cfg),
		routes: map[string][]route{},
	}, nil
}

// routePollInterval is how often the routing table is re-read. Routes change
// on deploys and console edits — human-paced events — and an unknown host
// triggers an immediate refresh anyway.
const routePollInterval = 10 * time.Second

func (s *Server) Run(ctx context.Context) error {
	if err := s.refreshRoutes(ctx); err != nil {
		// Not fatal: the API server may still be warming up; requests miss
		// and retry, and the poller keeps trying.
		log.Printf("initial route sync failed (will retry): %v", err)
	}
	go s.pollRoutes(ctx)

	health := &http.Server{Addr: s.cfg.HealthListen, Handler: healthMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("health listener: %v", err)
		}
	}()

	srv := &http.Server{
		Addr:    s.cfg.Listen,
		Handler: s.handler(),
		// App responses stream (SSE, downloads) — no write timeout. Headers
		// must still arrive promptly.
		ReadHeaderTimeout: 10 * time.Second,
	}
	errs := make(chan error, 1)
	go func() {
		log.Printf("donkey-gateway listening on %s (issuer %s)", s.cfg.Listen, s.cfg.OIDCIssuer)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = health.Shutdown(shutdownCtx)
	return srv.Shutdown(shutdownCtx)
}

func healthMux() *http.ServeMux {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"ok":true}`)
	}
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	return mux
}

func (s *Server) pollRoutes(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(routePollInterval):
		}
		if err := s.refreshRoutes(ctx); err != nil {
			log.Printf("route sync failed: %v", err)
		}
	}
}

// refreshRoutes rebuilds the host table from the HTTPRoutes donkey manages.
// Only routes carrying the backend annotation are this gateway's to serve —
// that annotation is how the renderer says "this route points at me".
func (s *Server) refreshRoutes(ctx context.Context) error {
	list, err := s.k8s.ListManagedRoutes(ctx)
	if err != nil {
		return err
	}
	table := map[string][]route{}
	for _, obj := range list {
		r, ok := routeFromObject(obj)
		if !ok {
			continue
		}
		table[r.Host] = append(table[r.Host], r)
	}
	for host := range table {
		sort.Slice(table[host], func(i, j int) bool {
			return len(table[host][i].Path) > len(table[host][j].Path)
		})
	}
	s.mu.Lock()
	s.routes = table
	s.lastRefresh = time.Now()
	s.mu.Unlock()
	return nil
}

func routeFromObject(obj unstructured.Unstructured) (route, bool) {
	backend := obj.GetAnnotations()[manifests.AnnotationBackend]
	if backend == "" {
		return route{}, false
	}
	target, err := url.Parse(backend)
	// The annotation is donkey-server's, but a proxy target still deserves a
	// sanity check: in-cluster plain http, nothing else.
	if err != nil || target.Scheme != "http" || target.Host == "" {
		return route{}, false
	}
	hosts, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "hostnames")
	if len(hosts) == 0 {
		return route{}, false
	}
	r := route{
		Host:    hosts[0],
		Path:    "/",
		Backend: target,
		Login:   obj.GetAnnotations()[manifests.AnnotationLogin] == manifests.LoginRequired,
	}
	if rules, _, _ := unstructured.NestedSlice(obj.Object, "spec", "rules"); len(rules) > 0 {
		if rule, ok := rules[0].(map[string]any); ok {
			if matches, _, _ := unstructured.NestedSlice(rule, "matches"); len(matches) > 0 {
				if m, ok := matches[0].(map[string]any); ok {
					if v, _, _ := unstructured.NestedString(m, "path", "value"); v != "" {
						r.Path = v
					}
				}
			}
		}
	}
	if raw := obj.GetAnnotations()[manifests.AnnotationAccess]; raw != "" {
		var access config.AccessConfig
		if err := json.Unmarshal([]byte(raw), &access); err == nil {
			r.Access = &access
		}
	}
	return r, true
}

// lookup finds the most specific route for a request. On a miss it refreshes
// once (rate-limited) — a hostname deployed seconds ago must already work.
func (s *Server) lookup(ctx context.Context, host, path string) (route, bool) {
	if r, ok := s.tableLookup(host, path); ok {
		return r, true
	}
	s.mu.RLock()
	stale := time.Since(s.lastRefresh) > 2*time.Second
	s.mu.RUnlock()
	if stale {
		if err := s.refreshRoutes(ctx); err == nil {
			return s.tableLookup(host, path)
		}
	}
	return route{}, false
}

func (s *Server) tableLookup(host, path string) (route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.routes[host] {
		if r.Path == "/" || path == r.Path || strings.HasPrefix(path, r.Path+"/") {
			return r, true
		}
	}
	return route{}, false
}

// requestHost is the hostname the browser is on, without any port.
func requestHost(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		return h
	}
	return r.Host
}
