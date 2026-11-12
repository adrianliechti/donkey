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

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/server"
	"github.com/adrianliechti/donkey/internal/ui"
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
			// Never quietly overwrite a donkey.yaml that merely has a typo.
			if s.AppErr != nil {
				return fmt.Errorf("%w\n\nFix the file (or delete it) and run `donkey init` again", s.AppErr)
			}
			if s.Resolved.App != nil {
				s.UI.Success("This folder is already set up as %s in the space %s — run %s when you're ready.",
					s.UI.Bold(s.Resolved.App.Name), s.UI.Bold(s.Space), s.UI.Bold("donkey deploy"))
				return nil
			}
			_, _, err = initFlow(ctx, s, cmd)
			return err
		},
	}
}

// initFlow works out what this folder is, asks the few questions worth asking,
// and writes donkey.yaml. Shared by `donkey init` and first-run `donkey deploy`.
//
// The reading itself happens on the server, as one plan: donkey's detector
// says how to build the app, the assistant reads the source for what no
// detector can see (the real port, a health route, the folders holding data),
// and the cluster answers the questions only it can — whether the name is
// free, whether the space exists, what already runs there. The console shows
// exactly this plan as its review screen, which is why both surfaces say the
// same things about the same app.
//
// The returned plan id lets the deploy that follows reuse this reading instead
// of paying for a second one.
func initFlow(ctx context.Context, s *session, cmd *cli.Command) (*config.AppConfig, string, error) {
	u := s.UI
	dir := s.Resolved.Flags.AppDir

	api, err := s.api()
	if err != nil {
		return nil, "", err
	}
	name := cmd.String("name")
	if name == "" {
		name = filepath.Base(absDir(dir))
	}

	// The upload is the same build context a deploy sends (secret-shaped files
	// excluded); only file names and manifest files go on to the assistant.
	u.Printf("Packing this folder up and asking the server to read it…")
	tarFile, skipped, err := apiclient.TarContextToTemp(dir)
	if err != nil {
		return nil, "", fmt.Errorf("couldn't pack the app folder: %w", err)
	}
	defer func() {
		tarFile.Close()
		os.Remove(tarFile.Name())
	}()
	if len(skipped) > 0 {
		u.Info("Not uploaded (secret-shaped or symlink): %s", strings.Join(shortList(skipped), ", "))
	}

	plan, err := api.Plan(ctx, tarFile, apiclient.PlanOpts{Space: s.Space, Name: name})
	if err != nil {
		return nil, "", fmt.Errorf("donkey couldn't read your app: %w", err)
	}

	u.Success("Recognized: %s", plan.Build.Summary)
	if plan.AppType != "" {
		u.Info("%s%s", plan.AppType, notesSuffix(plan.Notes))
	}
	switch plan.App.PortSource {
	case server.PortFromCode:
		u.Success("The app listens on port %d — %s", plan.App.Port, plan.App.PortReason)
	case server.PortFromEnv:
		u.Success("The app follows the PORT convention — donkey will tell it to use %d.", plan.App.Port)
	case server.PortGuessed:
		u.Info("Nothing in the code pins a port — donkey suggests %d.", plan.App.Port)
	}
	if plan.App.Healthcheck != "" {
		u.Info("Found a health route: %s", plan.App.Healthcheck)
	}
	showFindings(u, plan.Findings)

	edit := server.PlanEdit{
		Name:        plan.App.Name,
		Space:       plan.App.Space,
		Port:        plan.App.Port,
		Healthcheck: plan.App.Healthcheck,
		Exposure:    plan.App.Exposure,
		Storage:     confirmStorage(u, plan),
		Services:    confirmServices(u, plan),
	}

	if !u.AssumeYes && u.Interactive {
		portStr := strconv.Itoa(edit.Port)
		form := huh.NewForm(huh.NewGroup(
			huh.NewInput().
				Title("What should this app be called?").
				Description("Used for its web address and everything on the cluster.").
				Value(&edit.Name).
				Validate(func(v string) error {
					c := config.AppConfig{Name: v, Port: 80}
					c.ApplyDefaults()
					return c.Validate()
				}),
			huh.NewInput().
				Title("Which space does it belong to?").
				Description("Apps in one space share a namespace, can talk to each other by name, and live under <app>.<space>.<domain>.").
				Value(&edit.Space).
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
				Value(&edit.Healthcheck),
		))
		if err := form.Run(); err != nil {
			return nil, "", fmt.Errorf("setup was cancelled")
		}
		edit.Port, _ = strconv.Atoi(strings.TrimSpace(portStr))
	}

	// The answers go back to the plan, which re-runs its checks against them:
	// the name that was free before the form may belong to something now.
	plan, err = api.UpdatePlan(ctx, plan.ID, edit)
	if err != nil {
		return nil, "", err
	}
	if !plan.Ready {
		showFindings(u, plan.Findings)
		return nil, "", fmt.Errorf("this app can't be set up as it stands — see above")
	}

	// The server rendered the file; writing something else here is how the
	// console and the CLI would start disagreeing about the same app.
	if err := os.WriteFile(filepath.Join(dir, config.AppFileName), []byte(plan.Config), 0o644); err != nil {
		return nil, "", fmt.Errorf("couldn't write donkey.yaml: %w", err)
	}
	app, err := config.LoadApp(dir)
	if err != nil {
		return nil, "", err
	}
	u.Success("Created donkey.yaml for %s in the space %s (port %d).",
		u.Bold(plan.App.Name), u.Bold(plan.App.Space), plan.App.Port)
	return app, plan.ID, nil
}

