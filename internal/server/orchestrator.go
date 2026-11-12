package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/identity"
)

// donkey-orchestrator is this control plane started with `orchestrator`. It
// does the work that outlives an HTTP request: reading an upload, building and
// repairing it with BuildKit, pushing, applying, waiting for the rollout and
// recording the release. It also runs the few changes that must not
// interleave with a deploy. donkey-server stays the stateless API and console
// in front of it. The server authenticates every call, forwards these to the
// orchestrator, and relays the answer unchanged. Restarting the API therefore
// no longer waits for somebody's build, and restarting the orchestrator (one
// replica, drained before it goes) no longer takes the console with it.
//
// Both processes run the same Server against the same runtime. The pipeline
// is not split into a client and a service; only where it runs changed.

type orchestratedRoute struct {
	pattern string
	handler http.HandlerFunc
}

// orchestrated is every route donkey-orchestrator serves for callers.
// donkey-server forwards exactly these patterns, so the two lists cannot
// drift apart, and authorizeRoute sees the same r.Pattern in both processes.
func (s *Server) orchestrated() []orchestratedRoute {
	return []orchestratedRoute{
		{"GET /v1/spaces/{space}/apps/{app}/builds", s.handleBuilds},
		{"GET /v1/spaces/{space}/apps/{app}/builds/{sequence}", s.handleBuild},
		{"GET /v1/spaces/{space}/apps/{app}/security", s.handleSecurity},
		{"POST /v1/deploys", s.handleDeploy},
		// What the orchestrator is deploying now and finished lately: the
		// board lives in this process, beside the deploys it describes.
		{"GET /v1/deploys", s.handleDeploys},
		// A plan is a deploy read out loud before it happens: what the app is,
		// how it will be built, what it needs and what will break. The console
		// reviews one, `donkey init` prints one, and the deploy that follows
		// carries its id so the assistant is asked once, not twice. The plan
		// store is this process's memory, so plans are made where they are
		// deployed.
		{"POST /v1/plans", s.handlePlan},
		{"PUT /v1/plans/{id}", s.handlePlanUpdate},
		{"POST /v1/projects/plans", s.handleProjectPlan},
		{"PUT /v1/projects/plans/{id}", s.handleProjectUpdate},
		{"POST /v1/projects/deploys", s.handleProjectDeploy},
		// Network changes take the per-app lock a deploy holds through its
		// rollout (lockApp). A mutex only serializes what runs in its own
		// process, so they run where the deploys run.
		{"PUT /v1/spaces/{space}/apps/{app}/network", s.handlePutNetwork},
		{"PUT /v1/spaces/{space}/apps/{app}/env", s.handlePutEnv},
		{"POST /v1/spaces/{space}/apps/{app}/restart", s.handleRestart},
		// Settings can rewrite routes; creation must wait for startup migration.
		{"POST /v1/spaces", s.handleCreateSpace},
		{"PUT /v1/spaces/{space}/settings", s.handlePutSpaceConfig},
		// Services likewise: the runtime serializes creating and removing one
		// per space and name (addons.InstanceLocks), the same lock a deploy's
		// declared services take, and only inside the process that holds it.
		{"POST /v1/spaces/{space}/services", s.handleAddSpaceService},
		{"DELETE /v1/spaces/{space}/services/{service}", s.handleRemoveSpaceService},
		{"POST /v1/spaces/{space}/apps/{app}/services", s.handleAddAppService},
		{"DELETE /v1/spaces/{space}/apps/{app}/services/{service}", s.handleRemoveAppService},
		// The source history lives on this process's data volume, where the
		// deploys above write it. Reading it here too keeps one process the
		// owner of the repositories, so its in-process locks are all they
		// need, and the volume can be one a single node attaches.
		{"GET /v1/spaces/{space}/apps/{app}/commits", s.handleCommits},
		{"GET /v1/spaces/{space}/apps/{app}/commits/{commit}", s.handleCommit},
		{"GET /v1/spaces/{space}/apps/{app}/commits/{commit}/diff", s.handleCommitDiff},
		{"GET /v1/spaces/{space}/apps/{app}/commits/{commit}/tree", s.handleCommitTree},
		{"GET /v1/spaces/{space}/apps/{app}/commits/{commit}/file", s.handleCommitFile},
		{"GET /v1/spaces/{space}/apps/{app}/commits/{commit}/archive", s.handleCommitArchive},
		// A person's own machine. Pulling its image and making its disk can
		// take minutes, and a start must not die with the request or with a
		// rolling donkey-server.
		{"POST /v1/machine", s.handleStartMachine},
		{"POST /v1/machine/stop", s.handleStopMachine},
		{"DELETE /v1/machine", s.handleDeleteMachine},
		// Removing an app or a space takes its history with it.
		{"DELETE /v1/spaces/{space}/apps/{app}", s.handleDestroy},
		{"DELETE /v1/spaces/{space}", s.handleDeleteSpace},
	}
}

