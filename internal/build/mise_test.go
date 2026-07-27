package build

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rpmise "github.com/railwayapp/railpack/core/mise"
)

// fakeMise writes a stand-in for the mise binary that answers --version the
// way the real one does.
func fakeMise(t *testing.T, path, version string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a POSIX shell script")
	}
	script := "#!/bin/sh\necho '" + version + " linux-x64 (0000000 2026-01-01)'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The whole point of staging mise is that railpack finds it and skips its
// download, so the assertion is railpack's own installer — not a path donkey
// spells out twice and can get wrong in both places.
func TestEnsureMiseSatisfiesRailpack(t *testing.T) {
	dir := t.TempDir()
	baked := filepath.Join(dir, "baked-mise")
	fakeMise(t, baked, rpmise.Version)
	installDir := filepath.Join(dir, "install")

	if err := ensureMise(t.Context(), installDir, baked, nil); err != nil {
		t.Fatalf("ensureMise: %v", err)
	}
	// mise.New downloads from github.com unless the binary is already at the
	// path it derives; reaching for the network here would hang or fail.
	if _, err := rpmise.New(installDir); err != nil {
		t.Fatalf("railpack did not accept the staged mise: %v", err)
	}
}

// MisePath must track railpack's constants rather than repeat them.
func TestMisePathFollowsRailpack(t *testing.T) {
	got := MisePath()
	if dir := filepath.Dir(got); dir != rpmise.InstallDir {
		t.Errorf("MisePath() is in %q, railpack looks in %q", dir, rpmise.InstallDir)
	}
	if !strings.Contains(filepath.Base(got), rpmise.Version) {
		t.Errorf("MisePath() = %q, which does not name railpack's mise version %q", got, rpmise.Version)
	}
}

// A version that doesn't match railpack's must stop the installation at
// startup: railpack picks the binary by filename, so the wrong one would run
// under the right name and fail somewhere much less obvious.
func TestEnsureMiseRejectsVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	baked := filepath.Join(dir, "baked-mise")
	fakeMise(t, baked, "1999.1.1")

	err := ensureMise(t.Context(), filepath.Join(dir, "install"), baked, nil)
	if err == nil {
		t.Fatal("a mise of the wrong version was accepted")
	}
	if !strings.Contains(err.Error(), rpmise.Version) {
		t.Errorf("the error doesn't say which version is needed: %v", err)
	}
}

// Giving donkey no mise is the ordinary case: railpack downloads its own, as
// it always did. That must be silent — an error here would be logged at every
// startup of every installation.
func TestEnsureMiseWithoutProvidedBinary(t *testing.T) {
	dir := t.TempDir()
	installDir := filepath.Join(dir, "install")
	if err := ensureMise(t.Context(), installDir, filepath.Join(dir, "nope"), nil); err != nil {
		t.Fatalf("a missing mise should be left to railpack, got: %v", err)
	}
	if _, err := os.Stat(misePath(installDir)); err == nil {
		t.Error("something was staged although no binary was provided")
	}
}

// With a proxy or a mirror configured, what lands at railpack's path is a
// shim: railpack builds mise's environment from scratch, so this script is
// the only place those settings can be put back.
func TestEnsureMiseWritesShim(t *testing.T) {
	dir := t.TempDir()
	baked := filepath.Join(dir, "baked-mise")
	fakeMise(t, baked, rpmise.Version)
	installDir := filepath.Join(dir, "install")

	t.Setenv("HTTPS_PROXY", "http://proxy.example.com:3128")
	buildEnv := map[string]string{
		"MISE_NODE_MIRROR_URL": "https://mirror.example.com/node",
		"NPM_CONFIG_REGISTRY":  "https://mirror.example.com/npm",
	}
	if err := ensureMise(t.Context(), installDir, baked, buildEnv); err != nil {
		t.Fatalf("ensureMise: %v", err)
	}
	shim, err := os.ReadFile(misePath(installDir))
	if err != nil {
		t.Fatal(err)
	}
	got := string(shim)
	for _, want := range []string{
		"export HTTPS_PROXY='http://proxy.example.com:3128'",
		"export MISE_NODE_MIRROR_URL='https://mirror.example.com/node'",
		"exec \"" + baked + "\" \"$@\"",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the shim is missing %q:\n%s", want, got)
		}
	}
	// Only what mise reads — a package manager's registry is the build's
	// business, not the version resolver's.
	if strings.Contains(got, "NPM_CONFIG_REGISTRY") {
		t.Errorf("the shim carries build variables mise has no use for:\n%s", got)
	}
	// railpack still has to accept it.
	if _, err := rpmise.New(installDir); err != nil {
		t.Fatalf("railpack did not accept the shim: %v", err)
	}
}

// A value with a quote in it must not be able to end the export and run
// something else — these come from an install file, but a shim that can be
// broken by a password is a shim that will be.
func TestMiseShimQuoting(t *testing.T) {
	got := miseShim("/usr/local/lib/donkey/mise", map[string]string{
		"HTTPS_PROXY": "http://u:p'; touch /tmp/pwned; '@proxy:3128",
	})
	if strings.Contains(got, "touch /tmp/pwned;\n") {
		t.Errorf("a quote in a value escaped its export:\n%s", got)
	}
	if !strings.Contains(got, `'\''`) {
		t.Errorf("the quote was not escaped at all:\n%s", got)
	}
}

// Doctor has to say what will actually happen. With nothing provided that is
// "railpack downloads its own", which is information, not a failure.
func TestCheckMiseWithoutProvidedBinary(t *testing.T) {
	dir := t.TempDir()
	detail, err := checkMise(t.Context(), filepath.Join(dir, "install"), filepath.Join(dir, "absent"))
	if err != nil {
		t.Fatalf("no provided mise should not be an error: %v", err)
	}
	if !strings.Contains(detail, "github.com") || !strings.Contains(detail, bakedMiseEnv) {
		t.Errorf("the detail doesn't say what will happen or how to change it: %q", detail)
	}
}

// A binary that was provided and cannot be used is a different thing: someone
// set it, and it is wrong.
func TestCheckMiseRejectsUnstagedMismatch(t *testing.T) {
	dir := t.TempDir()
	baked := filepath.Join(dir, "baked-mise")
	fakeMise(t, baked, "1999.1.1")
	t.Setenv(bakedMiseEnv, baked)

	installDir := filepath.Join(dir, "install")
	if err := ensureMise(t.Context(), installDir, baked, nil); err == nil {
		t.Fatal("a provided mise of the wrong version was accepted")
	}
}
