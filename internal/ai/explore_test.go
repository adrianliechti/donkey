package ai

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A header-only fixture, with no private key material. Share it so scanners
// cannot mistake the Go source between repeated PEM headers for a key body.
const testPrivateKeyHeader = "-----BEGIN PRIVATE KEY-----"

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
	if got := toolQuery(ex, "glob", "**/*", ""); strings.Contains(got, "secrets.txt") {
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
		"secret.key": testPrivateKeyHeader,
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
	if got := toolQuery(ex, "glob", "**/*", ""); strings.Contains(got, ".env") || strings.Contains(got, "secret.key") {
		t.Errorf("listing must not name secret files, got %q", got)
	}
	if got := searchTest(ex, "BEGIN PRIVATE KEY", ""); strings.Contains(got, "secret.key") {
		t.Errorf("search must not reach into secret files, got %q", got)
	}
	if got := searchTest(ex, "import os", ""); !strings.Contains(got, "app.py") {
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
		"secret.key":                  testPrivateKeyHeader,
	})
	ex, err := newExplorer(app)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()

	if got := globTest(ex, "*.csproj"); !strings.Contains(got, "api/Service.csproj") {
		t.Errorf("find(*.csproj) = %q", got)
	}
	if got := globTest(ex, "vite.config.*"); !strings.Contains(got, "src/vite.config.ts") {
		t.Errorf("find(vite.config.*) = %q", got)
	}
	// Shallowest first: the root manifest is nearly always the relevant one.
	got := globTest(ex, "package.json")
	if !strings.HasPrefix(got, "package.json\n") {
		t.Errorf("the root package.json should come first, got %q", got)
	}
	// The same exclusions as everywhere else still apply.
	if strings.Contains(got, "node_modules") {
		t.Errorf("find must not walk into node_modules, got %q", got)
	}
	if out := globTest(ex, "*.key"); strings.Contains(out, "secret.key") {
		t.Errorf("find must not name secret files, got %q", out)
	}
	if out := globTest(ex, "nothing-like-this-*"); !strings.Contains(out, "No matches") {
		t.Errorf("a miss should say so, got %q", out)
	}
	// A broken pattern is a wrong guess, not a reason to end the exploration.
	if out := globTest(ex, "["); !strings.Contains(out, "Invalid glob") {
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

	all := searchTest(ex, "app.run", "")
	if !strings.Contains(all, "apps/web") || !strings.Contains(all, "apps/api") {
		t.Errorf("an unscoped search should find both, got %q", all)
	}
	scoped := searchTest(ex, "app.run", "apps/api")
	if strings.Contains(scoped, "apps/web") {
		t.Errorf("a scoped search must not leave its folder, got %q", scoped)
	}
	if !strings.Contains(scoped, "port=8000") {
		t.Errorf("a scoped search should still find its own match, got %q", scoped)
	}
	// A bad scope must be reported explicitly; never silently search elsewhere.
	if fallback := searchTest(ex, "app.run", "does/not/exist"); !strings.Contains(fallback, "No such search path") {
		t.Errorf("a bad scope should explain the error, got %q", fallback)
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

func toolQuery(ex *explorer, name, pattern, scope string) string {
	args, _ := json.Marshal(map[string]any{"pattern": pattern, "path": scope, "output_mode": "content"})
	return ex.call(name, string(args))
}
func searchTest(ex *explorer, pattern, scope string) string {
	return toolQuery(ex, "grep", pattern, scope)
}
func globTest(ex *explorer, pattern string) string { return toolQuery(ex, "glob", pattern, "") }

func TestReadOffsetsAndByteLimit(t *testing.T) {
	var data strings.Builder
	for i := 1; i <= 900; i++ {
		fmt.Fprintf(&data, "line-%03d %s\n", i, strings.Repeat("x", 100))
	}
	root := write(t, map[string]string{"large.txt": data.String()})
	ex, err := newExplorer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	first := ex.call("read", `{"file_path":"large.txt","offset":1,"limit":500}`)
	if len(first) > maxToolOutput || !strings.Contains(first, "use offset=") {
		t.Fatalf("missing continuation: %s", first)
	}
	var next int
	_, err = fmt.Sscanf(first[strings.Index(first, "use offset="):], "use offset=%d", &next)
	if err != nil {
		t.Fatal(err)
	}
	page := ex.call("read", fmt.Sprintf(`{"file_path":"large.txt","offset":%d,"limit":1}`, next))
	if !strings.HasPrefix(page, fmt.Sprintf("%d\tline-%03d", next, next)) {
		t.Fatalf("wrong continuation: %s", page)
	}
	beyond := ex.call("read", `{"file_path":"large.txt","offset":900,"limit":1}`)
	if !strings.HasPrefix(beyond, "900\tline-900") {
		t.Fatal(beyond)
	}
	if got := ex.call("read", `{"file_path":"large.txt","offset":0}`); !strings.Contains(got, "positive") {
		t.Fatal(got)
	}
}

func TestReadLargeFileRange(t *testing.T) {
	root := write(t, map[string]string{"large.txt": strings.Repeat(strings.Repeat("x", 100)+"\n", 50000) + "THE END\n"})
	ex, err := newExplorer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	got := ex.call("read", `{"file_path":"large.txt","offset":50001,"limit":1}`)
	if got != "50001\tTHE END\n" {
		t.Fatal(got)
	}
}

func TestSearchModesAndPagination(t *testing.T) {
	root := write(t, map[string]string{"src/a.ts": "before\nLISTEN(8080)\nafter\n", "src/nested/b.ts": "listen(9000)\n", "other.py": "listen(80)"})
	ex, err := newExplorer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Close()
	got := ex.call("glob", `{"pattern":"**/*.ts","path":"src","head_limit":1}`)
	if !strings.HasPrefix(got, "src/a.ts\n") || !strings.Contains(got, "skip=1") {
		t.Fatal(got)
	}
	got = ex.call("glob", `{"pattern":"**/*.ts","path":"src","skip":1}`)
	if got != "src/nested/b.ts\n" {
		t.Fatal(got)
	}
	got = ex.call("grep", `{"pattern":"listen\\([0-9]+","path":"src/a.ts","-i":true,"-C":1,"output_mode":"content"}`)
	for _, want := range []string{"src/a.ts:1: before", "src/a.ts:2: LISTEN(8080)", "src/a.ts:3: after"} {
		if !strings.Contains(got, want) {
			t.Fatal(got)
		}
	}
	got = ex.call("grep", `{"pattern":"listen","glob":"*.ts","-i":true,"output_mode":"count"}`)
	if got != "src/a.ts:1\nsrc/nested/b.ts:1\n" {
		t.Fatal(got)
	}
}
