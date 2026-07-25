// Package build turns source folders into pushed container images.
// Everything is embedded: BuildKit is driven over gRPC with the Go client,
// the buildkitd instance lives in a donkey-managed container on the local
// Docker daemon (reached via its API socket — never the docker CLI).
package build

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"monobox.ch/donkey/internal/config"
)

type Builder interface {
	Build(ctx context.Context, req Request) (*Result, error)
}

type Request struct {
	ContextDir string
	ImageRef   string // full ref including tag
	Platform   string
	Push       bool
	BuildEnv   map[string]string

	Strategy config.BuildStrategy
	// DockerfilePath is set for the dockerfile strategy (absolute or relative
	// to ContextDir).
	DockerfilePath string
	// RailpackPlan carries railpack's in-process plan for the railpack strategy.
	RailpackPlan *PlanResult

	// Progress receives the human-readable build progress stream (may be nil).
	Progress ProgressSink
}

type ProgressSink struct {
	// LogPath is where the full build log is written.
	LogPath string
	// Writer additionally receives the progress stream (e.g. the API
	// response the CLI is following). May be nil.
	Writer io.Writer
}

type Result struct {
	ImageRef string
	Digest   string
	LogPath  string
}

// ImageTag composes the unique tag for a deploy:
// <registry>/<app>:<gitsha12>-<timestamp>, or timestamp-only when no sha is
// known (the CLI reads it from the local .git and sends it along — uploaded
// build contexts deliberately exclude .git). Uniqueness per deploy
// guarantees a redeploy of identical code still rolls the Deployment.
func ImageTag(registryURL, appName, sha string, now time.Time) string {
	ts := now.UTC().Format("20060102150405")
	tag := ts
	if sha != "" {
		tag = fmt.Sprintf("%s-%s", sha, ts)
	}
	return fmt.Sprintf("%s/%s:%s", strings.TrimSuffix(registryURL, "/"), appName, tag)
}
