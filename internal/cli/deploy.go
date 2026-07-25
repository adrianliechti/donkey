package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"

	"monobox.ch/donkey/internal/ai"
	"monobox.ch/donkey/internal/apiclient"
	"monobox.ch/donkey/internal/gitutil"
	"monobox.ch/donkey/internal/ui"
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

	api, err := s.api()
	if err != nil {
		if interactive() && u.Confirm("This machine isn't connected to a donkey server yet. Connect now?", true) {
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

	u.Info("Packing up %s and sending it to the server…", u.Bold(app.Name))
	tarFile, err := apiclient.TarContextToTemp(s.Resolved.Flags.AppDir)
	if err != nil {
		return fmt.Errorf("couldn't pack the app folder: %w", err)
	}
	defer func() {
		tarFile.Close()
		os.Remove(tarFile.Name())
	}()

	if cmd.Bool("no-build") && cmd.String("tag") == "" {
		return fmt.Errorf("--no-build needs --tag: tell donkey which already-pushed image to deploy")
	}
	result, err := api.Deploy(ctx, tarFile, apiclient.DeployOpts{
		Platform:  cmd.String("platform"),
		Tag:       cmd.String("tag"),
		DryRun:    cmd.Bool("dry-run"),
		NoWait:    cmd.Bool("no-wait"),
		BuildOnly: cmd.Bool("build-only"),
		NoBuild:   cmd.Bool("no-build"),
		SHA:       gitutil.HeadSHA(s.Resolved.Flags.AppDir),
	},
		func(line string) { u.Printf("%s", line) },
		func(progress string) { u.Detail("%s", progress) },
	)
	if err != nil && strings.Contains(err.Error(), "mid-deploy") && !cmd.Bool("dry-run") && !cmd.Bool("build-only") {
		// The server keeps working when the stream drops (gateway config
		// updates can cut it) — follow along via status instead.
		u.Info("The connection was interrupted — the deploy continues on the server. Checking…")
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			time.Sleep(5 * time.Second)
			st, serr := api.Status(ctx, app.Name)
			if serr != nil {
				continue
			}
			if st.Found && st.Healthy {
				u.Success("%s is running.", u.Bold(app.Name))
				if st.URL != "" {
					u.Printf("  → %s", u.Bold(st.URL))
				}
				return nil
			}
		}
		return fmt.Errorf("lost the connection during the deploy and the app didn't become healthy — run `donkey status` to check")
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

// deployWithTag re-deploys the app's current image (no build) — used after
// addon changes so the Deployment picks up new bindings.
func deployWithTag(ctx context.Context, s *session, cmd *cli.Command, imageRef string) error {
	api, err := s.api()
	if err != nil {
		return err
	}
	tarFile, err := apiclient.TarContextToTemp(s.Resolved.Flags.AppDir)
	if err != nil {
		return err
	}
	defer func() {
		tarFile.Close()
		os.Remove(tarFile.Name())
	}()
	u := s.UI
	result, err := api.Deploy(ctx, tarFile, apiclient.DeployOpts{Tag: imageRef, NoBuild: true},
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
