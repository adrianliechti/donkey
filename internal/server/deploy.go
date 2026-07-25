package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"monobox.ch/donkey/internal/ai"
	"monobox.ch/donkey/internal/build"
	"monobox.ch/donkey/internal/config"
	"monobox.ch/donkey/internal/detect"
	"monobox.ch/donkey/internal/k8s"
	"monobox.ch/donkey/internal/k8s/manifests"
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

func (l *lineStreamer) Line(format string, a ...any) {
	b, _ := json.Marshal(map[string]string{"line": fmt.Sprintf(format, a...)})
	l.w.Write(append(b, '\n'))
	if l.f != nil {
		l.f.Flush()
	}
}

// Write lets the streamer act as the build progress sink.
func (l *lineStreamer) Write(p []byte) (int, error) {
	b, _ := json.Marshal(map[string]string{"progress": string(p)})
	l.w.Write(append(b, '\n'))
	if l.f != nil {
		l.f.Flush()
	}
	return len(p), nil
}

func (l *lineStreamer) Result(res DeployResult) {
	b, _ := json.Marshal(map[string]any{"result": res})
	l.w.Write(append(b, '\n'))
	if l.f != nil {
		l.f.Flush()
	}
}

// handleDeploy: POST /v1/deploys?platform=&dryRun=&noWait=&buildOnly=&tag=
// Body: tar.gz of the app folder (donkey.yaml included).
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	dir, err := os.MkdirTemp("", "donkey-ctx-*")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(dir)
	if err := untarContext(r.Body, dir); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	app, err := config.LoadApp(dir)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if app == nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("the uploaded folder has no donkey.yaml — run `donkey init` first"))
		return
	}

	out := newLineStreamer(w)
	// The deploy itself must survive a dropped client connection: applying a
	// SecurityPolicy updates Envoy's listener, which may drain the very
	// stream this response rides on. The CLI reconnects via status polling.
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
	out.Result(res)
}

// analysisDeadline bounds how long a deploy waits for the assistant after the
// build has finished.
const analysisDeadline = 90 * time.Second

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
	// before anything else happens.
	space := opts.Space
	if space == "" {
		space = app.Space
	}
	if space == "" {
		space = config.DefaultSpace
	}
	app.Space = space

	fail := func(err error) DeployResult {
		res := DeployResult{App: app.Name, Space: space, Error: err.Error()}
		s.diagnose(ctx, &res, err)
		return res
	}

	if err := s.k8s.CheckNamespaceFree(ctx, s.cfg.Namespace(space), space); err != nil {
		return fail(err)
	}

	imageRef := opts.Tag
	if imageRef == "" {
		imageRef = build.ImageTag(s.cfg.Registry.URL, app.Name, opts.SHA, time.Now())
	}

	if opts.NoBuild {
		if opts.Tag == "" {
			return fail(fmt.Errorf("a no-build deploy needs the image tag to deploy"))
		}
		return s.applyAndWait(ctx, out, app, opts.Tag, opts.NoWait)
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
		return nil
	}

	if opts.DryRun {
		if err := merge(); err != nil {
			return fail(err)
		}
		in, err := s.manifestInput(app, imageRef, false)
		if err != nil {
			return fail(err)
		}
		objs, err := manifests.AppObjects(in)
		if err != nil {
			return fail(err)
		}
		rendered, err := manifests.Render(objs)
		if err != nil {
			return fail(err)
		}
		return DeployResult{OK: true, App: app.Name, Space: space, ImageRef: imageRef, Manifests: rendered}
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
			s.diagnoseWith(ctx, &res, "build", tailBytes(logs, 15*1024), nil)
		}
		return res
	}
	out.Line("✓ Image ready: %s", result.ImageRef)

	if opts.BuildOnly {
		return DeployResult{OK: true, App: app.Name, Space: space, ImageRef: result.ImageRef}
	}

	if err := merge(); err != nil {
		return fail(err)
	}
	return s.applyAndWait(ctx, out, app, result.ImageRef, opts.NoWait)
}

