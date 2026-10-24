package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/history"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/internal/telemetry"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type Server struct {
	cfg       *Config
	rt        runtime.Provider
	assistant ai.Assistant
	// plans holds what donkey read about an upload between review and deploy,
	// so the assistant answers once per app rather than once per screen. It
	// lives in donkey-orchestrator, which serves every plan route.
	plans     *planStore
	telemetry *telemetry.Client
	resources *telemetry.ResourceExporter
	// history is every app's source as git, or nil when the installation
	// gives the server no volume for it.
	history *history.Store

	// deploying lets a terminating orchestrator finish in-flight deploys.
	deploying sync.WaitGroup
	// draining flips on SIGTERM: readiness fails and, in the orchestrator,
	// new deploys are refused.
	draining atomic.Bool
	// appLocks serializes deploys per app — two at once would interleave their
	// applies and race their rollout waits. Only donkey-orchestrator takes
	// them: everything that locks an app is routed there (see orchestrated).
	appLocks sync.Map
}

func New(cfg *Config, rt runtime.Provider) (*Server, error) {
	// A control plane without an assistant cannot read apps.
	assistant, err := ai.New(cfg.AIModel)
	if err != nil {
		return nil, err
	}
	// Configured is not the same as answering. Refusing to start leaves the
	// pod unready and out of rotation with the reason in its logs, which beats
	// accepting deploys it cannot carry out.
	if err := ai.Reachable(context.Background(), cfg.AIModel); err != nil {
		return nil, err
	}
	// A configured volume that cannot be written is a broken installation,
	// not a reason to deploy without the history it promised.
	var source *history.Store
	if cfg.DataDir != "" {
		if source, err = history.New(filepath.Join(cfg.DataDir, "source")); err != nil {
			return nil, err
		}
	}
	return &Server{
		cfg: cfg, rt: rt, assistant: assistant, plans: newPlanStore(),
		telemetry: telemetry.NewClient(cfg.TelemetryURL, cfg.TelemetryKey),
		resources: telemetry.NewResourceExporter(cfg.TelemetryOTLPEndpoint, cfg.TelemetryKey),
		history:   source,
	}, nil
}

