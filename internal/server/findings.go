package server

import (
	"fmt"
	"strings"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
)

// Finding is one thing donkey noticed about an app: a decision it made, a
// fact worth knowing, or something that will break the app once it runs.
//
// Findings exist so that the three surfaces — the deploy stream, the CLI and
// the console's review screen — say the same thing about the same app. The
// text is written once, here; a caller that can do better than a sentence
// (the console renders storage as a checkbox, not as prose) recognizes the
// finding by its Code and renders it its own way.
type Finding struct {
	Level string `json:"level"`
	// Code names the finding for callers that render it themselves. It is
	// stable: the console keys its layout off these.
	Code string `json:"code"`
	// Title is the finding in one sentence, in the user's language.
	Title string `json:"title"`
	// Detail is the evidence behind it — usually the assistant's reason.
	Detail string `json:"detail,omitempty"`
	// Fix is what to do about it, when there is something to do.
	Fix string `json:"fix,omitempty"`
	// File points at where in the app it was found, when it is one place.
	File string `json:"file,omitempty"`
}

// Levels. A block is a reason not to deploy at all — the deploy would refuse
// it anyway, and saying so before the upload beats saying it after the build.
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelBlock = "block"
)

// Text renders a finding as the one line a terminal shows. The console uses
// the fields directly.
func (f Finding) Text() string {
	out := f.Title
	if f.Detail != "" {
		out += " — " + f.Detail
	}
	if f.Fix != "" {
		out += " " + f.Fix
	}
	return out
}

// Blocking reports whether any finding is a reason not to deploy.
func Blocking(findings []Finding) bool {
	for _, f := range findings {
		if f.Level == LevelBlock {
			return true
		}
	}
	return false
}

// mergeAnalysis fills in what donkey.yaml left open and reports every
// decision plus everything the assistant found worth saying — what a deploy
// narrates as it goes. What the user wrote always wins: a setting present in
// donkey.yaml is never overwritten, only commented on.
func mergeAnalysis(app *config.AppConfig, analysis *ai.Analysis) []Finding {
	if analysis == nil {
		return nil
	}
	return append(applyAnalysis(app, analysis), analysisWarnings(app, analysis)...)
}

// applyAnalysis is the part that changes the config: the folders that need a
// disk and the health route, when donkey.yaml left them open. Each change
// comes back as a finding, so nothing is settled silently.
func applyAnalysis(app *config.AppConfig, analysis *ai.Analysis) []Finding {
	if analysis == nil {
		return nil
	}
	var out []Finding

	// Storage: nil means nobody decided yet. An empty list means the user
	// decided "no disk" — that decision stands. An app running several copies
	// can't gain a disk (it attaches to one machine at a time), so the finding
	// is reported instead of applied.
	switch {
	case app.Storage == nil && len(analysis.Volumes) > 0 && app.Replicas != nil && *app.Replicas > 1:
		out = append(out, Finding{
			Level: LevelWarn,
			Code:  "storage-replicas",
			Title: fmt.Sprintf("This app writes data (%s) but runs %d copies, so it cannot have a disk",
				strings.Join(analysis.VolumePaths(), ", "), *app.Replicas),
			Fix: "A disk needs `replicas: 1` plus a `storage:` block in donkey.yaml.",
		})
	case app.Storage == nil && len(analysis.Volumes) > 0:
		app.Storage = &config.StorageConfig{Size: config.DefaultStorage}
		for _, v := range analysis.Volumes {
			app.Storage.Paths = append(app.Storage.Paths, v.Path)
			out = append(out, Finding{
				Level:  LevelInfo,
				Code:   "storage",
				Title:  fmt.Sprintf("Keeping %s on a disk", v.Path),
				Detail: v.Reason,
			})
		}
		out = append(out, Finding{
			Level: LevelInfo,
			Code:  "storage-size",
			Title: fmt.Sprintf("One %s disk holds those folders", config.DefaultStorage),
			Fix:   "Put a `storage:` block in donkey.yaml to resize it, or an empty `paths: []` to do without.",
		})
	}

	if app.Healthcheck.Path == "" && analysis.HealthcheckPath != "" {
		app.Healthcheck.Path = analysis.HealthcheckPath
		out = append(out, Finding{
			Level: LevelInfo,
			Code:  "healthcheck",
			Title: fmt.Sprintf("Checking the app's health at %s", analysis.HealthcheckPath),
		})
	}
	return out
}

