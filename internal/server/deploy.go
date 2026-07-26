package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aymanbagabas/go-udiff"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// DeployResult is the final JSON line of a deploy stream.
type DeployResult struct {
	OK       bool   `json:"ok"`
	App      string `json:"app"`
	Space    string `json:"space,omitempty"`
	ImageRef string `json:"imageRef,omitempty"`
	URL      string `json:"url,omitempty"`
	Error    string `json:"error,omitempty"`
	// Diagnosis carries the AI failure analysis when a deploy failed.
	Diagnosis string         `json:"diagnosis,omitempty"`
	Proposals []ai.FixAction `json:"proposals,omitempty"`
	Manifests string         `json:"manifests,omitempty"` // dry-run only
}

// lineStreamer writes progress lines to the client as they happen.
type lineStreamer struct {
	w http.ResponseWriter
	f http.Flusher
}

func newLineStreamer(w http.ResponseWriter) *lineStreamer {
	f, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	return &lineStreamer{w: w, f: f}
}

func (l *lineStreamer) send(v map[string]any) {
	b, _ := json.Marshal(v)
	l.w.Write(append(b, '\n'))
	if l.f != nil {
		l.f.Flush()
	}
}

func (l *lineStreamer) Line(format string, a ...any) {
	l.send(map[string]any{"line": fmt.Sprintf(format, a...)})
}

// Write lets the streamer act as the build progress sink.
func (l *lineStreamer) Write(p []byte) (int, error) {
	l.send(map[string]any{"progress": string(p)})
	return len(p), nil
}

func (l *lineStreamer) Result(res DeployResult) {
	l.send(map[string]any{"result": res})
}

