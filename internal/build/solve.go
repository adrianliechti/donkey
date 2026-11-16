package build

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"

	"github.com/adrianliechti/donkey/internal/config"
)

// BuildKitBuilder drives any BuildKit daemon over gRPC — for donkey-server
// that's the in-cluster buildkitd service.
type BuildKitBuilder struct {
	Registry config.Registry
	// RegistryMirrors are also needed on the client side because BuildKit
	// delegates token exchange to the solve session.
	RegistryMirrors map[string]string
	BuildkitAddr    string
	PackagesURL     string
	// Settings carries the environment every build is handed on top of the
	// app's own.
	Settings config.BuildSettings
}

func (b *BuildKitBuilder) Build(ctx context.Context, req Request) (*Result, error) {
	if b.BuildkitAddr == "" {
		return nil, fmt.Errorf("no build engine address configured")
	}
	c, err := bkclient.New(ctx, b.BuildkitAddr)
	if err != nil {
		return nil, fmt.Errorf("connecting to the build engine: %w", err)
	}
	defer c.Close()

	// A build that has to prove the image starts, or pass a vulnerability
	// scan, runs through the gateway instead: the frontend's result is
	// solved first, the scan and the probe run on it, and only then is the
	// image exported. Nothing that cannot start, and nothing the policy
	// refuses, is ever pushed under an app's name.
	if req.Verify != nil || req.Scan != nil {
		return b.buildVerified(ctx, c, req)
	}

	opt, err := b.solveOpt(req)
	if err != nil {
		return nil, err
	}

	var resp *bkclient.SolveResponse
	if err := b.withProgress(ctx, req, func(ctx context.Context, ch chan *bkclient.SolveStatus) error {
		var err error
		// No definition: every build donkey does now goes through the
		// Dockerfile frontend, which produces its own.
		resp, err = c.Solve(ctx, nil, *opt, ch)
		return err
	}); err != nil {
		return nil, err
	}
	return &Result{
		ImageRef: req.ImageRef,
		Digest:   resp.ExporterResponse["containerimage.digest"],
		LogPath:  req.Progress.LogPath,
	}, nil
}

// withProgress runs one solve while its progress stream is rendered into the
// build log (and, when someone is following, into their response). The solve
// closes the status channel itself.
func (b *BuildKitBuilder) withProgress(ctx context.Context, req Request, solve func(context.Context, chan *bkclient.SolveStatus) error) error {
	logFile, err := os.Create(req.Progress.LogPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	var progressOut io.Writer = logFile
	if req.Progress.Writer != nil {
		progressOut = io.MultiWriter(logFile, req.Progress.Writer)
	}

	ch := make(chan *bkclient.SolveStatus)
	eg, ectx := errgroup.WithContext(ctx)
	eg.Go(func() error { return solve(ectx, ch) })
	eg.Go(func() error {
		display, err := progressui.NewDisplay(progressOut, progressui.PlainMode)
		if err != nil {
			return err
		}
		_, err = display.UpdateFrom(ectx, ch)
		return err
	})
	if err := eg.Wait(); err != nil {
		return fmt.Errorf("the build failed: %w", err)
	}
	return nil
}

// solveOpt describes the build to BuildKit. Every build donkey does is a
// Dockerfile build — an app either brings its own, or the assistant writes it
// one — so the frontend is always dockerfile.v0.
func (b *BuildKitBuilder) solveOpt(req Request) (*bkclient.SolveOpt, error) {
	if req.Strategy != config.BuildDockerfile {
		return nil, fmt.Errorf("build strategy %q is not supported", req.Strategy)
	}
	auth, err := registryAuth(b.Registry, b.RegistryMirrors)
	if err != nil {
		return nil, err
	}
	opt := &bkclient.SolveOpt{
		Exports: []bkclient.ExportEntry{{
			Type: bkclient.ExporterImage,
			Attrs: map[string]string{
				"name": req.ImageRef,
				"push": strconv.FormatBool(req.Push),
			},
		}},
		Session: []session.Attachable{auth},
	}
	if b.Registry.Insecure {
		opt.Exports[0].Attrs["registry.insecure"] = "true"
	}

	contextFS, err := mirroredContextFS(req.ContextDir, b.packagesURL())
	if err != nil {
		return nil, err
	}

	// The Dockerfile path comes from the uploaded donkey.yaml and must stay
	// inside the uploaded context: its parent directory is exposed to the
	// frontend, and a "../…" path would expose (and read as the Dockerfile)
	// files of the server itself.
	dockerfilePath := filepath.Join(req.ContextDir, req.DockerfilePath)
	if rel, err := filepath.Rel(req.ContextDir, dockerfilePath); err != nil ||
		rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("build.dockerfile %q points outside the app folder", req.DockerfilePath)
	}
	dockerfileFS, err := packageDockerfileFS(dockerfilePath, slices.Sorted(maps.Keys(req.BuildEnv))...)
	if err != nil {
		return nil, err
	}
	opt.Frontend = "dockerfile.v0"
	opt.FrontendAttrs = map[string]string{
		"filename":           filepath.Base(dockerfilePath),
		"platform":           req.Platform,
		"image-resolve-mode": "pull",
	}
	// The installation's build environment sits under the app's own, and
	// BuildKit hands these to every RUN — so a mirrored package registry
	// reaches build steps donkey never sees.
	for k, v := range b.buildEnv(req.BuildEnv) {
		opt.FrontendAttrs["build-arg:"+k] = v
	}
	for k, v := range req.ImageLabels {
		opt.FrontendAttrs["label:"+k] = v
	}
	// Attestations are generated by the frontend in this same solve and
	// pushed next to the image, so every image donkey publishes carries a
	// standard SLSA provenance statement (and, opted in, an SBOM) that any
	// registry tool can read. See BuildSettings.Provenance for why "max"
	// (which embeds build-arg values) is never the default.
	if mode := b.Settings.ProvenanceMode(); mode != "" {
		opt.FrontendAttrs["attest:provenance"] = "mode=" + mode
	}
	if b.Settings.SBOM {
		opt.FrontendAttrs["attest:sbom"] = ""
	}
	opt.LocalMounts = map[string]fsutil.FS{
		"context":    contextFS,
		"dockerfile": dockerfileFS,
	}
	return opt, nil
}

