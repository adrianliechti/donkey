package runtime

import (
	"context"
	"io"
	"maps"
	"net"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/packages"
)

// MachineProvider is the optional runtime capability behind personal
// machines. A runtime that has it hands out its Machines.
type MachineProvider interface {
	Machines() Machines
}

// Machines runs personal machines: one development container per signed-in
// user, with a disk of its own, reached only through the gateway and only by
// its owner. It is a type of its own rather than more verbs on Provider
// because a machine is nobody's app: it has no space, is never deployed, and
// none of the app verbs mean anything for it.
//
// Every method takes the machine's ID (config.MachineID). Callers serialize
// changes to one machine.
type Machines interface {
	// Status reports one machine. Nothing there at all — no container, no
	// disk — reads as State == MachineAbsent, not as an error.
	Status(ctx context.Context, id string) (*MachineStatus, error)
	// Start creates what is missing — the disk, the route, the container —
	// and starts the machine with this spec, replacing a running one. It
	// returns once the runtime has accepted it; Status says when it serves.
	Start(ctx context.Context, spec MachineSpec) error
	// Stop stops the container and withdraws its credentials. The disk
	// stays.
	Stop(ctx context.Context, id string) error
	// Delete removes the machine and its disk.
	Delete(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, opts LogOptions, out io.Writer) error
	// Remember keeps a list of the machine's folders with its disk, so that
	// a machine that is stopped can still say what is on it (Status reports
	// it as Folders). It is a note, not the truth: the disk is, and only a
	// running machine can read that. A machine without a disk has nothing to
	// keep a note with, which is no error.
	Remember(ctx context.Context, id string, folders []MachineFolder) error
}

// MachineFolder is one folder of a machine's workspace.
type MachineFolder struct {
	Name string `json:"name"`
	// Modified is when the folder was last opened, or last changed at its top
	// level — which is what puts the one in use first in a list.
	Modified time.Time `json:"modified"`
}

// MaxMachineFolders bounds the note Remember keeps. A workspace with more
// folders than this is remembered by the ones used last.
const MaxMachineFolders = 100

// MachineState is what a machine is doing.
type MachineState string

const (
	// MachineAbsent: nothing exists for this user.
	MachineAbsent MachineState = "absent"
	// MachineStopped: the disk exists, nothing runs.
	MachineStopped MachineState = "stopped"
	// MachineStarting: the runtime is bringing the container up.
	MachineStarting MachineState = "starting"
	MachineRunning  MachineState = "running"
	// MachineFailed: the container will not come up; Message says why.
	MachineFailed MachineState = "failed"
)

// MachineSpec is everything a start needs.
type MachineSpec struct {
	ID string
	// Owner is the IdP subject the machine belongs to — the only identity
	// its route admits. Email and Name are what the owner is called, for the
	// machine's git identity.
	Owner string
	Email string
	Name  string
	Image string
	// Host is the machine's public address (config.MachineHost).
	Host string
	// Token is a sealed donkey sign-in for the owner: the CLI inside the
	// machine uses it exactly as one on a laptop would. SignInExpires is
	// when it stops working — a restart hands the machine a new one.
	Token         string
	SignInExpires time.Time
	// GuardToken is what the gateway presents to this machine
	// (config.MachineGuardToken).
	GuardToken string
	// ControlToken is what donkey-server presents to this machine
	// (config.MachineControlToken).
	ControlToken string
	// AIToken stands in for the platform's AI credential at donkey-ai, which
	// accepts it for as long as the machine runs.
	AIToken string
	// Model is the model the coding agent starts with.
	Model string
	// Settings carry the installation's disk size and resource caps.
	Settings config.MachineSettings
}

// MachineStatus is a snapshot of one machine.
type MachineStatus struct {
	State MachineState
	Image string
	// Host is the address the machine is served at while it runs.
	Host string
	// Backend is the machine's front door as the control plane reaches it
	// inside the runtime (http://name:port), while there is a container.
	Backend   string
	StartedAt time.Time
	// Restarts counts how often the container came back on its own.
	Restarts int32
	// Message explains a machine that is starting slowly or failing.
	Message string
	// SignInExpires is when the donkey sign-in the machine was started with
	// runs out.
	SignInExpires time.Time
	// Folders is what the machine's disk held when it was last looked at
	// (Remember). Only a stopped machine reports it: a running one is asked.
	Folders []MachineFolder
}

// Exists says there is something to stop, start or delete.
func (s *MachineStatus) Exists() bool { return s != nil && s.State != "" && s.State != MachineAbsent }

