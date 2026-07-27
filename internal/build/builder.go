// Package build turns source folders into pushed container images.
// Everything is embedded: BuildKit is driven over gRPC with the Go client,
// the buildkitd instance lives in a donkey-managed container on the local
// Docker daemon (reached via its API socket — never the docker CLI).
package build

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
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
	// Verify, when set, runs the built image and requires it to start before
	// the image is published. Nil skips the check entirely.
	Verify *VerifySpec
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

// SameRegistry reports whether an image reference lives under the registry
// prefix donkey builds into. A deploy that skips the build (`--no-build
// --tag`) must not be able to run an arbitrary image under an app's name:
// the app's env and addon secrets are handed to whatever starts.
func SameRegistry(registryURL, imageRef string) bool {
	prefix := strings.TrimSuffix(registryURL, "/") + "/"
	return strings.HasPrefix(imageRef, prefix)
}

// ImageTag composes the tag for a deploy:
// <registry>/<app>:<gitsha12>-<timestamp>, or timestamp-only when no sha is
// known (the CLI reads it from the local .git and sends it along — uploaded
// build contexts deliberately exclude .git). The timestamp gives every deploy
// its own image, so redeploying identical code still rolls the Deployment.
//
// Two deploys of one commit inside the same second land on the same tag and
// overwrite it, which costs nothing on an ordinary registry. Registries that
// refuse to overwrite a tag (ECR tag immutability, GHCR immutable tags, a
// Harbor rule) reject that push instead — `immutable` adds a short random
// suffix so no tag is ever written twice.
func ImageTag(registryURL, appName, sha string, now time.Time, immutable bool) string {
	parts := []string{now.UTC().Format("20060102150405")}
	if sha != "" {
		parts = append([]string{sha}, parts...)
	}
	if immutable {
		parts = append(parts, uniqueSuffix())
	}
	return fmt.Sprintf("%s/%s:%s", strings.TrimSuffix(registryURL, "/"), appName, strings.Join(parts, "-"))
}

// uniqueSuffix is deliberately short: four hex characters make a same-second
// collision vanishingly unlikely without making the tag unreadable.
func uniqueSuffix() string {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		// Still distinguishes two deploys that share a second.
		return strconv.FormatInt(int64(time.Now().Nanosecond()), 36)
	}
	return hex.EncodeToString(b)
}
