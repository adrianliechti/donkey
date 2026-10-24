package server

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
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
	Apps     []DeployResult `json:"apps,omitempty"`
	OK       bool           `json:"ok"`
	App      string         `json:"app"`
	Space    string         `json:"space,omitempty"`
	ImageRef string         `json:"imageRef,omitempty"`
	URL      string         `json:"url,omitempty"`
	Error    string         `json:"error,omitempty"`
	// Diagnosis carries the AI failure analysis when a deploy failed.
	Diagnosis string         `json:"diagnosis,omitempty"`
	Proposals []ai.FixAction `json:"proposals,omitempty"`
	Manifests string         `json:"manifests,omitempty"` // dry-run only
}

// DeployUpdate is structured release progress for clients that can render a
// state panel beside the human-readable log. Unknown update fields are safe
// for older clients to ignore.
type DeployUpdate struct {
	App        string        `json:"app,omitempty"`
	Result     *DeployResult `json:"result,omitempty"`
	Stage      string        `json:"stage,omitempty"`
	BaseImages []string      `json:"baseImages,omitempty"`
	// Recipe is where the Dockerfile this build used sits in the build
	// context, reported once it is settled — the app's own, or the one the
	// assistant wrote. The file itself is in the source history.
	Recipe  string            `json:"recipe,omitempty"`
	Changes []ai.SourceChange `json:"changes,omitempty"`
}

// lineStreamer writes progress lines to the client as they happen, and
// keeps each app's entry on the deploy board current.
type lineStreamer struct {
	w       http.ResponseWriter
	f       http.Flusher
	release *releaseRecorder
	// tickets are the board entries of the apps this stream deploys; current
	// is the app the pipeline is on, which updates without an app name and
	// plain lines belong to.
	tickets map[string]*deployTicket
	current string
}

// watch puts an app of this stream on the deploy board.
func (l *lineStreamer) watch(app string, ticket *deployTicket) {
	if l.tickets == nil {
		l.tickets = map[string]*deployTicket{}
	}
	l.tickets[app] = ticket
	l.current = app
}

func (l *lineStreamer) ticket(app string) *deployTicket {
	if app == "" {
		app = l.current
	}
	return l.tickets[app]
}

func newLineStreamer(w http.ResponseWriter, release *releaseRecorder) *lineStreamer {
	f, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	return &lineStreamer{w: w, f: f, release: release}
}

func (l *lineStreamer) send(v map[string]any) {
	b, _ := json.Marshal(v)
	l.w.Write(append(b, '\n'))
	if l.f != nil {
		l.f.Flush()
	}
}

func (l *lineStreamer) Line(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	if l.release != nil {
		l.release.line(line)
	}
	l.ticket("").line(line)
	l.send(map[string]any{"line": line})
}

// Note goes to the client only: text a CLI user may want to copy right
// away, like the build recipe, whose files the source history keeps — so
// neither the release record nor the board repeats them.
func (l *lineStreamer) Note(format string, a ...any) {
	l.send(map[string]any{"line": fmt.Sprintf(format, a...)})
}

// Write lets the streamer act as the build progress sink.
func (l *lineStreamer) Write(p []byte) (int, error) {
	if l.release != nil {
		l.release.progress(string(p))
	}
	l.send(map[string]any{"progress": string(p)})
	return len(p), nil
}

func (l *lineStreamer) Result(res DeployResult) {
	if l.release != nil {
		l.release.result(res)
	}
	l.send(map[string]any{"result": res})
}

func (l *lineStreamer) Update(update DeployUpdate) {
	if l.release != nil {
		l.release.update(update)
	}
	if update.Stage != "" {
		l.ticket(update.App).stage(update.Stage)
	}
	l.send(map[string]any{"update": update})
}