// showFindings prints what the server found, in the shape a terminal reads:
// the claim on its own line, the evidence and the way out underneath it.
func showFindings(u *ui.UI, findings []server.Finding) {
	for _, f := range findings {
		switch f.Level {
		case server.LevelBlock:
			u.Fail("%s", f.Title)
		case server.LevelWarn:
			u.Warn("%s", f.Title)
		default:
			u.Info("%s", f.Title)
		}
		if detail := strings.TrimSpace(f.Detail + " " + f.Fix); detail != "" {
			u.Printf("  %s", u.Faint(detail))
		}
	}
}

// confirmStorage turns the plan's volume findings into the storage answer.
// Data folders are worth one question: getting them wrong either loses data
// (none mounted) or pins the app to a single copy for nothing.
func confirmStorage(u *ui.UI, plan *server.Plan) []string {
	if len(plan.Volumes) == 0 {
		return []string{}
	}
	u.Printf("")
	u.Printf("%s", u.Bold("These folders hold data that must survive a restart:"))
	for _, v := range plan.Volumes {
		u.Printf("  · %s — %s", u.Bold(v.Path), u.Faint(v.Reason))
	}
	size := plan.App.StorageSize
	if size == "" {
		size = config.DefaultStorage
	}
	u.Printf("  %s", u.Faint(fmt.Sprintf(
		"They share one %s disk (a folder each) and the app then runs as a single copy.", size)))
	if !u.Confirm("Keep these folders on a disk?", true) {
		// An empty list is a decision: deploys won't ask again.
		return []string{}
	}
	paths := []string{}
	for _, v := range plan.Volumes {
		paths = append(paths, v.Path)
	}
	return paths
}

// confirmServices asks about the backing services the app expects. Creating
// one is real — a database, a disk, a password — so each is its own question,
// and the default is yes: an app that needs postgres and doesn't get it fails
// on its first request.
//
// Unattended, nothing is created. `--yes` means "take the safe default", and
// provisioning a database because nobody was there to say no is not one; the
// plan's own warning says what to add instead.
func confirmServices(u *ui.UI, plan *server.Plan) []string {
	if u.AssumeYes || !u.Interactive {
		return []string{}
	}
	out := []string{}
	for _, svc := range plan.Services {
		if !svc.Known {
			continue
		}
		if !svc.Selected {
			continue
		}
		question := fmt.Sprintf("Create %s in the space %s and connect this app to it?", svc.Type, plan.App.Space)
		if svc.Exists {
			question = fmt.Sprintf("Connect this app to the %s the space already runs?", svc.Name)
		}
		if svc.Reason != "" {
			u.Info("This app looks like it needs %s — %s", u.Bold(svc.Type), u.Faint(svc.Reason))
		}
		if u.Confirm(question, true) {
			out = append(out, svc.Name)
		}
	}
	return out
}

// shortList keeps a list of file names to the few worth reading.
func shortList(names []string) []string {
	if len(names) <= 4 {
		return names
	}
	return append(append([]string{}, names[:4]...), fmt.Sprintf("and %d more", len(names)-4))
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
