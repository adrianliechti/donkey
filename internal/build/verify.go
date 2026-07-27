package build

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/buildkit/client/llb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Startup verification runs a freshly built image before it is published, and
// fails the build if what came out cannot start. It is the check a build is
// uniquely placed to make: everything else donkey knows about an app it
// learns by reading it, but whether the image runs is only answerable by
// running it.
//
// The probe deliberately tests one thing — that the app does not fall over on
// startup. That is the failure a generated Dockerfile actually produces: a
// missing interpreter, an entrypoint that is not executable, a dependency
// that was never installed. It is also the failure a person cannot diagnose
// from a build log, because the build succeeded.
//
// What it does NOT do is insist the app serve traffic. An app whose first act
// is to open its database would fail that, in a sandbox with no database, and
// be "fixed" by a model that had nothing to fix. So a process still running
// when the window closes passes; only a process that exits fails.
// VerifyNeverListened marks the one outcome that is neither pass nor fail:
// the app stayed up but never opened its port. donkey lets the build through
// — a sandbox is not the cluster — and says so, because the usual cause is an
// app listening somewhere other than where it is about to be deployed.
const VerifyNeverListened = "[verify] the app never listened on the port donkey will deploy it with"

const (
	// verifyBusyboxImage supplies the shell and the port probe. It is mounted
	// beside the app rather than expected inside it — the app's image may
	// have no shell at all, and adding one to make it testable would change
	// the thing under test.
	//
	// The uclibc variant, because it is the STATIC one. A dynamically linked
	// busybox carries its loader's path inside the ELF (/lib/ld-*.so), and
	// that path resolves against the app's filesystem, not the mount — so the
	// stock busybox:1.37 fails to exec with "no such file or directory" in
	// any image whose libc differs, which is most of them.
	verifyBusyboxImage = "docker.io/library/busybox:1.37-uclibc"
	// verifyToolsDir is where that mount lands, and verifyBusybox is the
	// binary inside it — a mount brings the image's whole root filesystem,
	// so busybox arrives at its own /bin/busybox, not at the mount point.
	verifyToolsDir = "/donkey-verify"
	verifyBusybox  = verifyToolsDir + "/bin/busybox"
)

// VerifySpec is what a build should check about the image it produced.
type VerifySpec struct {
	// Port is where the app is expected to listen; donkey tells the app the
	// same number through PORT.
	Port int
	// Seconds bounds the wait.
	Seconds int
	// Env is what the app is started with. Only what donkey sets itself goes
	// here — the app's own secrets are not needed to find out whether it
	// starts, and a sandbox is not where they belong.
	Env map[string]string
	// Platform is what the image was built for. It has to be said out loud:
	// llb.Image left to itself resolves against the platform of whatever
	// process built the graph, which is donkey-server — not the cluster the
	// image is for, and on a developer's machine not even Linux.
	Platform ocispecs.Platform
}

// verifyState builds the probe: the app's own image, the app started the way
// its image says to start it, and a wait for either a listening port or the
// process falling over.
func verifyState(app llb.State, cfg ocispecs.Image, spec VerifySpec) llb.State {
	st := app
	for _, kv := range cfg.Config.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			st = st.AddEnv(k, v)
		}
	}
	// donkey's own settings win: PORT is the contract donkey deploys with, so
	// it is the port the check waits on.
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

// verifyScript starts the app the way its image would and watches it. Written
// against busybox's shell, which is what is mounted — nothing is assumed
// about the app's own image beyond its being able to run its own entrypoint.
func verifyScript(cfg ocispecs.Image, spec VerifySpec) string {
	bb := verifyBusybox
	var b strings.Builder
	// The log lands in /tmp: the probe runs as whatever user the image says
	// to run as, and an unprivileged one cannot write to /. Getting that
	// wrong makes the redirect fail and takes the whole check down with it,
	// which reads as "the app exited" and is a lie.
	b.WriteString("set -u\n")
	fmt.Fprintf(&b, "%s >/tmp/donkey-app.log 2>&1 &\n", startCommand(cfg))
	b.WriteString("app=$!\n")
	fmt.Fprintf(&b, "i=0\nwhile [ $i -lt %d ]; do\n", spec.Seconds)
	// A process that is gone is the failure this exists to catch, and the log
	// is the only thing that can explain it — so it goes to the build output,
	// which is what the person (and the assistant) gets to read.
	b.WriteString("  if ! kill -0 $app 2>/dev/null; then\n")
	b.WriteString("    echo '[verify] the app exited instead of starting up:'\n")
	fmt.Fprintf(&b, "    %s cat /tmp/donkey-app.log\n", bb)
	b.WriteString("    exit 1\n  fi\n")
	fmt.Fprintf(&b, "  if %s nc -z 127.0.0.1 %d 2>/dev/null; then\n", bb, spec.Port)
	fmt.Fprintf(&b, "    echo '[verify] the app is listening on %d'\n", spec.Port)
	b.WriteString("    exit 0\n  fi\n")
	fmt.Fprintf(&b, "  %s sleep 1\n  i=$((i+1))\ndone\n", bb)
	// Still running, just not listening. That is an app waiting for something
	// this sandbox does not have, not a broken image — say so and pass. The
	// marker is what donkey looks for afterwards to warn about it: passing
	// silently is how an app that listens on the wrong port gets all the way
	// to a rollout timeout before anybody hears about it.
	fmt.Fprintf(&b, "echo '%s after %ds — leaving it to the cluster'\n", VerifyNeverListened, spec.Seconds)
	fmt.Fprintf(&b, "%s cat /tmp/donkey-app.log\n", bb)
	b.WriteString("exit 0\n")
	return b.String()
}

// startCommand is the image's own entrypoint and command, as one shell line.
func startCommand(cfg ocispecs.Image) string {
	argv := append(append([]string{}, cfg.Config.Entrypoint...), cfg.Config.Cmd...)
	if len(argv) == 0 {
		// Nothing to start is itself the answer, and a clearer one than
		// whatever a shell would do with an empty line.
		return "echo '[verify] the image names no command to run (no ENTRYPOINT and no CMD)'; exit 1"
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// imageConfigOf reads the config the frontend produced for the built image —
// where its entrypoint, environment and working directory come from.
func imageConfigOf(metadata map[string][]byte) (ocispecs.Image, error) {
	var cfg ocispecs.Image
	raw, ok := metadata["containerimage.config"]
	if !ok {
		return cfg, fmt.Errorf("the build produced no image configuration to verify")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("the built image's configuration couldn't be read: %w", err)
	}
	return cfg, nil
}
