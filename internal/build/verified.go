package build

import (
	"context"
	"fmt"

	"github.com/containerd/platforms"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
)

// buildVerified builds the image, proves it starts, and only then lets it be
// exported.
//
// The ordering is the whole point, and it is why this runs through the
// gateway rather than a plain solve: a plain solve exports whatever the
// frontend produced, so a broken image would already be in the registry
// under the app's name by the time anything could object. Here the frontend's
// result is solved first, the probe is solved against it, and the result is
// only handed back for export once the probe has passed.
func (b *BuildKitBuilder) buildVerified(ctx context.Context, c *bkclient.Client, req Request) (*Result, error) {
	_, opt, err := b.solveOpt(ctx, req)
	if err != nil {
		return nil, err
	}
	platform, err := platforms.Parse(req.Platform)
	if err != nil {
		return nil, fmt.Errorf("platform %q: %w", req.Platform, err)
	}
	// c.Build takes the frontend through the gateway, so the frontend's own
	// name and options move off the SolveOpt and into the request below.
	frontend, frontendAttrs := opt.Frontend, opt.FrontendAttrs
	if frontend == "" {
		return nil, fmt.Errorf("startup verification only applies to Dockerfile builds")
	}
	opt.Frontend, opt.FrontendAttrs = "", nil

	buildFn := func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		res, err := gw.Solve(ctx, gateway.SolveRequest{
			Frontend:    frontend,
			FrontendOpt: frontendAttrs,
		})
		if err != nil {
			return nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}
		if ref == nil {
			return nil, fmt.Errorf("the build produced no image to verify")
		}
		cfg, err := imageConfigOf(res.Metadata)
		if err != nil {
			return nil, err
		}
		app, err := ref.ToState()
		if err != nil {
			return nil, err
		}
		spec := *req.Verify
		spec.Platform = platform
		def, err := verifyState(app, cfg, spec).Marshal(ctx, llb.Platform(platform))
		if err != nil {
			return nil, fmt.Errorf("preparing the startup check: %w", err)
		}
		// Evaluate, because nothing downstream consumes this: the result
		// handed back for export is the image itself, so without asking for
		// it the probe would never run.
		if _, err := gw.Solve(ctx, gateway.SolveRequest{
			Definition: def.ToPB(),
			Evaluate:   true,
		}); err != nil {
			return nil, fmt.Errorf("the image was built, but it doesn't start: %w", err)
		}
		return res, nil
	}

	var resp *bkclient.SolveResponse
	if err := b.withProgress(ctx, req, func(ctx context.Context, ch chan *bkclient.SolveStatus) error {
		var err error
		resp, err = c.Build(ctx, *opt, "donkey", buildFn, ch)
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
