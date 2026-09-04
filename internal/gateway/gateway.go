// Package gateway is donkey's edge: it terminates TLS, routes hostnames to
// apps, runs the OIDC login, checks
// each route's access rules and forwards with identity headers.
//
// Keeping login, TLS and routing together gives every supported runtime the
// same authentication boundary.
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

	// TLSMode is how the gateway gets certificates: "cert-manager" (Secrets
	// issued for exact hostnames), "acme" (on-demand), or "ca" (a built-in
	// private CA).
	TLSMode string
	// ACMEEmail is the ACME account contact; ACMEDirectory overrides the
	// ACME endpoint (staging, or an internal CA speaking ACME).
	ACMEEmail     string
	ACMEDirectory string
	// SystemNamespace is where cert-manager Secrets and the gateway's ACME/CA
	// state live.
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

	// TelemetryEndpoint is donkey-telemetry's OTLP/HTTP base URL. Empty keeps
	// telemetry disabled for standalone gateway development.
	TelemetryEndpoint string
	// TelemetryKey authenticates gateway metrics and derives privacy-preserving
	// visitor identifiers without sharing the login-session sealing key.
	TelemetryKey []byte
}

func FromEnv() (*Config, error) {
	c := &Config{
		Runtime:           envOr("DONKEY_RUNTIME", "kubernetes"),
		StateDir:          envOr("DONKEY_STATE_DIR", "/var/lib/donkey"),
		Listen:            envOr("DONKEY_LISTEN", ":8090"),
		TLSListen:         envOr("DONKEY_TLS_LISTEN", ":8443"),
		HealthListen:      envOr("DONKEY_HEALTH_LISTEN", ":8091"),
		TLSMode:           envOr("DONKEY_TLS_MODE", "acme"),
		ACMEEmail:         os.Getenv("DONKEY_ACME_EMAIL"),
		ACMEDirectory:     os.Getenv("DONKEY_ACME_DIRECTORY"),
		SystemNamespace:   envOr("DONKEY_SYSTEM_NAMESPACE", manifests.DefaultSystemNamespace),
		WorkloadNamespace: os.Getenv("DONKEY_WORKLOAD_NAMESPACE"),
		ServerHost:        os.Getenv("DONKEY_SERVER_HOST"),
		OIDCIssuer:        os.Getenv("DONKEY_OIDC_ISSUER"),
		OIDCClientID:      os.Getenv("DONKEY_OIDC_CLIENT_ID"),
		OIDCClientSecret:  os.Getenv("DONKEY_OIDC_CLIENT_SECRET"),
		Domain:            os.Getenv("DONKEY_DOMAIN"),
		TelemetryEndpoint: os.Getenv("DONKEY_TELEMETRY_OTLP_ENDPOINT"),
	}
	switch c.TLSMode {
	case "acme", "ca", "cert-manager":
	default:
		return nil, fmt.Errorf("DONKEY_TLS_MODE must be acme, ca or cert-manager, not %q", c.TLSMode)
	}
	// No issuer is a legal installation: the gateway still routes every
	// app, it just has no login to offer — and it fails closed if a route
	// demands one anyway.
	key, err := hex.DecodeString(os.Getenv("DONKEY_SESSION_KEY"))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("DONKEY_SESSION_KEY must be 32 bytes of hex — `donkey server install` generates it")
	}
	c.SessionKey = key
	if c.TelemetryEndpoint != "" {
		telemetryKey, err := hex.DecodeString(os.Getenv("DONKEY_TELEMETRY_KEY"))
		if err != nil || len(telemetryKey) != 32 {
			return nil, fmt.Errorf("DONKEY_TELEMETRY_KEY must be 32 bytes of hex — `donkey server install` generates it")
		}
		c.TelemetryKey = telemetryKey
	}
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
	// Space/App are donkey's logical identity, independent of qualified
	// Kubernetes object names. Only app routes carry them; console traffic is
	// intentionally not mixed into tenant analytics.
	Space string
	App   string
	Host  string
	// Path is the route's prefix; "/" for an app's own address.
	Path    string
	Backend *url.URL
	// Login says visitors must sign in before this route forwards.
	Login  bool
	Access *config.AccessConfig
	// Token forwards a short-lived sealed credential (X-Access-Token) to the
	// backend. Only donkey's own API route carries it: an app given one
	// could replay its visitors' credentials against the API.
	Token bool
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

	// warmed remembers which hostnames this process already asked an ACME
	// certificate for, so route syncs don't re-kick issuance forever.
	warmed sync.Map
	// warm pre-issues a certificate for a hostname (set in acme mode).
	warm func(host string)

	telemetry *edgeTelemetry
}

func New(cfg *Config) (*Server, error) {
	telemetry, err := newEdgeTelemetry(cfg)
	if err != nil {
		return nil, fmt.Errorf("configuring gateway telemetry: %w", err)
	}
	s := &Server{
		cfg:       cfg,
		auth:      newAuthenticator(cfg),
		routes:    map[string][]route{},
		telemetry: telemetry,
	}
	switch cfg.Runtime {
	case "docker":
		s.source = fileRoutes{dir: cfg.StateDir}
	default:
		client, err := k8s.NewInCluster()
		if err != nil {
			_ = telemetry.Shutdown(context.Background())
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
		r := route{Space: rec.Space, App: rec.App, Host: rec.Host, Path: rec.Path, Backend: target, Login: rec.Login, Access: rec.Access, Token: rec.Token}
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
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.telemetry.Shutdown(shutdown); err != nil {
			log.Printf("gateway telemetry shutdown: %v", err)
		}
	}()
	// TLS first: edgeTLS installs s.warm, and route syncs read it — setting
	// it up before the first sync (and the poller goroutine) means the
	// initial routes get their certificates pre-issued too, without a race.
	tlsCfg, httpHandler, err := s.edgeTLS(ctx)
	if err != nil {
		return err
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
	errs := make(chan error, 2)
	servers := []*http.Server{httpsSrv, httpSrv}
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
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = health.Shutdown(shutdownCtx)
	var shutdownErr error
	for _, srv := range servers {
		if serr := srv.Shutdown(shutdownCtx); serr != nil {
			shutdownErr = serr
		}
	}
	return shutdownErr
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
	space, app := cm.Labels[manifests.LabelSpace], cm.Labels[manifests.LabelApp]
	// Scope is part of a route's required identity, not optional telemetry
	// decoration. Serving an unscoped route would work while every request and
	// view metric is silently discarded by the telemetry boundary. The one
	// intentional exception is donkey-server's own console/API route: it is
	// control-plane traffic, not a tenant app stream.
	systemRoute := space == "" && app == "" &&
		cm.Labels[manifests.LabelRole] == manifests.SystemRole(manifests.ServerName) &&
		ann[manifests.AnnotationToken] == manifests.TokenForward
	if (!config.ValidName(space) || !config.ValidName(app)) && !systemRoute {
		return route{}, false
	}
	r := route{
		Space:   space,
		App:     app,
		Host:    ann[manifests.AnnotationHost],
		Path:    "/",
		Backend: target,
		Login:   ann[manifests.AnnotationLogin] == manifests.LoginRequired,
		Token:   systemRoute,
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
