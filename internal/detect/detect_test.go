package detect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An app that brings no Dockerfile gets one written for it. That is now the
// only fallback there is, which is the point: every build donkey runs is a
// Dockerfile build.
func TestDetectAutoWritesADockerfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n")

	got, err := Detect(dir, nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildAI {
		t.Errorf("strategy = %q, want ai", got.Strategy)
	}
}

// A Dockerfile in the folder always wins: donkey writes one only for apps
// that don't say how to build themselves.
func TestDetectDockerfileWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\n")

	got, err := Detect(dir, nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildDockerfile {
		t.Errorf("strategy = %q, want dockerfile", got.Strategy)
	}
}

// build.type: ai overrides a Dockerfile sitting in the folder — the app is
// asking to be read rather than built as written.
func TestDetectAppBuildTypeWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\n")
	app := &config.AppConfig{Build: config.BuildConfig{Type: config.BuildAI}}

	got, err := Detect(dir, app)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildAI {
		t.Errorf("strategy = %q, want ai", got.Strategy)
	}
}

// A Dockerfile donkey wrote earlier is built as an ordinary Dockerfile: it
// only exists because someone accepted it for this app, and every later
// deploy should be the same build with no model in the way.
func TestDetectAIDockerfileIsReused(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, AIDockerfilePath, "FROM scratch\n")

	got, err := Detect(dir, nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildDockerfile || got.DockerfilePath != AIDockerfilePath {
		t.Errorf("strategy = %q, dockerfile = %q", got.Strategy, got.DockerfilePath)
	}
}

// Even when the app asks for build.type: ai, a Dockerfile donkey already
// wrote is what gets built — the assistant is asked once per app, not once
// per deploy.
func TestDetectAIReusesItsOwnDockerfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, AIDockerfilePath, "FROM scratch\n")
	app := &config.AppConfig{Build: config.BuildConfig{Type: config.BuildAI}}

	got, err := Detect(dir, app)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildDockerfile || got.DockerfilePath != AIDockerfilePath {
		t.Errorf("strategy = %q, dockerfile = %q", got.Strategy, got.DockerfilePath)
	}
}

// build.type: dockerfile with no Dockerfile is a mistake worth naming.
func TestDetectMissingDockerfile(t *testing.T) {
	dir := t.TempDir()
	app := &config.AppConfig{Build: config.BuildConfig{Type: config.BuildDockerfile}}
	if _, err := Detect(dir, app); err == nil {
		t.Fatal("a missing Dockerfile was not reported")
	}
}
