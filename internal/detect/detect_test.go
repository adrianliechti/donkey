package detect

import (
	"os"
	"path/filepath"
	"strings"
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

	got, err := Detect(dir, nil, config.BuildSettings{Builder: config.BuildBuildpacks})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildBuildpacks {
		t.Errorf("strategy = %q, want buildpacks", got.Strategy)
	}
	if !strings.Contains(got.Summary, config.DefaultBuildpacksBuilderImage) {
		t.Errorf("the summary doesn't say which builder will run: %q", got.Summary)
	}
	// Buildpacks detect inside the build, so nothing about the app is read
	// here — and there is no provider list to guess a port from.
	if got.Railpack != nil {
		t.Error("the buildpacks path produced a railpack plan")
	}
}

// A Dockerfile in the folder always wins: the installation's builder is the
// fallback for apps that don't say how to build themselves.
func TestDetectDockerfileBeatsConfiguredBuilder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Dockerfile", "FROM scratch\n")

	got, err := Detect(dir, nil, config.BuildSettings{Builder: config.BuildBuildpacks})
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
	app := &config.AppConfig{Build: config.BuildConfig{Type: config.BuildBuildpacks}}

	got, err := Detect(dir, app, config.BuildSettings{Builder: config.BuildRailpack})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if got.Strategy != config.BuildBuildpacks {
		t.Errorf("strategy = %q, want buildpacks", got.Strategy)
	}
}

// The AI-proposed Dockerfile is still preferred over the configured builder:
// it only exists because someone accepted it for this app.
func TestDetectAIDockerfileBeatsConfiguredBuilder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, AIDockerfilePath, "FROM scratch\n")

	got, err := Detect(dir, nil, config.BuildSettings{Builder: config.BuildBuildpacks})
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
