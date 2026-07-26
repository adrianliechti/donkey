package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustRead(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("expected %s to exist: %v", rel, err)
	}
	return string(b)
}

func TestExtractContextZip(t *testing.T) {
	dir := t.TempDir()
	b := zipBytes(t, map[string]string{
		"donkey.yaml":               "name: web\nport: 3000\n",
		"src/main.py":               "print('hi')",
		"node_modules/pkg/index.js": "junk",
		".env":                      "SECRET=1",
		"certs/server.key":          "key material",
	})
	if _, err := extractContext(bytes.NewReader(b), dir); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, dir, "donkey.yaml"); got != "name: web\nport: 3000\n" {
		t.Fatalf("donkey.yaml content wrong: %q", got)
	}
	mustRead(t, dir, "src/main.py")
	for _, gone := range []string{"node_modules/pkg/index.js", ".env", "certs/server.key"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Fatalf("%s should have been skipped", gone)
		}
	}
}

func TestExtractContextZipStripsSingleRoot(t *testing.T) {
	dir := t.TempDir()
	b := zipBytes(t, map[string]string{
		"myapp-main/donkey.yaml": "name: web\nport: 3000\n",
		"myapp-main/app.js":      "listen()",
	})
	if _, err := extractContext(bytes.NewReader(b), dir); err != nil {
		t.Fatal(err)
	}
	mustRead(t, dir, "donkey.yaml")
	mustRead(t, dir, "app.js")
}

// A zip made with Finder's "Compress" carries a __MACOSX sibling tree; it
// must not defeat the single-root unwrap.
func TestExtractContextZipFinderStyle(t *testing.T) {
	dir := t.TempDir()
	b := zipBytes(t, map[string]string{
		"myapp/donkey.yaml":           "name: web\nport: 3000\n",
		"myapp/app.js":                "listen()",
		"__MACOSX/._donkey.yaml":      "resource fork",
		"__MACOSX/myapp/._app.js":     "resource fork",
		"myapp/.DS_Store":             "finder junk",
		"myapp/.git/config":           "[core]",
		"myapp/node_modules/dep/i.js": "junk",
	})
	if _, err := extractContext(bytes.NewReader(b), dir); err != nil {
		t.Fatal(err)
	}
	mustRead(t, dir, "donkey.yaml")
	mustRead(t, dir, "app.js")
	for _, gone := range []string{"__MACOSX", ".DS_Store", ".git", "node_modules", "myapp"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Fatalf("%s should not exist after extraction", gone)
		}
	}
}

// Tars need not list a directory before the files inside it.
func TestExtractContextTarWithoutDirEntries(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{"src/deep/main.py", "print('hi')"},
		{".env", "SECRET=1"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	if _, err := extractContext(&buf, dir); err != nil {
		t.Fatal(err)
	}
	mustRead(t, dir, "src/deep/main.py")
	// Server-side skip rules apply to tar uploads too, not just zips.
	if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
		t.Fatal(".env should have been skipped")
	}
}

func TestExtractContextZipRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	b := zipBytes(t, map[string]string{"../evil.txt": "x"})
	if _, err := extractContext(bytes.NewReader(b), dir); err == nil {
		t.Fatal("expected a traversal error")
	}
}

func TestExtractContextTarGz(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	content := "name: web\nport: 3000\n"
	if err := tw.WriteHeader(&tar.Header{Name: "donkey.yaml", Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	if _, err := extractContext(&buf, dir); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, dir, "donkey.yaml"); got != content {
		t.Fatalf("content wrong: %q", got)
	}
}

func TestExtractContextRejectsGarbage(t *testing.T) {
	if _, err := extractContext(bytes.NewReader([]byte("hello, not an archive")), t.TempDir()); err == nil {
		t.Fatal("expected an error for a non-archive upload")
	}
}

func TestAppFromParams(t *testing.T) {
	app, err := appFromParams("web", "3000")
	if err != nil || app.Name != "web" || app.Port != 3000 {
		t.Fatalf("unexpected: %+v, %v", app, err)
	}
	if _, err := appFromParams("", ""); err == nil {
		t.Fatal("expected an error without name and port")
	}
	if _, err := appFromParams("Web App", "3000"); err == nil {
		t.Fatal("expected an error for an invalid name")
	}
	if _, err := appFromParams("web", "notaport"); err == nil {
		t.Fatal("expected an error for a bad port")
	}
}

func TestContextRootFindsSubfolderApp(t *testing.T) {
	dir := t.TempDir()
	b := zipBytes(t, map[string]string{
		"repo-main/backend/donkey.yaml": "name: api\nport: 8000\n",
		"repo-main/backend/main.py":     "x",
		"repo-main/README.md":           "docs",
	})
	if _, err := extractContext(bytes.NewReader(b), dir); err != nil {
		t.Fatal(err)
	}
	root, took, err := contextRoot(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if took != "backend" {
		t.Fatalf("expected to descend into backend, got %q", took)
	}
	mustRead(t, root, "donkey.yaml")
}

func TestContextRootAmbiguousMonorepo(t *testing.T) {
	dir := t.TempDir()
	b := zipBytes(t, map[string]string{
		"api/donkey.yaml": "name: api\nport: 8000\n",
		"web/donkey.yaml": "name: web\nport: 3000\n",
	})
	if _, err := extractContext(bytes.NewReader(b), dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := contextRoot(dir, ""); err == nil {
		t.Fatal("expected an error naming both apps")
	}
	// …but an explicit path resolves it.
	root, took, err := contextRoot(dir, "web")
	if err != nil || took != "web" {
		t.Fatalf("explicit path failed: %v (took %q)", err, took)
	}
	mustRead(t, root, "donkey.yaml")
}

func TestContextRootRejectsEscape(t *testing.T) {
	if _, _, err := contextRoot(t.TempDir(), "../../etc"); err == nil {
		t.Fatal("expected an error for a path outside the upload")
	}
}
