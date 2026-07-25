package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/urfave/cli/v3"

	"monobox.ch/donkey/internal/ai"
	"monobox.ch/donkey/internal/config"
	"monobox.ch/donkey/internal/detect"
	"monobox.ch/donkey/internal/ui"
)

func initCommand() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "look at this folder's app and set up donkey.yaml",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "name", Usage: "name for the app (defaults to the folder name)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, err := newSession(cmd)
			if err != nil {
				return err
			}
			if s.Resolved.App != nil {
				s.UI.Success("This folder is already set up as %s in the space %s — run %s when you're ready.",
					s.UI.Bold(s.Resolved.App.Name), s.UI.Bold(s.Space), s.UI.Bold("donkey deploy"))
				return nil
			}
			_, err = initFlow(ctx, s, cmd)
			return err
		},
	}
}

// initFlow works out what this folder is, asks the few questions worth asking,
// and writes donkey.yaml. Shared by `donkey init` and first-run `donkey deploy`.
//
// Donkey's own detector and the assistant both run: the detector knows how to
// build the app, the assistant reads the source for the things no detector can
// see — the real port, a health route, and which folders hold data that has to
// survive a restart.
func initFlow(ctx context.Context, s *session, cmd *cli.Command) (*config.AppConfig, error) {
	u := s.UI
	dir := s.Resolved.Flags.AppDir

	u.Printf("Looking at your app…")
	det, detErr := detect.Detect(dir, nil)
	analysis, err := aiAnalyze(ctx, s, dir, det, detErr)
	if err != nil {
		return nil, err
	}

	if detErr != nil {
		if det, err = acceptBuildProposal(s, dir, analysis); err != nil {
			return nil, err
		}
		if det == nil {
			return nil, fmt.Errorf("setup stopped — nothing was written")
		}
	}
	u.Success("Recognized: %s", det.Summary)
	if analysis.AppType != "" {
		u.Info("%s%s", analysis.AppType, notesSuffix(analysis.Notes))
	}

	app := &config.AppConfig{
		Name:  cmd.String("name"),
		Space: s.Space,
		Port:  defaultPort(det, analysis),
	}
	if app.Name == "" {
		app.Name = config.SanitizeName(filepath.Base(absDir(dir)))
	}
	{
		if analysis.Port > 0 {
			u.Success("The app listens on port %d — %s", analysis.Port, analysis.PortReason)
		} else if analysis.PortReason != "" {
			u.Success("The app follows the PORT convention — any port works.")
		}
		if analysis.HealthcheckPath != "" {
			app.Healthcheck.Path = analysis.HealthcheckPath
			u.Info("Found a health route: %s", analysis.HealthcheckPath)
		}
		app.Storage = confirmStorage(u, analysis)
		for _, svc := range analysis.Services {
			u.Info("Looks like it needs %s (%s) — add it later with %s",
				svc.Type, svc.Reason, u.Bold("donkey addon add "+svc.Type))
		}
	}

	if !u.AssumeYes && u.Interactive {
		portStr := strconv.Itoa(app.Port)
		health := app.Healthcheck.Path
		form := huh.NewForm(huh.NewGroup(
			huh.NewInput().
				Title("What should this app be called?").
				Description("Used for its web address and everything on the cluster.").
				Value(&app.Name).
				Validate(func(v string) error {
					c := config.AppConfig{Name: v, Port: 80}
					c.ApplyDefaults()
					return c.Validate()
				}),
			huh.NewInput().
				Title("Which space does it belong to?").
				Description("Apps in one space share a namespace, can talk to each other by name, and live under <app>.<space>.<domain>.").
				Value(&app.Space).
				Validate(func(v string) error {
					c := config.AppConfig{Name: "app", Space: v, Port: 80}
					c.ApplyDefaults()
					return c.Validate()
				}),
			huh.NewInput().
				Title("Which port does the app listen on?").
				Value(&portStr).
				Validate(func(v string) error {
					n, err := strconv.Atoi(strings.TrimSpace(v))
					if err != nil || n < 1 || n > 65535 {
						return fmt.Errorf("enter a number between 1 and 65535")
					}
					return nil
				}),
			huh.NewInput().
				Title("Health check path (optional)").
				Description("A path that answers OK when the app is fine, e.g. /healthz. Leave empty to just check the app's port.").
				Value(&health),
		))
		if err := form.Run(); err != nil {
			return nil, fmt.Errorf("setup was cancelled")
		}
		app.Port, _ = strconv.Atoi(strings.TrimSpace(portStr))
		app.Healthcheck.Path = health
	}

	app.ApplyDefaults()
	if err := app.Validate(); err != nil {
		return nil, err
	}
	if err := app.Save(dir); err != nil {
		return nil, fmt.Errorf("couldn't write donkey.yaml: %w", err)
	}
	u.Success("Created donkey.yaml for %s in the space %s (port %d).", u.Bold(app.Name), u.Bold(app.Space), app.Port)
	return app, nil
}

