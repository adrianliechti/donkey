package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/identity"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type Server struct {
	cfg       *Config
	rt        runtime.Provider
	assistant ai.Assistant
	// plans holds what donkey read about an upload between the review and the
	// deploy that follows it — so the assistant answers once per app, not once
	// per screen.
	plans *planStore

	// deploying counts the deploys currently in flight, so a terminating pod
	// can finish them instead of killing builds halfway.
	deploying sync.WaitGroup
	// draining flips on SIGTERM: readiness starts failing and new deploys are
	// refused, while running ones are given time to finish.
	draining atomic.Bool
	// appLocks serializes deploys per app: two concurrent deploys of the same
	// app would interleave their applies and race their rollout waits, and
	// whichever image ended up live would be anybody's guess.
	appLocks sync.Map
}

func New(cfg *Config, rt runtime.Provider) (*Server, error) {
	// donkey reads every app with its assistant, so a control plane without
	// one is misconfigured — better to say so at startup than to half-work.
	assistant, err := ai.New(cfg.AIModel)
	if err != nil {
		return nil, err
	}
	// railpack's images are package variables, so an installation that
	// mirrors them redirects them once, here, before any build runs.
	build.ConfigureRailpackImages(cfg.Build.Railpack)
	// The version resolver is the one thing railpack downloads from inside
	// this process. Staging it now turns a mid-deploy timeout into a line in
	// the log at startup.
	//
	// Unconditionally, even where buildpacks is the installation's builder:
	// an app's own donkey.yaml may still say build.type: railpack, and on the
	// restricted network this all exists for, that deploy would otherwise
	// hang reaching for github.com. The binary is baked into donkey's image
	// either way, so staging it costs a file copy.
	if err := build.EnsureMise(context.Background(), cfg.Build.Env); err != nil {
		log.Printf("railpack's version resolver isn't ready: %v", err)
	}
	return &Server{cfg: cfg, rt: rt, assistant: assistant, plans: newPlanStore()}, nil
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
	// healthz answers while draining too — kubelet's liveness must not kill a
	// pod that is deliberately finishing its deploys.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	// readyz fails as soon as the pod is draining, so the gateway stops
	// sending it new work while the old deploys run out.
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
	mux.Handle("POST /v1/deploys", s.auth(s.handleDeploy))
	// A plan is a deploy read out loud before it happens: what the app is,
	// how it will be built, what it needs and what will break. The console
	// reviews one, `donkey init` prints one, and the deploy that follows
	// carries its id so the assistant is asked once, not twice.
	mux.Handle("POST /v1/plans", s.auth(s.handlePlan))
	mux.Handle("PUT /v1/plans/{id}", s.auth(s.handlePlanUpdate))
	// Apps live in a space, so their address does too.
	mux.Handle("GET /v1/apps", s.auth(s.handleListApps))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/status", s.auth(s.handleStatus))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/logs", s.auth(s.handleLogs))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/env", s.auth(s.handleGetEnv))
	mux.Handle("PUT /v1/spaces/{space}/apps/{app}/env", s.auth(s.handlePutEnv))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/restart", s.auth(s.handleRestart))
	// How the app is reached (exposure, domain) and who may open it —
	// changeable from the console without a deploy.
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/network", s.auth(s.handleGetNetwork))
	mux.Handle("PUT /v1/spaces/{space}/apps/{app}/network", s.auth(s.handlePutNetwork))
	mux.Handle("PUT /v1/spaces/{space}/apps/{app}/access", s.auth(s.handlePutAccess))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/diagnose", s.auth(s.handleDiagnose))
	mux.Handle("GET /v1/spaces", s.auth(s.handleListSpaces))
	mux.Handle("POST /v1/spaces", s.auth(s.handleCreateSpace))
	mux.Handle("DELETE /v1/spaces/{space}", s.auth(s.handleDeleteSpace))
	// The service catalog, and the services of a space. Containers with safe
	// names (services, databases, tables, buckets) live in the path; leaf
	// items with arbitrary names (object keys, row keys) ride in query/body.
	mux.Handle("GET /v1/catalog", s.auth(s.handleCatalog))
	mux.Handle("GET /v1/spaces/{space}/services", s.auth(s.handleListSpaceServices))
	mux.Handle("POST /v1/spaces/{space}/services", s.auth(s.handleAddSpaceService))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}", s.auth(s.handleRemoveSpaceService))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/binding", s.auth(s.handleServiceBinding))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/tunnel", s.auth(s.handleServiceTunnel))
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
	mux.Handle("GET /v1/spaces/{space}/services/{service}/databases/{db}/tables", s.auth(s.handleDbTables))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}", s.auth(s.handleDbDropTable))
	mux.Handle("GET /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}/rows", s.auth(s.handleDbRows))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}/rows", s.auth(s.handleDbInsertRow))
	mux.Handle("PUT /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}/rows", s.auth(s.handleDbUpdateRow))
	mux.Handle("DELETE /v1/spaces/{space}/services/{service}/databases/{db}/tables/{table}/rows", s.auth(s.handleDbDeleteRow))
	mux.Handle("POST /v1/spaces/{space}/services/{service}/databases/{db}/query", s.auth(s.handleDbQuery))
	// The services an app is connected to.
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/services", s.auth(s.handleListAppServices))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/services", s.auth(s.handleAddAppService))
	mux.Handle("DELETE /v1/spaces/{space}/apps/{app}/services/{service}", s.auth(s.handleRemoveAppService))
	// The app's disk: browse, upload, download, rename, remove.
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleListFiles))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/files/download", s.auth(s.handleReadFile))
	mux.Handle("PUT /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleWriteFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleWriteFile))
	mux.Handle("DELETE /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleDeleteFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files/move", s.auth(s.handleMoveFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files/folder", s.auth(s.handleMakeFolder))
	mux.Handle("DELETE /v1/spaces/{space}/apps/{app}", s.auth(s.handleDestroy))
	// The openai addon's proxy — its own per-space tokens, not the admin
	// token; the real upstream credential never travels past this handler.
	mux.Handle(aiProxyPrefix, s.aiProxyHandler())
	mux.Handle("GET /v1/doctor", s.auth(s.handleDoctor))
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

// drainTimeout bounds how long a terminating server waits for its deploys.
// A build plus rollout can legitimately take minutes; the Deployment's
// terminationGracePeriodSeconds is sized to match.
const drainTimeout = 10 * time.Minute

// Run serves until the process is asked to stop, then drains: new deploys are
// refused and the running ones are given until drainTimeout to finish. Without
// this, every control-plane upgrade would kill whatever was mid-build.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:    s.cfg.Listen,
		Handler: s.routes(),
		// Deploy streams run for minutes, so no global write timeout — but
		// headers must arrive promptly (slowloris protection).
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

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	s.draining.Store(true)
	log.Printf("donkey-server draining: finishing deploys already under way (up to %s)", drainTimeout)

	finished := make(chan struct{})
	go func() {
		s.deploying.Wait()
		close(finished)
	}()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	select {
	case <-finished:
		log.Print("donkey-server: all deploys finished")
	case <-shutdownCtx.Done():
		log.Print("donkey-server: gave up waiting for deploys still running")
	}
	return srv.Shutdown(shutdownCtx)
}

// auth is who may call the API. The usual path: the donkey gateway fronted
// the request, ran the OIDC login (or verified a sealed CLI token), checked
// the admin rules and forwarded the identity as headers — its presence is
// the authorization. The direct path (a port-forward past the gateway):
// a sealed CLI token this server verifies itself, against the same admin
// rules. In-cluster callers who can reach the Service unprompted are
// already inside the trust boundary this PoC draws — see the README.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Request-User") != "" {
			next(w, r)
			return
		}
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && tok != "" {
			if id, err := identity.VerifyToken(s.cfg.SessionKey, tok); err == nil &&
				s.cfg.Admins.Allows(id.Email, id.Groups, id.Roles) {
				next(w, r)
				return
			}
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in — run `donkey login`"})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