// packagesURL is the package mirror's address as a build step reaches it.
func (b *BuildKitBuilder) packagesURL() string {
	if address := strings.TrimRight(b.PackagesURL, "/"); address != "" {
		return address
	}
	return "http://donkey-packages"
}

func (b *BuildKitBuilder) buildEnv(appEnv map[string]string) map[string]string {
	env := map[string]string{}
	for key, value := range b.Settings.MergeEnv(appEnv) {
		env[key] = value
	}
	// These names are platform-owned: their upstream side is configurable on
	// donkey-packages, while every build consistently sees only the internal
	// service and never an upstream credential.
	packagesURL := b.packagesURL()
	packagesHost := strings.TrimPrefix(strings.TrimPrefix(packagesURL, "http://"), "https://")
	for key, value := range map[string]string{
		"NPM_CONFIG_REGISTRY": packagesURL + "/npm/",
		// Corepack fetches the package manager a project names (pnpm, yarn)
		// and reads no npm setting for it; without this, `corepack enable`
		// in a Dockerfile asks registry.npmjs.org. No trailing slash: it
		// joins the name on with its own.
		"COREPACK_NPM_REGISTRY": packagesURL + "/npm",
		"PIP_INDEX_URL":         packagesURL + "/pypi/",
		"PIP_TRUSTED_HOST":      packagesHost,
		// uv reads no pip setting. Not UV_DEFAULT_INDEX, which `uv add`
		// writes into pyproject.toml. A uv.lock names the index it was
		// written against, and uv refuses a locked install against another
		// one — the build's copy of the lockfile names this one (see
		// mirroredContextFS).
		"UV_INDEX_URL": packagesURL + "/pypi/",
		"GOPROXY":      packagesURL + "/go/",
		// Maven 4 and the wrapper bootstrap honor these directly. Maven 3
		// builds use DONKEY_MAVEN_MIRROR in the settings.xml generated by
		// donkey's Dockerfile assistant.
		"DONKEY_MAVEN_MIRROR": packagesURL + "/maven/",
		"MAVEN_REPO_CENTRAL":  packagesURL + "/maven/",
		"MVNW_REPOURL":        packagesURL + "/maven/",
		// NuGet.Config expands environment variables in source values. A
		// donkey-specific name avoids RestoreSources overriding the config's
		// allowInsecureConnections attribute on modern NuGet clients.
		"DONKEY_NUGET_SOURCE": packagesURL + "/nuget/index.json",
		// A restricted build cannot reach sum.golang.org independently. The
		// configured module proxy remains responsible for serving verified
		// module content, as enterprise Go proxies already are.
		"GOSUMDB":   "off",
		"GONOSUMDB": "*",
	} {
		env[key] = value
	}
	// Empty values deliberately override app build.env when a mirror is off.
	for _, name := range []string{"DONKEY_DEBIAN_MIRROR", "DONKEY_ALPINE_MIRROR"} {
		env[name] = ""
	}
	if b.Settings.DebianMirror {
		env["DONKEY_DEBIAN_MIRROR"] = packagesURL + "/debian"
	}
	if b.Settings.AlpineMirror {
		env["DONKEY_ALPINE_MIRROR"] = packagesURL + "/alpine"
	}
	return env
}