// applyAndWait applies the app's manifests with the given image and waits
// for the rollout (unless noWait).
func (s *Server) applyAndWait(ctx context.Context, out *lineStreamer, app *config.AppConfig, imageRef string, noWait bool) DeployResult {
	space := app.Space
	if space == "" {
		space = config.DefaultSpace
	}
	ns := s.cfg.Namespace(space)
	failed := func(err error) DeployResult {
		return DeployResult{App: app.Name, Space: space, Error: err.Error()}
	}

	out.Line("Setting up on the cluster…")
	if app.HasStorage() {
		out.Line("· One disk (%s) for %s", app.Storage.Size, strings.Join(app.Storage.Paths, ", "))
		// An app that just gained a disk has to leave rolling updates behind.
		if err := s.k8s.SwitchToRecreate(ctx, ns, app.Name); err != nil {
			return failed(fmt.Errorf("couldn't switch %s to single-copy rollouts: %w", app.Name, err))
		}
	}
	in, err := s.manifestInput(app, imageRef, true)
	if err != nil {
		return failed(err)
	}
	objs, err := manifests.AppObjects(in)
	if err != nil {
		return failed(err)
	}
	if err := s.k8s.Apply(ctx, objs); err != nil {
		return failed(fmt.Errorf("the cluster refused the changes: %v", err))
	}
	// The app's certificate joins the gateway's https listener; Envoy serves
	// the right one per hostname.
	if err := s.attachCertificate(ctx, space, app.Name); err != nil {
		return failed(err)
	}
	if note := s.waitForCertificate(ctx, space, app.Name, in.Domain); note != "" {
		out.Line("· %s", note)
	}
	// Server-side apply doesn't delete what the new set omits: when the app
	// is (now) public, retract any login protection from earlier deploys.
	if app.Public || s.cfg.OIDCIssuer == "" {
		if err := s.k8s.DeleteObject(ctx, "SecurityPolicy", ns, app.Name); err != nil {
			return failed(err)
		}
		if err := s.k8s.DeleteObject(ctx, "Secret", ns, manifests.OIDCSecretName(app.Name)); err != nil {
			return failed(err)
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
			s.diagnoseWith(ctx, &res, "rollout", re.Evidence.PodLogs, re.Evidence.Events)
		}
		return res
	}
	return DeployResult{OK: true, App: app.Name, Space: space, ImageRef: imageRef, URL: s.cfg.AppURL(space, app)}
}

func (s *Server) manifestInput(app *config.AppConfig, imageRef string, includeCreds bool) (manifests.Input, error) {
	space := app.Space
	if space == "" {
		space = config.DefaultSpace
	}
	in := manifests.Input{
		App:          app,
		Space:        space,
		Namespace:    s.cfg.Namespace(space),
		ImageRef:     imageRef,
		RegistryHost: s.cfg.Registry.Host(),
		Domain:       s.cfg.AppHost(space, app),
		TLS: manifests.TLSConfig{
			Issuer: s.cfg.TLSIssuer,
			Kind:   s.cfg.TLSIssuerKind,
		},
		GatewayName:      s.cfg.GatewayName,
		GatewayNamespace: s.cfg.GatewayNamespace,
		OIDC: manifests.OIDCConfig{
			Issuer:       s.cfg.OIDCIssuer,
			ClientID:     s.cfg.OIDCClientID,
			ClientSecret: s.cfg.OIDCClientSecret,
		},
	}
	if includeCreds {
		password, err := s.cfg.Registry.Password()
		if err != nil {
			return in, err
		}
		in.RegistryUsername = s.cfg.Registry.Username
		in.RegistryPassword = password
	}
	for _, a := range app.Addons {
		in.AddonSecretNames = append(in.AddonSecretNames, app.Name+"-addon-"+a.Name)
	}
	return in, nil
}

// diagnose / diagnoseWith run the AI failure analysis server-side and attach
// it to the result. Proposals are never applied — the CLI shows them.
func (s *Server) diagnose(ctx context.Context, res *DeployResult, err error) {
	s.diagnoseWith(ctx, res, "deploy", err.Error(), nil)
}

func (s *Server) diagnoseWith(ctx context.Context, res *DeployResult, stage, logs string, events []string) {
	dctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	proposal, aerr := s.assistant.DiagnoseFailure(dctx, &ai.FailureContext{Stage: stage, Logs: logs, Events: events})
	if aerr != nil || proposal == nil {
		return
	}
	res.Diagnosis = proposal.Diagnosis
	res.Proposals = proposal.Actions
}

func tailBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
