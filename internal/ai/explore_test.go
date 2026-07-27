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
	if got := ex.search("BEGIN PRIVATE KEY", ""); strings.Contains(got, "secret.key") {
		t.Errorf("search must not reach into secret files, got %q", got)
	}
	if got := ex.search("import os", ""); !strings.Contains(got, "app.py") {
		t.Errorf("search should still find ordinary source, got %q", got)
	}
}

// find is what makes exploring a repository nobody has seen affordable:
// "where is the vite config" has to be one call, not a walk the model reads.
func TestExplorerFind(t *testing.T) {
	app := write(t, map[string]string{
		"src/vite.config.ts":          "export default {}",
		"packages/ui/package.json":    "{}",
		"package.json":                "{}",
		"api/Service.csproj":          "<Project/>",
		"node_modules/x/package.json": "{}",
		"secret.key":                  "-----BEGIN PRIVATE KEY-----",
	})
	ex, err := newExplorer(app)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	if got := ex.find("*.csproj"); !strings.Contains(got, "api/Service.csproj") {
		t.Errorf("find(*.csproj) = %q", got)
	}
	if got := ex.find("vite.config.*"); !strings.Contains(got, "src/vite.config.ts") {
		t.Errorf("find(vite.config.*) = %q", got)
	}
	// Shallowest first: the root manifest is nearly always the relevant one.
	got := ex.find("package.json")
	if !strings.HasPrefix(got, "package.json\n") {
		t.Errorf("the root package.json should come first, got %q", got)
	}
	// The same exclusions as everywhere else still apply.
	if strings.Contains(got, "node_modules") {
		t.Errorf("find must not walk into node_modules, got %q", got)
	}
	if out := ex.find("*.key"); strings.Contains(out, "secret.key") {
		t.Errorf("find must not name secret files, got %q", out)
	}
	if out := ex.find("nothing-like-this-*"); !strings.Contains(out, "No file matches") {
		t.Errorf("a miss should say so, got %q", out)
	}
	// A broken pattern is a wrong guess, not a reason to end the exploration.
	if out := ex.find("["); !strings.Contains(out, "not a valid pattern") {
		t.Errorf("a bad pattern should come back as text, got %q", out)
	}
}

// Scoping matters on a monorepo, where the same import appears in a dozen
// packages and only one of them is being deployed.
func TestExplorerScopedSearch(t *testing.T) {
	app := write(t, map[string]string{
		"apps/web/main.py": "app.run(port=5000)\n",
		"apps/api/main.py": "app.run(port=8000)\n",
	})
	ex, err := newExplorer(app)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	all := ex.search("app.run", "")
	if !strings.Contains(all, "apps/web") || !strings.Contains(all, "apps/api") {
		t.Errorf("an unscoped search should find both, got %q", all)
	}
	scoped := ex.search("app.run", "apps/api")
	if strings.Contains(scoped, "apps/web") {
		t.Errorf("a scoped search must not leave its folder, got %q", scoped)
	}
	if !strings.Contains(scoped, "port=8000") {
		t.Errorf("a scoped search should still find its own match, got %q", scoped)
	}
	// A scope that is not a folder is a wrong guess; searching everything
	// beats refusing to answer.
	if fallback := ex.search("app.run", "does/not/exist"); !strings.Contains(fallback, "apps/api") {
		t.Errorf("a bad scope should fall back to the whole app, got %q", fallback)
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
