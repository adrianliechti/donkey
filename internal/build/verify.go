package build

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/buildkit/client/llb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Startup verification runs a freshly built image before publishing it. It
// tests one thing: that the app does not fall over on startup — the failure a
// generated Dockerfile actually produces, and the one nobody can diagnose
// from a build log, because the build succeeded.
//
// It does NOT insist the app serve traffic: an app whose first act is to open
// its database would fail that in a sandbox with no database, and be "fixed"
// by a model with nothing to fix. A process still running when the window
// closes passes; only one that exits fails.
//
// VerifyNeverListened is the outcome that is neither: up, but never opened its
// port. Let through — a sandbox is not the cluster — but said out loud,
// because the usual cause is an app listening somewhere else.
const VerifyNeverListened = "[verify] the app never listened on the port donkey will deploy it with"

const (
	// Mounted beside the app, not expected inside it: the app's image may have
	// no shell, and adding one would change the thing under test.
	//
	// uclibc because it is the STATIC build. A dynamically linked busybox
	// carries its loader path (/lib/ld-*.so) inside the ELF, resolved against
	// the app's filesystem rather than the mount — so stock busybox:1.37 fails
	// to exec in any image whose libc differs, which is most of them.
	verifyBusyboxImage = "docker.io/library/busybox:1.37-uclibc"
	// A mount brings the image's whole root, so busybox lands at its own
	// /bin/busybox rather than at the mount point.
	verifyToolsDir = "/donkey-verify"
	verifyBusybox  = verifyToolsDir + "/bin/busybox"
)

// VerifySpec is what a build should check about the image it produced.
type VerifySpec struct {
	Port    int
	Seconds int
	// Env is only what donkey sets itself: the app's own secrets are not
	// needed to find out whether it starts, and a sandbox is not where they
	// belong.
	Env map[string]string
	// Platform must be explicit: llb.Image otherwise resolves against the
	// platform of whatever built the graph — donkey-server, which on a
	// developer's machine is not even Linux.
	Platform ocispecs.Platform
}

// verifyState starts the app the way its own image says to, and waits for a
// listening port or the process falling over.
func verifyState(app llb.State, cfg ocispecs.Image, spec VerifySpec) llb.State {
	st := app
	for _, kv := range cfg.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			st = st.AddEnv(k, v)
		}
	}
	// PORT is the contract donkey deploys with, so it is what the check waits on.
	st = st.AddEnv("PORT", strconv.Itoa(spec.Port))
	for k, v := range spec.Env {
		st = st.AddEnv(k, v)
	}
	if cfg.Config.WorkingDir != "" {
		st = st.Dir(cfg.Config.WorkingDir)
	}
	if cfg.Config.User != "" {
		st = st.User(cfg.Config.User)
	}
	return st.Run(
		llb.Args([]string{verifyBusybox, "sh", "-c", verifyScript(cfg, spec)}),
		llb.WithCustomName("[verify] start the app and wait for it"),
		llb.AddMount(verifyToolsDir,
			llb.Image(verifyBusyboxImage, llb.Platform(spec.Platform)), llb.Readonly),
	).Root()
}

// verifyScript is written against busybox's shell, the one thing mounted —
// nothing is assumed about the app's image beyond running its own entrypoint.
func verifyScript(cfg ocispecs.Image, spec VerifySpec) string {
	bb := verifyBusybox
	var b strings.Builder
	// /tmp because the probe runs as whatever user the image names, and an
	// unprivileged one cannot write to /. A failed redirect would take the
	// check down and read as "the app exited", which is a lie.
	b.WriteString("set -u\n")
	fmt.Fprintf(&b, "%s >/tmp/donkey-app.log 2>&1 &\n", startCommand(cfg))
	b.WriteString("app=$!\n")
	fmt.Fprintf(&b, "i=0\nwhile [ $i -lt %d ]; do\n", spec.Seconds)
	// The log is the only thing that can explain the exit, so it goes to the
	// build output that the person (and the assistant) reads.
	b.WriteString("  if ! kill -0 $app 2>/dev/null; then\n")
	b.WriteString("    echo '[verify] the app exited instead of starting up:'\n")
	fmt.Fprintf(&b, "    %s cat /tmp/donkey-app.log\n", bb)
	b.WriteString("    exit 1\n  fi\n")
	fmt.Fprintf(&b, "  if %s nc -z 127.0.0.1 %d 2>/dev/null; then\n", bb, spec.Port)
	fmt.Fprintf(&b, "    echo '[verify] the app is listening on %d'\n", spec.Port)
	b.WriteString("    exit 0\n  fi\n")
	fmt.Fprintf(&b, "  %s sleep 1\n  i=$((i+1))\ndone\n", bb)
	// Running but not listening: an app waiting for something the sandbox
	// lacks, not a broken image. The marker is what donkey warns on later —
	// passing silently is how a wrong port reaches a rollout timeout.
	fmt.Fprintf(&b, "echo '%s after %ds — leaving it to the cluster'\n", VerifyNeverListened, spec.Seconds)
	fmt.Fprintf(&b, "%s cat /tmp/donkey-app.log\n", bb)
	b.WriteString("exit 0\n")
	return b.String()
}

// startCommand is the image's own entrypoint and command, as one shell line.
func startCommand(cfg ocispecs.Image) string {
	argv := append(append([]string{}, cfg.Config.Entrypoint...), cfg.Config.Cmd...)
	if len(argv) == 0 {
		// A clearer answer than whatever a shell does with an empty line.
		return "echo '[verify] the image names no command to run (no ENTRYPOINT and no CMD)'; exit 1"
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// imageConfig reads the frontend's config for the built image.
func imageConfig(raw []byte) (ocispecs.Image, error) {
	var cfg ocispecs.Image
	if len(raw) == 0 {
		return cfg, fmt.Errorf("the build produced no image configuration to verify")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("the built image's configuration couldn't be read: %w", err)
	}
	return cfg, nil
}