// lockApp serializes work on one app. The returned function releases it.
func (s *Server) lockApp(space, app string) func() {
	value, _ := s.appLocks.LoadOrStore(space+"/"+app, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	// healthz answers while stopping too — kubelet's liveness must not kill a
	// pod that is deliberately finishing its requests.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	// readyz fails as soon as the pod is stopping, so the Service sends new
	// requests to its replacement.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"ok": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	// Ping is the cheap "server up, token good" check — doctor runs the
	// expensive dependency probes, login and preflights only need this.
	mux.Handle("GET /v1/ping", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
	}))
	// Plans, deploys and the changes that have to wait for a deploy run in
	// donkey-orchestrator. They are authenticated here like every other call,
	// so an anonymous upload never travels further, and handed over with the
	// caller's token, which the orchestrator verifies again before it
	// authorizes the space the upload actually names.
	forward := s.orchestratorForwarder()
	for _, route := range s.orchestrated() {
		mux.Handle(route.pattern, s.auth(forward.ServeHTTP))
	}
	// Apps live in a space, so their address does too.
	mux.Handle("GET /v1/apps", s.auth(s.handleListApps))
	mux.Handle("GET /v1/fleet", s.auth(s.handleFleet))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/status", s.auth(s.handleStatus))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/builds", s.auth(s.handleBuilds))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/builds/{sequence}", s.auth(s.handleBuild))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/logs", s.auth(s.handleLogs))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/env", s.auth(s.handleGetEnv))
	mux.Handle("PUT /v1/spaces/{space}/apps/{app}/env", s.auth(s.handlePutEnv))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/restart", s.auth(s.handleRestart))
	// How the app is reached (exposure, domain), changeable without a deploy.
	// Changing it takes the app's deploy lock, so the change is orchestrated.
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/network", s.auth(s.handleGetNetwork))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/diagnose", s.auth(s.handleDiagnose))
	mux.Handle("GET /v1/spaces", s.auth(s.handleListSpaces))
	mux.Handle("POST /v1/spaces", s.auth(s.handleCreateSpace))
	mux.Handle("GET /v1/spaces/{space}/settings", s.auth(s.handleGetSpaceConfig))
	mux.Handle("GET /v1/platform/settings", s.auth(s.handleGetPlatformConfig))
	mux.Handle("PUT /v1/platform/settings", s.auth(s.handlePutPlatformConfig))
	mux.Handle("PUT /v1/spaces/{space}/settings", s.auth(s.handlePutSpaceConfig))
	mux.Handle("GET /v1/spaces/{space}/traffic", s.auth(s.handleTraffic))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/traffic", s.auth(s.handleTraffic))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/resources", s.auth(s.handleResources))
	// The service catalog, and the services of a space. Containers with safe
	// names (services, databases, tables, buckets) live in the path; leaf
	// items with arbitrary names (object keys, row keys) ride in query/body.
	mux.Handle("GET /v1/catalog", s.auth(s.handleCatalog))
	mux.Handle("GET /v1/spaces/{space}/catalog", s.auth(s.handleCatalog))
	// Creating and removing a service is orchestrated, like connecting one.
	mux.Handle("GET /v1/spaces/{space}/services", s.auth(s.handleListSpaceServices))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/binding", s.auth(s.handleServiceBinding))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/http/request", s.auth(s.handleServiceHTTPRequest))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/tunnel", s.auth(s.handleServiceTunnel))
	mux.Handle("/v1/spaces/{space}/services/{service}/tunnel/{path...}", s.auth(s.handleServiceHTTPTunnel))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/ai/models", s.auth(s.handleServiceAI))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/ai/chat/completions", s.auth(s.handleServiceAI))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/exa/search", s.auth(s.handleServiceExa))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/exa/contents", s.auth(s.handleServiceExa))
	// The bucket browser, for services that speak S3.
	mux.Handle("GET /v1/spaces/{space}/services/{service}/buckets", s.auth(s.handleS3Buckets))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/buckets", s.auth(s.handleS3CreateBucket))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}/buckets/{bucket}", s.auth(s.handleS3DeleteBucket))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/buckets/{bucket}/objects", s.auth(s.handleS3Objects))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/buckets/{bucket}/objects", s.auth(s.handleS3Upload))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/buckets/{bucket}/objects/download", s.auth(s.handleS3Download))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}/buckets/{bucket}/objects", s.auth(s.handleS3DeleteObject))
	// The database browser, for services that speak SQL. Schema is a query
	// detail (default public) — most users never touch it.
	mux.Handle("GET /v1/spaces/{space}/services/{service}/databases", s.auth(s.handleDbDatabases))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/databases", s.auth(s.handleDbCreateDatabase))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}/databases/{db}", s.auth(s.handleDbDropDatabase))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/databases/{db}/schemas", s.auth(s.handleDbSchemas))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/databases/{db}/tables", s.auth(s.handleDbTables))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}", s.auth(s.handleDbDropTable))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}/rows", s.auth(s.handleDbRows))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}/rows", s.auth(s.handleDbInsertRow))
	mux.Handle("PUT /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}/rows", s.auth(s.handleDbUpdateRow))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}/rows", s.auth(s.handleDbDeleteRow))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/databases/{db}/query", s.auth(s.handleDbQuery))
	// SQL without an explicit database uses the service's configured default.
	mux.Handle("POST /v1/spaces/{space}/services/{service}/query", s.auth(s.handleDbQuery))
	// Redis keys stay behind the same admin session as the database browser.
	mux.Handle("GET /v1/spaces/{space}/services/{service}/keys", s.auth(s.handleRedisKeys))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/keys", s.auth(s.handleRedisSave))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/keys/value", s.auth(s.handleRedisValue))
	mux.Handle("PUT /v1/spaces/{space}/services/{service}/keys/value", s.auth(s.handleRedisSave))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}/keys/value", s.auth(s.handleRedisDelete))
	// The services an app is connected to.
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/services", s.auth(s.handleListAppServices))
	// The app's disk: browse, upload, download, rename, remove.
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleListFiles))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/files/download", s.auth(s.handleReadFile))
	mux.Handle("PUT /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleWriteFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleWriteFile))
	mux.Handle("DELETE /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleDeleteFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files/move", s.auth(s.handleMoveFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files/folder", s.auth(s.handleMakeFolder))
	mux.Handle("GET /v1/doctor", s.auth(s.handleDoctor))
	// The console's AI chat, forwarded to donkey-ai — see aiforward.go for
	// why it lands on this origin and why nothing is verified here.
	mux.Handle(aiForwardPrefix, aiForwardHandler(s.cfg.AIURL))
	// Everything else is the admin console.
	mux.Handle("/", uiHandler())

	// Browser-side CSRF protection (Go's Fetch-metadata checks). This is
	// load-bearing: the console rides the gateway's OIDC cookie, which the
	// browser attaches to any request — including one a hostile page makes.
	// These checks refuse the cross-site ones. Non-browser clients send
	// neither Sec-Fetch-Site nor Origin and are unaffected, so the CLI is
	// untouched.
	return http.NewCrossOriginProtection().Handler(mux)
}

// shutdownGrace bounds how long a stopping donkey-server keeps answering the
// requests it already has. Nothing here outlives a request any more: a
// deploy stream cut short keeps building in donkey-orchestrator, and the CLI
// and console both fall back to following the app's status. So an upgrade
// replaces the API in seconds instead of waiting for somebody's build; the
// Deployment's terminationGracePeriodSeconds is sized to match.
const shutdownGrace = 15 * time.Second

// Run serves the API and console until the process is asked to stop, then
// takes itself out of rotation and closes what is still open.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:    s.cfg.Listen,
		Handler: s.routes(),
		// Deploy and log streams run for minutes, so no global write timeout
		// — but headers must arrive promptly (slowloris protection).
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Printf("donkey-server %s listening on %s (domain *.%s)", Version, s.cfg.Listen, s.cfg.Domain)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()
	go s.resourceTelemetryLoop(ctx)

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// readyz fails from here on, so the rolling update's new pod takes the
	// traffic while this one finishes the short requests it has.
	s.draining.Store(true)
	log.Printf("donkey-server stopping: closing open requests within %s (deploys carry on in donkey-orchestrator)", shutdownGrace)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		// Log follows and relayed deploy streams never end on their own;
		// their clients reconnect to the replacement.
		return srv.Close()
	}
	return err
}