func (s *Server) orchestratorRoutes() http.Handler {
	mux := http.NewServeMux()
	// healthz answers while draining too — kubelet's liveness must not kill a
	// pod that is deliberately finishing its deploys.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	// readyz fails as soon as the pod is draining, so donkey-server stops
	// reaching it with new work while the old deploys run out.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]bool{"ok": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	for _, route := range s.orchestrated() {
		mux.Handle(route.pattern, s.orchestratorAuth(route.handler))
	}
	// donkey-server's doctor asks this for the checks only the orchestrator
	// can make (see orchestratorChecks).
	mux.Handle("GET /v1/doctor", s.orchestratorAuth(s.handleBuildChecks))
	// No cross-origin middleware here: no browser reaches this listener, and
	// the one caller it admits has already applied it.
	return mux
}

// orchestratorTokenHeader carries donkey-server's own credential, next to
// the caller's sealed token in X-Access-Token.
const orchestratorTokenHeader = "X-Donkey-Orchestrator-Token"

const orchestratorTokenPurpose = "donkey/orchestrator/server/v1"

// orchestratorToken is donkey-server's credential at donkey-orchestrator. It
// is derived from the session key both processes already hold, under a
// purpose of its own (the way telemetry derives its query token), so it is
// worth nothing anywhere else and needs no secret of its own to generate and
// keep. It rotates with the session key, which rolls both deployments.
func orchestratorToken(key []byte) string {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(orchestratorTokenPurpose))
	return hex.EncodeToString(h.Sum(nil))
}

func orchestratorTokenMatches(got, want string) bool {
	return len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// orchestratorAuth admits donkey-server and nobody else, then authorizes the
// caller exactly as donkey-server's own routes do. The forwarded sealed token
// is verified again with the session key, the platform settings are read,
// and authorizeRoute checks the route's space; uploads go on to check the
// space their donkey.yaml names, in their handlers. Trusting an identity the
// server merely passed along would make this listener the weakest door into
// the cluster. On Docker it sits on every space network, next to tenant code.
func (s *Server) orchestratorAuth(next http.HandlerFunc) http.Handler {
	want := orchestratorToken(s.cfg.SessionKey)
	authorized := s.auth(s.serializeMutation(next))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !orchestratorTokenMatches(r.Header.Get(orchestratorTokenHeader), want) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "donkey-orchestrator only takes requests from donkey-server — send them to the donkey API instead",
			})
			return
		}
		authorized.ServeHTTP(w, r)
	})
}

// serializeMutation keeps space-wide changes from racing a deploy, and app
// changes from racing another change to the same app. Upload handlers take
// these same locks once their target space and app are known.
func (s *Server) serializeMutation(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if space := r.PathValue("space"); space != "" {
			mu := s.spaceLock(space)
			if r.Pattern == "DELETE /v1/spaces/{space}" || r.Pattern == "PUT /v1/spaces/{space}/settings" {
				mu.Lock()
				defer mu.Unlock()
			} else {
				mu.RLock()
				defer mu.RUnlock()
			}
			if app := r.PathValue("app"); app != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
				unlock := s.lockApp(space, app)
				defer unlock()
			}
		}
		next(w, r)
	}
}

// orchestratorTransport keeps the hop to the orchestrator off any forward
// proxy the installation configured (control-plane traffic never goes through
// one), and sets no response deadline: a plan answers once the assistant has
// read the app, which takes minutes.
var orchestratorTransport http.RoundTripper = &http.Transport{
	Proxy:               nil,
	DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     90 * time.Second,
}

var errOrchestratorUnavailable = errors.New("donkey-orchestrator isn't answering — it may be finishing the deploys already under way before a restart; try again in a moment")

