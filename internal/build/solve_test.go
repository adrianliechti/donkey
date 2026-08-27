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
		"NPM_CONFIG_REGISTRY": "http://donkey-packages/npm/",
		"PIP_INDEX_URL":       "http://donkey-packages/pypi/",
		"PIP_TRUSTED_HOST":    "donkey-packages",
		"GOPROXY":             "http://donkey-packages/go/",
		"GOSUMDB":             "off",
		"GONOSUMDB":           "*",
		"DONKEY_MAVEN_MIRROR": "http://donkey-packages/maven/",
		"MAVEN_REPO_CENTRAL":  "http://donkey-packages/maven/",
		"MVNW_REPOURL":        "http://donkey-packages/maven/",
		"DONKEY_NUGET_SOURCE": "http://donkey-packages/nuget/index.json",
		"SHARED":              "app",
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
	for _, name := range []string{"DONKEY_MAVEN_MIRROR", "MAVEN_REPO_CENTRAL", "MVNW_REPOURL", "DONKEY_NUGET_SOURCE"} {
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
