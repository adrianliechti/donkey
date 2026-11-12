package runtime

import (
	"context"
	"io"
	"maps"
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
}

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
	Host      string
	StartedAt time.Time
	// Restarts counts how often the container came back on its own.
	Restarts int32
	// Message explains a machine that is starting slowly or failing.
	Message string
	// SignInExpires is when the donkey sign-in the machine was started with
	// runs out.
	SignInExpires time.Time
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
		"DONKEY_MACHINE_USER_NAME":   spec.Name,
		"DONKEY_MACHINE_USER_EMAIL":  spec.Email,
		// The donkey CLI reads its sign-in from here; the server address goes
		// into its config file when the machine starts.
		"DONKEY_SERVER_URL": apiURL,
		"DONKEY_TOKEN":      spec.Token,
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
// reach it, whether the agent behind it answers. It is the one path that
// needs no credential, and it reveals nothing else.
const MachineHealthPath = "/.donkey/healthz"

// MachineAIKey is the entry of a machine's stored environment that holds its
// AI stand-in token — what the runtimes hand to donkey-ai as valid.
const MachineAIKey = "OPENAI_API_KEY"