// MachineEnv is the contract between the control plane and the machine
// image, the same on every runtime. apiURL, aiURL and packagesURL are the
// control plane as the machine reaches it from inside the runtime. Every
// value here is the owner's own: a credential that opens something else
// never goes in.
func MachineEnv(spec MachineSpec, apiURL, aiURL, packagesURL string) map[string]string {
	env := map[string]string{
		"DONKEY_MACHINE_ID":          spec.ID,
		"DONKEY_MACHINE_HOST":        spec.Host,
		"DONKEY_MACHINE_GUARD_TOKEN": spec.GuardToken,
		// The control plane's own credential at the front door, for the few
		// things it asks a machine itself: its folders, and to open one.
		"DONKEY_MACHINE_CONTROL_TOKEN": spec.ControlToken,
		"DONKEY_MACHINE_USER_NAME":     spec.Name,
		"DONKEY_MACHINE_USER_EMAIL":    spec.Email,
		// The donkey CLI reads its sign-in from here; the server address goes
		// into its config file when the machine starts.
		"DONKEY_SERVER_URL": apiURL,
		"DONKEY_TOKEN":      spec.Token,
	}
	if idle := spec.Settings.IdleTimeout(); idle > 0 {
		// The machine knows best whether it is in use, so it is the one that
		// asks to be stopped — with its owner's sign-in, like any other stop.
		env["DONKEY_MACHINE_IDLE"] = idle.String()
	}
	if aiURL != "" && spec.AIToken != "" {
		// An ordinary OpenAI-compatible endpoint: the coding agent and
		// anything else the owner runs speak to it the same way.
		env["OPENAI_BASE_URL"] = aiURL
		env["OPENAI_API_KEY"] = spec.AIToken
		if spec.Model != "" {
			env["WINGMAN_MODEL"] = spec.Model
		}
	}
	if packagesURL = strings.TrimRight(packagesURL, "/"); packagesURL != "" {
		// Package managers install through the installation's mirror, the
		// way a build's do: the same registries, the same private packages,
		// and no upstream credential anywhere near the machine. Nothing in
		// the machine has to know — the tools read these names themselves —
		// and it keeps working where the network lets nothing else out.
		maps.Copy(env, packages.ManagerEnv(packagesURL))
		// The mirror is plain HTTP inside the runtime, which pip only takes
		// from a host it was told to trust.
		env["PIP_TRUSTED_HOST"] = strings.TrimPrefix(strings.TrimPrefix(packagesURL, "http://"), "https://")
		// A machine that reaches only the mirror cannot reach Go's checksum
		// database. go.sum still pins what was downloaded.
		env["GOSUMDB"] = "off"
	}
	for key, value := range env {
		if value == "" {
			delete(env, key)
		}
	}
	return env
}

// MachineHealthPath is where a machine's front door says, to anyone who can
// reach it, that it is up and taking requests. It is the one path that needs
// no credential, and it reveals nothing else — not even whether a folder is
// open behind it.
const MachineHealthPath = "/.donkey/healthz"

// MachineDialPatience is how long a connection to a machine's front door is
// tried again before it counts as failed.
const MachineDialPatience = 20 * time.Second

// MachineDialer connects to a machine's front door, and keeps trying for a
// while when it cannot: use it as the DialContext of whatever talks to
// machines — the gateway, for the owner's browser, and the control plane.
//
// A machine is called running the moment its own container says it is up,
// and both of them ask for it right then: the console opens a folder and
// shows the agent as soon as it reads "running". The way to a container
// that came up a second ago is often not there yet. On Kubernetes its
// Service has no endpoint on the asking node until that node has heard of
// the pod, and the fence around machines is still being drawn for it — the
// connection is refused, or simply not answered. That is a start that has
// not finished, not a failure, so the caller waits it out here instead of
// handing its owner an error that a reload a moment later would not show.
//
// Only connecting is tried again — nothing has been sent — so it is safe for
// any request. Each attempt is short, so an address that swallows the
// connection instead of refusing it costs one attempt, not the whole wait.
func MachineDialer(patience time.Duration) func(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		waiting, cancel := context.WithTimeout(ctx, patience)
		defer cancel()
		var failed error
		for {
			conn, err := dialer.DialContext(waiting, network, address)
			if err == nil {
				return conn, nil
			}
			// What stopped the wait is not why the machine did not answer:
			// the caller reads the last real refusal.
			if failed == nil || waiting.Err() == nil {
				failed = err
			}
			select {
			case <-waiting.Done():
				return nil, failed
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

// MachineAIKey is the entry of a machine's stored environment that holds its
// AI stand-in token — what the runtimes hand to donkey-ai as valid.
const MachineAIKey = "OPENAI_API_KEY"
