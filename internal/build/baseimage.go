package build

import (
	"context"
	"fmt"
	"strings"

	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
)

// BaseImageInspector checks candidates without building or publishing an app.
// Keeping it optional lets builders without inspection still use final scans.
type BaseImageInspector interface {
	InspectBaseImage(context.Context, string, string, ScanSpec) (*BaseImage, error)
}

type BaseImage struct {
	Reference string            `json:"reference"` // digest of the image actually scanned
	Platform  string            `json:"platform"`
	Versions  map[string]string `json:"runtimeVersions,omitempty"`
	Scan      *ScanReport       `json:"-"`
}

func (b *BuildKitBuilder) InspectBaseImage(ctx context.Context, image, platform string, spec ScanSpec) (*BaseImage, error) {
	if b.BuildkitAddr == "" || spec.Matcher == nil {
		return nil, fmt.Errorf("base-image inspection requires a build engine and vulnerability matcher")
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference: %w", err)
	}
	image = reference.TagNameOnly(named).String()
	p, err := platforms.Parse(platform)
	if err != nil {
		return nil, err
	}
	auth, err := registryAuth(b.Registry, b.RegistryMirrors)
	if err != nil {
		return nil, err
	}
	c, err := bkclient.New(ctx, b.BuildkitAddr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ch := make(chan *bkclient.SolveStatus)
	go func() {
		for range ch {
		}
	}()
	var result *BaseImage
	// BuildKit may return a transported error instead of the callback's
	// error. Keep the scanner cause, as the final-image build does.
	var scanErr error
	_, err = c.Build(ctx, bkclient.SolveOpt{Session: []session.Attachable{auth}}, "donkey-base-image", func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		_, digest, raw, err := gw.ResolveImageConfig(ctx, image, sourceresolver.Opt{ImageOpt: &sourceresolver.ResolveImageOpt{
			Platform: &p, ResolveMode: llb.ResolveModeForcePull.String(),
		}})
		if err != nil {
			return nil, err
		}
		pinned, err := reference.WithDigest(named, digest)
		if err != nil {
			return nil, err
		}
		cfg, err := imageConfig(raw)
		if err != nil {
			return nil, err
		}
		// Inventory the exact digest resolved above, never a tag that could
		// move between inspection and scanning. The candidate is never
		// exported, and its entrypoint is never run.
		spec.Platform, spec.Secrets, spec.Judge = p, false, nil
		report, err := scan(ctx, gw, llb.Image(pinned.String(), llb.Platform(p)), spec)
		if err != nil {
			scanErr = &ScanError{Err: err}
			return nil, scanErr
		}
		result = &BaseImage{Reference: pinned.String(), Platform: platforms.Format(p), Versions: runtimeVersions(cfg.Config.Env), Scan: report}
		return gateway.NewResult(), nil
	}, ch)
	if err != nil {
		if scanErr != nil {
			return nil, scanErr
		}
		return nil, err
	}
	return result, nil
}

// Image environment can contain credentials; only the standard runtime
// version declarations are useful for choosing a compatible current tag.
func runtimeVersions(env []string) map[string]string {
	versions := map[string]string{}
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		switch key {
		case "NODE_VERSION", "PYTHON_VERSION", "GOLANG_VERSION", "RUBY_VERSION", "JAVA_VERSION", "DOTNET_VERSION", "ASPNET_VERSION":
			versions[key] = value
		}
	}
	return versions
}
