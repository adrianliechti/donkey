// Package gateway is donkey's edge: it terminates TLS (issuing the
// certificates itself), routes hostnames to apps, runs the OIDC login, checks
// each route's access rules and forwards with identity headers.
//
// Login and TLS live here rather than in an ingress vendor's CRDs because no
// proxy stack has a standard auth story — this is the only auth donkey can
// promise on every runtime it supports.
package gateway

import (
	"context"
	"crypto/tls"
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

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime/docker"
	k8s "github.com/adrianliechti/donkey/pkg/runtime/kubernetes"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

type Config struct {
	// Runtime is which provider's world the gateway lives in: "kubernetes"
	// (the default — routes from ConfigMaps, certificates in Secrets) or
	// "docker" (routes and certificates from the shared state directory).
	Runtime string
	// StateDir is the docker runtime's state directory (mounted read-only
	// from the provider's volume).
	StateDir string

	// Listen serves plain http (the https redirect, and ACME HTTP-01
	// challenges); TLSListen terminates TLS. HealthListen serves the
	// kubelet's probes — separate, because /healthz on a traffic port would
	// shadow every app's own /healthz.
	Listen       string
	TLSListen    string
	HealthListen string

	// TLSMode is how the gateway gets certificates: "acme" (on-demand per
	// hostname, Let's Encrypt by default), "ca" (a self-signed CA minting
	// per-hostname certificates — local and internal installs), "static" (a
	// provided certificate, usually a wildcard), or "" — no TLS, plain http
	// on Listen (local development).
	TLSMode string
	// ACMEEmail is the ACME account contact; ACMEDirectory overrides the
	// ACME endpoint (staging, or an internal CA speaking ACME).
	ACMEEmail     string
	ACMEDirectory string
	// TLSSecret names the kubernetes.io/tls Secret of "static" mode.
	TLSSecret string
	// SystemNamespace is where the gateway keeps its certificates (the
	// issued-certs cache, the ACME account, the built-in CA).
	SystemNamespace string
	// WorkloadNamespace, when set, is the single namespace apps live in.
	// Together with SystemNamespace it is the whole of what this gateway may
	// read — and what stops it from picking up a second installation's routes
	// and serving that installation's apps under this one's TLS and login.
	WorkloadNamespace string
	// ServerHost is the console's hostname — always entitled to a
	// certificate, whatever the routing table says.
	ServerHost string

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string

	// Domain centralizes logins: every hostname under it shares one callback
	// and one domain-wide cookie, so there is a single redirect URI to
	// register and one sign-in covers every app. Hostnames outside it keep
	// the per-host flow.
	Domain string

	// SessionKey seals the session cookie (32 bytes, hex). Sessions live in
	// the cookie, not in memory, so replicas need nothing but the same key.
	SessionKey []byte
}

func FromEnv() (*Config, error) {
	c := &Config{
		Runtime:           envOr("DONKEY_RUNTIME", "kubernetes"),
		StateDir:          envOr("DONKEY_STATE_DIR", "/var/lib/donkey"),
		Listen:            envOr("DONKEY_LISTEN", ":8090"),
		TLSListen:         envOr("DONKEY_TLS_LISTEN", ":8443"),
		HealthListen:      envOr("DONKEY_HEALTH_LISTEN", ":8091"),
		TLSMode:           os.Getenv("DONKEY_TLS_MODE"),
		ACMEEmail:         os.Getenv("DONKEY_ACME_EMAIL"),
		ACMEDirectory:     os.Getenv("DONKEY_ACME_DIRECTORY"),
		TLSSecret:         envOr("DONKEY_TLS_SECRET", manifests.GatewayStaticTLSSecret),
		SystemNamespace:   envOr("DONKEY_SYSTEM_NAMESPACE", manifests.DefaultSystemNamespace),
		WorkloadNamespace: os.Getenv("DONKEY_WORKLOAD_NAMESPACE"),
		ServerHost:        os.Getenv("DONKEY_SERVER_HOST"),
		OIDCIssuer:        os.Getenv("DONKEY_OIDC_ISSUER"),
		OIDCClientID:      os.Getenv("DONKEY_OIDC_CLIENT_ID"),
		OIDCClientSecret:  os.Getenv("DONKEY_OIDC_CLIENT_SECRET"),
		Domain:            os.Getenv("DONKEY_DOMAIN"),
	}
	switch c.TLSMode {
	case "", "off", "acme", "ca", "static":
	default:
		return nil, fmt.Errorf("DONKEY_TLS_MODE must be acme, ca, static or off, not %q", c.TLSMode)
	}
	if c.TLSMode == "off" {
		c.TLSMode = ""
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
// serves, read from the route ConfigMaps donkey rendered.
type route struct {
	Host string
	// Path is the route's prefix; "/" for an app's own address.
	Path    string
	Backend *url.URL
	// Login says visitors must sign in before this route forwards.
	Login  bool
	Access *config.AccessConfig
}

// routeSource is where the routing table comes from — the one seam between
// the gateway and the runtime it serves on.
type routeSource interface {
	list(ctx context.Context) ([]route, error)
}

type Server struct {
	cfg    *Config
	k8s    *k8s.Client // set in kubernetes mode only (routes, cert storage)
	source routeSource
	auth   *authenticator

	mu     sync.RWMutex
	routes map[string][]route // host -> routes, most specific path first
	// lastAttempt rate-limits the on-miss refresh: a fresh deploy's hostname
	// must work immediately, an unknown-host scan (or a down API server)
	// must not turn every request into a List.
	lastAttempt time.Time
	// refresh collapses concurrent on-miss refreshes into one List — an
	// unknown-host burst must not fan out to the API server.
	refresh singleflight.Group

	// warmed remembers which hostnames this process already asked a
	// certificate for, so route syncs don't re-kick issuance forever.
	warmed sync.Map
	// warm pre-issues a certificate for a hostname (set in acme mode).
	warm func(host string)
}

func New(cfg *Config) (*Server, error) {
	s := &Server{
		cfg:    cfg,
		auth:   newAuthenticator(cfg),
		routes: map[string][]route{},
	}
	switch cfg.Runtime {
	case "docker":
		s.source = fileRoutes{dir: cfg.StateDir}
	default:
		client, err := k8s.NewInCluster()
		if err != nil {
			return nil, err
		}
		// The gateway speaks for one installation: its own namespace is its
		// identity, and a workload namespace confines it to the two it was
		// given.
		client = client.ForInstallation(cfg.SystemNamespace, cfg.WorkloadNamespace)
		s.k8s = client
		s.source = k8sRoutes{client: client}
	}
	return s, nil
}

// k8sRoutes reads the route ConfigMaps this installation rendered.
type k8sRoutes struct{ client *k8s.Client }

func (k k8sRoutes) list(ctx context.Context) ([]route, error) {
	cms, err := k.client.ListManagedRoutes(ctx)
	if err != nil {
		return nil, err
	}
	var out []route
	for _, cm := range cms {
		if r, ok := routeFromConfigMap(cm); ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// fileRoutes reads the docker provider's routes folder.
type fileRoutes struct{ dir string }

func (f fileRoutes) list(ctx context.Context) ([]route, error) {
	records, err := docker.ReadRoutes(f.dir)
	if err != nil {
		return nil, err
	}
	var out []route
	for _, rec := range records {
		target, err := url.Parse(rec.Backend)
		if err != nil || target.Scheme != "http" || target.Host == "" || rec.Host == "" {
			continue
		}
		r := route{Host: rec.Host, Path: rec.Path, Backend: target, Login: rec.Login, Access: rec.Access}
		if r.Path == "" {
			r.Path = "/"
		}
		out = append(out, r)
	}
	return out, nil
}

// routePollInterval is how often the routing table is re-read. Routes change
// on deploys and console edits — human-paced events — and an unknown host
// triggers an immediate refresh anyway.
const routePollInterval = 10 * time.Second

func (s *Server) Run(ctx context.Context) error {
	// TLS first: edgeTLS installs s.warm, and route syncs read it — setting
	// it up before the first sync (and the poller goroutine) means the
	// initial routes get their certificates pre-issued too, without a race.
	var tlsCfg *tls.Config
	var httpHandler http.Handler
	if s.cfg.TLSMode != "" {
		var err error
		if tlsCfg, httpHandler, err = s.edgeTLS(ctx); err != nil {
			return err
		}
	}
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

	errs := make(chan error, 2)
	servers := []*http.Server{}
	if s.cfg.TLSMode == "" {
		// No TLS: one plain listener — local development, or an edge in
		// front that terminates for us.
		srv := &http.Server{
			Addr:    s.cfg.Listen,
			Handler: s.handler(),
			// App responses stream (SSE, downloads) — no write timeout.
			// Headers must still arrive promptly.
			ReadHeaderTimeout: 10 * time.Second,
		}
		servers = append(servers, srv)
		go func() {
			log.Printf("donkey-gateway listening on %s (no TLS; issuer %s)", s.cfg.Listen, s.cfg.OIDCIssuer)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
				return
			}
			errs <- nil
		}()
	} else {
		httpsSrv := &http.Server{
			Addr:              s.cfg.TLSListen,
			Handler:           s.handler(),
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 10 * time.Second,
		}
		// The plain listener redirects to https — and in acme mode it also
		// answers the HTTP-01 challenges (edgeTLS wrapped that in).
		httpSrv := &http.Server{
			Addr:              s.cfg.Listen,
			Handler:           httpHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		servers = append(servers, httpsSrv, httpSrv)
		go func() {
			log.Printf("donkey-gateway serving https on %s (tls %s; issuer %s)", s.cfg.TLSListen, s.cfg.TLSMode, s.cfg.OIDCIssuer)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
				return
			}
			errs <- nil
		}()
		go func() {
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
				return
			}
			errs <- nil
		}()
	}
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = health.Shutdown(shutdownCtx)
	var err error
	for _, srv := range servers {
		if serr := srv.Shutdown(shutdownCtx); serr != nil {
			err = serr
		}
	}
	return err
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

// refreshRoutes rebuilds the host table from the route source. Only entries
// carrying a backend are this gateway's to serve — that is how the renderer
// says "this route points at me".
func (s *Server) refreshRoutes(ctx context.Context) error {
	_, err, _ := s.refresh.Do("routes", func() (any, error) {
		s.mu.Lock()
		s.lastAttempt = time.Now()
		s.mu.Unlock()
		list, err := s.source.list(ctx)
		if err != nil {
			return nil, err
		}
		table := map[string][]route{}
		for _, r := range list {
			table[r.Host] = append(table[r.Host], r)
		}
		for host := range table {
			sort.Slice(table[host], func(i, j int) bool {
				return len(table[host][i].Path) > len(table[host][j].Path)
			})
		}
		s.mu.Lock()
		s.routes = table
		s.mu.Unlock()
		// A hostname that just appeared gets its certificate now, not on
		// its first visit — the deploy's announcement should be
		// immediately true.
		if s.warm != nil {
			for host := range table {
				if _, seen := s.warmed.LoadOrStore(host, true); !seen {
					go s.warm(host)
				}
			}
		}
		return nil, nil
	})
	return err
}

func routeFromConfigMap(cm corev1.ConfigMap) (route, bool) {
	ann := cm.Annotations
	backend := ann[manifests.AnnotationBackend]
	if backend == "" {
		return route{}, false
	}
	target, err := url.Parse(backend)
	// The annotation is donkey-server's, but a proxy target still deserves a
	// sanity check: in-cluster plain http, nothing else.
	if err != nil || target.Scheme != "http" || target.Host == "" {
		return route{}, false
	}
	if ann[manifests.AnnotationHost] == "" {
		return route{}, false
	}
	r := route{
		Host:    ann[manifests.AnnotationHost],
		Path:    "/",
		Backend: target,
		Login:   ann[manifests.AnnotationLogin] == manifests.LoginRequired,
	}
	if p := ann[manifests.AnnotationPath]; p != "" {
		r.Path = p
	}
	if raw := ann[manifests.AnnotationAccess]; raw != "" {
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
	stale := time.Since(s.lastAttempt) > 2*time.Second
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

// requestHost is the hostname the browser is on, without any port —
// normalized the way the route table is keyed (and allowHost checks), so an
// uppercase or trailing-dot spelling doesn't handshake fine and then 404.
func requestHost(r *http.Request) string {
	h := r.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}
