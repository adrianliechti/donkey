package runtime

import (
	"slices"
	"sort"
	"time"
)

// The system is what donkey runs on and donkey itself, in words every
// runtime shares: the machines, and the control plane's own workloads. A
// Kubernetes node and the host of a Docker engine are both a Node; a
// Deployment in the system namespace and a donkey-* container are both a
// Component. Whoever shows them never asks which runtime answered.

// Unavailable marks something a runtime cannot report by design: not a
// failure, and nothing a retry changes. Reason is a sentence worth showing.
type Unavailable struct{ Reason string }

func (e Unavailable) Error() string { return e.Reason }

// Info describes a runtime.
type Info struct {
	// Name is the runtime's own name: "Kubernetes", "Docker".
	Name    string
	Version string
}

// Node is one machine apps run on.
type Node struct {
	Name string
	// Ready says the node runs workloads at all. Schedulable is false for a
	// node that is fine and closed to new ones (cordoned).
	Ready       bool
	Schedulable bool
	// Problems are the conditions that are wrong, in the runtime's own
	// words: MemoryPressure, DiskPressure, why the node is not ready.
	Problems []string
	// Roles are what the runtime says the node is for (control-plane);
	// none for a plain worker.
	Roles []string
	// Version is the node agent's: the kubelet's, the Docker engine's.
	Version   string
	OS        string
	Arch      string
	CreatedAt time.Time
	// CPU is counted in cores, Memory in bytes.
	CPU    NodeResource
	Memory NodeResource
	// Instances is how many workload units run on the node (pods,
	// containers); InstanceCapacity how many it takes, 0 for no set limit.
	Instances        int
	InstanceCapacity int
}

// NodeResource is one resource of a node.
type NodeResource struct {
	// Capacity is what workloads can have: the machine's own, less what the
	// runtime keeps for itself.
	Capacity float64
	// Used is what is in use right now; nil when the runtime cannot measure
	// it (a cluster without a metrics API).
	Used *float64
	// Reserved is what workloads asked to be guaranteed, which is what
	// placement goes by; nil where the runtime knows no such thing.
	Reserved *float64
}

// Component is one of donkey's own workloads.
type Component struct {
	// Name is the workload's (donkey-server); Role what it is without the
	// naming plumbing (server), the same word on every runtime.
	Name  string
	Role  string
	Image string
	// Ready of Want instances run and answer.
	Ready int32
	Want  int32
	// Restarts sums restarts across instances; StartedAt is when the newest
	// instance came up.
	Restarts  int32
	StartedAt time.Time
	// Message says why an instance is not ready, in the runtime's words.
	Message string
}

// Healthy says every instance the component should have is ready.
func (c Component) Healthy() bool { return c.Want > 0 && c.Ready >= c.Want }

// componentOrder is the control plane read front to back: what people talk
// to, what works for them, what those lean on.
var componentOrder = []string{
	"server", "orchestrator", "gateway", "ai", "proxy", "telemetry", "packages", "buildkitd", "registry",
}

// SortComponents puts components in the one order every runtime lists them
// in; roles this list does not know follow, by name.
func SortComponents(components []Component) {
	rank := func(role string) int {
		if i := slices.Index(componentOrder, role); i >= 0 {
			return i
		}
		return len(componentOrder)
	}
	sort.SliceStable(components, func(i, j int) bool {
		if a, b := rank(components[i].Role), rank(components[j].Role); a != b {
			return a < b
		}
		return components[i].Name < components[j].Name
	})
}
