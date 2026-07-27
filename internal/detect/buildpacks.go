package detect

import (
	"github.com/adrianliechti/donkey/internal/config"
)

// detectBuildpacks does no work and cannot fail — which is the point. Cloud
// Native Buildpacks decide what an app is *inside the builder image*, during
// the build, using toolchains that are already baked into it. Nothing about
// the app is worked out on the machine holding the source, so this path needs
// no egress at all: not on donkey-server, and not on the laptop running
// `donkey init`.
//
// The cost is that an app the buildpacks don't recognize is only found out at
// build time, and that there is no provider list to guess a port from — the
// CNB launcher's own convention (8080) stands in, and donkey.yaml's explicit
// port overrides it as always.
func detectBuildpacks(settings config.BuildSettings) *Detection {
	return &Detection{
		Strategy: config.BuildBuildpacks,
		Summary:  "Cloud Native Buildpacks (" + settings.BuildpacksBuilderImage() + ")",
	}
}
