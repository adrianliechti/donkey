package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	// Workspace is the folder the agent opens (default /data/workspace).
	Workspace string
	// Command is the agent. The default runs Wingman's workspace server.
	Command []string
	// Getenv reads the machine's environment (default os.Getenv).
	Getenv func(string) string
	// Log receives the few lines the front door itself has to say.
	Log io.Writer
}

// agentStopGrace is how long a stopping machine gives the agent to save what
// it is doing.
const agentStopGrace = 10 * time.Second

func (o *Options) defaults() error {
	if o.Listen == "" {
		o.Listen = fmt.Sprintf(":%d", config.MachinePort)
	}
	if o.Upstream == "" {
		o.Upstream = "http://127.0.0.1:9000"
	}
	if o.Workspace == "" {
		o.Workspace = filepath.Join(config.MachineDataPath, "workspace")
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

// Serve runs a machine until ctx ends or the agent exits: it makes the
// owner's home what they expect, starts the agent in the workspace and
// stands in front of it. An agent that exits takes the machine down with it,
// so the runtime starts a fresh one.
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
	upstream, _ := url.Parse(opts.Upstream)
	guard := &Guard{Token: token, Host: host, Upstream: upstream}
	listener, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: guard.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	defer srv.Close()

	agent := exec.Command(opts.Command[0], opts.Command[1:]...)
	agent.Dir = opts.Workspace
	agent.Env = agentEnv(os.Environ())
	agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
	if err := agent.Start(); err != nil {
		return fmt.Errorf("the agent couldn't be started (%s): %w", strings.Join(opts.Command, " "), err)
	}
	fmt.Fprintf(opts.Log, "donkey machine: serving %s for https://%s\n", strings.Join(opts.Command, " "), host)
	exited := make(chan error, 1)
	go func() { exited <- agent.Wait() }()

	select {
	case err := <-exited:
		if err == nil {
			err = errors.New("it exited")
		}
		return fmt.Errorf("the agent stopped: %w", err)
	case err := <-serveErr:
		_ = agent.Process.Kill()
		<-exited
		return err
	case <-ctx.Done():
	}
	// Asked to stop: the agent gets the chance to finish what it is writing.
	_ = agent.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(agentStopGrace):
		_ = agent.Process.Kill()
		<-exited
	}
	return nil
}

// agentEnv is the environment the agent — and with it every terminal and
// command of the machine — runs in.
func agentEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+1)
	sandbox := false
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "DONKEY_MACHINE_GUARD_TOKEN":
			// The front door's own credential. Nothing behind the door has a
			// use for it.
			continue
		case "WINGMAN_SANDBOX":
			sandbox = true
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
