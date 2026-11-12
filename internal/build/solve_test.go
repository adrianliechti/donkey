package build

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestBuildEnvAlwaysAddsInternalPackageSettings(t *testing.T) {
	builder := BuildKitBuilder{Settings: config.BuildSettings{Env: map[string]string{
		"SHARED":  "installation",
		"GOPROXY": "http://operator-bypass/go/",
	}}}

	got := builder.buildEnv(map[string]string{"SHARED": "app"})
	want := map[string]string{
		"NPM_CONFIG_REGISTRY":  "http://donkey-packages/npm/",
		"PIP_INDEX_URL":        "http://donkey-packages/pypi/",
		"PIP_TRUSTED_HOST":     "donkey-packages",
		"UV_INDEX_URL":         "http://donkey-packages/pypi/",
		"GOPROXY":              "http://donkey-packages/go/",
		"GOSUMDB":              "off",
		"GONOSUMDB":            "*",
		"DONKEY_MAVEN_MIRROR":  "http://donkey-packages/maven/",
		"MAVEN_REPO_CENTRAL":   "http://donkey-packages/maven/",
		"MVNW_REPOURL":         "http://donkey-packages/maven/",
		"DONKEY_NUGET_SOURCE":  "http://donkey-packages/nuget/index.json",
		"DONKEY_DEBIAN_MIRROR": "",
		"DONKEY_ALPINE_MIRROR": "",
		"SHARED":               "app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildEnv() = %#v, want %#v", got, want)
	}
}

