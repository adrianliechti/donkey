package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	Registry     config.Registry
	BuildkitAddr string
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

	// A build that has to prove the image starts runs through the gateway
	// instead: the frontend's result is solved first, the probe runs on it,
	// and only then is the image exported. Nothing that cannot start is ever
	// pushed under an app's name.
	if req.Verify != nil {
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
	auth, err := registryAuth(b.Registry)
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

	contextFS, err := fsutil.NewFS(req.ContextDir)
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
	dockerfileFS, err := fsutil.NewFS(filepath.Dir(dockerfilePath))
	if err != nil {
		return nil, err
	}
	opt.Frontend = "dockerfile.v0"
	opt.FrontendAttrs = map[string]string{
		"filename": filepath.Base(dockerfilePath),
		"platform": req.Platform,
	}
	// The installation's build environment sits under the app's own, and
	// BuildKit hands these to every RUN — so a mirrored package registry
	// reaches build steps donkey never sees.
	for k, v := range b.Settings.MergeEnv(req.BuildEnv) {
		opt.FrontendAttrs["build-arg:"+k] = v
	}
	opt.LocalMounts = map[string]fsutil.FS{
		"context":    contextFS,
		"dockerfile": dockerfileFS,
	}
	return opt, nil
}
