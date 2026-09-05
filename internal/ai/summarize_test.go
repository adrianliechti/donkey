package ai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSummarizeCapsAndDenylist(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"name":"x"}`)
	write(".env", "SECRET_TOKEN=abc123")
	write("config/secrets.yaml", "password: hunter2")
	write("server.key", "PRIVATE KEY")
	write("node_modules/dep/index.js", "junk")
	write("src/index.js", "console.log('hi')")
	write("very/deep/workspace/packages/backend/service/package.json", `{"name":"deep-app"}`)

	sum, err := Summarize(dir)
	if err != nil {
		t.Fatal(err)
	}
	rendered := sum.Render()
	for _, leaked := range []string{"abc123", "hunter2", "PRIVATE KEY"} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("summary leaked secret content %q", leaked)
		}
	}
	if strings.Contains(sum.Tree, "node_modules") {
		t.Error("tree should skip node_modules")
	}
	if !strings.Contains(rendered, `{"name":"x"}`) {
		t.Error("manifest content should be included")
	}
	if !strings.Contains(rendered, "console.log('hi')") {
		t.Error("entrypoint source (src/index.js) should be included — ports live there")
	}
	if !strings.Contains(rendered, `{"name":"deep-app"}`) {
		t.Error("deeply nested manifests should be included")
	}
}

func TestInterestingFile(t *testing.T) {
	for _, name := range []string{"server.ts", "main.go", "app.py", "vite.config.js", "package.json", "Procfile"} {
		if !interestingFile(name) {
			t.Errorf("%s should be interesting", name)
		}
	}
	for _, name := range []string{"helper.js", "styles.css", "photo.png", "notes.txt"} {
		if interestingFile(name) {
			t.Errorf("%s should not be interesting", name)
		}
	}
}

func TestDeniedFile(t *testing.T) {
	for _, name := range []string{".env", ".env.local", "id_rsa", "tls.key", "cert.pem", "my-credentials.json", "api_token.txt"} {
		if !deniedFile(name) {
			t.Errorf("%s should be denied", name)
		}
	}
	for _, name := range []string{"package.json", "main.py", "README.md"} {
		if deniedFile(name) {
			t.Errorf("%s should be allowed", name)
		}
	}
}