func (l *lineStreamer) Inspection(build PlanBuild, analysis *ai.Analysis) {
	if l.release != nil {
		l.release.inspection(build, analysis)
	}
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

	// A reviewed plan carries answers somebody already gave. Asking the model
	// again risks a different answer to a screen the user said yes to.
	var planned *storedPlan
	if id := r.URL.Query().Get("plan"); id != "" {
		p, ok := s.plans.get(id)
		if !ok || p.project != nil {
			writeErr(w, http.StatusGone,
				fmt.Errorf("this deploy's plan has expired — upload the app again"))
			return
		}
		planned = p.snapshot()
	}

	dir, err := os.MkdirTemp("", "donkey-ctx-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(dir)
	if _, err := extractContext(r.Body, dir); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// The app is not always at the top of the upload (a monorepo, a folder
	// dragged from one level up) — find the folder that holds it.
	uploadRoot := dir
	appPath := r.URL.Query().Get("path")
	if planned != nil {
		appPath = planned.root
	}
	dir, took, err := contextRoot(dir, appPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	app, err := config.LoadApp(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	switch {
	case planned != nil:
		// The reviewed plan is the config: it already carries what the
		// upload's donkey.yaml said, plus every decision the person deploying
		// confirmed on top of it.
		app = planned.appCopy()
		if err := app.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	case app == nil:
		// Browser uploads have no `donkey init` to lean on — name and port
		// from the deploy form stand in for a donkey.yaml. One in the upload
		// always wins (it is the app's own config, not the form's).
		app, err = appFromParams(r.URL.Query().Get("name"), r.URL.Query().Get("port"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}

	space := r.URL.Query().Get("space")
	if planned != nil {
		space = ""
	}
	space = cmp.Or(space, app.Space, config.DefaultSpace)
	if !s.requireSpace(w, r, space, false) {
		return
	}

	started := time.Now()
	var reviewed *Plan
	if planned != nil {
		reviewed = s.renderPlan(context.WithoutCancel(r.Context()), r.URL.Query().Get("plan"), planned)
	}
	release := newReleaseRecorder(started, reviewed)
	out := newLineStreamer(w, release)
	if took != "" {
		out.Line("· Building %s — that's the folder with the donkey.yaml.", took)
	}
	// The deploy itself must survive a dropped client connection: route
	// changes can drain the very stream this response rides on. The CLI
	// reconnects via status polling.
	// A reviewed plan settles the space too — it is one of the things the
	// review screen shows, so a query parameter left over from before the
	// review must not quietly move the app somewhere else.
	opts := deployOpts{
		Root:      uploadRoot,
		Analysis:  planned.reading(),
		Space:     space,
		Platform:  r.URL.Query().Get("platform"),
		Tag:       r.URL.Query().Get("tag"),
		DryRun:    r.URL.Query().Get("dryRun") == "true",
		NoWait:    r.URL.Query().Get("noWait") == "true",
		BuildOnly: r.URL.Query().Get("buildOnly") == "true",
		NoBuild:   r.URL.Query().Get("noBuild") == "true",
		SHA:       r.URL.Query().Get("sha"),
	}
	deployCtx := context.WithoutCancel(r.Context())
	// On the board from here on, queued while another deploy holds the app.
	// A dry run changes nothing and stays off it. The console uploads
	// through a reviewed plan; everything else is the CLI.
	if !opts.DryRun {
		via := "cli"
		if planned != nil {
			via = "console"
		}
		ticket := s.board.start(space, app.Name, deployer(r), via, started)
		defer ticket.finish(false, "the deploy stopped before it finished")
		out.watch(app.Name, ticket)
	}
	// Keep the release and source history in the same order as deployments,
	// and finish saving them before deletion or settings changes can run.
	spaceMu := s.spaceLock(space)
	spaceMu.RLock()
	defer spaceMu.RUnlock()
	unlock := s.lockApp(space, app.Name)
	defer unlock()
	res := s.deployApp(deployCtx, out, dir, app, opts)
	// The stream is the caller's record; this line is the server's. Without
	// it nothing on the cluster remembers that a deploy ever happened.
	outcome := "ok"
	if !res.OK {
		outcome = "failed: " + res.Error
	}
	log.Printf("deploy app=%s space=%s image=%s sha=%s duration=%s %s",
		res.App, res.Space, res.ImageRef, r.URL.Query().Get("sha"),
		time.Since(started).Round(time.Second), outcome)
	out.ticket(app.Name).finish(res.OK, res.Error)
	out.Result(res)
	if !opts.DryRun && !opts.BuildOnly {
		s.persistRelease(deployCtx, res.Space, res.App, release.finish(s, deployCtx, app))
	}
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

type deployOpts struct {
	Root      string // the uploaded repository, bounding build.context
	Hosts     map[string]string
	Reviewed  bool
	NoVerify  bool // startup needs the live services or sibling apps
	Space     string
	Platform  string
	Tag       string
	DryRun    bool
	NoWait    bool
	BuildOnly bool
	NoBuild   bool
	SHA       string
	// Analysis is the assistant's reading of this app, when a reviewed plan
	// already has one. Nil means this deploy asks for itself.
	Analysis *ai.Analysis
	// Pristine is a copy of Root from before reviewed source fixes were
	// applied to it, when there were any: the source history records the
	// person's upload as they sent it and the fixes as the platform's.
	Pristine string
}

// deployApp requires the caller to hold the app's lock through its health
// checks. A project holds every sibling's lock until all rollouts finish.
func (s *Server) deployApp(ctx context.Context, out *lineStreamer, dir string, app *config.AppConfig, opts deployOpts) DeployResult {
	// The space decides namespace and address, so it is settled first.
	// Validation failures return plainly — diagnosing a self-explanatory
	// message only delays the answer.
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
	if err := s.authorizeSpace(ctx, space, false); err != nil {
		return fail(err)
	}
	buildDir, err := buildContext(opts.Root, dir, app.Build.Context)
	if err != nil {
		return fail(err)
	}

	if err := s.rt.ValidateSpace(space); err != nil {
		return fail(err)
	}
	if err := s.rt.CheckSpaceFree(ctx, space); err != nil {
		return fail(err)
	}

	// Network choices made in the console hold as long as donkey.yaml
	// doesn't say otherwise — an explicit setting there always wins.
	if !opts.Reviewed {
		for _, line := range s.adoptConsoleChoices(ctx, space, app) {
			out.Line("· %s", line)
		}
	}
	if err := runtime.CheckAppDomain(ctx, s.rt, app, space, s.cfg.Domain); err != nil {
		return fail(err)
	}

	// Services take plain names too ("postgres" is reachable like an app);
	// an app cannot move in over one.
	if data, err := s.rt.ServiceBinding(ctx, space, addons.SharedOwner, app.Name); err == nil && len(data) > 0 {
		return fail(fmt.Errorf("a service named %q already runs in this space — the app needs another name", app.Name))
	}
	if err := s.bindEnvironment(ctx, app); err != nil {
		return fail(err)
	}

	imageRef := opts.Tag
	if imageRef == "" {
		imageRef = build.ImageTag(s.cfg.Registry.URL, space, app.Name, opts.SHA, time.Now(), s.cfg.Registry.Immutable)
	}
	// A caller-supplied image bypasses the build, so it must come from
	// donkey's own registry — otherwise any image on the internet could run
	// under an app's name and inherit its env and addon secrets.
	if opts.Tag != "" && !build.SameRegistry(s.cfg.Registry.URL, opts.Tag) {
		return fail(fmt.Errorf("--tag must point at %s (donkey only deploys images it built)", s.cfg.Registry.Host()))
	}

	if opts.NoBuild {
		if opts.Tag == "" {
			return fail(fmt.Errorf("a no-build deploy needs the image tag to deploy"))
		}
		// No build means no analysis — keep what an earlier deploy decided
		// (storage, health route) instead of silently rendering without it.
		// The tag's own build record wins: a rollback adopts the decisions
		// that image was verified with, not whatever a newer analysis found.
		for _, line := range s.adoptStoredDecisions(ctx, space, opts.Tag, app) {
			out.Line("· %s", line)
		}
		if opts.DryRun {
			return s.renderDryRun(app, opts.Tag, opts.Hosts)
		}
		out.Update(DeployUpdate{Stage: "releasing"})
		return s.applyAndWait(ctx, out, app, dir, opts.Tag, s.storedStack(ctx, space, app.Name, opts.Tag), opts.NoWait, opts.Hosts)
	}

	out.Update(DeployUpdate{Stage: "detecting"})
	out.Line("Looking at your app…")
	det, detErr := detect.Detect(dir, app)
	if detErr == nil {
		out.Inspection(describePlanBuild(dir, det, nil), nil)
	}

	// Analysis and the build must agree on the runtime before generating or
	// verifying an image, especially when storage masks image directories.
	if detErr != nil {
		return fail(detErr)
	}
	analysis := opts.Analysis
	if analysis == nil {
		readCtx, cancel := context.WithCancel(ctx)
		var signals *ai.Signals
		analysis, signals, err = s.analyzeApp(readCtx, dir, det, nil)
		cancel()
		if err != nil {
			return fail(err)
		}
		out.Inspection(describePlanBuild(dir, det, signals), analysis)
	}
	for _, finding := range mergeAnalysis(app, analysis) {
		out.Line("· %s", finding.Text())
	}
	if app.CountsVisits() {
		out.Line("· Counting visits: donkey-gateway adds its cookieless analytics script to the app's pages (`analytics: false` in donkey.yaml turns it off)")
	}
	if err := app.Validate(); err != nil {
		return fail(err)
	}
	if opts.DryRun {
		return s.renderDryRun(app, imageRef, opts.Hosts)
	}
	out.Line("✓ %s will be built with %s", app.Name, det.Summary)

	// Every app gets an isolated copy, including shared workspace packages.
	// An automatic repair must not alter a sibling's reviewed build inputs.
	workspace, err := os.MkdirTemp("", "donkey-build-workspace-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(workspace)
	appRel, err := filepath.Rel(buildDir, dir)
	if err != nil {
		return fail(err)
	}
	if err := os.CopyFS(workspace, os.DirFS(buildDir)); err != nil {
		return fail(fmt.Errorf("preparing the build workspace: %w", err))
	}
	// The source history keeps both versions: the upload as it arrived and,
	// when the build changed its copy, what was actually built. The copy is
	// gone once this returns, so the difference is taken on the way out.
	if release := out.release; release != nil && !opts.BuildOnly {
		uploaded := buildDir
		if opts.Pristine != "" {
			if rel, err := filepath.Rel(opts.Root, buildDir); err == nil {
				uploaded = filepath.Join(opts.Pristine, rel)
			}
		}
		defer func() { release.sourceCaptured(captureSource(uploaded, workspace, appRel, opts.SHA)) }()
	}
	buildDir, dir = workspace, filepath.Join(workspace, appRel)

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

	out.Update(DeployUpdate{Stage: "building"})
	out.Line("Building the image…")
	if mirrors := registryMirrorSummary(s.cfg.RegistryMirrors); mirrors != "" {
		// BuildKit progress retains the source image name even when its requests
		// use a mirror. Print the effective mapping first so a later
		// "docker.io/..." progress line is not mistaken for a bypass.
		out.Line("· BuildKit mirrors: %s", mirrors)
	}
	builder := &build.BuildKitBuilder{
		Registry:        s.cfg.Registry,
		RegistryMirrors: s.cfg.RegistryMirrors,
		BuildkitAddr:    s.cfg.BuildkitAddr,
		PackagesURL:     s.cfg.PackagesURL,
		Settings:        s.cfg.Build,
	}
	// The standard OCI vocabulary, so any registry tool can answer where an
	// image came from without knowing donkey.
	imageLabels := map[string]string{
		"org.opencontainers.image.created": time.Now().UTC().Format(time.RFC3339),
	}
	if opts.SHA != "" {
		imageLabels["org.opencontainers.image.revision"] = opts.SHA
	}
	req := build.Request{
		ContextDir:     buildDir,
		ImageRef:       imageRef,
		Platform:       platform,
		Push:           true,
		BuildEnv:       app.Build.Env,
		Strategy:       det.Strategy,
		DockerfilePath: det.DockerfilePath,
		ImageLabels:    imageLabels,
		Progress:       build.ProgressSink{LogPath: logFile.Name(), Writer: out},
	}
	if det.DockerfilePath != "" {
		req.DockerfilePath, err = filepath.Rel(buildDir, filepath.Join(dir, det.DockerfilePath))
		if err != nil {
			return fail(err)
		}
		req.DockerfilePath = filepath.ToSlash(req.DockerfilePath)
	}
	req.Verify = s.verifySpec(app)
	if opts.NoVerify {
		req.Verify = nil
	}
	// Runtime-only values cannot be recreated faithfully inside BuildKit.
	// The rollout remains mandatory even when this preliminary check is skipped.
	if stored, e := s.rt.GetEnv(ctx, space, app.Name); e != nil || len(stored) > 0 {
		req.Verify = nil
	}
	var release func(*build.Result, string) error
	if !opts.BuildOnly {
		release = func(result *build.Result, stack string) error {
			return s.rt.Deploy(ctx, runtime.DeploySpec{App: app, Space: space, ImageRef: result.ImageRef,
				Decisions: decisionsJSON(app), Stack: stack, NoWait: opts.NoWait, Hosts: opts.Hosts}, out)
		}
	}
	outcome, err := s.buildGenerated(ctx, out, builder, dir, app, req, release)
	res := DeployResult{App: app.Name, Space: space}
	if outcome != nil && outcome.Result != nil {
		res.ImageRef = outcome.Result.ImageRef
	}
	if err != nil {
		res.Error = err.Error()
		var rollout *runtime.RolloutError
		if errors.As(err, &rollout) {
			s.diagnoseWith(ctx, &res, buildDir, space, app.Name, "rollout", rollout.Evidence.Logs, rollout.Evidence.Events)
		} else if logs, e := os.ReadFile(logFile.Name()); e == nil {
			s.diagnoseWith(ctx, &res, buildDir, space, app.Name, "build", tailBytes(logs, 15*1024), nil)
		}
		return res
	}
	res.OK = true
	if !opts.BuildOnly {
		res.URL = s.cfg.AppURL(space, app)
	}
	if outcome.Attempts > 1 {
		out.Line("✓ Completed after %d attempts.", outcome.Attempts)
	}
	if det.Strategy != config.BuildDockerfile || outcome.Attempts > 1 {
		// CLI clients consume text progress; the console and the release
		// history find these files in the build's commit.
		out.Note("· Build recipe — save as %s:\n%s", detect.AIDockerfilePath, outcome.Dockerfile)
		out.Note("· Build exclusions — save as %s.dockerignore:\n%s", detect.AIDockerfilePath, outcome.Dockerignore)
		for _, change := range outcome.Changes {
			out.Note("· Source repair in the build copy — %s:\n%s", change.Path, change.Diff)
		}
	}
	return res

}

func registryMirrorSummary(mirrors map[string]string) string {
	entries := make([]string, 0, len(mirrors))
	for _, source := range slices.Sorted(maps.Keys(mirrors)) {
		normalizedSource, sourceErr := config.NormalizeRegistrySource(source)
		target, targetErr := config.ParseRegistryMirror(mirrors[source])
		if sourceErr == nil && targetErr == nil {
			entries = append(entries, normalizedSource+" → "+target.Address)
		}
	}
	slices.Sort(entries)
	return strings.Join(entries, ", ")
}

// renderDryRun renders the app's objects without touching the runtime.
func (s *Server) renderDryRun(app *config.AppConfig, imageRef string, hosts map[string]string) DeployResult {
	res := DeployResult{App: app.Name, Space: app.Space, ImageRef: imageRef}
	rendered, err := s.rt.Render(runtime.DeploySpec{
		App: app, Space: app.Space, ImageRef: imageRef, Decisions: decisionsJSON(app), Hosts: hosts,
	})
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	res.Manifests = rendered
	return res
}

// deployDecisions persists what analysis settled, so a no-build deploy keeps
// the same shape instead of quietly unmounting the last deploy's disk.
type deployDecisions struct {
	Storage         *config.StorageConfig `json:"storage,omitempty"`
	HealthcheckPath string                `json:"healthcheckPath,omitempty"`
}

// consoleChoices live in their own annotation, written by the console
// endpoints only: a deploy re-renders decisions from donkey.yaml, while a
// console choice survives because server-side apply owns only what it writes.
type consoleChoices struct {
	// Mode is web or api. Exposure is public, protected or internal. Empty
	// values mean the donkeyfile (or the default) decides.
	Mode     string `json:"mode,omitempty"`
	Exposure string `json:"exposure,omitempty"`
	Host     string `json:"host,omitempty"`
	CORS     string `json:"cors,omitempty"`
	// Analytics is config.AnalyticsOff when web analytics was turned off.
	Analytics string `json:"analytics,omitempty"`
}

func (s *Server) readConsoleChoices(ctx context.Context, space, app string) consoleChoices {
	var c consoleChoices
	raw, err := s.rt.AppMeta(ctx, space, app, runtime.MetaConsole)
	if err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c
}

// adoptConsoleChoices fills what donkey.yaml leaves open from the console's
// settings. The file wins where it speaks: mode, exposure, host, a CORS
// policy or web analytics. Adoption that would invalidate the config is
// dropped, and only ever the adopted values.
func (s *Server) adoptConsoleChoices(ctx context.Context, space string, app *config.AppConfig) []string {
	c := s.readConsoleChoices(ctx, space, app.Name)
	var lines []string
	adoptedMode, adoptedExposure, adoptedHost, adoptedCORS := false, false, false, false
	if app.Mode == "" && c.Mode == config.AppModeAPI {
		app.Mode = c.Mode
		adoptedMode = true
		lines = append(lines, fmt.Sprintf("Keeping the console's setting: %s mode", c.Mode))
	}
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
	if app.CORS == "" && c.CORS != "" {
		app.CORS = c.CORS
		adoptedCORS = true
		lines = append(lines, "Keeping the console's setting: allow browser requests from any origin")
	}
	if app.Analytics == nil && c.Analytics == config.AnalyticsOff {
		off := false
		app.Analytics = &off
		lines = append(lines, "Keeping the console's setting: no web analytics")
	}
	if app.Host == "" && c.Host != "" && !app.Internal {
		app.Host = c.Host
		adoptedHost = true
		lines = append(lines, fmt.Sprintf("Keeping the console's address: %s", c.Host))
	}
	if app.Validate() != nil {
		if adoptedMode {
			app.Mode = ""
		}
		if adoptedExposure {
			app.Public, app.Internal = false, false
		}
		if adoptedHost {
			app.Host = ""
		}
		if adoptedCORS {
			app.CORS = ""
		}
		return nil
	}
	return lines
}

// storedStack is what a no-build deploy's image is written in: the image's
// own build record names the recipe's base images, and the running app
// answers for images that predate the build store.
func (s *Server) storedStack(ctx context.Context, space, app, imageRef string) string {
	if record, err := s.telemetry.BuildByImage(ctx, space, app, imageRef); err == nil {
		var release AppRelease
		if json.Unmarshal(record, &release) == nil && len(release.Update.BaseImages) > 0 {
			return detect.Stack(release.Update.BaseImages)
		}
	}
	if status, err := s.rt.Status(ctx, space, app); err == nil && status != nil {
		return status.Stack
	}
	return ""
}

// adoptStoredDecisions fills what donkey.yaml leaves open, and says what it
// kept. The deployed tag's own build record is asked first — its decisions
// are the ones that image was verified with — and the running Deployment's
// record answers for images that predate the build store or when telemetry
// is unreachable. An adopted value that no longer fits is dropped again, and
// only ever the adopted values.
func (s *Server) adoptStoredDecisions(ctx context.Context, space, imageRef string, app *config.AppConfig) []string {
	var raw string
	if imageRef != "" {
		if record, err := s.telemetry.BuildByImage(ctx, space, app.Name, imageRef); err == nil {
			var release AppRelease
			if json.Unmarshal(record, &release) == nil && len(release.Decisions) > 0 {
				raw = string(release.Decisions)
			}
		}
	}
	if raw == "" {
		var err error
		if raw, err = s.rt.AppMeta(ctx, space, app.Name, runtime.MetaDecisions); err != nil || raw == "" {
			return nil
		}
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
func (s *Server) applyAndWait(ctx context.Context, out *lineStreamer, app *config.AppConfig, root, imageRef, stack string, noWait bool, hosts map[string]string) DeployResult {
	space := app.Space
	err := s.rt.Deploy(ctx, runtime.DeploySpec{
		App:       app,
		Space:     space,
		ImageRef:  imageRef,
		Decisions: decisionsJSON(app),
		Stack:     stack,
		NoWait:    noWait,
		Hosts:     hosts,
	}, out)
	if err != nil {
		res := DeployResult{App: app.Name, Space: space, ImageRef: imageRef, Error: err.Error()}
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

// diagnoseWith attaches AI failure analysis to the result. Proposals are
// never applied: the server computes the diff, the user decides. root is the
// server's own temp copy of the app, never the user's working tree.
func (s *Server) diagnoseWith(ctx context.Context, res *DeployResult, root, space, app, stage, logs string, events []string) {
	dctx, cancel := context.WithCancel(ctx)
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
		// Not the safety check — os.Root already blocks escapes — but the
		// honesty one: "a/../../x" would otherwise fail as "not found" and be
		// shown as a tidy new-file diff for a change that can never apply.
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

// knownSecrets is what donkey handed the app — env, addon bindings, its own
// credentials — so the assistant sees them scrubbed. Best effort: a secret
// donkey cannot read is one it cannot redact, and that must not cost the
// user their diagnosis.
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
	if _, pw, err := s.cfg.Registry.Credentials(); err == nil {
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

// readDockerfile returns the app's own Dockerfile for the build record,
// bounded: the record stores the recipe, not an arbitrarily large file.
func readDockerfile(dir, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) > 256<<10 {
		return ""
	}
	return string(raw)
}
