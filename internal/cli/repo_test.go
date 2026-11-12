package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A folder with no donkey.yaml is far more often a repository than a new app.
// Getting that wrong is what deployed a monorepo as one mystery app named
// after its root.
func TestFindAppDirs(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "donkey.yaml"), []byte("name: x\nport: 80\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("services/api")
	write("services/web")
	// Deeper than the apps above, and inside one of them: it belongs to that
	// app, and must not be offered as an app of the repository.
	write("services/api/vendor-thing/nested")
	// Never walked at all.
	write("node_modules/pkg")

	got := findAppDirs(root)
	if len(got) != 2 {
		t.Fatalf("expected the two services, got %v", got)
	}
	for i, want := range []string{"services/api", "services/web"} {
		if rel, _ := filepath.Rel(root, got[i]); rel != want {
			t.Errorf("got[%d] = %q, want %q", i, rel, want)
		}
	}

	// A folder that is itself an app is one app, never a repository of them.
	if err := os.WriteFile(filepath.Join(root, "donkey.yaml"), []byte("name: x\nport: 80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findAppDirs(root); got != nil {
		t.Errorf("a folder with its own config must not be searched, got %v", got)
	}
}
