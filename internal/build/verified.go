package build

import (
	"context"
	"fmt"

	"github.com/containerd/platforms"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// buildVerified builds the image, scans it, proves it starts, and only then
// exports it. The ordering is why this runs through the gateway rather than
// a plain solve: a plain solve exports whatever the frontend produced, so a
// broken or refused image would already be in the registry before anything
// could object.
func (b *BuildKitBuilder) buildVerified(ctx context.Context, c *bkclient.Client, req Request) (*Result, error) {
	opt, err := b.solveOpt(req)
	if err != nil {
		return nil, err
	}
	platform, err := platforms.Parse(req.Platform)
	if err != nil {
		return nil, fmt.Errorf("platform %q: %w", req.Platform, err)
	}
	// c.Build drives the frontend through the gateway, so its name and options
	// move off the SolveOpt and into the request below.
	frontend, frontendAttrs := opt.Frontend, opt.FrontendAttrs
	if frontend == "" {
		return nil, fmt.Errorf("startup verification only applies to Dockerfile builds")
	}
	opt.Frontend, opt.FrontendAttrs = "", nil

	// What the gateway function learns is kept here: BuildKit wraps the
	// error it returns, and which side's error wins the race is not
	// something a caller should have to unwrap.
	var report *ScanReport
	var scanErr error
	buildFn := func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		res, err := gw.Solve(ctx, gateway.SolveRequest{
			Frontend:    frontend,
			FrontendOpt: frontendAttrs,
			// Finish the app build before starting the scanner, so a lazy
			// build failure cannot be mistaken for a scanner outage.
			Evaluate: true,
		})
		if err != nil {
			return nil, err
		}
		ref, cfg, err := builtImage(res, platform)
		if err != nil {
			return nil, err
		}
		app, err := ref.ToState()
		if err != nil {
			return nil, err
		}
		// The scan comes first: its verdict is deterministic and cheap next
		// to waiting for an app to come up, so a refused image fails fast.
		if req.Scan != nil {
			spec := *req.Scan
			spec.Platform = platform
			report, err = scan(ctx, gw, app, spec)
			if err != nil {
				scanErr = &ScanError{Err: err}
				return nil, scanErr
			}
			if spec.Judge != nil {
				if err := spec.Judge(ctx, report); err != nil {
					scanErr = err
					return nil, err
				}
			}
		}
		if req.Verify != nil {
			spec := *req.Verify
			spec.Platform = platform
			def, err := verifyState(app, cfg, spec).Marshal(ctx, llb.Platform(platform))
			if err != nil {
				return nil, fmt.Errorf("preparing the startup check: %w", err)
			}
			// Evaluate: nothing downstream consumes this, so without asking
			// the probe would never run.
			if _, err := gw.Solve(ctx, gateway.SolveRequest{
				Definition: def.ToPB(),
				Evaluate:   true,
			}); err != nil {
				return nil, fmt.Errorf("the image was built, but it doesn't start: %w", err)
			}
		}
		return res, nil
	}

	var resp *bkclient.SolveResponse
	if err := b.withProgress(ctx, req, func(ctx context.Context, ch chan *bkclient.SolveStatus) error {
		var err error
		resp, err = c.Build(ctx, *opt, "donkey", buildFn, ch)
		return err
	}); err != nil {
		if scanErr != nil {
			return nil, scanErr
		}
		return nil, err
	}
	return &Result{
		ImageRef: req.ImageRef,
		Digest:   resp.ExporterResponse["containerimage.digest"],
		LogPath:  req.Progress.LogPath,
		Scan:     report,
	}, nil
}

// builtImage picks the built image's ref and configuration out of a frontend
// result. With attestations enabled the frontend answers with a multi-ref
// result — the image keyed by its platform, attestations alongside — where
// SingleRef would refuse the very build it just made.
func builtImage(res *gateway.Result, platform ocispecs.Platform) (gateway.Reference, ocispecs.Image, error) {
	var cfg ocispecs.Image
	if len(res.Refs) == 0 {
		ref, err := res.SingleRef()
		if err != nil {
			return nil, cfg, err
		}
		if ref == nil {
			return nil, cfg, fmt.Errorf("the build produced no image to verify")
		}
		cfg, err = imageConfig(res.Metadata["containerimage.config"])
		return ref, cfg, err
	}
	id := platforms.Format(platforms.Normalize(platform))
	ref, ok := res.Refs[id]
	if !ok && len(res.Refs) == 1 {
		for only, r := range res.Refs {
			id, ref, ok = only, r, true
		}
	}
	if !ok || ref == nil {
		return nil, cfg, fmt.Errorf("the build produced no image for platform %s", id)
	}
	cfg, err := imageConfig(res.Metadata["containerimage.config/"+id])
	return ref, cfg, err
}
