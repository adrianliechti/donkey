package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	rpbuildkit "github.com/railwayapp/railpack/buildkit"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"

	"github.com/adrianliechti/donkey/internal/config"
)

// BuildKitBuilder drives any BuildKit daemon over gRPC — for donkey-server
// that's the in-cluster buildkitd service.
type BuildKitBuilder struct {
	Registry     config.Registry
	BuildkitAddr string
	// Settings is the installation's build configuration: the images the
	// buildpacks builder runs, and the environment every build is handed on
	// top of the app's own.
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

	def, opt, err := b.solveOpt(ctx, req)
	if err != nil {
		return nil, err
	}

	var resp *bkclient.SolveResponse
	if err := b.withProgress(ctx, req, func(ctx context.Context, ch chan *bkclient.SolveStatus) error {
		var err error
		resp, err = c.Solve(ctx, def, *opt, ch)
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

func (b *BuildKitBuilder) solveOpt(ctx context.Context, req Request) (*llb.Definition, *bkclient.SolveOpt, error) {
	auth, err := registryAuth(b.Registry)
	if err != nil {
		return nil, nil, err
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
		return nil, nil, err
	}

	// The installation's build environment sits under the app's own. This is
	// the one place layer #3 of the mirroring problem is solved for every
	// builder: BuildKit hands these to every RUN, so a mirrored package
	// registry reaches build steps donkey never sees.
	buildEnv := b.Settings.MergeEnv(req.BuildEnv)

	switch req.Strategy {
	case config.BuildDockerfile:
		// The Dockerfile path comes from the uploaded donkey.yaml and must
		// stay inside the uploaded context: its parent directory is exposed
		// to the frontend, and a "../…" path would expose (and read as the
		// Dockerfile) files of the server itself.
		dockerfilePath := filepath.Join(req.ContextDir, req.DockerfilePath)
		if rel, err := filepath.Rel(req.ContextDir, dockerfilePath); err != nil ||
			rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, nil, fmt.Errorf("build.dockerfile %q points outside the app folder", req.DockerfilePath)
		}
		dockerfileFS, err := fsutil.NewFS(filepath.Dir(dockerfilePath))
		if err != nil {
			return nil, nil, err
		}
		opt.Frontend = "dockerfile.v0"
		opt.FrontendAttrs = map[string]string{
			"filename": filepath.Base(dockerfilePath),
			"platform": req.Platform,
		}
		for k, v := range buildEnv {
			opt.FrontendAttrs["build-arg:"+k] = v
		}
		opt.LocalMounts = map[string]fsutil.FS{
			"context":    contextFS,
			"dockerfile": dockerfileFS,
		}
		return nil, opt, nil

	case config.BuildRailpack:
		if req.RailpackPlan == nil || req.RailpackPlan.Result == nil {
			return nil, nil, fmt.Errorf("railpack build requested without a plan")
		}
		platform, err := rpbuildkit.ParsePlatformWithDefaults(req.Platform)
		if err != nil {
			return nil, nil, fmt.Errorf("platform %q: %w", req.Platform, err)
		}
		state, image, err := rpbuildkit.ConvertPlanToLLB(req.RailpackPlan.Result.Plan, rpbuildkit.ConvertPlanOptions{
			BuildPlatform: platform,
			SecretsHash:   secretsHash(buildEnv),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("preparing the build steps: %w", err)
		}
		imageBytes, err := json.Marshal(image)
		if err != nil {
			return nil, nil, err
		}
		def, err := state.Marshal(ctx, llb.Platform(platform))
		if err != nil {
			return nil, nil, fmt.Errorf("preparing the build steps: %w", err)
		}
		opt.Exports[0].Attrs["containerimage.config"] = string(imageBytes)
		opt.LocalMounts = map[string]fsutil.FS{"context": contextFS}
		secretsMap := map[string][]byte{}
		for k, v := range buildEnv {
			secretsMap[k] = []byte(v)
		}
		opt.Session = append(opt.Session, secretsprovider.FromMap(secretsMap))
		return def, opt, nil

	default:
		return nil, nil, fmt.Errorf("build strategy %q is not supported yet", req.Strategy)
	}
}

// secretsHash invalidates cached layers when build env values change.
func secretsHash(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(env)) {
		fmt.Fprintf(h, "%s=%s\n", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