// orchestratorForwarder hands a call donkey-server has authenticated to
// donkey-orchestrator and relays the answer unchanged: status, headers and
// body, with an NDJSON progress stream flushed line by line as it is
// written. The upload streams straight through; nothing is buffered here.
//
// The caller's sealed token travels on in X-Access-Token, whichever way it
// arrived, and the orchestrator verifies it itself. Nothing else speaks for
// the caller: identity headers are stripped, so a header a client typed can
// never pass for one the server set.
//
// The forwarded request carries the caller's context, so a client that goes
// away cancels the hop but not the deploy. The orchestrator runs that on
// context.WithoutCancel, as it always has.
func (s *Server) orchestratorForwarder() http.Handler {
	base, err := url.Parse(s.cfg.OrchestratorURL)
	if err != nil || base.Host == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeErr(w, http.StatusServiceUnavailable,
				fmt.Errorf("the orchestrator address %q is not a URL (DONKEY_ORCHESTRATOR_URL)", s.cfg.OrchestratorURL))
		})
	}
	credential := orchestratorToken(s.cfg.SessionKey)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			token := callerToken(pr.In)
			identity.StripHeaders(pr.Out.Header)
			pr.Out.Header.Del("Authorization")
			// The orchestrator reads no cookies; whatever a browser sent
			// stays on this side.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("X-Access-Token", token)
			pr.Out.Header.Set(orchestratorTokenHeader, credential)
		},
		Transport: orchestratorTransport,
		// Progress streams line by line; buffering it would turn a live build
		// into one long silence followed by everything at once.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				log.Printf("forwarding %s %s to donkey-orchestrator: %v", r.Method, r.URL.Path, err)
			}
			writeErr(w, http.StatusServiceUnavailable, errOrchestratorUnavailable)
		},
	}
}

// orchestratorChecks is donkey-orchestrator's part of `donkey doctor`: that it
// answers at all, and the build checks only it can make. It asks with the
// caller's token, so the orchestrator authorizes the doctor like any route.
func (s *Server) orchestratorChecks(r *http.Request) []DoctorCheck {
	check := DoctorCheck{Name: "Orchestrator (donkey-orchestrator)"}
	fail := func(detail string) []DoctorCheck {
		check.Detail = detail
		return []DoctorCheck{check}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		strings.TrimSuffix(s.cfg.OrchestratorURL, "/")+"/v1/doctor", nil)
	if err != nil {
		return fail(err.Error())
	}
	req.Header.Set("X-Access-Token", callerToken(r))
	req.Header.Set(orchestratorTokenHeader, orchestratorToken(s.cfg.SessionKey))
	res, err := (&http.Client{Transport: orchestratorTransport}).Do(req)
	if err != nil {
		return fail(fmt.Sprintf("not answering, so nothing builds or deploys: %v", err))
	}
	defer res.Body.Close()
	var answer struct {
		Checks []DoctorCheck `json:"checks"`
		Error  string        `json:"error"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&answer)
	switch {
	case res.StatusCode != http.StatusOK && answer.Error != "":
		return fail(answer.Error)
	case res.StatusCode != http.StatusOK:
		return fail("answered " + res.Status)
	case decodeErr != nil:
		return fail("answered, but not with its checks — is it the same version as donkey-server?")
	}
	check.OK, check.Detail = true, "answering"
	return append([]DoctorCheck{check}, answer.Checks...)
}

// drainTimeout bounds how long a terminating orchestrator waits for its
// deploys. The Deployment's terminationGracePeriodSeconds and the Docker stop
// timeout are sized from the same value.
const drainTimeout = config.DeployDrainTimeout

// RunOrchestrator serves the orchestrated routes until the process is asked
// to stop, then drains: new deploys are refused and the running ones are
// given until drainTimeout to finish. Without this, every orchestrator
// upgrade would kill whatever was mid-build.
func (s *Server) RunOrchestrator(ctx context.Context) error {
	if err := s.migrateTelemetryDeployments(ctx); err != nil {
		return err
	}
	background, stopBackground := context.WithCancel(ctx)
	backgroundDone := make(chan struct{})
	go func() { defer close(backgroundDone); s.scanLoop(background) }()
	defer func() { stopBackground(); <-backgroundDone }()
	srv := &http.Server{
		Addr:    s.cfg.Listen,
		Handler: s.orchestratorRoutes(),
		// Deploy streams run for minutes, so no global write timeout — but
		// headers must arrive promptly (slowloris protection).
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Printf("donkey-orchestrator %s listening on %s (domain *.%s)", Version, s.cfg.Listen, s.cfg.Domain)
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
	log.Printf("donkey-orchestrator draining: finishing deploys already under way (up to %s)", drainTimeout)

	finished := make(chan struct{})
	go func() {
		s.deploying.Wait()
		close(finished)
	}()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	defer cancel()
	select {
	case <-finished:
		log.Print("donkey-orchestrator: all deploys finished")
	case <-shutdownCtx.Done():
		log.Print("donkey-orchestrator: gave up waiting for deploys still running")
	}
	return srv.Shutdown(shutdownCtx)
}
