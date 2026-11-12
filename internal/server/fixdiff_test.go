package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/ai"
)

// A proposal is shown to the user as a patch and may be written back over
// their own source, so the two things that must hold are that the patch
// describes a real change and that it cannot land outside the app.
func TestWithDiffs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "app.py"), []byte("port = 3000\nrun()\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := withDiffs(root, []ai.FixAction{
		{Type: "edit_file", Path: "app.py", NewContent: "port = 8080\nrun()\n"},
		// Unchanged: nothing to show and nothing to apply.
		{Type: "edit_file", Path: "app.py", NewContent: "port = 3000\nrun()\n"},
		// Escaping the app — never offered, however it is spelled. The last
		// one is the case that reads as "not found" rather than as an
		// escape, because it stops at the missing folder first.
		{Type: "edit_file", Path: "../outside.py", NewContent: "x = 1\n"},
		{Type: "edit_file", Path: "/etc/passwd", NewContent: "x = 1\n"},
		{Type: "edit_file", Path: "a/../../outside.py", NewContent: "x = 1\n"},
		// A file that does not exist yet is a valid proposal: it diffs
		// against nothing and reads as an addition.
		{Type: "edit_file", Path: "Procfile", NewContent: "web: python app.py\n"},
		// Not a file change, so it passes through untouched.
		{Type: "set_env", Key: "PORT", Value: "8080"},
	})

	var kinds []string
	for _, a := range got {
		kinds = append(kinds, a.Type+":"+a.Path)
	}
	if len(got) != 3 {
		t.Fatalf("expected the real edit, the new file and the env action, got %v", kinds)
	}

	if !strings.Contains(got[0].Diff, "-port = 3000") || !strings.Contains(got[0].Diff, "+port = 8080") {
		t.Errorf("the diff should show the line replaced, got:\n%s", got[0].Diff)
	}
	if !strings.Contains(got[1].Diff, "+web: python app.py") {
		t.Errorf("a new file should read as an addition, got:\n%s", got[1].Diff)
	}
	if got[2].Type != "set_env" || got[2].Diff != "" {
		t.Errorf("a non-file action should pass through without a diff, got %+v", got[2])
	}
}