// handleDeploy: POST /v1/deploys?platform=&dryRun=&noWait=&buildOnly=&tag=
// Body: tar.gz of the app folder (donkey.yaml included).
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	// A draining pod is on its way out: taking on a new build it cannot
	// finish would strand the caller for the whole drain window.
	if s.draining.Load() {
		writeErr(w, http.StatusServiceUnavailable,
			fmt.Errorf("the donkey server is restarting — try again in a moment"))
		return
	}
	s.deploying.Add(1)
	defer s.deploying.Done()

	dir, err := os.MkdirTemp("", "donkey-ctx-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(dir)
	if err := extractContext(r.Body, dir); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The app is not always at the top of the upload (a monorepo, a folder
	// dragged from one level up) — find the folder that holds it.
	dir, took, err := contextRoot(dir, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	app, err := config.LoadApp(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if app == nil {
		// Browser uploads have no `donkey init` to lean on — name and port
		// from the deploy form stand in for a donkey.yaml. One in the upload
		// always wins (it is the app's own config, not the form's).
		app, err = appFromParams(r.URL.Query().Get("name"), r.URL.Query().Get("port"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}

	out := newLineStreamer(w)
	if took != "" {
		out.Line("· Building %s — that's the folder with the donkey.yaml.", took)
	}
	// The deploy itself must survive a dropped client connection: route
	// changes can drain the very stream this response rides on. The CLI
	// reconnects via status polling.
	started := time.Now()
	res := s.deploy(context.WithoutCancel(r.Context()), out, dir, app, deployOpts{
		Space:     r.URL.Query().Get("space"),
		Platform:  r.URL.Query().Get("platform"),
		Tag:       r.URL.Query().Get("tag"),
		DryRun:    r.URL.Query().Get("dryRun") == "true",
		NoWait:    r.URL.Query().Get("noWait") == "true",
		BuildOnly: r.URL.Query().Get("buildOnly") == "true",
		NoBuild:   r.URL.Query().Get("noBuild") == "true",
		SHA:       r.URL.Query().Get("sha"),
	})
	// The stream is the caller's record; this line is the server's. Without
	// it nothing on the cluster remembers that a deploy ever happened.
	outcome := "ok"
	if !res.OK {
		outcome = "failed: " + res.Error
	}
	log.Printf("deploy app=%s space=%s image=%s sha=%s duration=%s %s",
		res.App, res.Space, res.ImageRef, r.URL.Query().Get("sha"),
		time.Since(started).Round(time.Second), outcome)
	out.Result(res)
}

// appFromParams synthesizes the minimal app config a web deploy needs when
// the upload carries no donkey.yaml.
func appFromParams(name, portStr string) (*config.AppConfig, error) {
	if name == "" || portStr == "" {
		return nil, fmt.Errorf("the upload has no donkey.yaml — run `donkey init` in the project (CLI), or give the deploy a name and port (console)")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("%q is not a port number", portStr)
	}
	app := &config.AppConfig{Name: name, Port: port}
	app.ApplyDefaults()
	if err := app.Validate(); err != nil {
		return nil, err
	}
	return app, nil
}

// analysisDeadline bounds how long a deploy waits for the assistant after the
// build has finished. It must outlast the assistant's own budget (a 2-minute
// request timeout with one retry) — a shorter deadline would fail deploys
// whose model call is still legitimately in flight.
const analysisDeadline = 5 * time.Minute

type deployOpts struct {
	Space     string
	Platform  string
	Tag       string
	DryRun    bool
	NoWait    bool
	BuildOnly bool
	NoBuild   bool
	SHA       string
}

func (s *Server) deploy(ctx context.Context, out *lineStreamer, dir string, app *config.AppConfig, opts deployOpts) DeployResult {
	// The space decides the namespace and the address, so it is settled
	// before anything else happens. Validation failures return plainly —
	// running the AI diagnosis over a self-explanatory message would only
	// delay the answer.
	space := opts.Space
	if space == "" {
		space = app.Space
	}
	if space == "" {
		space = config.DefaultSpace
	}
	app.Space = space

	fail := func(err error) DeployResult {
		return DeployResult{App: app.Name, Space: space, Error: err.Error()}
	}

	if err := s.rt.ValidateSpace(space); err != nil {
		return fail(err)
	}
	if err := s.rt.CheckSpaceFree(ctx, space); err != nil {
		return fail(err)
	}

	// One deploy per app at a time: concurrent deploys would interleave their
	// applies and race their rollout waits, leaving the live image undefined.
	unlock := s.lockApp(space, app.Name)
	defer unlock()

	// Network choices made in the console hold as long as donkey.yaml
	// doesn't say otherwise — an explicit setting there always wins.
	for _, line := range s.adoptConsoleChoices(ctx, space, app) {
		out.Line("· %s", line)
	}

	if app.Name == addons.SharedOwner {
		return fail(fmt.Errorf("the app name %q is reserved for a space's shared services — pick another name", addons.SharedOwner))
	}
	// Services take plain names too ("postgres" is reachable like an app);
	// an app cannot move in over one.
	if data, err := s.rt.ServiceBinding(ctx, space, addons.SharedOwner, app.Name); err == nil && len(data) > 0 {
		return fail(fmt.Errorf("a service named %q already runs in this space — the app needs another name", app.Name))
	}
	// An unknown addon type has to stop the deploy here, not after minutes
	// of building.
	for _, a := range app.Addons {
		if _, err := addons.Get(a.Type); err != nil {
			return fail(err)
		}
	}

	imageRef := opts.Tag
	if imageRef == "" {
		imageRef = build.ImageTag(s.cfg.Registry.URL, app.Name, opts.SHA, time.Now(), s.cfg.Registry.Immutable)
	}
	// A caller-supplied image bypasses the build pipeline entirely, so it has
	// to come from the registry donkey builds into — otherwise any image on
	// the internet could be deployed under an app's name (and would inherit
	// that app's env and addon secrets).
	if opts.Tag != "" && !build.SameRegistry(s.cfg.Registry.URL, opts.Tag) {
		return fail(fmt.Errorf("--tag must point at %s (donkey only deploys images it built)", s.cfg.Registry.Host()))
	}

	if opts.NoBuild {
		if opts.Tag == "" {
			return fail(fmt.Errorf("a no-build deploy needs the image tag to deploy"))
		}
		// No build means no analysis — keep what the last deploy decided
		// (storage, health route) instead of silently rendering without it.
		for _, line := range s.adoptStoredDecisions(ctx, space, app) {
			out.Line("· %s", line)
		}
		if opts.DryRun {
			return s.renderDryRun(app, opts.Tag)
		}
		return s.applyAndWait(ctx, out, app, dir, opts.Tag, opts.NoWait)
	}

	out.Line("Looking at your app…")
	det, detErr := detect.Detect(dir, app)

	// The assistant reads every deploy: it confirms the detection and, more
	// usefully, finds the folders whose contents have to survive a restart.
	// When detection worked it runs alongside the build — a model call should
	// not sit between the user and their deploy.
	type analysisResult struct {
		analysis *ai.Analysis
		err      error
	}
	analysisReady := make(chan analysisResult, 1)
	run := func() {
		analysis, err := s.analyzeApp(ctx, dir, det, detErr)
		analysisReady <- analysisResult{analysis, err}
	}
	if detErr != nil {
		run() // nothing to build yet — the proposal has to come first
	} else {
		go run()
	}

	if detErr != nil {
		res := <-analysisReady
		analysisReady <- res // the merge step still wants it
		if res.err != nil {
			return fail(fmt.Errorf("%w\n\n%s", detErr, res.err))
		}
		out.Line("Donkey couldn't recognize this app by itself — the assistant thinks it is %s.", res.analysis.AppType)
		if err := rescueDetection(dir, res.analysis); err != nil {
			return fail(fmt.Errorf("%w\n\nthe assistant couldn't propose a way to build it either", detErr))
		}
		var err error
		if det, err = detect.Detect(dir, app); err != nil {
			return fail(err)
		}
	}
	out.Line("✓ %s will be built with %s", app.Name, det.Summary)

	// merge fills in what donkey.yaml left open, right before the app's
	// objects are rendered. Exactly one of the paths below runs it, and it
	// reports every decision. A model that never answers stops the deploy
	// instead of quietly deploying a half-read app.
	merge := func() error {
		var res analysisResult
		select {
		case res = <-analysisReady:
		case <-time.After(analysisDeadline):
			return fmt.Errorf("the assistant didn't answer within %s — check the AI endpoint in the install settings, or deploy again", analysisDeadline)
		}
		if res.err != nil {
			return res.err
		}
		for _, line := range mergeAnalysis(app, res.analysis) {
			out.Line("· %s", line)
		}
		// The merge changed the config after its load-time validation — a
		// proposal that produces an invalid combination must not reach the
		// cluster.
		if err := app.Validate(); err != nil {
			return fmt.Errorf("the assistant's findings don't fit this app's donkey.yaml: %w", err)
		}
		return nil
	}

	if opts.DryRun {
		if err := merge(); err != nil {
			return fail(err)
		}
		return s.renderDryRun(app, imageRef)
	}

	// Platform precedence: --platform flag > donkey.yaml > the server's
	// default (it knows what its nodes run).
	platform := s.cfg.DefaultPlatform
	if app.Build.Platform != "" {
		platform = app.Build.Platform
	}
	if opts.Platform != "" {
		platform = opts.Platform
	}

	logFile, err := os.CreateTemp("", "donkey-build-*.log")
	if err != nil {
		return fail(err)
	}
	logFile.Close()
	defer os.Remove(logFile.Name())

	out.Line("Building the image…")
	builder := &build.BuildKitBuilder{Registry: s.cfg.Registry, BuildkitAddr: s.cfg.BuildkitAddr}
	result, err := builder.Build(ctx, build.Request{
		ContextDir:     dir,
		ImageRef:       imageRef,
		Platform:       platform,
		Push:           true,
		BuildEnv:       app.Build.Env,
		Strategy:       det.Strategy,
		DockerfilePath: det.DockerfilePath,
		RailpackPlan:   det.Railpack,
		Progress:       build.ProgressSink{LogPath: logFile.Name(), Writer: out},
	})
	if err != nil {
		res := DeployResult{App: app.Name, Space: space, Error: err.Error()}
		if logs, rerr := os.ReadFile(logFile.Name()); rerr == nil {
			s.diagnoseWith(ctx, &res, dir, space, app.Name, "build", tailBytes(logs, 15*1024), nil)
		}
		return res
	}
	out.Line("✓ Image ready: %s", result.ImageRef)

	if opts.BuildOnly {
		return DeployResult{OK: true, App: app.Name, Space: space, ImageRef: result.ImageRef}
	}

	if err := merge(); err != nil {
		// The image is already pushed — hand its reference back so a
		// `--no-build --tag` retry doesn't have to rebuild.
		res := fail(err)
		res.ImageRef = result.ImageRef
		return res
	}
	return s.applyAndWait(ctx, out, app, dir, result.ImageRef, opts.NoWait)
}

// renderDryRun renders the app's objects without touching the runtime.
func (s *Server) renderDryRun(app *config.AppConfig, imageRef string) DeployResult {
	res := DeployResult{App: app.Name, Space: app.Space, ImageRef: imageRef}
	rendered, err := s.rt.Render(runtime.DeploySpec{
		App: app, Space: app.Space, ImageRef: imageRef, Decisions: decisionsJSON(app),
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	res.Manifests = rendered
	return res
}

// deployDecisions is what deploy-time analysis (or donkey.yaml) settled for
// an app — persisted as a Deployment annotation so a no-build deploy keeps
// the same shape instead of quietly unmounting the disk the last real deploy
// set up.
type deployDecisions struct {
	Storage         *config.StorageConfig `json:"storage,omitempty"`
	HealthcheckPath string                `json:"healthcheckPath,omitempty"`
}

// consoleChoices are the network settings made in the admin console. They
// live in their own annotation (donkey.dev/console, written by the console
// endpoints only, via patch): a deploy re-renders the decisions annotation
// from donkey.yaml, while a console choice must survive deploys — and it
// does, because server-side apply only owns the keys it writes.
type consoleChoices struct {
	// Exposure is "public", "protected" or "internal"; empty means the
	// donkeyfile (or the default) decides.
	Exposure     string               `json:"exposure,omitempty"`
	HostOverride string               `json:"hostOverride,omitempty"`
	Access       *config.AccessConfig `json:"access,omitempty"`
}

func (s *Server) readConsoleChoices(ctx context.Context, space, app string) consoleChoices {
	var c consoleChoices
	raw, err := s.rt.AppMeta(ctx, space, app, runtime.MetaConsole)
	if err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c
}

// adoptConsoleChoices fills the network settings this donkey.yaml leaves
// open from what the console configured — the file always wins where it
// speaks (an omitted bool is indistinguishable from an explicit false, so
// "speaks" means: sets public/internal/domain/access to something). Runs on
// every deploy; adoption that would make the config invalid is dropped
// again, and only the adopted values, never the user's own.
func (s *Server) adoptConsoleChoices(ctx context.Context, space string, app *config.AppConfig) []string {
	c := s.readConsoleChoices(ctx, space, app.Name)
	var lines []string
	adoptedExposure, adoptedHost, adoptedAccess := false, false, false
	if !app.Public && !app.Internal {
		switch c.Exposure {
		case "public":
			app.Public, adoptedExposure = true, true
			lines = append(lines, "Keeping the console's setting: no login on this app (public)")
		case "internal":
			app.Internal, adoptedExposure = true, true
			lines = append(lines, "Keeping the console's setting: internal only")
		}
	}
	if app.Host == "" && c.HostOverride != "" && !app.Internal {
		app.Host = c.HostOverride
		adoptedHost = true
		lines = append(lines, fmt.Sprintf("Keeping the console's address: %s", c.HostOverride))
	}
	if app.Access == nil && !c.Access.Empty() {
		app.Access = c.Access
		adoptedAccess = true
		lines = append(lines, fmt.Sprintf("Keeping the console's access rules (%d users, %d groups, %d roles)",
			len(c.Access.Users), len(c.Access.Groups), len(c.Access.Roles)))
	}
	if app.Validate() != nil {
		if adoptedExposure {
			app.Public, app.Internal = false, false
		}
		if adoptedHost {
			app.Host = ""
		}
		if adoptedAccess {
			app.Access = nil
		}
		return nil
	}
	return lines
}

// adoptStoredDecisions fills what this donkey.yaml leaves open from the
// decisions recorded on the running Deployment, and says what it kept.
// Adoption never makes the config invalid — an adopted value that no longer
// fits (say, replicas went up since the disk was found) is dropped again,
// and only the adopted values, never the user's own.
func (s *Server) adoptStoredDecisions(ctx context.Context, space string, app *config.AppConfig) []string {
	raw, err := s.rt.AppMeta(ctx, space, app.Name, runtime.MetaDecisions)
	if err != nil || raw == "" {
		return nil
	}
	var dec deployDecisions
	if json.Unmarshal([]byte(raw), &dec) != nil {
		return nil
	}
	var lines []string
	adoptedStorage, adoptedHealth := false, false
	if app.Storage == nil && dec.Storage != nil && len(dec.Storage.Paths) > 0 {
		app.Storage = dec.Storage
		adoptedStorage = true
		lines = append(lines, fmt.Sprintf("Keeping the disk from the last deploy (%s)", strings.Join(dec.Storage.Paths, ", ")))
	}
	if app.Healthcheck.Path == "" && dec.HealthcheckPath != "" {
		app.Healthcheck.Path = dec.HealthcheckPath
		adoptedHealth = true
		lines = append(lines, fmt.Sprintf("Keeping the health check from the last deploy (%s)", dec.HealthcheckPath))
	}
	if app.Validate() != nil {
		// Reverting the adopted fields restores exactly the load-time
		// config, which was already validated.
		if adoptedStorage {
			app.Storage = nil
		}
		if adoptedHealth {
			app.Healthcheck.Path = ""
		}
		return nil
	}
	return lines
}

// applyAndWait hands the built image to the runtime provider, which owns the
// whole go-live choreography (storage, declared services, routes, waiting),
// and turns its typed failures into the deploy's result — with the AI
// diagnosis attached where there is real evidence.
func (s *Server) applyAndWait(ctx context.Context, out *lineStreamer, app *config.AppConfig, root, imageRef string, noWait bool) DeployResult {
	space := app.Space
	err := s.rt.Deploy(ctx, runtime.DeploySpec{
		App:       app,
		Space:     space,
		ImageRef:  imageRef,
		Decisions: decisionsJSON(app),
		NoWait:    noWait,
	}, out)
	if err != nil {
		res := DeployResult{App: app.Name, Space: space, ImageRef: imageRef, Error: err.Error()}
		var je *runtime.JobError
		if errors.As(err, &je) && je.Logs != "" {
			out.Line("Output of the pre-deploy step:\n%s", je.Logs)
			s.diagnoseWith(ctx, &res, root, space, app.Name, "predeploy", je.Logs, nil)
		}
		var re *runtime.RolloutError
		if errors.As(err, &re) {
			for _, p := range re.Evidence.Summaries {
				out.Line("  %s", p)
			}
			if re.Evidence.Logs != "" {
				out.Line("Last lines from the app:\n%s", re.Evidence.Logs)
			}
			s.diagnoseWith(ctx, &res, root, space, app.Name, "rollout", re.Evidence.Logs, re.Evidence.Events)
		}
		return res
	}
	return DeployResult{OK: true, App: app.Name, Space: space, ImageRef: imageRef, URL: s.cfg.AppURL(space, app)}
}

// decisionsJSON records what this deploy settled on (storage, health route)
// for the Deployment annotation no-build deploys read.
func decisionsJSON(app *config.AppConfig) string {
	dec := deployDecisions{Storage: app.Storage, HealthcheckPath: app.Healthcheck.Path}
	if dec.Storage == nil && dec.HealthcheckPath == "" {
		return ""
	}
	b, err := json.Marshal(dec)
	if err != nil {
		return ""
	}
	return string(b)
}

// diagnoseWith runs the AI failure analysis server-side (for build and
// rollout failures — the stages with real evidence) and attaches it to the
// result. Proposals are never applied here: the server computes the diff, and
// the user decides.
//
// root is the uploaded copy of the app, which the assistant reads to ground
// its answer in the actual code. It is a temp dir the server owns, never the
// user's working tree.
func (s *Server) diagnoseWith(ctx context.Context, res *DeployResult, root, space, app, stage, logs string, events []string) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	proposal, aerr := s.assistant.DiagnoseFailure(dctx, &ai.FailureContext{
		Stage:        stage,
		Logs:         logs,
		Events:       events,
		Root:         root,
		KnownSecrets: s.knownSecrets(ctx, space, app),
	})
	if aerr != nil || proposal == nil {
		return
	}
	res.Diagnosis = proposal.Diagnosis
	res.Proposals = withDiffs(root, proposal.Actions)
}

// withDiffs turns each proposed file rewrite into a diff against what is
// actually on disk, and drops the ones that turn out to change nothing. The
// diff is computed here rather than asked for, because a model-written patch
// cannot be trusted to apply and would have to be parsed back anyway.
func withDiffs(dir string, actions []ai.FixAction) []ai.FixAction {
	// The upload is read through a root, so a proposal naming a path that
	// leaves it cannot be diffed against something outside the app — the read
	// simply fails and the old side stays empty.
	root, rerr := os.OpenRoot(dir)
	if rerr == nil {
		defer root.Close()
	}

	out := actions[:0]
	for _, a := range actions {
		if a.Type != "edit_file" {
			out = append(out, a)
			continue
		}
		// os.Root already makes an escaping path unreadable and unwritable,
		// so this is not the safety check — it is the honesty one. Without
		// it a path like "a/../../x" fails the read with "not found" (it
		// stops at the missing "a/" before it ever escapes), and would then
		// be shown as a tidy new-file diff for a change that can never be
		// applied. fs.ValidPath is the standard library's own definition of
		// a legal path inside a tree, so there is no bespoke rule here.
		rel := path.Clean(filepath.ToSlash(strings.TrimSpace(a.Path)))
		if !fs.ValidPath(rel) || rel == "." {
			continue
		}
		a.Path = rel
		old := ""
		if root != nil {
			if b, err := root.ReadFile(rel); err == nil {
				old = string(b)
			}
		}
		if old == a.NewContent {
			continue
		}
		a.Diff = udiff.Unified("a/"+rel, "b/"+rel, old, a.NewContent)
		if strings.TrimSpace(a.Diff) == "" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// knownSecrets collects the values donkey itself handed to an app: env vars,
// addon bindings, and the control plane's own credentials. Failure evidence
// is full of them — an app that cannot reach its database prints the
// connection string it tried — so the assistant gets them scrubbed out.
// Best effort: a secret donkey cannot read is one it cannot redact, but that
// must never cost the user their diagnosis.
func (s *Server) knownSecrets(ctx context.Context, space, app string) []string {
	var out []string
	add := func(m map[string]string) {
		for _, v := range m {
			out = append(out, v)
		}
	}
	if env, err := s.rt.GetEnv(ctx, space, app); err == nil {
		add(env)
	}
	if bindings, err := s.rt.ListServices(ctx, space, app); err == nil {
		for _, b := range bindings {
			if data, err := s.rt.ServiceBinding(ctx, space, app, b.Name); err == nil {
				add(data)
			}
		}
	}
	// Registry and OIDC credentials surface in build and startup logs.
	if pw, err := s.cfg.Registry.Password(); err == nil {
		out = append(out, pw)
	}
	return append(out, s.cfg.OIDCClientSecret, hex.EncodeToString(s.cfg.SessionKey))
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
