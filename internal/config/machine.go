package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"

	"github.com/distribution/reference"
)

// A machine is one person's development environment: a container with the
// usual toolchains, the donkey CLI signed in as its owner and a coding agent,
// reachable only by that owner through the gateway. It belongs to a user,
// never to a space.

const (
	// MachineSpace is the name machines live under: on Kubernetes their
	// namespace, on Docker the prefix of their containers. It is one of the
	// names no space may take, so no app can ever sit beside a machine.
	MachineSpace = "platform"
	// MachinePort is where the machine's own front door listens. Everything
	// behind it only answers on the loopback.
	MachinePort = 8080
	// MachineDataPath is the machine's disk: the owner's home and workspace.
	// Everything outside it comes from the image and is gone after a restart.
	MachineDataPath = "/data"
	// MachineWorkspacePath is where a machine's folders live: one per thing
	// its owner works on, each opened by the agent as a workspace of its own.
	MachineWorkspacePath = MachineDataPath + "/workspace"
	// MachineHostname is what a machine calls itself. Its owner reads it in
	// every terminal prompt (dev@donkey), where the runtime's own name for
	// the container — a pod name, a container ID — says nothing to them.
	// Every machine has the same one: nothing reaches a machine by it.
	MachineHostname = "donkey"
	// MachineUser is who the machine's processes run as. The image creates
	// the account; the runtime pins the number so the disk stays writable.
	MachineUser = int64(1000)

	DefaultMachineStorage = "10Gi"
	DefaultMachineCPU     = "2"
	DefaultMachineMemory  = "4Gi"

	// MachineGuardHeader carries the credential the gateway presents to a
	// machine. The machine's front door refuses requests without it, so being
	// able to reach the container is not enough to use it.
	MachineGuardHeader = "X-Donkey-Machine-Token"
	// MachineControlHeader carries the control plane's own credential: what
	// donkey-server presents when it asks a machine which folders it has, or
	// to open one. It is a different credential from the gateway's on purpose
	// — the gateway speaks for the owner's browser, and nothing a browser
	// sends, the owner's included, is ever taken for the control plane.
	MachineControlHeader = "X-Donkey-Machine-Control"

	// DefaultMachineIdle is how long a machine may go unused before it stops.
	DefaultMachineIdle = 60 * time.Minute
	// MachineIdleNever, as machines.idle, leaves machines running until their
	// owners stop them.
	MachineIdleNever = "never"
	// minMachineIdle keeps a typo from stopping machines under their owners.
	minMachineIdle = time.Minute
)

// MachineSettings is what an installation decides about machines. An empty
// image leaves the feature off.
type MachineSettings struct {
	// Image is the machine image, pullable by the runtime.
	Image string `json:"image,omitempty"`
	// Storage is the size of each machine's disk (default 10Gi). Docker
	// volumes have no size, so it only applies on Kubernetes.
	Storage string `json:"storage,omitempty"`
	// StorageClass is the Kubernetes StorageClass; empty uses the default.
	StorageClass string `json:"storageClass,omitempty"`
	// CPU and Memory cap one machine (defaults 2 and 4Gi).
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	// Egress is what a machine may reach: "restricted" (the default) — the
	// control plane and nothing beyond it — or "open", the network at large.
	Egress string `json:"egress,omitempty"`
	// Idle is how long a machine may go unused before it is stopped — a
	// duration such as 60m (the default) or 2h, or "never". Stopping keeps
	// the disk; it is the compute that is given back.
	Idle string `json:"idle,omitempty"`
}

// What a machine may reach. Restricted is the default, and it is a decision
// about machines rather than about workloads in general: a machine runs an
// agent that acts on what it reads, with its owner's sign-in in reach. Left
// with the package mirror, its model and the API, it can still build, test
// and deploy — and has nowhere to send anything else. Open is for
// installations whose machines have to reach what donkey does not provide,
// a Git host first of all.
const (
	MachineEgressRestricted = "restricted"
	MachineEgressOpen       = "open"
)

// RestrictEgress says machines reach the control plane only.
func (m MachineSettings) RestrictEgress() bool { return m.Egress != MachineEgressOpen }

// EgressMode is the setting with its default filled in.
func (m MachineSettings) EgressMode() string {
	if m.RestrictEgress() {
		return MachineEgressRestricted
	}
	return MachineEgressOpen
}

func (m MachineSettings) Enabled() bool { return m.Image != "" }

// IdleTimeout is how long a machine may go unused before it is stopped; zero
// means never. A machine is somebody's desk, not a service: it is worth its
// CPU and memory while that person is at it or their agent is working, and
// a start brings everything back, so an unused one gives them up.
func (m MachineSettings) IdleTimeout() time.Duration {
	switch m.Idle {
	case "":
		return DefaultMachineIdle
	case MachineIdleNever:
		return 0
	}
	if d, err := time.ParseDuration(m.Idle); err == nil && d >= minMachineIdle {
		return d
	}
	return DefaultMachineIdle
}

func (m MachineSettings) StorageSize() string {
	if m.Storage == "" {
		return DefaultMachineStorage
	}
	return m.Storage
}

func (m MachineSettings) CPULimit() string {
	if m.CPU == "" {
		return DefaultMachineCPU
	}
	return m.CPU
}

func (m MachineSettings) MemoryLimit() string {
	if m.Memory == "" {
		return DefaultMachineMemory
	}
	return m.Memory
}