func TestBuildEnvUsesDockerPackagesURL(t *testing.T) {
	builder := BuildKitBuilder{PackagesURL: "http://donkey-packages:8090/"}
	got := builder.buildEnv(nil)
	for key, want := range map[string]string{
		"NPM_CONFIG_REGISTRY": "http://donkey-packages:8090/npm/",
		"PIP_INDEX_URL":       "http://donkey-packages:8090/pypi/",
		"PIP_TRUSTED_HOST":    "donkey-packages:8090",
		"UV_INDEX_URL":        "http://donkey-packages:8090/pypi/",
		"GOPROXY":             "http://donkey-packages:8090/go/",
		"DONKEY_MAVEN_MIRROR": "http://donkey-packages:8090/maven/",
		"DONKEY_NUGET_SOURCE": "http://donkey-packages:8090/nuget/index.json",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func TestPackageDockerfileFSReportsTransformedSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(path, []byte("FROM busybox:1.37\nRUN true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay, err := packageDockerfileFS(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := overlay.Open("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}

	var size int64 = -1
	if err := overlay.Walk(context.Background(), string(filepath.Separator), func(path string, entry os.DirEntry, err error) error {
		if err != nil || filepath.Clean(path) != "Dockerfile" {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			size = info.Size()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if size != int64(len(contents)) {
		t.Fatalf("walked Dockerfile size = %d, transformed contents = %d", size, len(contents))
	}
}

func TestInjectPackageBuildArgsIntoEveryStage(t *testing.T) {
	source := []byte("# syntax=docker/dockerfile:1\nARG NODE_VERSION=22\nFROM node:${NODE_VERSION} AS build\nRUN npm ci\nFROM node:${NODE_VERSION} \\\n  AS runtime\nCMD [\"node\", \"server.js\"]")
	got, err := injectPackageBuildArgs(source)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if count := strings.Count(text, "ARG NPM_CONFIG_REGISTRY\n"); count != 2 {
		t.Fatalf("package arguments were injected into %d stages, want 2:\n%s", count, text)
	}
	for _, name := range []string{"DONKEY_MAVEN_MIRROR", "MAVEN_REPO_CENTRAL", "MVNW_REPOURL", "DONKEY_NUGET_SOURCE", "DONKEY_DEBIAN_MIRROR", "DONKEY_ALPINE_MIRROR"} {
		if count := strings.Count(text, "ARG "+name+"\n"); count != 2 {
			t.Errorf("%s was injected into %d stages, want 2", name, count)
		}
	}
	if !strings.HasPrefix(text, "# syntax=docker/dockerfile:1\nARG NODE_VERSION=22\nFROM node:${NODE_VERSION} AS build\nARG NPM_CONFIG_REGISTRY\n") {
		t.Fatalf("parser directive or global arguments moved:\n%s", text)
	}
	if strings.Contains(string(source), "NPM_CONFIG_REGISTRY") {
		t.Fatal("the source Dockerfile was modified")
	}
}

func TestBrowserURLBuildArgsReachEveryStage(t *testing.T) {
	source := []byte("FROM node:22 AS build\nRUN npm run build\nFROM nginx:alpine\nCOPY --from=build /app/dist /usr/share/nginx/html\n")
	got, err := injectPackageBuildArgs(source, "VITE_API_URL", "NEXT_PUBLIC_API_URL")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"VITE_API_URL", "NEXT_PUBLIC_API_URL"} {
		if strings.Count(string(got), "ARG "+key+"\n") != 2 {
			t.Fatalf("missing browser build variable %s:\n%s", key, got)
		}
	}
	if _, err := injectPackageBuildArgs(source, "X\nRUN injected"); err == nil {
		t.Fatal("invalid build variable accepted")
	}
}

// Every image donkey publishes carries standard metadata: SLSA provenance by
// default (min — max would embed build-arg values, which routinely hold
// credentials), an SBOM only when the installation opted in, and OCI labels.
func TestSolveOptAttestationsAndLabels(t *testing.T) {
	base := func(settings config.BuildSettings) map[string]string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		b := &BuildKitBuilder{Registry: config.Registry{URL: "reg.example.com/donkey"}, Settings: settings}
		opt, err := b.solveOpt(Request{
			ContextDir: dir, ImageRef: "reg.example.com/donkey/web:1",
			Platform: "linux/amd64", Strategy: config.BuildDockerfile, DockerfilePath: "Dockerfile",
			ImageLabels: map[string]string{"org.opencontainers.image.revision": "abc123"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return opt.FrontendAttrs
	}

	attrs := base(config.BuildSettings{})
	if attrs["image-resolve-mode"] != "pull" {
		t.Fatal("build may silently reuse an outdated local base image")
	}
	if attrs["attest:provenance"] != "mode=min" {
		t.Errorf("provenance default = %q, want mode=min", attrs["attest:provenance"])
	}
	if _, ok := attrs["attest:sbom"]; ok {
		t.Error("SBOM must stay opt-in")
	}
	if attrs["label:org.opencontainers.image.revision"] != "abc123" {
		t.Error("OCI image labels should reach the frontend")
	}

	attrs = base(config.BuildSettings{Provenance: "off", SBOM: true})
	if _, ok := attrs["attest:provenance"]; ok {
		t.Error("provenance off must render no attestation attr")
	}
	if got, ok := attrs["attest:sbom"]; !ok || got != "" {
		t.Errorf("SBOM opt-in should render the attr, got %q/%v", got, ok)
	}
}

func TestOSPackageMirrorBuildEnvironment(t *testing.T) {
	appEnv := map[string]string{
		"DONKEY_DEBIAN_MIRROR": "https://bypass.example/debian",
		"DONKEY_ALPINE_MIRROR": "https://bypass.example/alpine",
	}
	for _, enabled := range []bool{false, true} {
		b := BuildKitBuilder{PackagesURL: "http://172.31.250.10:8090/", Settings: config.BuildSettings{
			DebianMirror: enabled, AlpineMirror: enabled,
		}}
		got := b.buildEnv(appEnv)
		for name, value := range map[string]string{
			"DONKEY_DEBIAN_MIRROR": "http://172.31.250.10:8090/debian",
			"DONKEY_ALPINE_MIRROR": "http://172.31.250.10:8090/alpine",
		} {
			if !enabled {
				value = ""
			}
			if got[name] != value {
				t.Errorf("enabled=%v: %s = %q, want %q", enabled, name, got[name], value)
			}
		}
	}
}
