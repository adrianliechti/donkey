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

// An app that brings no Dockerfile gets the installation's builder. This is
// the whole of the setting: everything else about the choice is downstream.
func TestDetectAutoUsesConfiguredBuilder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n")

	got, err := Detect(dir, nil, config.BuildSettings{Builder: config.BuildAI})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildAI {
		t.Errorf("strategy = %q, want ai", got.Strategy)
	}
	// donkey's own builder decides nothing here: the Dockerfile is written
	// during the build, so there is no provider list to guess a port from.
	if got.Railpack != nil {
		t.Error("donkey's own builder produced a railpack plan")
	}
}

// A Dockerfile in the folder always wins: the installation's builder is the
// fallback for apps that don't say how to build themselves.
func TestDetectDockerfileBeatsConfiguredBuilder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\n")

	got, err := Detect(dir, nil, config.BuildSettings{Builder: config.BuildAI})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildDockerfile {
		t.Errorf("strategy = %q, want dockerfile", got.Strategy)
	}
}

// And so does an app that names its builder itself — even against an
// installation that defaults to the other one.
func TestDetectAppBuildTypeWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "main.go", "package main\n")
	app := &config.AppConfig{Build: config.BuildConfig{Type: config.BuildAI}}

	got, err := Detect(dir, app, config.BuildSettings{Builder: config.BuildRailpack})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildAI {
		t.Errorf("strategy = %q, want ai", got.Strategy)
	}
}

// A Dockerfile donkey wrote earlier is preferred over the configured builder:
// it only exists because someone accepted it for this app.
func TestDetectAIDockerfileBeatsConfiguredBuilder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, AIDockerfilePath, "FROM scratch\n")

	got, err := Detect(dir, nil, config.BuildSettings{Builder: config.BuildAI})
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
	if _, err := Detect(dir, app, config.BuildSettings{}); err == nil {
		t.Fatal("a missing Dockerfile was not reported")
	}
}
