package build

import (
	rpgenerate "github.com/railwayapp/railpack/core/generate"
	rpplan "github.com/railwayapp/railpack/core/plan"

	"github.com/adrianliechti/donkey/internal/config"
)

// ConfigureRailpackImages points railpack's two images at this installation's
// own registry.
//
// railpack hard-codes ghcr.io references for the image it builds in and the
// image it deploys onto, derived from the mise version it pins. They are
// package variables rather than options, so this is where an installation
// that cannot reach ghcr.io redirects them — everything else about a railpack
// build already goes through the build engine, which has its mirrors.
//
// Process-wide, so it runs once at startup, before any build.
func ConfigureRailpackImages(settings config.InstallRailpack) {
	if settings.BuilderImage != "" {
		rpgenerate.RailpackBuilderImage = settings.BuilderImage
	}
	if settings.RuntimeImage != "" {
		rpplan.RailpackRuntimeImage = settings.RuntimeImage
	}
}

// RailpackImages reports the two images railpack builds will pull, after any
// override — what doctor checks and what an admin has to mirror.
func RailpackImages() (builder, runtime string) {
	return rpgenerate.RailpackBuilderImage, rpplan.RailpackRuntimeImage
}
