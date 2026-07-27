//go:build e2e

// An opt-in integration test for the buildpacks builder, against a real
// BuildKit daemon, a real registry and a real CNB builder image. Everything
// else about this path can be unit-tested; whether the CNB lifecycle accepts
// the graph donkey hands it cannot be.
//
// It needs:
//
//	DONKEY_E2E_BUILDKIT_ADDR  a BuildKit daemon, e.g. tcp://127.0.0.1:11234
//	DONKEY_E2E_REGISTRY       a registry prefix the daemon can push to, as
//	                          reachable FROM the build (e.g. reg:5000/donkey)
//	DONKEY_E2E_CNB_BUILDER    a CNB builder image (default heroku/builder:24,
//	                          which is one of the few published for arm64)
//	DONKEY_E2E_PLATFORM       build platform (default linux/arm64)
//	DONKEY_E2E_APP_DIR        the app to build (default testdata/express-api)
//
// Run: go test -tags e2e -count=1 -timeout 30m ./internal/build -run Buildpacks
package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestBuildpacksAgainstRealLifecycle(t *testing.T) {
	addr := os.Getenv("DONKEY_E2E_BUILDKIT_ADDR")
	registry := os.Getenv("DONKEY_E2E_REGISTRY")
	if addr == "" || registry == "" {
		t.Skip("DONKEY_E2E_BUILDKIT_ADDR and DONKEY_E2E_REGISTRY not set")
	}
	builderImage := os.Getenv("DONKEY_E2E_CNB_BUILDER")
	if builderImage == "" {
		builderImage = "heroku/builder:24"
	}
	platform := os.Getenv("DONKEY_E2E_PLATFORM")
	if platform == "" {
		platform = "linux/arm64"
	}

	b := &BuildKitBuilder{
		// Insecure: the test registry speaks plain http, which the lifecycle
		// — not BuildKit — has to be told about.
		Registry:     config.Registry{URL: registry, Insecure: true},
		BuildkitAddr: addr,
		Settings: config.BuildSettings{
			Builder:    config.BuildBuildpacks,
			Buildpacks: config.InstallBuildpacks{BuilderImage: builderImage},
		},
	}

	appDir := os.Getenv("DONKEY_E2E_APP_DIR")
	if appDir == "" {
		appDir = filepath.Join("..", "..", "testdata", "express-api")
	}

	logPath := filepath.Join(t.TempDir(), "build.log")
	res, err := b.Build(t.Context(), Request{
		ContextDir: appDir,
		ImageRef:   strings.TrimSuffix(registry, "/") + "/e2e-buildpacks:test",
		Platform:   platform,
		Push:       true,
		Strategy:   config.BuildBuildpacks,
		Progress:   ProgressSink{LogPath: logPath, Writer: os.Stderr},
	})
	if err != nil {
		if log, rerr := os.ReadFile(logPath); rerr == nil {
			t.Logf("build log:\n%s", log)
		}
		t.Fatalf("the buildpacks build failed: %v", err)
	}
	// The digest comes from the lifecycle's own report — with no image
	// exporter in play there is nothing else to read it from.
	if !strings.HasPrefix(res.Digest, "sha256:") {
		t.Errorf("digest = %q, want a sha256 digest from the lifecycle's report", res.Digest)
	}
	t.Logf("built and pushed %s at %s", res.ImageRef, res.Digest)
}
