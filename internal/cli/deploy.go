package cli

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/gitutil"
	"github.com/adrianliechti/donkey/internal/server"
	"github.com/adrianliechti/donkey/internal/ui"
)

func deployCommand() *cli.Command {
	return &cli.Command{
		Name:  "deploy",
		Usage: "build this folder's app on the donkey server and put it live",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "tag", Usage: "use this exact image reference instead of an auto-generated one"},
			&cli.BoolFlag{Name: "build-only", Usage: "build and push the image, but don't deploy it"},
			&cli.BoolFlag{Name: "no-build", Usage: "deploy the given --tag without building"},
			&cli.BoolFlag{Name: "dry-run", Usage: "print the Kubernetes objects the server would apply, change nothing"},
			&cli.BoolFlag{Name: "no-wait", Usage: "don't wait for the new version to become healthy"},
			&cli.StringFlag{Name: "platform", Usage: "target platform of the cluster nodes (overrides donkey.yaml)"},
		},
		Action: deployAction,
	}
}

func deployAction(ctx context.Context, cmd *cli.Command) error {
	s, err := newSession(cmd)
	if err != nil {
		return err
	}
	u := s.UI
	if s.AppErr != nil {
		return s.AppErr
	}
	if cmd.Bool("no-build") && cmd.String("tag") == "" {
		return fmt.Errorf("--no-build needs --tag: tell donkey which already-pushed image to deploy")
	}

	api, err := s.api()
	if err != nil {
		if u.Interactive && u.Confirm("This machine isn't connected to a donkey server yet. Connect now?", true) {
			if err := runLogin(ctx, cmd); err != nil {
				return err
			}
			if s, err = newSession(cmd); err != nil {
				return err
			}
			u = s.UI
			if api, err = s.api(); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	if s.Resolved.App == nil {
		// First deploy in this folder: run the init flow, then continue.
		if _, err := initFlow(ctx, s, cmd); err != nil {
			return err
		}
		if s, err = newSession(cmd); err != nil {
			return err
		}
		u = s.UI
		if s.Resolved.App == nil {
			return fmt.Errorf("setting up donkey.yaml didn't complete — run `donkey init` and try again")
		}
	}
	app := s.Resolved.App

	// The old image tells the reconnect path what "done" looks like — a
	// still-healthy previous version must never read as success. When this
	// lookup itself fails, that certainty is gone and the reconnect path
	// stays conservative instead of guessing.
	priorImage := ""
	priorKnown := false
	if st, serr := api.Status(ctx, s.Space, app.Name); serr == nil {
		priorKnown = true
		if st.Found {
			priorImage = st.Image
		}
	}

	// In a dry run the manifests own stdout, so progress goes to stderr —
	// `donkey deploy --dry-run > app.yaml` must yield clean YAML.
	sayLine := func(line string) { u.Printf("%s", line) }
	if cmd.Bool("dry-run") {
		sayLine = func(line string) { fmt.Fprintln(os.Stderr, line) }
	}

	if !cmd.Bool("dry-run") {
		u.Info("Packing up %s (space %s) and sending it to the server…", u.Bold(app.Name), u.Bold(s.Space))
	}
	result, err := uploadAndDeploy(ctx, s, api, apiclient.DeployOpts{
		Space:     s.Space,
		Platform:  cmd.String("platform"),
		Tag:       cmd.String("tag"),
		DryRun:    cmd.Bool("dry-run"),
		NoWait:    cmd.Bool("no-wait"),
		BuildOnly: cmd.Bool("build-only"),
		NoBuild:   cmd.Bool("no-build"),
		SHA:       gitutil.HeadSHA(s.Resolved.Flags.AppDir),
	},
		sayLine,
		buildProgressPrinter(u),
	)
	if err != nil && strings.Contains(err.Error(), "mid-deploy") && !cmd.Bool("dry-run") && !cmd.Bool("build-only") {
		// The server keeps working when the stream drops (gateway config
		// updates can cut it) — follow along via status instead.
		u.Info("The connection was interrupted — the deploy continues on the server. Checking…")
		deadline := time.Now().Add(10 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
			st, serr := api.Status(ctx, s.Space, app.Name)
			if serr != nil {
				continue
			}
			if st.Found && st.Healthy && newVersionRunning(st.Image, cmd.String("tag"), priorImage, priorKnown) {
				u.Success("%s is running.", u.Bold(app.Name))
				if st.URL != "" {
					u.Printf("  → %s", u.Bold(st.URL))
				}
				return nil
			}
		}
		return fmt.Errorf("lost the connection during the deploy and the new version didn't come up within 10 minutes — run `donkey status` to check (builds can take longer)")
	}
	if err != nil {
		return err
	}

	if result.Manifests != "" { // dry-run
		fmt.Print(result.Manifests)
		return nil
	}
	if !result.OK {
		showServerDiagnosis(u, result.Diagnosis, result.Proposals)
		return fmt.Errorf("%s", result.Error)
	}
	if cmd.Bool("build-only") {
		u.Success("Image ready: %s", result.ImageRef)
		return nil
	}
	u.Success("%s is running.", u.Bold(result.App))
	if result.URL != "" {
		u.Printf("  → %s", u.Bold(result.URL))
	}
	return nil
}

// uploadAndDeploy packs the app folder (saying what stays behind) and runs
// one server-side deploy — the shared transport of every deploy variant.
func uploadAndDeploy(ctx context.Context, s *session, api *apiclient.Client, opts apiclient.DeployOpts, onLine, onProgress func(string)) (*server.DeployResult, error) {
	tarFile, skipped, err := apiclient.TarContextToTemp(s.Resolved.Flags.AppDir)
	if err != nil {
		return nil, fmt.Errorf("couldn't pack the app folder: %w", err)
	}
	defer func() {
		tarFile.Close()
		os.Remove(tarFile.Name())
	}()
	// A file the build needed but never got is a mystery worth preventing.
	// In a dry run the manifests own stdout, so this goes to stderr with the
	// rest of the progress.
	if len(skipped) > 0 {
		names := skipped
		if len(names) > 4 {
			names = append(append([]string{}, names[:4]...), fmt.Sprintf("and %d more", len(skipped)-4))
		}
		line := fmt.Sprintf("Not uploaded (secret-shaped or symlink): %s", strings.Join(names, ", "))
		if opts.DryRun {
			fmt.Fprintln(os.Stderr, line)
		} else {
			s.UI.Info("%s", line)
		}
	}
	return api.Deploy(ctx, tarFile, opts, onLine, onProgress)
}

// buildStepRE matches BuildKit's step-header lines ("#5 [2/6] RUN npm ci").
var buildStepRE = regexp.MustCompile(`^#\d+ \[`)

// buildProgressPrinter keeps a build from looking frozen: verbose mode gets
// everything, the default view gets the step headers — one line per build
// step instead of minutes of silence.
func buildProgressPrinter(u *ui.UI) func(string) {
	return func(progress string) {
		if u.Verbose {
			u.Detail("%s", progress)
			return
		}
		for _, line := range strings.Split(progress, "\n") {
			if buildStepRE.MatchString(line) {
				u.Printf("  %s", u.Faint(line))
			}
		}
	}
}

// newVersionRunning guards the reconnect path against a false green: with an
// explicit tag the running image must match it; otherwise it must at least
// have changed from what ran before the deploy — and when the pre-deploy
// state couldn't be read at all, "healthy" alone proves nothing.
func newVersionRunning(current, expectedTag, priorImage string, priorKnown bool) bool {
	if expectedTag != "" {
		return current == expectedTag
	}
	if !priorKnown {
		return false
	}
	return priorImage == "" || current != priorImage
}

// deployWithTag re-deploys the app's current image (no build) — used after
// addon changes so the Deployment picks up new bindings.
func deployWithTag(ctx context.Context, s *session, cmd *cli.Command, imageRef string) error {
	api, err := s.api()
	if err != nil {
		return err
	}
	u := s.UI
	result, err := uploadAndDeploy(ctx, s, api, apiclient.DeployOpts{Space: s.Space, Tag: imageRef, NoBuild: true},
		func(line string) { u.Detail("%s", line) },
		func(progress string) { u.Detail("%s", progress) },
	)
	if err != nil {
		return err
	}
	if !result.OK {
		showServerDiagnosis(u, result.Diagnosis, result.Proposals)
		return fmt.Errorf("%s", result.Error)
	}
	u.Success("%s is running with the updated services.", u.Bold(result.App))
	return nil
}

func showServerDiagnosis(u *ui.UI, diagnosis string, proposals []ai.FixAction) {
	if diagnosis == "" {
		return
	}
	u.Printf("")
	u.Printf("%s %s", u.Bold("What happened:"), diagnosis)
	for _, p := range proposals {
		switch p.Type {
		case "set_env":
			u.Printf("  · Suggested: donkey env set %s=%s — %s", p.Key, p.Value, p.Explanation)
		case "edit_file":
			u.Printf("  · Suggested change to %s — %s", p.Path, p.Explanation)
			u.Printf("    %s", u.Faint("(review and apply it in your editor, then deploy again)"))
		default:
			u.Printf("  · %s", p.Explanation)
		}
	}
}
