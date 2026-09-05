package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditBatchAndCreate(t *testing.T) {
	root := write(t, map[string]string{"run.sh": "#!/bin/sh\necho hello\n"})
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0755); err != nil {
		t.Fatal(err)
	}
	ex, err := newExplorer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	ex.writable = true
	ex.read("run.sh")
	got := ex.call("edit", `{"edits":[{"file_path":"run.sh","old_string":"hello","new_string":"world"},{"file_path":"src/new.txt","old_string":"","new_string":"created\n"},{"file_path":"run.sh","old_string":"world","new_string":"ready"}]}`)
	if !strings.HasPrefix(got, "Edited ") {
		t.Fatal(got)
	}
	data, err := os.ReadFile(filepath.Join(root, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "#!/bin/sh\necho ready\n" {
		t.Fatal(string(data))
	}
	info, err := os.Stat(filepath.Join(root, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("lost executable permission: %v", info.Mode())
	}
	changes := ex.changes()
	if len(changes) != 2 || !strings.Contains(changes[0].Diff, "-echo hello") || !strings.Contains(changes[0].Diff, "+echo ready") {
		t.Fatalf("wrong diffs: %+v", changes)
	}
}

func TestEditFailedBatchChangesNothing(t *testing.T) {
	root := write(t, map[string]string{"a": "original", "b": "duplicate duplicate"})
	ex, err := newExplorer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	ex.writable = true
	ex.read("a")
	ex.read("b")
	got := ex.call("edit", `{"edits":[{"file_path":"a","old_string":"original","new_string":"changed"},{"file_path":"new","old_string":"","new_string":"created"},{"file_path":"b","old_string":"duplicate","new_string":"one"}]}`)
	if !strings.Contains(got, "matches 2 times") {
		t.Fatal(got)
	}
	data, _ := os.ReadFile(filepath.Join(root, "a"))
	if string(data) != "original" {
		t.Fatal("partial write")
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatal("partially created file")
	}
	if len(ex.changes()) != 0 {
		t.Fatal("failed batch recorded changes")
	}
}

func TestEditRequiresFreshReadAndProtectsPaths(t *testing.T) {
	root := write(t, map[string]string{"a": "old", ".env": "hidden", "donkey.yaml": "name: app"})
	ex, err := newExplorer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	args := `{"edits":[{"file_path":"a","old_string":"old","new_string":"new"}]}`
	if got := ex.call("edit", args); !strings.Contains(got, "read-only") {
		t.Fatal(got)
	}
	for _, tool := range ex.tools() {
		if tool.OfFunction.Name == "edit" {
			t.Fatal("planning exposes edits")
		}
	}
	ex.writable = true
	if got := ex.call("edit", args); !strings.Contains(got, "before editing") {
		t.Fatal(got)
	}
	// A failed read must not authorize writing.
	ex.call("read", `{"file_path":"a","offset":500}`)
	if got := ex.call("edit", args); !strings.Contains(got, "before editing") {
		t.Fatal(got)
	}
	ex.read("a")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("older"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := ex.call("edit", args); !strings.Contains(got, "changed since") {
		t.Fatal(got)
	}
	if err := os.Symlink(".env", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if got := ex.read("alias"); strings.Contains(got, "hidden") {
		t.Fatal("read secret alias")
	}
	for _, name := range []string{".env", "donkey.yaml", "../escape", "alias"} {
		got := ex.call("edit", `{"edits":[{"file_path":"`+name+`","old_string":"","new_string":"oops"}]}`)
		if strings.HasPrefix(got, "Edited ") {
			t.Fatalf("edited protected path %s", name)
		}
	}
}