// cpuRE accepts whole and fractional cores and millicores; the runtime does
// the real parsing.
var cpuRE = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?|[0-9]+m)$`)

func (m MachineSettings) Validate() error {
	if !m.Enabled() {
		if m.Storage != "" || m.StorageClass != "" || m.CPU != "" || m.Memory != "" || m.Egress != "" || m.Idle != "" {
			return fmt.Errorf("machines.image is required to turn machines on — the other machines settings do nothing without it")
		}
		return nil
	}
	if _, err := reference.ParseNormalizedNamed(m.Image); err != nil {
		return fmt.Errorf("machines.image %q is not an image reference: %w", m.Image, err)
	}
	if m.Storage != "" && !sizeRE.MatchString(m.Storage) {
		return fmt.Errorf("machines.storage %q is not a valid size (try 10Gi)", m.Storage)
	}
	if m.Memory != "" && !sizeRE.MatchString(m.Memory) {
		return fmt.Errorf("machines.memory %q is not a valid size (try 4Gi)", m.Memory)
	}
	if m.CPU != "" && !cpuRE.MatchString(m.CPU) {
		return fmt.Errorf("machines.cpu %q is not a valid amount (try 2 or 500m)", m.CPU)
	}
	switch m.Egress {
	case "", MachineEgressRestricted, MachineEgressOpen:
	default:
		return fmt.Errorf("machines.egress must be %s or %s, not %q", MachineEgressRestricted, MachineEgressOpen, m.Egress)
	}
	if m.Idle != "" && m.Idle != MachineIdleNever {
		if d, err := time.ParseDuration(m.Idle); err != nil || d < minMachineIdle {
			return fmt.Errorf("machines.idle %q is not a time a machine may go unused (try 60m, 2h or %s)", m.Idle, MachineIdleNever)
		}
	}
	return nil
}

// Env is how the installer hands the settings to the control plane.
func (m MachineSettings) Env() map[string]string {
	return map[string]string{
		"DONKEY_MACHINE_IMAGE":         m.Image,
		"DONKEY_MACHINE_STORAGE":       m.Storage,
		"DONKEY_MACHINE_STORAGE_CLASS": m.StorageClass,
		"DONKEY_MACHINE_CPU":           m.CPU,
		"DONKEY_MACHINE_MEMORY":        m.Memory,
		"DONKEY_MACHINE_EGRESS":        m.Egress,
		"DONKEY_MACHINE_IDLE":          m.Idle,
	}
}

// MachineSettingsFromEnv reads what Env wrote.
func MachineSettingsFromEnv(get func(string) string) MachineSettings {
	return MachineSettings{
		Image:        get("DONKEY_MACHINE_IMAGE"),
		Storage:      get("DONKEY_MACHINE_STORAGE"),
		StorageClass: get("DONKEY_MACHINE_STORAGE_CLASS"),
		CPU:          get("DONKEY_MACHINE_CPU"),
		Memory:       get("DONKEY_MACHINE_MEMORY"),
		Egress:       get("DONKEY_MACHINE_EGRESS"),
		Idle:         get("DONKEY_MACHINE_IDLE"),
	}
}

// MachineID names the machine of one signed-in user. It is derived from the
// IdP subject and nothing else, so it is the same on every request, a valid
// object name on every runtime, and says nothing about who the person is.
func MachineID(subject string) string {
	sum := sha256.Sum256([]byte("donkey/machine/v1\x00" + subject))
	return "m" + hex.EncodeToString(sum[:8])
}

var machineIDRE = regexp.MustCompile(`^m[0-9a-f]{16}$`)

// ValidMachineID reports whether s is a name MachineID produces.
func ValidMachineID(s string) bool { return machineIDRE.MatchString(s) }

// MachineHost is the address machines are served at: one for the whole
// installation, where the gateway leads each signed-in person to their own
// machine and to no other. Nothing about a machine is in it, so starting one
// creates no DNS record and no certificate — the name is set up once, with
// the installation — and no name that stands for a person reaches a
// certificate log.
//
// It is an origin of its own, apart from the console's: what runs in a
// machine cannot read the console's page or act with its session. It has one
// label under the domain, where an app's address has two
// (<app>.<space>.<domain>), and the gateway serves nothing but machines on it.
func MachineHost(domain string) string {
	return "machine." + domain
}

// MachineGuardToken is what the gateway presents to one machine. It is
// derived from the session key under a purpose of its own (the way the
// orchestrator's credential is), so the gateway needs no record of it and
// the route needs no secret: both sides compute it. It says nothing about the
// key, opens exactly one machine, and rotates with the session key.
func MachineGuardToken(sessionKey []byte, id string) string {
	h := hmac.New(sha256.New, sessionKey)
	_, _ = h.Write([]byte("donkey/machine/guard/v1\x00" + id))
	return hex.EncodeToString(h.Sum(nil))
}

// MachineControlToken is what donkey-server presents to one machine when it
// acts on it for its owner (MachineControlHeader). Derived like the gateway's
// credential, under a purpose of its own, so holding one never yields the
// other.
func MachineControlToken(sessionKey []byte, id string) string {
	h := hmac.New(sha256.New, sessionKey)
	_, _ = h.Write([]byte("donkey/machine/control/v1\x00" + id))
	return hex.EncodeToString(h.Sum(nil))
}

// machineFolderRE is one path segment that reads the same everywhere: in a
// shell, in a URL and in a list. No leading dot, so a folder is never hidden
// and never "." or "..".
var machineFolderRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidMachineFolder reports whether name can be given to a new folder of a
// machine's workspace (MachineWorkspacePath).
func ValidMachineFolder(name string) bool { return machineFolderRE.MatchString(name) }

// SafeMachineFolder reports whether name can be a folder that is already
// there. The owner makes folders in a terminal too, with names nobody
// checked, so this asks only what keeps a name inside the workspace: one
// visible path segment.
func SafeMachineFolder(name string) bool {
	if name == "" || len(name) > 255 || name[0] == '.' {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
