package machine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/examples/skills"
	"github.com/adrianliechti/donkey/internal/config"
)

// Options is how a machine is started. The zero value of each field takes
// the image's defaults.
type Options struct {
	// Listen is the front door (default :8080, where the runtime sends the
	// gateway).
	Listen string
	// Upstream is where the agent listens once it is up (default
	// http://127.0.0.1:9000). It must be a loopback address: the front door
	// is the only way in.
	Upstream string
	// Workspace is where the machine's folders live (default
	// /data/workspace). The agent opens one of them at a time.
	Workspace string
	// Relay is the loopback address the machine reaches its model through
	// while it counts as used by doing so (default 127.0.0.1:9001).
	Relay string
	// Command is the agent. The default runs Wingman's workspace server.
	Command []string
	// Getenv reads the machine's environment (default os.Getenv).
	Getenv func(string) string
	// Log receives the few lines the front door itself has to say.
	Log io.Writer
}

// agentStopGrace is how long a stopping agent gets to save what it is doing.
const agentStopGrace = 10 * time.Second

func (o *Options) defaults() error {
	if o.Listen == "" {
		o.Listen = fmt.Sprintf(":%d", config.MachinePort)
	}
	if o.Upstream == "" {
		o.Upstream = "http://127.0.0.1:9000"
	}
	if o.Workspace == "" {
		o.Workspace = config.MachineWorkspacePath
	}
	if o.Relay == "" {
		o.Relay = "127.0.0.1:9001"
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
	upstream, err := url.Parse(o.Upstream)
	if err != nil || upstream.Scheme != "http" || upstream.Port() == "" {
		return fmt.Errorf("the agent address %q must look like http://127.0.0.1:9000", o.Upstream)
	}
	if ip := net.ParseIP(upstream.Hostname()); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("the agent address %q must be a loopback address — the front door is the only way in", o.Upstream)
	}
	if len(o.Command) == 0 {
		o.Command = []string{"wingman", "server", "--no-browser", "--port", upstream.Port()}
	}
	return nil
}

// Serve runs a machine until ctx ends: it makes the owner's home what they
// expect and stands at the front door. The agent starts when the control
// plane opens a folder for the owner, in that folder, and starts again in
// another when they open another — the machine itself stays as it is.
func Serve(ctx context.Context, opts Options) error {
	if err := opts.defaults(); err != nil {
		return err
	}
	token, host := opts.Getenv("DONKEY_MACHINE_GUARD_TOKEN"), opts.Getenv("DONKEY_MACHINE_HOST")
	if token == "" || host == "" {
		return fmt.Errorf("this command is a machine's entry point: it needs the environment donkey starts a machine with (DONKEY_MACHINE_GUARD_TOKEN, DONKEY_MACHINE_HOST)")
	}
	if err := prepare(opts); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A machine that just started has its whole idle time ahead of it.
	activity := &Activity{}
	activity.Mark()
	idle, _ := time.ParseDuration(opts.Getenv("DONKEY_MACHINE_IDLE"))
	api := API{Server: opts.Getenv("DONKEY_SERVER_URL"), Token: opts.Getenv("DONKEY_TOKEN"), Log: opts.Log}

	upstream, _ := url.Parse(opts.Upstream)
	guard := &Guard{
		Token: token, Host: host, Upstream: upstream,
		ControlToken: opts.Getenv("DONKEY_MACHINE_CONTROL_TOKEN"),
		Used:         activity.Mark,
	}
	replace := map[string]string{}
	if idle > 0 {
		if relay, address := startModelRelay(opts, activity.Mark); relay != nil {
			defer relay.Close()
			replace["OPENAI_BASE_URL"] = address
			_, port, _ := net.SplitHostPort(relay.Addr)
			guard.Reserved = append(guard.Reserved, port)
		}
	}
	workspace := &Workspace{
		Root:    opts.Workspace,
		Command: opts.Command,
		Env:     agentEnv(os.Environ(), replace),
		Agent:   upstream,
		Fetch:   api.FetchSource,
		Used:    activity.Mark,
		Log:     opts.Log,
	}
	workspace.sweep()
	defer workspace.Close()
	guard.Control = workspace.Handler()

	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: guard.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	defer srv.Close()
	fmt.Fprintf(opts.Log, "donkey machine: serving https://%s — %s starts in the folder that is opened\n", host, strings.Join(opts.Command, " "))

	switch {
	case idle <= 0:
	case api.Server == "" || api.Token == "":
		fmt.Fprintln(opts.Log, "donkey machine: without a donkey sign-in this machine cannot ask to be stopped when it goes unused")
	default:
		go watchIdle(ctx, activity, idle, api.StopMachine, opts.Log)
	}

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		// Asked to stop: Close gives the agent the chance to finish what it
		// is writing.
		return nil
	}
}

