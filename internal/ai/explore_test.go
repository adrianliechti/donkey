package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The assistant chooses these paths itself, so the folder it was given has to
// be the whole of what it can reach. Containment is *os.Root's job — these
// tests are here to prove donkey actually routes through it, including the
// symlink case a string check would wave through.
func TestExplorerStaysInsideTheApp(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secrets.txt"), []byte("password123"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := t.TempDir()
	if err := os.WriteFile(filepath.Join(app, "main.py"), []byte("port = 8000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink pointing out of the app: resolving it naively would read a
	// file that is not part of the upload at all.
	if err := os.Symlink(outside, filepath.Join(app, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ex, err := newExplorer(app)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	if got := ex.read("main.py"); !strings.Contains(got, "port = 8000") {
		t.Errorf("a file in the app should be readable, got %q", got)
	}
	for _, bad := range []string{
		"../" + filepath.Base(outside) + "/secrets.txt",
		"escape/secrets.txt",
		"/etc/hosts",
		"main.py/../../etc/hosts",
	} {
		if got := ex.read(bad); strings.Contains(got, "password123") || strings.Contains(got, "localhost") {
			t.Errorf("read(%q) reached outside the app: %q", bad, got)
		}
	}
	// Listing must not walk out through the symlink either.
	if got := ex.list(""); strings.Contains(got, "secrets.txt") {
		t.Errorf("list should not follow a symlink out of the app, got %q", got)
	}
}

// Files donkey refuses to send stay invisible to every tool, not just to the
// one that reads them.
func TestExplorerHidesSecrets(t *testing.T) {
	app := t.TempDir()
	for name, body := range map[string]string{
		".env":       "DATABASE_URL=postgres://u:p@host/db",
		"app.py":     "import os\n",
		"secret.key": "-----BEGIN PRIVATE KEY-----",
	} {
		if err := os.WriteFile(filepath.Join(app, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ex, err := newExplorer(app)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	if got := ex.read(".env"); strings.Contains(got, "postgres://") {
		t.Errorf(".env must never be readable, got %q", got)
	}
	if got := ex.list(""); strings.Contains(got, ".env") || strings.Contains(got, "secret.key") {
		t.Errorf("listing must not name secret files, got %q", got)
	}
	if got := ex.search("BEGIN PRIVATE KEY"); strings.Contains(got, "secret.key") {
		t.Errorf("search must not reach into secret files, got %q", got)
	}
	if got := ex.search("import os"); !strings.Contains(got, "app.py") {
		t.Errorf("search should still find ordinary source, got %q", got)
	}
}

func TestFSPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                      ".",
		"main.py":               "main.py",
		"./main.py":             "main.py",
		"/app/blog/settings.py": "blog/settings.py",
		"/main.py":              "main.py",
		"a/b/../c.py":           "a/c.py",
	} {
		if got := fsPath(in); got != want {
			t.Errorf("fsPath(%q) = %q, want %q", in, got, want)
		}
	}
}
