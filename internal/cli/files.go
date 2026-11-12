package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/apiclient"
)

// filesCommand manages what is on an app's disk. The disk outlives deploys,
// so it is the one part of an app donkey cannot rebuild from the repository:
// seeding it, fetching something out of it and clearing it are day-2 work.
func filesCommand() *cli.Command {
	return &cli.Command{
		Name:  "files",
		Usage: "browse and change the files on the app's disk",
		Commands: []*cli.Command{
			filesListCommand(),
			filesGetCommand(),
			filesPutCommand(),
			filesRemoveCommand(),
			filesMoveCommand(),
		},
	}
}

// filesSession is the shared preamble of every subcommand.
func filesSession(cmd *cli.Command) (*session, *apiclient.Client, string, error) {
	s, err := newSession(cmd)
	if err != nil {
		return nil, nil, "", err
	}
	app, err := requireApp(s)
	if err != nil {
		return nil, nil, "", err
	}
	api, err := s.api()
	if err != nil {
		return nil, nil, "", err
	}
	return s, api, app.Name, nil
}

func filesListCommand() *cli.Command {
	return &cli.Command{
		Name:      "ls",
		Usage:     "list a folder on the disk (default: the disk's top level)",
		ArgsUsage: "[path]",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, api, app, err := filesSession(cmd)
			if err != nil {
				return err
			}
			u := s.UI
			listing, err := api.ListFiles(ctx, s.Space, app, cmd.Args().First())
			if err != nil {
				return err
			}
			// At the top level the folder names are donkey's own (derived from
			// the mount paths), so show which app path each one is.
			for _, f := range listing.Folders {
				u.Printf("%s %s", u.Bold(f.Name+"/"), u.Faint("→ "+f.Path+" in the app"))
			}
			if len(listing.Folders) > 0 && len(listing.Entries) > 0 {
				u.Printf("")
			}
			for _, e := range listing.Entries {
				name := e.Name
				if e.IsDir {
					name += "/"
				}
				u.Printf("%s  %10s  %s  %s", u.Faint(e.Mode), humanSize(e.Size),
					u.Faint(e.ModTime.Local().Format("2006-01-02 15:04")), name)
			}
			switch {
			case listing.Truncated:
				u.Info("There are more files than donkey lists at once — open a subfolder to see the rest.")
			case len(listing.Entries) == 0 && len(listing.Folders) == 0:
				u.Info("Nothing here yet.")
			}
			return nil
		},
	}
}

func filesGetCommand() *cli.Command {
	return &cli.Command{
		Name:      "get",
		Usage:     "download a file (to stdout, or to a local path with -o)",
		ArgsUsage: "PATH",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "write to this local file instead of stdout"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, api, app, err := filesSession(cmd)
			if err != nil {
				return err
			}
			remote := cmd.Args().First()
			if remote == "" {
				return fmt.Errorf("tell donkey which file to download, e.g. `donkey files get app-data/notes.db`")
			}
			out := io.Writer(os.Stdout)
			if dest := cmd.String("output"); dest != "" {
				f, err := os.Create(dest)
				if err != nil {
					return err
				}
				defer f.Close()
				out = f
			}
			if err := api.DownloadFile(ctx, s.Space, app, remote, out); err != nil {
				return err
			}
			if dest := cmd.String("output"); dest != "" {
				s.UI.Success("Saved %s.", s.UI.Bold(dest))
			}
			return nil
		},
	}
}

func filesPutCommand() *cli.Command {
	return &cli.Command{
		Name:      "put",
		Usage:     "upload a local file to the disk",
		ArgsUsage: "LOCAL [REMOTE]",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, api, app, err := filesSession(cmd)
			if err != nil {
				return err
			}
			local := cmd.Args().First()
			if local == "" {
				return fmt.Errorf("tell donkey which file to upload, e.g. `donkey files put seed.csv app-data/seed.csv`")
			}
			f, err := os.Open(local)
			if err != nil {
				return err
			}
			defer f.Close()

			remote := cmd.Args().Get(1)
			// A bare folder (or nothing) keeps the local file's name.
			if remote == "" || strings.HasSuffix(remote, "/") {
				remote = path.Join(remote, filepath.Base(local))
			}
			if err := api.UploadFile(ctx, s.Space, app, remote, f); err != nil {
				return err
			}
			s.UI.Success("Uploaded to %s.", s.UI.Bold(remote))
			s.UI.Info("The app sees it immediately — no deploy needed.")
			return nil
		},
	}
}

func filesRemoveCommand() *cli.Command {
	return &cli.Command{
		Name:      "rm",
		Usage:     "remove a file or folder from the disk",
		ArgsUsage: "PATH",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "force", Usage: "don't ask for confirmation (for scripts)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, api, app, err := filesSession(cmd)
			if err != nil {
				return err
			}
			target := cmd.Args().First()
			if target == "" {
				return fmt.Errorf("tell donkey what to remove, e.g. `donkey files rm app-data/old.log`")
			}
			u := s.UI
			// Deleting from the disk is unrecoverable: nothing here comes back
			// with a redeploy.
			if !cmd.Bool("force") && !u.Confirm(fmt.Sprintf("Permanently delete %s from %s's disk?", target, app), false) {
				if !u.Interactive || u.AssumeYes {
					return fmt.Errorf("removing %s needs confirmation — pass --force to skip the prompt", target)
				}
				u.Info("Nothing changed.")
				return nil
			}
			if err := api.RemoveFile(ctx, s.Space, app, target); err != nil {
				return err
			}
			u.Success("%s is gone.", target)
			return nil
		},
	}
}

func filesMoveCommand() *cli.Command {
	return &cli.Command{
		Name:      "mv",
		Usage:     "move or rename something on the disk",
		ArgsUsage: "FROM TO",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			s, api, app, err := filesSession(cmd)
			if err != nil {
				return err
			}
			from, to := cmd.Args().First(), cmd.Args().Get(1)
			if from == "" || to == "" {
				return fmt.Errorf("tell donkey what to move and where, e.g. `donkey files mv app-data/a.txt app-data/b.txt`")
			}
			if err := api.MoveFile(ctx, s.Space, app, from, to); err != nil {
				return err
			}
			s.UI.Success("Moved %s to %s.", from, to)
			return nil
		},
	}
}

// humanSize renders a byte count the way a person reads it.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
