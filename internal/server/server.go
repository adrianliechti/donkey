package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/k8s"
)

type Server struct {
	cfg       *Config
	k8s       *k8s.Client
	assistant ai.Assistant

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

func New(cfg *Config) (*Server, error) {
	client, err := k8s.NewInCluster()
	if err != nil {
		return nil, err
	}
	// donkey reads every app with its assistant, so a control plane without
	// one is misconfigured — better to say so at startup than to half-work.
	assistant, err := ai.New(cfg.AIModel)
	if err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, k8s: client, assistant: assistant}, nil
}

// lockApp serializes work on one app. The returned function releases it.
func (s *Server) lockApp(ns, app string) func() {
	value, _ := s.appLocks.LoadOrStore(ns+"/"+app, &sync.Mutex{})
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
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/addons", s.auth(s.handleListAddons))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/addons", s.auth(s.handleAddAddon))
	mux.Handle("DELETE /v1/spaces/{space}/apps/{app}/addons/{addon}", s.auth(s.handleRemoveAddon))
	// The app's disk: browse, upload, download, rename, remove.
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleListFiles))
	mux.Handle("GET /v1/spaces/{space}/apps/{app}/files/download", s.auth(s.handleReadFile))
	mux.Handle("PUT /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleWriteFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleWriteFile))
	mux.Handle("DELETE /v1/spaces/{space}/apps/{app}/files", s.auth(s.handleDeleteFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files/move", s.auth(s.handleMoveFile))
	mux.Handle("POST /v1/spaces/{space}/apps/{app}/files/folder", s.auth(s.handleMakeFolder))
	mux.Handle("DELETE /v1/spaces/{space}/apps/{app}", s.auth(s.handleDestroy))
	mux.Handle("GET /v1/doctor", s.auth(s.handleDoctor))
	mux.Handle("POST /v1/ai/analyze", s.auth(s.handleAIAnalyze))
	// Everything else is the admin console.
	mux.Handle("/", uiHandler())

	// Browser-side CSRF protection (Go's Fetch-metadata checks). The API's
	// own auth is a bearer header, which a cross-origin page cannot set, so
	// this is defence in depth today — but it becomes load-bearing the moment
	// anything cookie-based sits in front of the console (putting it behind
	// the gateway's OIDC does exactly that). Non-browser clients send neither
	// Sec-Fetch-Site nor Origin and are unaffected, so the CLI is untouched.
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
		log.Printf("donkey-server %s listening on %s (domain *.%s)", Version, s.cfg.Listen, s.cfg.BaseDomain)
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

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.APIToken)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing API token — run `donkey login`"})
			return
		}
		next(w, r)
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
