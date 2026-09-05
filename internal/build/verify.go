package build

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/buildkit/client/llb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
)

// Startup verification runs the reviewed command and non-secret environment,
// with disposable mounts matching the runtime's storage layout. The app must
// open a non-loopback socket (and answer its health route, when configured).
// Apps requiring runtime-only services or secrets are checked at rollout.
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
	// Env contains only non-secret runtime settings. Apps needing secrets or
	// live services are checked by the runtime instead of this preliminary run.
	Env             map[string]string
	Command         []string
	StoragePaths    []string
	HealthcheckPath string
	Host            string
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
	for k, v := range spec.Env {
		st = st.AddEnv(k, v)
	}
	st = st.AddEnv("PORT", strconv.Itoa(spec.Port))
	if cfg.Config.WorkingDir != "" {
		st = st.Dir(cfg.Config.WorkingDir)
	}
	if cfg.Config.User != "" {
		st = st.User(cfg.Config.User)
	}
	opts := []llb.RunOption{
		llb.Args([]string{verifyBusybox, "sh", "-c", verifyScript(cfg, spec)}),
		llb.WithCustomName("[verify] start the app and check readiness"),
		llb.IgnoreCache,
		llb.AddMount(verifyToolsDir, llb.Image(verifyBusyboxImage, llb.Platform(spec.Platform)), llb.Readonly),
	}
	for _, p := range spec.StoragePaths {
		// Reproduce masking by runtime mounts, with disposable writable data.
		data := llb.Scratch().File(llb.Mkdir("/data", 0777))
		opts = append(opts, llb.AddMount(p, data, llb.SourcePath("/data")))
	}
	return st.Run(opts...).Root()

}

// verifyScript is written against busybox's shell, the one thing mounted —
// nothing is assumed about the app's image beyond running its own entrypoint.
func verifyScript(cfg ocispecs.Image, spec VerifySpec) string {
	if len(spec.Command) > 0 {
		cfg.Config.Entrypoint = spec.Command
		cfg.Config.Cmd = nil
	}
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
	// Inspect the socket table: connecting from localhost would accept a
	// loopback-only server that no runtime probe or gateway can reach.
	check := bb + " awk " + shellQuote(socketProbe(spec.Port)) + " /proc/net/tcp /proc/net/tcp6"
	if spec.HealthcheckPath != "" {
		url := "http://127.0.0.1:" + strconv.Itoa(spec.Port) + spec.HealthcheckPath
		check += " && " + bb + " wget -q -T 2 -O /dev/null"
		if spec.Host != "" {
			check += " --header " + shellQuote("Host: "+spec.Host)
		}
		check += " " + shellQuote(url)
	}
	fmt.Fprintf(&b, "  if %s 2>/dev/null; then\n", check)
	fmt.Fprintf(&b, "    echo '[verify] the app is ready on %d'\n", spec.Port)
	b.WriteString("    exit 0\n  fi\n")
	fmt.Fprintf(&b, "  %s sleep 1\n  i=$((i+1))\ndone\n", bb)
	// Never report an image that only stays alive as ready.
	fmt.Fprintf(&b, "echo '%s after %ds'\n", VerifyNeverListened, spec.Seconds)
	fmt.Fprintf(&b, "%s cat /tmp/donkey-app.log\n", bb)
	b.WriteString("exit 1\n")
	return b.String()
}

// Linux's socket table stores IPv4 bytes and each IPv6 word in host order.
// Avoid interval regex syntax: the static BusyBox awk does not always enable it.
func socketProbe(port int) string {
	return fmt.Sprintf(`$4 == "0A" {
 split($2,a,":"); ip=a[1];
 loop4=(length(ip)==8 && substr(ip,7,2)=="7F");
 loop6=(ip=="00000000000000000000000001000000");
 mapped=(substr(ip,1,24)=="0000000000000000FFFF0000" && substr(ip,31,2)=="7F");
 if(a[2]=="%04X" && !loop4 && !loop6 && !mapped) found=1
} END {exit !found}`, port)
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
		quoted[i] = shellQuote(a)
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

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
