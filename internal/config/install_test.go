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
  builder: buildpacks
  registryMirrors:
    docker.io: https://artifactory.example.com/docker-remote
  env:
    NPM_CONFIG_REGISTRY: https://artifactory.example.com/api/npm/npm
  proxy:
    https: http://proxy.example.com:3128
    noProxy: .example.com,10.0.0.0/8
  buildpacks:
    builderImage: artifactory.example.com/paketobuildpacks/builder-jammy-base:latest
    runImage: artifactory.example.com/paketobuildpacks/run-jammy-base:latest
    platformApi: "0.14"
  railpack:
    builderImage: artifactory.example.com/railwayapp/railpack-builder:mise-2026.7.11
    runtimeImage: artifactory.example.com/railwayapp/railpack-runtime:mise-2026.7.11
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
	if f.Build.Builder != BuildBuildpacks {
		t.Errorf("builder = %q, want buildpacks", f.Build.Builder)
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
	if f.Build.Buildpacks.PlatformAPI != "0.14" || f.Build.Buildpacks.RunImage == "" {
		t.Errorf("buildpacks = %+v", f.Build.Buildpacks)
	}
	if f.Build.Railpack.RuntimeImage == "" {
		t.Errorf("railpack = %+v", f.Build.Railpack)
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
	if again.Build.Builder != f.Build.Builder || again.Build.Buildpacks != f.Build.Buildpacks {
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
		{"railpack", InstallBuild{BuildSettings: BuildSettings{Builder: BuildRailpack}}, false},
		{"buildpacks", InstallBuild{BuildSettings: BuildSettings{Builder: BuildBuildpacks}}, false},
		// The builder is what an app with no Dockerfile gets, so these two
		// are not answers to the question.
		{"dockerfile", InstallBuild{BuildSettings: BuildSettings{Builder: BuildDockerfile}}, true},
		{"auto", InstallBuild{BuildSettings: BuildSettings{Builder: BuildAuto}}, true},
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
	if s.BuilderOrDefault() != BuildRailpack {
		t.Errorf("the default builder changed to %q", s.BuilderOrDefault())
	}
	if s.BuildpacksBuilderImage() != DefaultBuildpacksBuilderImage {
		t.Errorf("BuildpacksBuilderImage() = %q", s.BuildpacksBuilderImage())
	}
	s.Buildpacks.BuilderImage = "example.com/my/builder:1"
	if s.BuildpacksBuilderImage() != "example.com/my/builder:1" {
		t.Errorf("a configured builder image was ignored")
	}
}