// analysisWarnings is the part that changes nothing: what the assistant found
// that the person deploying should know. Every one of these is a way an app
// deploys green and still doesn't work.
func analysisWarnings(app *config.AppConfig, analysis *ai.Analysis) []Finding {
	if analysis == nil {
		return nil
	}
	var out []Finding

	// The port is required in donkey.yaml, so it is always the user's — but a
	// mismatch here is the most common reason an app never becomes healthy.
	if analysis.Port > 0 && analysis.Port != app.Port {
		out = append(out, Finding{
			Level:  LevelWarn,
			Code:   "port-mismatch",
			Title:  fmt.Sprintf("donkey.yaml says port %d, but the code listens on %d", app.Port, analysis.Port),
			Detail: analysis.PortReason,
			Fix:    fmt.Sprintf("Set the port to %d, or make the app listen on %d.", analysis.Port, app.Port),
		})
	}

	for _, svc := range analysis.Services {
		if hasAddonType(app, svc.Type) {
			continue
		}
		out = append(out, Finding{
			Level:  LevelWarn,
			Code:   "service-missing",
			Title:  fmt.Sprintf("This app looks like it needs %s", svc.Type),
			Detail: svc.Reason,
			Fix:    fmt.Sprintf("Add it with `donkey addon add %s`.", svc.Type),
		})
	}

	// An app that binds loopback starts, logs a URL and passes nothing: the
	// probe fails and the reason is invisible. Say it before that happens.
	if analysis.BindsLoopback {
		out = append(out, Finding{
			Level:  LevelWarn,
			Code:   "loopback-bind",
			Title:  "This app listens on 127.0.0.1, which inside a container means itself and nobody else — it will start and still never answer",
			Detail: analysis.BindReason,
			Fix:    "Bind 0.0.0.0 instead (Flask: `app.run(host=\"0.0.0.0\")`, Streamlit: `--server.address 0.0.0.0`).",
		})
	}
	out = append(out, localURLWarnings(analysis)...)

	// The app already declares its own steps — repeating the suggestion would
	// only be noise.
	if len(app.PreDeploy) == 0 {
		for _, step := range analysis.PreDeploy {
			out = append(out, Finding{
				Level:  LevelWarn,
				Code:   "predeploy",
				Title:  fmt.Sprintf("This app looks like it needs `%s` to run before each deploy", step.Command),
				Detail: step.Reason,
				Fix:    "Add it under `preDeploy:` in donkey.yaml, or the first request will fail on a database that was never migrated.",
			})
		}
	}

	// Green probe, working app, 400 for everyone. Worth saying plainly.
	if h := analysis.HostRestriction; h != nil {
		fix := "Add the app's address there, or allow any host."
		if h.EnvVar != "" {
			fix = fmt.Sprintf("Set %s to the app's address (`donkey env set %s=…`), or allow any host.", h.EnvVar, h.EnvVar)
		}
		where := ""
		if h.File != "" {
			where = " in " + h.File
		}
		out = append(out, Finding{
			Level: LevelWarn,
			Code:  "host-restriction",
			Title: fmt.Sprintf("%s%s does not cover the address this app gets, so it will reject every request while still looking healthy",
				h.Setting, where),
			Detail: h.Reason,
			Fix:    fix,
			File:   h.File,
		})
	}
	return out
}

// localURLWarnings explains hardcoded addresses that stop meaning what their
// author intended on a cluster. This is the most common way an app that works
// on a laptop fails its first deploy, and the failure is silent: the app
// starts, looks healthy, and only the calls go nowhere.
func localURLWarnings(analysis *ai.Analysis) []Finding {
	var out []Finding
	for _, u := range analysis.LocalURLs {
		where := ""
		if u.File != "" {
			where = " (" + u.File + ")"
		}
		if u.RunsIn == "browser" {
			out = append(out, Finding{
				Level: LevelWarn,
				Code:  "local-url-browser",
				Title: fmt.Sprintf("%s%s runs in the visitor's browser, where that address is *their* machine, not your backend",
					u.URL, where),
				Detail: u.Reason,
				Fix:    "Call a relative path like /api and let this app proxy it to `http://<other-app>`, or point it at the other app's public address at build time (`build.env`).",
				File:   u.File,
			})
			continue
		}
		// Server-side localhost is only wrong when it means another app.
		out = append(out, Finding{
			Level:  LevelWarn,
			Code:   "local-url-server",
			Title:  fmt.Sprintf("Check %s%s: inside the cluster localhost is this app's own container", u.URL, where),
			Detail: u.Reason,
			Fix:    "Another app in the space is reached by its name instead (http://<other-app>).",
			File:   u.File,
		})
	}
	return out
}

func hasAddonType(app *config.AppConfig, typ string) bool {
	for _, a := range app.Addons {
		if a.Type == typ {
			return true
		}
	}
	return false
}
