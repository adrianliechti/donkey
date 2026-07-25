package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
)

// analyzeApp reads the uploaded folder with the assistant — donkey's own
// detector is good at "which language is this", and poor at "which folder
// holds data that must survive a restart". The two answer different questions,
// so the detector's verdict is handed to the assistant as evidence.
//
// A failure here fails the deploy: donkey would otherwise apply a half-read
// app (no disk for the folder holding the database, say) without anyone
// noticing.
func (s *Server) analyzeApp(ctx context.Context, dir string, det *detect.Detection, detErr error) (*ai.Analysis, error) {
	sum, err := ai.Summarize(dir)
	if err != nil {
		return nil, fmt.Errorf("couldn't read the uploaded folder: %w", err)
	}
	in := &ai.AnalyzeInput{Summary: sum, DetectionFailed: detErr != nil}
	switch {
	case detErr != nil:
		in.Detected = detErr.Error()
	case det != nil:
		in.Detected = det.Summary
	}
	if b, err := os.ReadFile(filepath.Join(dir, config.AppFileName)); err == nil {
		in.CurrentConfig = string(b)
	}
	analysis, err := s.assistant.Analyze(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("the assistant couldn't read this app: %w", err)
	}
	return analysis, nil
}

// rescueDetection writes the assistant's proposed build file into the (throw
// away) build context so railpack or BuildKit can use it. The user's own
// files are never touched — this folder only exists for this one build.
func rescueDetection(dir string, analysis *ai.Analysis) error {
	if analysis == nil {
		return fmt.Errorf("no proposal available")
	}
	var relPath, content string
	switch {
	case analysis.Strategy == "dockerfile" && strings.TrimSpace(analysis.Dockerfile) != "":
		relPath, content = detect.AIDockerfilePath, analysis.Dockerfile
	case strings.TrimSpace(analysis.RailpackConfig) != "":
		relPath, content = detect.AIRailpackConfPath, analysis.RailpackConfig
	default:
		return fmt.Errorf("no proposal available")
	}
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

// mergeAnalysis fills in what donkey.yaml left open and returns one line per
// decision, so a deploy never changes something silently. What the user wrote
// always wins: a setting that is present in donkey.yaml is never overwritten,
// only commented on.
func mergeAnalysis(app *config.AppConfig, analysis *ai.Analysis) []string {
	if analysis == nil {
		return nil
	}
	var lines []string

	// Storage: nil means nobody decided yet. An empty list means the user
	// decided "no disk" — that decision stands. An app running several copies
	// can't gain a disk (it attaches to one machine at a time), so the finding
	// is reported instead of applied.
	switch {
	case app.Storage == nil && len(analysis.Volumes) > 0 && app.Replicas != nil && *app.Replicas > 1:
		lines = append(lines, fmt.Sprintf(
			"This app writes data (%s) but runs %d copies — a disk needs replicas: 1 plus a `storage:` block in donkey.yaml.",
			strings.Join(analysis.VolumePaths(), ", "), *app.Replicas))
	case app.Storage == nil && len(analysis.Volumes) > 0:
		app.Storage = &config.StorageConfig{Size: config.DefaultStorage}
		for _, v := range analysis.Volumes {
			app.Storage.Paths = append(app.Storage.Paths, v.Path)
			lines = append(lines, fmt.Sprintf("Keeping %s on a disk — %s", v.Path, v.Reason))
		}
		lines = append(lines, fmt.Sprintf(
			"One %s disk holds those folders. Put a `storage:` block in donkey.yaml to resize it, or an empty `paths: []` to do without.",
			config.DefaultStorage))
	}

	if app.Healthcheck.Path == "" && analysis.HealthcheckPath != "" {
		app.Healthcheck.Path = analysis.HealthcheckPath
		lines = append(lines, fmt.Sprintf("Checking the app's health at %s", analysis.HealthcheckPath))
	}

	// The port is required in donkey.yaml, so it is always the user's — but a
	// mismatch here is the most common reason an app never becomes healthy.
	if analysis.Port > 0 && analysis.Port != app.Port {
		lines = append(lines, fmt.Sprintf("Careful: donkey.yaml says port %d, but the code listens on %d — %s",
			app.Port, analysis.Port, analysis.PortReason))
	}

	for _, svc := range analysis.Services {
		if hasAddonType(app, svc.Type) {
			continue
		}
		lines = append(lines, fmt.Sprintf("This app looks like it needs %s (%s) — add it with `donkey addon add %s`",
			svc.Type, svc.Reason, svc.Type))
	}
	// An app that binds loopback starts, logs a URL and passes nothing: the
	// probe fails and the reason is invisible. Say it before that happens.
	if analysis.BindsLoopback {
		lines = append(lines, fmt.Sprintf(
			"Careful: this app listens on 127.0.0.1, which inside a container means itself and nobody else — it will start and still never answer. Bind 0.0.0.0 instead (Flask: `app.run(host=\"0.0.0.0\")`, Streamlit: `--server.address 0.0.0.0`). %s",
			analysis.BindReason))
	}
	lines = append(lines, localURLWarnings(analysis)...)

	// The app already declares its own steps — repeating the suggestion would
	// only be noise.
	if len(app.PreDeploy) == 0 {
		for _, step := range analysis.PreDeploy {
			lines = append(lines, fmt.Sprintf(
				"This app looks like it needs `%s` to run before each deploy (%s) — add it under `preDeploy:` in donkey.yaml, or the first request will fail on a database that was never migrated.",
				step.Command, step.Reason))
		}
	}
	// Green probe, working app, 400 for everyone. Worth saying plainly.
	if h := analysis.HostRestriction; h != nil {
		where := h.File
		if where != "" {
			where = " in " + where
		}
		fix := "Add the app's address there, or allow any host."
		if h.EnvVar != "" {
			fix = fmt.Sprintf("Set %s to the app's address (`donkey env set %s=…`), or allow any host%s.", h.EnvVar, h.EnvVar, where)
		}
		lines = append(lines, fmt.Sprintf(
			"Careful: %s%s does not cover the address this app gets, so it will reject every request with an error while still looking healthy. %s %s",
			h.Setting, where, fix, h.Reason))
	}
	return lines
}

// localURLWarnings explains hardcoded addresses that stop meaning what their
// author intended on a cluster. This is the most common way an app that works
// on a laptop fails its first deploy, and the failure is silent: the app
// starts, looks healthy, and only the calls go nowhere.
func localURLWarnings(analysis *ai.Analysis) []string {
	var lines []string
	for _, u := range analysis.LocalURLs {
		where := u.File
		if where != "" {
			where = " (" + where + ")"
		}
		if u.RunsIn == "browser" {
			lines = append(lines, fmt.Sprintf(
				"Careful: %s%s runs in the visitor's browser, where that address is *their* machine, not your backend. Call a relative path like /api and let this app proxy it to `http://<other-app>`, or point it at the other app's public address at build time (`build.env`).",
				u.URL, where))
			continue
		}
		// Server-side localhost is only wrong when it means another app.
		lines = append(lines, fmt.Sprintf(
			"Check %s%s: inside the cluster localhost is this app's own container. Another app in the space is reached by its name instead (http://<other-app>). %s",
			u.URL, where, u.Reason))
	}
	return lines
}

func hasAddonType(app *config.AppConfig, typ string) bool {
	for _, a := range app.Addons {
		if a.Type == typ {
			return true
		}
	}
	return false
}
