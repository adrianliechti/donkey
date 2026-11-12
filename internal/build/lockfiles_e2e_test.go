//go:build e2e

package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/packages"
	"github.com/adrianliechti/donkey/internal/testenv"
)

// Run without a Donkey installation:
//
// DONKEY_E2E_LOCKFILES=true go test -tags e2e -run '^TestLockfilesInstallThroughTheMirror$' -v -timeout 20m ./internal/build
//
// The test owns a disposable BuildKit container. Real package managers run
// their frozen installs from lockfiles written somewhere else — against the
// public registries, against a registry donkey was never told about, or
// through `donkey packages` on a developer's machine — in build steps whose
// only way out is donkey-packages: every other address is sent to a proxy
// that is not there. Docker and internet are needed.
func TestLockfilesInstallThroughTheMirror(t *testing.T) {
	if os.Getenv("DONKEY_E2E_LOCKFILES") != "true" {
		t.Skip("set DONKEY_E2E_LOCKFILES=true to run real frozen installs in Docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 18*time.Minute)
	defer cancel()
	addr, host := testenv.Buildkit(t, ctx)
	endpoint := testenv.PackageProxy(t, ctx, host, packages.Config{
		NPMUpstream: config.DefaultNPMUpstream, PyPIUpstream: config.DefaultPyPIUpstream,
		GoUpstream: config.DefaultGoUpstream, MavenUpstream: config.DefaultMavenUpstream, NuGetUpstream: config.DefaultNuGetUpstream,
	})
	builder := &BuildKitBuilder{BuildkitAddr: addr, PackagesURL: endpoint, Settings: config.BuildSettings{Provenance: "off"}}

	const uv = "ghcr.io/astral-sh/uv:python3.12-alpine"
	for _, tc := range []struct {
		name, fixture, base, install, installed string
		// direct are hosts the step may reach besides the mirror: the ones
		// an app's own configuration assigns a package to.
		direct string
	}{
		{name: "yarn1-public-registry", fixture: "yarn", base: "node:22-alpine", install: "yarn install --frozen-lockfile", installed: "test -d node_modules/is-odd"},
		{name: "yarn1-unknown-registry", fixture: "yarn-unknown", base: "node:22-alpine", install: "yarn install --frozen-lockfile", installed: "test -d node_modules/is-odd"},
		{name: "uv-frozen-public-index", fixture: "uv", base: uv, install: "uv sync --frozen", installed: ".venv/bin/python -c 'import idna'"},
		{name: "uv-locked-public-index", fixture: "uv", base: uv, install: "uv sync --locked", installed: ".venv/bin/python -c 'import idna'"},
		// One package locked in two versions: uv then names the index in the
		// dependency entries too, and refuses a lockfile where they disagree.
		{name: "uv-frozen-two-versions", fixture: "uv-forked", base: uv, install: "uv sync --frozen", installed: ".venv/bin/python -c 'import anyio, idna'"},
		{name: "uv-locked-two-versions", fixture: "uv-forked", base: uv, install: "uv sync --locked", installed: ".venv/bin/python -c 'import anyio, idna'"},
		// A dependency package.json names by URL: its name in the lockfile
		// has to stay what package.json says, only the download moves.
		{name: "yarn1-url-dependency", fixture: "yarn-url", base: "node:22-alpine", install: "yarn install --frozen-lockfile", installed: "test -d node_modules/is-odd"},
		{name: "uv-frozen-unknown-index", fixture: "uv-unknown", base: uv, install: "uv sync --frozen", installed: ".venv/bin/python -c 'import idna'"},
		{name: "uv-locked-unknown-index", fixture: "uv-unknown", base: uv, install: "uv sync --locked", installed: ".venv/bin/python -c 'import idna'"},
		// One package comes from an index the app names for it. That index
		// stays in the lockfile and is reached directly; the default index
		// beside it is the mirror.
		{name: "uv-locked-named-index", fixture: "uv-named", base: uv, install: "uv sync --locked",
			installed: `.venv/bin/python -c 'import idna, filelock' && grep -q 'registry = "https://download.pytorch.org/whl/cpu"' uv.lock && grep -q "registry = \"$UV_INDEX_URL\"" uv.lock`,
			direct:    ",download.pytorch.org,files.pythonhosted.org"},
		{name: "bun-developer-machine", fixture: "bun", base: "oven/bun:1-alpine", install: "bun install --frozen-lockfile", installed: "test -d node_modules/is-odd"},
		{name: "npm-developer-machine", fixture: "npm", base: "node:22-alpine", install: "npm ci", installed: "test -d node_modules/is-odd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.CopyFS(dir, os.DirFS(filepath.Join("testdata", "lockfiles", tc.fixture))); err != nil {
				t.Fatal(err)
			}
			// The first RUN proves the fence holds: a registry asked directly
			// does not answer. The install after it can only have come
			// through the mirror.
			recipe := fmt.Sprintf(`FROM %[1]s
WORKDIR /app
COPY . .
ENV HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 \
    NO_PROXY=%[2]s no_proxy=%[2]s UV_PYTHON_DOWNLOADS=never
RUN ! wget -q -T 10 -O /dev/null https://registry.npmjs.org/is-odd
RUN %[3]s && %[4]s
`, tc.base, host+tc.direct, tc.install, tc.installed)
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(recipe), 0o600); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(t.TempDir(), "build.log")
			_, err := builder.Build(ctx, Request{ContextDir: dir, DockerfilePath: "Dockerfile", Strategy: config.BuildDockerfile,
				ImageRef: "example.invalid/lockfiles:" + tc.name, Platform: "linux/" + runtime.GOARCH,
				Progress: ProgressSink{LogPath: logPath}})
			contents, _ := os.ReadFile(logPath)
			t.Log(string(contents))
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
