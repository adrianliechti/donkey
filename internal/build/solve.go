package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets/secretsprovider"
	"github.com/moby/buildkit/util/progress/progressui"
	rpbuildkit "github.com/railwayapp/railpack/buildkit"
	"github.com/tonistiigi/fsutil"
	"golang.org/x/sync/errgroup"

	"monobox.ch/donkey/internal/config"
)

// BuildKitBuilder drives any BuildKit daemon over gRPC — for donkey-server
// that's the in-cluster buildkitd service.
type BuildKitBuilder struct {
	Registry     config.Registry
	BuildkitAddr string
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

	def, opt, err := b.solveOpt(ctx, req)
	if err != nil {
		return nil, err
	}

	logFile, err := os.Create(req.Progress.LogPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	var progressOut io.Writer = logFile
	if req.Progress.Writer != nil {
		progressOut = io.MultiWriter(logFile, req.Progress.Writer)
	}

	ch := make(chan *bkclient.SolveStatus)
	eg, ectx := errgroup.WithContext(ctx)
	var resp *bkclient.SolveResponse
	eg.Go(func() error {
		var err error
		resp, err = c.Solve(ectx, def, *opt, ch)
		return err
	})
	eg.Go(func() error {
		display, err := progressui.NewDisplay(progressOut, progressui.PlainMode)
		if err != nil {
			return err
		}
		_, err = display.UpdateFrom(ectx, ch)
		return err
	})
	if err := eg.Wait(); err != nil {
		return nil, fmt.Errorf("the build failed: %w", err)
	}
	return &Result{
		ImageRef: req.ImageRef,
		Digest:   resp.ExporterResponse["containerimage.digest"],
		LogPath:  req.Progress.LogPath,
	}, nil
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

	switch req.Strategy {
	case config.BuildDockerfile:
		dockerfilePath := req.DockerfilePath
		if !filepath.IsAbs(dockerfilePath) {
			dockerfilePath = filepath.Join(req.ContextDir, dockerfilePath)
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
		for k, v := range req.BuildEnv {
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
			SecretsHash:   secretsHash(req.BuildEnv),
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
		for k, v := range req.BuildEnv {
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
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(h, "%s=%s\n", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
