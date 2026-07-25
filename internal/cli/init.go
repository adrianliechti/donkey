package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/urfave/cli/v3"

	"monobox.ch/donkey/internal/ai"
	"monobox.ch/donkey/internal/config"
	"monobox.ch/donkey/internal/detect"
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
				s.UI.Success("This folder is already set up as %s — run %s when you're ready.",
					s.UI.Bold(s.Resolved.App.Name), s.UI.Bold("donkey deploy"))
				return nil
			}
			_, err = initFlow(ctx, s, cmd)
			return err
		},
	}
}

// initFlow detects the app (with AI fallback), runs the wizard, and writes
// donkey.yaml. Shared by `donkey init` and first-run `donkey deploy`.
func initFlow(ctx context.Context, s *session, cmd *cli.Command) (*config.AppConfig, error) {
	u := s.UI
	dir := s.Resolved.Flags.AppDir

	u.Printf("Looking at your app…")
	det, detErr := detect.Detect(dir, nil)
	if detErr != nil {
		var aiErr error
		det, aiErr = aiDetectFallback(ctx, s, dir)
		if aiErr != nil {
			return nil, aiErr
		}
		if det == nil {
			return nil, detErr
		}
	}
	u.Success("Recognized: %s", det.Summary)

	// With AI available, read the source to find the real port and health
	// route instead of relying on per-language defaults. Never blocks: any
	// problem falls back to the defaults.
	if det.SuggestedPort == 0 {
		aiRefineSettings(ctx, s, dir, det)
	}

	app := &config.AppConfig{
		Name: cmd.String("name"),
		Port: defaultPort(det),
	}
	if det.SuggestedHealth != "" {
		app.Healthcheck.Path = det.SuggestedHealth
	}
	if app.Name == "" {
		app.Name = sanitizeName(filepath.Base(absDir(dir)))
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
	u.Success("Created donkey.yaml for %s (port %d).", u.Bold(app.Name), app.Port)
	return app, nil
}

// aiRefineSettings asks OpenAI for the app's real port and health route and
// stores them as hints on the detection.
func aiRefineSettings(ctx context.Context, s *session, dir string, det *detect.Detection) {
	u := s.UI
	assistant := newAssistant(s)
	if !assistant.Enabled() {
		return
	}
	u.Printf("Checking the code for the right port (summary sent to OpenAI)…")
	sum, err := ai.Summarize(dir)
	if err != nil {
		return
	}
	settings, err := assistant.SuggestAppSettings(ctx, sum)
	if err != nil {
		u.Detail("AI settings check skipped: %v", err)
		return
	}
	switch {
	case settings.Port > 0:
		det.SuggestedPort = settings.Port
		u.Success("The app listens on port %d — %s", settings.Port, settings.Reason)
	default:
		u.Success("The app follows the PORT convention — any port works.")
	}
	if settings.HealthcheckPath != "" {
		det.SuggestedHealth = settings.HealthcheckPath
		u.Info("Found a health route: %s", settings.HealthcheckPath)
	}
}

// aiDetectFallback asks OpenAI to figure out an unrecognized app. Returns
// (nil, nil) when AI is off or the user declines the proposal.
func aiDetectFallback(ctx context.Context, s *session, dir string) (*detect.Detection, error) {
	u := s.UI
	assistant := newAssistant(s)
	if !assistant.Enabled() {
		return nil, nil
	}
	u.Info("Donkey couldn't recognize this app by itself.")
	u.Printf("Sending a summary (file names and manifest files, never secrets) to OpenAI…")
	sum, err := ai.Summarize(dir)
	if err != nil {
		return nil, err
	}
	sug, err := assistant.SuggestBuildConfig(ctx, sum)
	if err != nil {
		return nil, err
	}
	u.Success("The assistant thinks this is: %s", u.Bold(sug.AppType))
	if sug.Notes != "" {
		u.Printf("  %s", u.Faint(sug.Notes))
	}

	var path, content string
	if sug.Strategy == "dockerfile" && strings.TrimSpace(sug.Dockerfile) != "" {
		path, content = detect.AIDockerfilePath, sug.Dockerfile
	} else if strings.TrimSpace(sug.RailpackConfig) != "" {
		path, content = detect.AIRailpackConfPath, sug.RailpackConfig
	} else {
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
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return nil, err
	}

	det, err := detect.Detect(dir, nil)
	if err != nil {
		return nil, fmt.Errorf("the AI proposal didn't work out: %w", err)
	}
	if sug.Port > 0 {
		det.SuggestedPort = sug.Port
	}
	if sug.HealthcheckPath != "" {
		det.SuggestedHealth = sug.HealthcheckPath
	}
	return det, nil
}

// newAssistant returns the server-backed assistant: the CLI never talks to
// OpenAI itself. Without a server connection (or with --no-ai) it is inert.
func newAssistant(s *session) ai.Assistant {
	if s.Resolved.Flags.NoAI {
		return ai.NewDisabled()
	}
	api, err := s.api()
	if err != nil {
		return ai.NewDisabled()
	}
	return api.Assistant()
}

// defaultPort guesses a sensible port from what was detected.
func defaultPort(det *detect.Detection) int {
	if det.SuggestedPort > 0 {
		return det.SuggestedPort
	}
	if det.Railpack != nil {
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

var nameCleanRE = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeName(base string) string {
	name := strings.ToLower(base)
	name = nameCleanRE.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "app"
	}
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}

func absDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}