// startModelRelay puts the door between the machine and its model, so that a
// machine whose agent is working counts as used (see modelRelay). It answers
// on the loopback only. Without a model, or where the port is taken, the
// machine reaches its model directly and only browsers keep it awake.
func startModelRelay(opts Options, used func()) (*http.Server, string) {
	target, err := url.Parse(opts.Getenv("OPENAI_BASE_URL"))
	if err != nil || target.Host == "" {
		return nil, ""
	}
	listener, err := net.Listen("tcp", opts.Relay)
	if err != nil {
		fmt.Fprintf(opts.Log, "donkey machine: the model is reached directly, so an agent working on its own will not keep this machine awake: %v\n", err)
		return nil, ""
	}
	relay := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           modelRelay(target, used),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = relay.Serve(listener) }()
	// The same address with the door in front: whatever path the endpoint
	// lives under stays.
	return relay, (&url.URL{Scheme: "http", Host: relay.Addr, Path: target.Path}).String()
}

// agentEnv is the environment the agent — and with it every terminal and
// command of the machine — runs in. replace swaps values the door stands in
// for.
func agentEnv(environ []string, replace map[string]string) []string {
	env := make([]string, 0, len(environ)+1)
	sandbox := false
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "DONKEY_MACHINE_GUARD_TOKEN", "DONKEY_MACHINE_CONTROL_TOKEN":
			// The front door's own credentials. Nothing behind the door has
			// a use for them.
			continue
		case "WINGMAN_SANDBOX":
			sandbox = true
		}
		if value, ok := replace[name]; ok {
			entry = name + "=" + value
		}
		env = append(env, entry)
	}
	if !sandbox {
		// The agent would wrap each command in a sandbox of its own, built
		// from user namespaces — which a container without privileges cannot
		// create, so every command would fail. The machine is the sandbox:
		// an unprivileged user, no capabilities, a network fence, and
		// nothing on its disk but its owner's files.
		env = append(env, "WINGMAN_SANDBOX=off")
	}
	return env
}

// prepare makes a fresh disk a home. Everything here is idempotent, and
// nothing the owner already set is overwritten.
func prepare(opts Options) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("the machine has no home folder: %w", err)
	}
	for _, dir := range []string{home, opts.Workspace} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("the machine's disk isn't writable: %w", err)
		}
	}
	if err := connectCLI(opts.Getenv("DONKEY_SERVER_URL")); err != nil {
		fmt.Fprintf(opts.Log, "donkey machine: the donkey CLI is not connected: %v\n", err)
	}
	if err := installSkill(home); err != nil {
		fmt.Fprintf(opts.Log, "donkey machine: the donkey skill could not be installed: %v\n", err)
	}
	// Commits made here are the owner's. Only filled in while empty: a name
	// they chose themselves stays.
	gitIdentity("user.name", opts.Getenv("DONKEY_MACHINE_USER_NAME"))
	gitIdentity("user.email", opts.Getenv("DONKEY_MACHINE_USER_EMAIL"))
	return nil
}

// connectCLI points the donkey CLI at the control plane. Only the address is
// written: the sign-in stays in the environment (DONKEY_TOKEN), where every
// start replaces it, instead of going stale on the disk.
func connectCLI(server string) error {
	if server == "" {
		return nil
	}
	path, err := config.DefaultGlobalPath()
	if err != nil {
		return err
	}
	cfg, err := config.LoadGlobal(path)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &config.GlobalConfig{}
	}
	server = strings.TrimSuffix(server, "/")
	if cfg.Server.URL == server && cfg.Server.Token == "" {
		return nil
	}
	cfg.Server.URL = server
	// A token saved on an earlier start would outlive the sign-in it came
	// from and shadow nothing useful: the environment always wins.
	cfg.Server.Token = ""
	return cfg.Save(path)
}

// installSkill gives the machine's agent the donkey skill: how to deploy and
// operate apps with the CLI it finds here, and what is different about
// working in a machine. It goes where agents look for a person's own skills
// by convention (~/.agents/skills), which Wingman reads and other agents the
// owner may run do too. Written on every start, so it is the skill of the
// CLI in this image; a skill the owner keeps under their agent's own folder
// (~/.wingman/skills) takes precedence over it.
func installSkill(home string) error {
	dir := filepath.Join(home, ".agents", "skills", "donkey")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skills.Donkey), 0o640)
}

func gitIdentity(key, value string) {
	if value == "" {
		return
	}
	if out, err := exec.Command("git", "config", "--global", "--get", key).Output(); err == nil && strings.TrimSpace(string(out)) != "" {
		return
	}
	_ = exec.Command("git", "config", "--global", key, value).Run()
}
