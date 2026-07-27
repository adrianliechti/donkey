package config

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// The build section reaches donkey-server as BuildSettings, embedded inline
// in InstallBuild — so it has to read back as one flat block, not as a
// nested "buildSettings" key nobody writes.
func TestInstallFileBuildRoundTrip(t *testing.T) {
	const in = `
image: ghcr.io/example/donkey-server:latest
domain: example.com
registry:
  url: registry.example.com/donkey
build:
  registryMirrors:
    docker.io: https://artifactory.example.com/docker-remote
  env:
    NPM_CONFIG_REGISTRY: https://artifactory.example.com/api/npm/npm
  proxy:
    https: http://proxy.example.com:3128
    noProxy: .example.com,10.0.0.0/8
  verify: false
  verifySeconds: 90
  attempts: 2
`
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	// UnmarshalStrict: an unknown key here is a typo an admin has to hear
	// about, so every field below has to actually be spelled that way.
	f, err := LoadInstallFile(path)
	if err != nil {
		t.Fatalf("LoadInstallFile: %v", err)
	}
	if got := f.Build.RegistryMirrors["docker.io"]; got != "https://artifactory.example.com/docker-remote" {
		t.Errorf("registryMirrors[docker.io] = %q", got)
	}
	if got := f.Build.Env["NPM_CONFIG_REGISTRY"]; got != "https://artifactory.example.com/api/npm/npm" {
		t.Errorf("env[NPM_CONFIG_REGISTRY] = %q", got)
	}
	if f.Build.Proxy.HTTPS != "http://proxy.example.com:3128" || f.Build.Proxy.NoProxy != ".example.com,10.0.0.0/8" {
		t.Errorf("proxy = %+v", f.Build.Proxy)
	}
	if f.Build.VerifyStartup() || f.Build.VerifyWindow() != 90 || f.Build.BuildAttempts() != 2 {
		t.Errorf("builder settings did not survive: %+v", f.Build.BuildSettings)
	}

	// And back out again in the same shape.
	out, err := yaml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var again InstallFile
	if err := yaml.UnmarshalStrict(out, &again); err != nil {
		t.Fatalf("what donkey writes, donkey cannot read: %v\n%s", err, out)
	}
	if again.Build.VerifyWindow() != f.Build.VerifyWindow() {
		t.Errorf("the build section did not survive the round trip:\n%s", out)
	}
}

func TestInstallBuildValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   InstallBuild
		wantErr bool
	}{
		{"empty", InstallBuild{}, false},
		{"negative attempts", InstallBuild{BuildSettings: BuildSettings{Attempts: -1}}, true},
		{"negative verify seconds", InstallBuild{BuildSettings: BuildSettings{VerifySeconds: -1}}, true},
		{"mirror with no host", InstallBuild{RegistryMirrors: map[string]string{"": "https://mirror"}}, true},
		{"mirror with no address", InstallBuild{RegistryMirrors: map[string]string{"docker.io": ""}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.build.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// An installation's build variables are the fallback; an app that pins its
// own has a reason to.
func TestBuildSettingsMergeEnv(t *testing.T) {
	settings := BuildSettings{Env: map[string]string{
		"NPM_CONFIG_REGISTRY": "https://mirror.example.com/npm",
		"GOPROXY":             "https://mirror.example.com/go",
	}}
	got := settings.MergeEnv(map[string]string{
		"GOPROXY":  "direct",
		"NODE_ENV": "production",
	})
	for k, want := range map[string]string{
		"NPM_CONFIG_REGISTRY": "https://mirror.example.com/npm",
		"GOPROXY":             "direct",
		"NODE_ENV":            "production",
	} {
		if got[k] != want {
			t.Errorf("merged[%s] = %q, want %q", k, got[k], want)
		}
	}
	// The installation's own map must not be edited by a merge — the next
	// app's build reads it too.
	if settings.Env["GOPROXY"] != "https://mirror.example.com/go" {
		t.Error("merging an app's variables changed the installation's")
	}
	if got := settings.MergeEnv(nil); got["GOPROXY"] != "https://mirror.example.com/go" {
		t.Errorf("an app with no variables of its own lost the installation's: %v", got)
	}
}

func TestBuildSettingsDefaults(t *testing.T) {
	var s BuildSettings
	// Startup verification is on unless an installation says otherwise —
	// an unset pointer must not read as "off".
	if !s.VerifyStartup() {
		t.Error("startup verification should default to on")
	}
	if s.VerifyWindow() != DefaultVerifySeconds || s.BuildAttempts() != DefaultAttempts {
		t.Errorf("defaults not applied: %ds, %d attempts", s.VerifyWindow(), s.BuildAttempts())
	}
	s.Verify = new(false)
	if s.VerifyStartup() {
		t.Error("an explicit verify: false was ignored")
	}
}
