package server

import (
	"context"
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
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
	"github.com/adrianliechti/donkey/internal/k8s"
	"github.com/adrianliechti/donkey/internal/k8s/manifests"
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

// preDeployTimeout bounds a pre-deploy step. Migrations on a large table are
// slow but not endless, and a step that hangs would otherwise hold the deploy
// open indefinitely.
const preDeployTimeout = 15 * time.Minute

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

	ns, err := s.cfg.SpaceNamespace(space)
	if err != nil {
		return fail(err)
	}
	if err := s.k8s.CheckNamespaceFree(ctx, ns); err != nil {
		return fail(err)
	}

	// One deploy per app at a time: concurrent deploys would interleave their
	// applies and race their rollout waits, leaving the live image undefined.
	unlock := s.lockApp(ns, app.Name)
	defer unlock()

	// Network choices made in the console hold as long as donkey.yaml
	// doesn't say otherwise — an explicit setting there always wins.
	for _, line := range s.adoptConsoleChoices(ctx, ns, app) {
		out.Line("· %s", line)
	}

	if app.Name == addons.SharedOwner {
		return fail(fmt.Errorf("the app name %q is reserved for a space's shared services — pick another name", addons.SharedOwner))
	}
	// Services take plain names too ("postgres" is reachable like an app);
	// an app cannot move in over one.
	if data, err := s.k8s.GetSecretData(ctx, ns, addons.SecretName(addons.SharedOwner, app.Name)); err == nil && len(data) > 0 {
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
		for _, line := range s.adoptStoredDecisions(ctx, ns, app) {
			out.Line("· %s", line)
		}
		if opts.DryRun {
			return s.renderDryRun(app, opts.Tag)
		}
		return s.applyAndWait(ctx, out, app, dir, ns, opts.Tag, opts.NoWait)
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
			s.diagnoseWith(ctx, &res, dir, ns, app.Name, "build", tailBytes(logs, 15*1024), nil)
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
	return s.applyAndWait(ctx, out, app, dir, ns, result.ImageRef, opts.NoWait)
}

// renderDryRun renders the app's objects without touching the cluster.
func (s *Server) renderDryRun(app *config.AppConfig, imageRef string) DeployResult {
	res := DeployResult{App: app.Name, Space: app.Space, ImageRef: imageRef}
	in, err := s.manifestInput(app, imageRef, false)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	objs, err := manifests.AppObjects(in)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	rendered, err := manifests.Render(objs)
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
	Exposure string               `json:"exposure,omitempty"`
	Domain   string               `json:"domain,omitempty"`
	Access   *config.AccessConfig `json:"access,omitempty"`
}

func (s *Server) readConsoleChoices(ctx context.Context, ns, app string) consoleChoices {
	var c consoleChoices
	raw, err := s.k8s.DeploymentAnnotation(ctx, ns, app, manifests.AnnotationConsole)
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
func (s *Server) adoptConsoleChoices(ctx context.Context, ns string, app *config.AppConfig) []string {
	c := s.readConsoleChoices(ctx, ns, app.Name)
	var lines []string
	adoptedExposure, adoptedDomain, adoptedAccess := false, false, false
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
	if app.Domain == "" && c.Domain != "" && !app.Internal {
		app.Domain = c.Domain
		adoptedDomain = true
		lines = append(lines, fmt.Sprintf("Keeping the console's domain: %s", c.Domain))
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
		if adoptedDomain {
			app.Domain = ""
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
func (s *Server) adoptStoredDecisions(ctx context.Context, ns string, app *config.AppConfig) []string {
	raw, err := s.k8s.DeploymentAnnotation(ctx, ns, app.Name, manifests.AnnotationDecisions)
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

// applyAndWait applies the app's manifests with the given image and waits
// for the rollout (unless noWait). The space is settled and validated by the
// caller; ns is its namespace.
func (s *Server) applyAndWait(ctx context.Context, out *lineStreamer, app *config.AppConfig, root, ns, imageRef string, noWait bool) DeployResult {
	space := app.Space
	failed := func(err error) DeployResult {
		return DeployResult{App: app.Name, Space: space, ImageRef: imageRef, Error: err.Error()}
	}

	out.Line("Setting up on the cluster…")
	// A file-browser helper holds the app's disk, and an RWO volume attaches
	// to one node at a time — leaving it running would strand the new pod on
	// another node with a multi-attach error until the rollout times out.
	if app.HasStorage() {
		if err := s.k8s.RemoveFilesHelper(ctx, ns, app.Name); err != nil {
			return failed(err)
		}
	}
	if !app.HasStorage() && s.k8s.HasDataClaim(ctx, ns, app.Name) {
		out.Line("· A disk from an earlier setup still exists (with its data). This deploy doesn't mount it — add its paths back under `storage:` in donkey.yaml to use it, or `donkey destroy` removes it.")
	}
	if app.HasStorage() {
		out.Line("· One disk (%s) for %s", app.Storage.Size, strings.Join(app.Storage.Paths, ", "))
		// An app that just gained a disk has to leave rolling updates behind.
		if err := s.k8s.SwitchToRecreate(ctx, ns, app.Name); err != nil {
			return failed(fmt.Errorf("couldn't switch %s to single-copy rollouts: %w", app.Name, err))
		}
	}
	// donkey.yaml is declarative about addons too: services belong to the
	// space, and an entry here means "this app uses that service" — created
	// on the spot when the space doesn't have it yet, before the app (and
	// its pre-deploy step) needs the binding. An existing service is left
	// alone: a second app naming it simply joins it.
	for _, a := range app.Addons {
		shared, err := s.k8s.GetSecretData(ctx, ns, addons.SecretName(addons.SharedOwner, a.Name))
		if err != nil {
			return failed(err)
		}
		if len(shared) == 0 {
			if a.External {
				return failed(fmt.Errorf("addon %s runs elsewhere and its address isn't stored in donkey.yaml — connect it once with `donkey addon attach %s --url …`", a.Name, a.Type))
			}
			out.Line("Setting up %s (%s) for the space — the first time can take a minute…", a.Name, a.Type)
			if shared, _, _, err = s.createSpaceService(ctx, space, ns, AddonRequest{Type: a.Type, Name: a.Name, Version: a.Version, Storage: a.Storage}); err != nil {
				return failed(err)
			}
			out.Line("✓ %s is ready.", a.Name)
		}
		// The attachment copy is re-applied every deploy, so it stays current
		// with the space's service.
		if err := s.k8s.Apply(ctx, []runtime.Object{addons.AttachmentSecret(space, app.Name, ns, a.Name, shared)}); err != nil {
			return failed(err)
		}
	}
	in, err := s.manifestInput(app, imageRef, true)
	if err != nil {
		return failed(err)
	}
	// Addons added in the console aren't in this donkey.yaml; the cluster
	// knows them by their binding secrets, and they keep working across
	// deploys instead of silently losing their env.
	if bindings, err := s.k8s.ListAddonBindings(ctx, ns, app.Name); err == nil {
		known := map[string]bool{}
		for _, n := range in.AddonSecretNames {
			known[n] = true
		}
		for _, b := range bindings {
			if n := addons.SecretName(app.Name, b.Name); !known[n] {
				in.AddonSecretNames = append(in.AddonSecretNames, n)
				out.Line("· Keeping %s, attached in the console", b.Name)
			}
		}
	}
	objs, err := manifests.AppObjects(in)
	if err != nil {
		return failed(err)
	}
	// The Deployment is applied last. Everything it needs — secrets, the
	// disk, addon bindings — goes first, so a pre-deploy step runs with the
	// new configuration while the old version is still the one serving.
	var deployment []runtime.Object
	rest := objs[:0]
	for _, o := range objs {
		if o.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
			deployment = append(deployment, o)
			continue
		}
		rest = append(rest, o)
	}
	if err := s.k8s.Apply(ctx, rest); err != nil {
		return failed(fmt.Errorf("the cluster refused the changes: %v", err))
	}
	if len(app.PreDeploy) > 0 {
		out.Line("Running the pre-deploy step…")
		if err := s.k8s.RunJob(ctx, manifests.PreDeployJob(in), preDeployTimeout); err != nil {
			// The Deployment was deliberately not touched, so whatever is
			// live stays live — that is the point of running this first.
			r := failed(fmt.Errorf("the pre-deploy step failed, so nothing was changed and the running version keeps serving: %w", err))
			var je *k8s.JobError
			if errors.As(err, &je) && je.Logs != "" {
				out.Line("Output of the pre-deploy step:\n%s", je.Logs)
				s.diagnoseWith(ctx, &r, root, ns, app.Name, "predeploy", je.Logs, nil)
			}
			return r
		}
		out.Line("✓ Pre-deploy step finished.")
	}
	if err := s.k8s.Apply(ctx, deployment); err != nil {
		return failed(fmt.Errorf("the cluster refused the changes: %v", err))
	}
	// A `routes:` entry that was dropped from donkey.yaml has to stop
	// serving; server-side apply only ever adds.
	if err := s.retractRemovedRoutes(ctx, ns, in); err != nil {
		return failed(err)
	}
	for _, r := range in.RouteTargets {
		out.Line("· Also answering https://%s%s", r.Host, r.Path)
	}
	// Login lived in Envoy SecurityPolicies before the donkey-gateway; the
	// leftovers must go regardless of exposure, or Envoy keeps enforcing a
	// config donkey no longer writes.
	if err := s.retractLegacyLogin(ctx, ns, app.Name); err != nil {
		return failed(err)
	}
	// Server-side apply never deletes what the new set leaves out, so every
	// way an app can stop being exposed has to retract the objects itself.
	if in.Domain == "" {
		// Internal: no hostname, so the route and its certificate go —
		// including when the app used to have them.
		if err := s.k8s.DeleteObject(ctx, "HTTPRoute", ns, app.Name); err != nil {
			return failed(err)
		}
		if err := s.detachCertificate(ctx, space, app.Name); err != nil {
			return failed(err)
		}
		// The donkey-gateway grant stays only while path routes still use it.
		if len(in.RouteTargets) == 0 {
			if err := s.k8s.DeleteObject(ctx, "ReferenceGrant", manifests.SystemNamespace, manifests.GatewayGrantName(space, app.Name)); err != nil {
				return failed(err)
			}
		}
		out.Line("· Internal only — the apps of this space reach it at http://%s", app.Name)
	} else {
		// The app's certificate joins the shared gateway's https listener;
		// Envoy serves the right one per hostname.
		if err := s.attachCertificate(ctx, space, app.Name); err != nil {
			return failed(err)
		}
		if note := s.waitForCertificate(ctx, space, app.Name, in.Domain); note != "" {
			out.Line("· %s", note)
		}
		// All traffic rides through donkey-gateway, so the route serves only
		// once the Gateway resolved that cross-namespace reference. For a
		// protected app this is also the privacy check — the route points
		// into the login, or it doesn't serve at all.
		if err := s.waitForRouteReady(ctx, ns, app.Name); err != nil {
			return failed(err)
		}
		if !app.Public && s.cfg.OIDCIssuer != "" {
			out.Line("· Login protection is in force.")
		}
	}
	if noWait {
		return DeployResult{OK: true, App: app.Name, Space: space, ImageRef: imageRef, URL: s.cfg.AppURL(space, app)}
	}

	out.Line("Waiting for the app to become healthy…")
	if err := s.k8s.WaitForRollout(ctx, ns, app.Name); err != nil {
		res := DeployResult{App: app.Name, Space: space, ImageRef: imageRef, Error: err.Error()}
		var re *k8s.RolloutError
		if errors.As(err, &re) {
			for _, p := range re.Evidence.PodSummaries {
				out.Line("  %s", p)
			}
			if re.Evidence.PodLogs != "" {
				out.Line("Last lines from the app:\n%s", re.Evidence.PodLogs)
			}
			s.diagnoseWith(ctx, &res, root, ns, app.Name, "rollout", re.Evidence.PodLogs, re.Evidence.Events)
		}
		return res
	}
	return DeployResult{OK: true, App: app.Name, Space: space, ImageRef: imageRef, URL: s.cfg.AppURL(space, app)}
}

func (s *Server) manifestInput(app *config.AppConfig, imageRef string, includeCreds bool) (manifests.Input, error) {
	space := app.Space
	in := manifests.Input{
		App:          app,
		Space:        space,
		Namespace:    s.cfg.Namespace(space),
		ImageRef:     imageRef,
		Decisions:    decisionsJSON(app),
		RegistryHost: s.cfg.Registry.Host(),
		Domain:       s.cfg.AppHost(space, app),
		TLS: manifests.TLSConfig{
			Issuer: s.cfg.TLSIssuer,
			Kind:   s.cfg.TLSIssuerKind,
		},
		GatewayName:      s.cfg.GatewayName,
		GatewayNamespace: s.cfg.GatewayNamespace,
	}
	// The donkey gateway is every route's real backend; whether it can also
	// offer a login depends on the installation having an IdP.
	in.Gateway = &manifests.GatewayRef{
		Service:   manifests.GatewayAppName,
		Namespace: manifests.SystemNamespace,
		Auth:      s.cfg.OIDCIssuer != "",
	}
	// Credentials only when the objects are going to the cluster. A dry run
	// prints its YAML to the user's terminal (and into CI logs), so the
	// registry password must not be in it.
	if includeCreds {
		password, err := s.cfg.Registry.Password()
		if err != nil {
			return in, err
		}
		in.RegistryUsername = s.cfg.Registry.Username
		in.RegistryPassword = password
	}
	for _, a := range app.Addons {
		in.AddonSecretNames = append(in.AddonSecretNames, addons.SecretName(app.Name, a.Name))
	}
	// Each route joins another app's address, so its hostname is that app's.
	// Only the default hostname is resolvable from here — a target with its
	// own `domain:` would need looking up, which the validation below says.
	for _, r := range app.Routes {
		in.RouteTargets = append(in.RouteTargets, manifests.RouteTarget{
			App:  r.App,
			Host: r.App + "." + space + "." + s.cfg.BaseDomain,
			Path: r.Path,
		})
	}
	return in, nil
}

// retractRemovedRoutes deletes the shared-address routes (and their login
// policies) an app used to claim and no longer does. Server-side apply never
// removes what a new set leaves out, so dropping a `routes:` entry would
// otherwise keep serving it forever.
func (s *Server) retractRemovedRoutes(ctx context.Context, ns string, in manifests.Input) error {
	keep := map[string]bool{}
	for _, r := range in.RouteTargets {
		keep[manifests.PathRouteName(in.App.Name, r.App, r.Path)] = true
	}
	names, err := s.k8s.ListRouteNames(ctx, ns, in.App.Name)
	if err != nil {
		return err
	}
	for _, name := range names {
		if keep[name] {
			continue
		}
		for _, kind := range []string{"SecurityPolicy", "HTTPRoute"} {
			if err := s.k8s.DeleteObject(ctx, kind, ns, name); err != nil {
				return err
			}
		}
	}
	return nil
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
func (s *Server) diagnoseWith(ctx context.Context, res *DeployResult, root, ns, app, stage, logs string, events []string) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	proposal, aerr := s.assistant.DiagnoseFailure(dctx, &ai.FailureContext{
		Stage:        stage,
		Logs:         logs,
		Events:       events,
		Root:         root,
		KnownSecrets: s.knownSecrets(ctx, ns, app),
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
func (s *Server) knownSecrets(ctx context.Context, ns, app string) []string {
	var out []string
	add := func(m map[string]string) {
		for _, v := range m {
			out = append(out, v)
		}
	}
	if env, err := s.k8s.GetEnv(ctx, ns, app); err == nil {
		add(env)
	}
	if bindings, err := s.k8s.ListAddonBindings(ctx, ns, app); err == nil {
		for _, b := range bindings {
			if data, err := s.k8s.GetSecretData(ctx, ns, addons.SecretName(app, b.Name)); err == nil {
				add(data)
			}
		}
	}
	// Registry and OIDC credentials surface in build and startup logs.
	if pw, err := s.cfg.Registry.Password(); err == nil {
		out = append(out, pw)
	}
	return append(out, s.cfg.OIDCClientSecret, s.cfg.APIToken)
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