// auth is who may call the API: a sealed token, verified here, checked
// against the platform and space roles. The browser console's rides in
// X-Access-Token, minted per request by the gateway from the login session;
// the CLI's rides as Authorization: Bearer, through the gateway or a
// port-forward alike. An identity header is just a header: anything in the
// cluster can type one, and app pods legitimately reach this Service (the
// AI proxy), so presence must never be authorization.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := callerToken(r); token != "" {
			if id, err := identity.VerifyToken(s.cfg.SessionKey, token); err == nil {
				settings, err := s.loadPlatformConfig(r.Context(), id)
				if err != nil {
					writeErr(w, http.StatusBadGateway, err)
					return
				}
				ctx := context.WithValue(r.Context(), identityContextKey{}, id)
				r = r.WithContext(context.WithValue(ctx, platformContextKey{}, settings))
				if s.authorizeRoute(w, r) {
					// A tunnel's requests are the service's, not changes to apps.
					if r.Method != http.MethodGet && r.Method != http.MethodHead && !strings.Contains(r.Pattern, "/tunnel/") {
						if provider, ok := s.rt.(runtime.AppSummaryProvider); ok {
							defer provider.InvalidateAppSummaries()
						}
					}
					next(w, r)
				}
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in — run `donkey login`"})
	})
}

// callerToken is the sealed token a request carries: the console's in
// X-Access-Token, the CLI's as a Bearer. auth decides whether it is any good.
func callerToken(r *http.Request) string {
	if token := r.Header.Get("X-Access-Token"); token != "" {
		return token
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	if errors.Is(err, errSpaceForbidden) {
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