// confirmStorage turns the assistant's volume findings into a storage block.
// Data folders are worth one question: getting them wrong either loses data
// (none mounted) or pins the app to a single copy for nothing.
func confirmStorage(u *ui.UI, analysis *ai.Analysis) *config.StorageConfig {
	if len(analysis.Volumes) == 0 {
		return nil
	}
	u.Printf("")
	u.Printf("%s", u.Bold("These folders hold data that must survive a restart:"))
	for _, v := range analysis.Volumes {
		u.Printf("  · %s — %s", u.Bold(v.Path), u.Faint(v.Reason))
	}
	u.Printf("  %s", u.Faint(fmt.Sprintf(
		"They share one %s disk (a folder each) and the app then runs as a single copy.", config.DefaultStorage)))
	if !u.Confirm("Keep these folders on a disk?", true) {
		// An empty list is a decision: deploys won't ask again.
		return &config.StorageConfig{Paths: []string{}}
	}
	return &config.StorageConfig{Size: config.DefaultStorage, Paths: analysis.VolumePaths()}
}

// acceptBuildProposal shows the assistant's build file and, once confirmed,
// writes it next to the app so every later build uses it.
func acceptBuildProposal(s *session, dir string, analysis *ai.Analysis) (*detect.Detection, error) {
	u := s.UI
	u.Info("Donkey couldn't recognize this app by itself.")
	u.Success("The assistant thinks this is: %s", u.Bold(analysis.AppType))
	if analysis.Notes != "" {
		u.Printf("  %s", u.Faint(analysis.Notes))
	}

	var path, content string
	switch {
	case analysis.Strategy == "dockerfile" && strings.TrimSpace(analysis.Dockerfile) != "":
		path, content = detect.AIDockerfilePath, analysis.Dockerfile
	case strings.TrimSpace(analysis.RailpackConfig) != "":
		path, content = detect.AIRailpackConfPath, analysis.RailpackConfig
	default:
		return nil, fmt.Errorf("the AI assistant couldn't propose a way to build this app")
	}

	u.Printf("")
	u.Printf("Proposed %s:", u.Bold(path))
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		u.Printf("  %s", u.Faint(line))
	}
	if !u.Confirm(fmt.Sprintf("Save this as %s and use it to build?", path), true) {
		return nil, nil
	}
	if err := writeAIFile(dir, path, content); err != nil {
		return nil, err
	}
	det, err := detect.Detect(dir, nil)
	if err != nil {
		return nil, fmt.Errorf("the AI proposal didn't work out: %w", err)
	}
	return det, nil
}

// aiAnalyze asks the server's assistant to read this folder. Donkey needs it:
// the assistant is what finds the real port, the health route and the folders
// that hold data — so a failure here stops setup with the reason.
func aiAnalyze(ctx context.Context, s *session, dir string, det *detect.Detection, detErr error) (*ai.Analysis, error) {
	u := s.UI
	api, err := s.api()
	if err != nil {
		return nil, err
	}
	u.Printf("Reading the code (file names and manifest files, never secrets, go to the assistant)…")
	sum, err := ai.Summarize(dir)
	if err != nil {
		return nil, fmt.Errorf("couldn't read this folder: %w", err)
	}
	in := &ai.AnalyzeInput{Summary: sum, DetectionFailed: detErr != nil}
	switch {
	case detErr != nil:
		in.Detected = detErr.Error()
	case det != nil:
		in.Detected = det.Summary
	}
	analysis, err := api.Analyze(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("donkey couldn't read your app: %w", err)
	}
	return analysis, nil
}

// defaultPort guesses a sensible port from what was found.
func defaultPort(det *detect.Detection, analysis *ai.Analysis) int {
	if analysis.Port > 0 {
		return analysis.Port
	}
	if det != nil && det.Railpack != nil {
		for _, p := range det.Railpack.Providers {
			switch p {
			case "python":
				return 8000
			case "node", "ruby":
				return 3000
			case "golang", "go", "java", "php":
				return 8080
			case "staticfile":
				return 80
			}
		}
	}
	return 8080
}

// writeAIFile stores a proposed build file under .donkey/ — donkey never
// overwrites a file the user wrote.
func writeAIFile(dir, relPath, content string) error {
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func notesSuffix(notes string) string {
	if notes == "" {
		return ""
	}
	return " — " + notes
}

func absDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}
