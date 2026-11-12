package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/urfave/cli/v3"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/ui"
)

// maxAppSearchDepth bounds how far below a repository root donkey looks for
// apps. Two levels covers the shapes people actually use — services/api,
// apps/web, packages/worker — without walking a whole tree of node_modules
// lookalikes.
const maxAppSearchDepth = 3

// findAppDirs lists the folders below dir that hold a donkey.yaml, nearest
// first. dir itself is excluded: a folder with its own config is one app, and
// never a repository of them.
//
// The search stops at the first level that yields anything. A repository whose
// apps sit in services/ must not also pick up a donkey.yaml nested deeper
// inside one of them — the shallower set is the answer, and anything below it
// belongs to an app that was already found.
func findAppDirs(dir string) []string {
	if _, err := os.Stat(filepath.Join(dir, config.AppFileName)); err == nil {
		return nil
	}
	var found []string
	level := []string{dir}
	for d := 0; d < maxAppSearchDepth && len(found) == 0; d++ {
		var next []string
		for _, parent := range level {
			entries, err := os.ReadDir(parent)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || config.SkippedUploadDir(e.Name()) {
					continue
				}
				child := filepath.Join(parent, e.Name())
				next = append(next, child)
				if _, err := os.Stat(filepath.Join(child, config.AppFileName)); err == nil {
					found = append(found, child)
				}
			}
		}
		level = next
	}
	sort.Strings(found)
	return found
}

// deployEach deploys every app of a repository, one after another.
//
// Sequential on purpose. The apps of one repository are usually the parts of
// one system, so a failure part-way through is something the user has to see
// and reason about — running them in parallel would interleave the output of
// the very failure they need to read. It also keeps the build queue honest:
// donkey builds server-side, and a repository is not entitled to the whole of
// it at once.
//
// A failure stops the run rather than pressing on. The apps that already
// deployed stay deployed, which is said plainly, because a half-deployed
// system is a state worth knowing you are in.
func deployEach(ctx context.Context, cmd *cli.Command, u *ui.UI, dirs []string) error {
	root := cmd.String("app-dir")
	rel := func(d string) string {
		if r, err := filepath.Rel(root, d); err == nil {
			return r
		}
		return d
	}

	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		names = append(names, rel(d))
	}
	if len(dirs) == 1 {
		u.Info("The app in this folder is %s — deploying that.", u.Bold(names[0]))
	} else {
		u.Info("This folder holds %d apps: %s", len(dirs), u.Bold(joinNames(names)))
	}

	for i, dir := range dirs {
		if len(dirs) > 1 {
			u.Printf("")
			u.Printf("%s", u.Bold(fmt.Sprintf("[%d/%d] %s", i+1, len(dirs), rel(dir))))
		}
		// Each app is deployed exactly as if the user had run donkey from its
		// own folder — same flags, same questions, same output.
		if err := cmd.Set("app-dir", dir); err != nil {
			return err
		}
		if err := deployAction(ctx, cmd); err != nil {
			if i > 0 {
				u.Printf("")
				u.Warn("%s failed. The %d app(s) before it are deployed and still running.", rel(dir), i)
			}
			return err
		}
	}
	if len(dirs) > 1 {
		u.Printf("")
		u.Success("All %d apps are deployed.", len(dirs))
	}
	return nil
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	out := ""
	for i, n := range names {
		switch {
		case i == 0:
			out = n
		case i == len(names)-1:
			out += " and " + n
		default:
			out += ", " + n
		}
	}
	return out
}
