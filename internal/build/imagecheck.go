package build

import (
	"context"
	"fmt"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"

	"github.com/adrianliechti/donkey/internal/config"
)

// NormalizeImageRef expands a reference the way every registry client does:
// "heroku/builder:24" means "docker.io/heroku/builder:24", and a reference
// with no tag means ":latest".
//
// This is not cosmetic. llb.Image normalizes on its own, but the build
// engine's ResolveImageConfig does not — handed a short reference it reads
// the first path element as the registry host and goes looking for a host
// called "heroku". Every reference donkey resolves goes through here first.
func NormalizeImageRef(ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("%q is not a usable image reference: %w", ref, err)
	}
	return reference.TagNameOnly(named).String(), nil
}

// CheckImage asks the build engine to resolve an image reference — the same
// way a build resolves it, through the engine's own registry configuration.
//
// That is the point: it answers the question an admin behind a mirror
// actually has, which is not "can I reach this registry" but "can the thing
// that pulls it reach it". A doctor check that resolved the reference in
// donkey-server's own process would pass on a network where every build
// fails.
func CheckImage(ctx context.Context, buildkitAddr string, reg config.Registry, ref, platform string) error {
	if buildkitAddr == "" {
		return fmt.Errorf("no build engine address configured")
	}
	ref, err := NormalizeImageRef(ref)
	if err != nil {
		return err
	}
	p, err := platforms.Parse(platform)
	if err != nil {
		return fmt.Errorf("platform %q: %w", platform, err)
	}
	auth, err := registryAuth(reg)
	if err != nil {
		return err
	}
	c, err := bkclient.New(ctx, buildkitAddr)
	if err != nil {
		return fmt.Errorf("connecting to the build engine: %w", err)
	}
	defer c.Close()

	// The status channel has to be drained or the solve blocks on it; nothing
	// here has any progress worth showing.
	ch := make(chan *bkclient.SolveStatus)
	go func() {
		for range ch {
		}
	}()
	opt := bkclient.SolveOpt{Session: []session.Attachable{auth}}
	_, err = c.Build(ctx, opt, "donkey-doctor", func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		_, _, _, err := gw.ResolveImageConfig(ctx, ref, sourceresolver.Opt{
			LogName:  "resolve " + ref,
			ImageOpt: &sourceresolver.ResolveImageOpt{Platform: &p},
		})
		if err != nil {
			return nil, err
		}
		return gateway.NewResult(), nil
	}, ch)
	if err != nil {
		return fmt.Errorf("the build engine couldn't reach %s: %w", ref, err)
	}
	return nil
}
