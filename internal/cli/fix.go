package cli

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/ui"
)

// showServerDiagnosis prints what the assistant made of a failure, and — for
// changes to files — the exact patch, then offers to apply it.
//
// Showing the diff is the point. "Change app.py" tells a user nothing they can
// check, so the old advice to go and edit it in an editor put the burden back
// on the person least able to carry it. A patch is reviewable by someone who
// does not know the language: they can see three lines change and read why.
func showServerDiagnosis(u *ui.UI, dir string, diagnosis string, proposals []ai.FixAction) {
	if diagnosis == "" {
		return
	}
	u.Printf("")
	u.Printf("%s %s", u.Bold("What happened:"), diagnosis)

	var edits []ai.FixAction
	for _, p := range proposals {
		switch p.Type {
		case "set_env":
			u.Printf("")
			u.Printf("  %s", u.Bold(fmt.Sprintf("donkey env set %s=%s", p.Key, p.Value)))
			u.Printf("  %s", u.Faint(p.Explanation))
		case "edit_file":
			if p.Diff == "" {
				continue
			}
			u.Printf("")
			u.Printf("  %s %s", u.Bold(p.Path), u.Faint("— "+p.Explanation))
			printDiff(u, p.Diff)
			edits = append(edits, p)
		default:
			u.Printf("")
			u.Printf("  %s", u.Faint(p.Explanation))
		}
	}

	if len(edits) == 0 {
		return
	}
	if u.AssumeYes || !u.Interactive {
		// --yes means "answer the safe questions for me", and rewriting
		// someone's source is not one of those.
		u.Printf("")
		u.Printf("  %s", u.Faint("Run `donkey deploy` again without --yes to apply one of these."))
		return
	}
	for _, e := range edits {
		u.Printf("")
		if !u.Confirm(fmt.Sprintf("Apply this change to %s?", e.Path), false) {
			continue
		}
		if err := applyEdit(dir, e); err != nil {
			u.Fail("Couldn't write %s: %v", e.Path, err)
			continue
		}
		u.Success("Updated %s.", e.Path)
	}
}

// printDiff renders a unified diff the way a terminal reader expects: added
// lines green, removed red, hunk headers faint. The header lines the diff
// carries (---/+++) are dropped, because the file is already named above and
// repeating it costs two of the few lines the user will actually read.
func printDiff(u *ui.UI, diff string) {
	for _, line := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
			continue
		case strings.HasPrefix(line, "@@"):
			u.Printf("    %s", u.Faint(line))
		case strings.HasPrefix(line, "+"):
			u.Printf("    %s", u.Green(line))
		case strings.HasPrefix(line, "-"):
			u.Printf("    %s", u.Red(line))
		default:
			u.Printf("    %s", line)
		}
	}
}

// applyEdit writes one proposed change into the working tree.
//
// This is the only place donkey writes a file the assistant wrote, into a
// folder the user owns, so containment is delegated to the operating system:
// an *os.Root refuses "..", absolute paths and symlinks that leave the folder
// at the syscall level. A string check here would have to be right about
// every one of those, and would still leave a window between the check and
// the write.
func applyEdit(dir string, e ai.FixAction) error {
	// A proposal built from redacted evidence would write the redaction back
	// over a real value. Nothing donkey shows the assistant is scrubbed today,
	// but a file is not the place to find out that changed.
	if strings.Contains(e.NewContent, "***") && !strings.Contains(e.Diff, "-***") {
		return fmt.Errorf("the proposed content contains a redaction marker — apply it by hand")
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	name := path.Clean(strings.TrimPrefix(filepath.ToSlash(e.Path), "./"))
	if parent := path.Dir(name); parent != "." {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	// Keep the file's existing permissions; a new file gets the usual 0644.
	perm := os.FileMode(0o644)
	if info, err := root.Stat(name); err == nil {
		perm = info.Mode().Perm()
	}
	return root.WriteFile(name, []byte(e.NewContent), perm)
}
