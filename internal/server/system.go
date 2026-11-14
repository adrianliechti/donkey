package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianliechti/donkey/pkg/runtime"
)

// The system page is what donkey runs on and donkey itself: the runtime, its
// machines, the control plane's own workloads. Every word of it comes from
// the runtime (runtime.Provider's Info, Nodes and Components), so the page is
// the same on a cluster and on a Docker engine. Doctor beside it asks the
// other question — not "is each part running" but "does each thing donkey
// depends on answer".

const (
	// Several open consoles share one read. Nodes cost the most on a
	// cluster (every pod's requests), and their usage moves in seconds;
	// the control plane's workloads change when somebody upgrades them.
	nodesCacheTTL  = 10 * time.Second
	systemCacheTTL = 5 * time.Second
	// A runtime that cannot answer is asked again soon, not on every poll.
	failedCacheTTL = time.Second
	systemTimeout  = 15 * time.Second
)

// briefly keeps one answer for a moment. Its lock coalesces concurrent
// readers into one read, the way the fleet snapshot does.
type briefly[T any] struct {
	mu    sync.Mutex
	value T
	err   error
	until time.Time
	// changes counts forget calls; read is the count the kept answer was
	// read at. An answer read while something changed is not kept.
	changes atomic.Uint64
	read    uint64
}

func (b *briefly[T]) get(ttl time.Duration, read func() (T, error)) (T, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Now().Before(b.until) && b.read == b.changes.Load() {
		return b.value, b.err
	}
	b.read = b.changes.Load()
	b.value, b.err = read()
	if b.err != nil {
		ttl = failedCacheTTL
	}
	b.until = time.Now().Add(ttl)
	return b.value, b.err
}

// forget makes the next get read again. It never waits for a read under
// way: whoever writes what an answer is made from must not be held up by
// somebody reading it.
func (b *briefly[T]) forget() { b.changes.Add(1) }

// SystemResponse is the control plane as it runs right now.
type SystemResponse struct {
	// Version is donkey's own, the same one /v1/ping answers with.
	Version string `json:"version"`
	Domain  string `json:"domain"`
	// Runtime is what donkey runs apps on, and the version that answers.
	Runtime    SystemRuntime     `json:"runtime"`
	Components []SystemComponent `json:"components"`
	// ComponentsError says why the components could not be read.
	ComponentsError string `json:"componentsError,omitempty"`
}

type SystemRuntime struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// SystemComponent is one of donkey's own workloads.
type SystemComponent struct {
	Name string `json:"name"`
	// Role is what it is on every runtime: server, orchestrator, gateway…
	Role  string `json:"role"`
	Image string `json:"image,omitempty"`
	// Version is the image's tag, when it has one.
	Version string `json:"version,omitempty"`
	Healthy bool   `json:"healthy"`
	Ready   int32  `json:"ready"`
	Want    int32  `json:"want"`
	// Restarts sums restarts across instances.
	Restarts  int32  `json:"restarts,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
	// Message says why an instance is not ready.
	Message string `json:"message,omitempty"`
}

// NodesResponse is the machines apps run on.
type NodesResponse struct {
	Nodes []SystemNode `json:"nodes"`
	// Note says why there are none to show when the runtime may not look —
	// which is a fact about the installation, not a failure.
	Note string `json:"note,omitempty"`
}

type SystemNode struct {
	Name        string   `json:"name"`
	Ready       bool     `json:"ready"`
	Schedulable bool     `json:"schedulable"`
	Problems    []string `json:"problems,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	Version     string   `json:"version,omitempty"`
	OS          string   `json:"os,omitempty"`
	Arch        string   `json:"arch,omitempty"`
	CreatedAt   string   `json:"createdAt,omitempty"`
	// CPU is counted in cores, Memory in bytes.
	CPU    SystemNodeResource `json:"cpu"`
	Memory SystemNodeResource `json:"memory"`
	// Instances run on the node (pods, containers); InstanceCapacity is how
	// many it takes, absent where the runtime sets no limit.
	Instances        int `json:"instances"`
	InstanceCapacity int `json:"instanceCapacity,omitempty"`
}

// SystemNodeResource is one resource of a node: what workloads can have,
// what is in use, and what has been reserved — the last two only where the
// runtime knows.
type SystemNodeResource struct {
	Capacity float64  `json:"capacity"`
	Used     *float64 `json:"used,omitempty"`
	Reserved *float64 `json:"reserved,omitempty"`
}

// handleSystem: GET /v1/system.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	system, _ := s.system.get(systemCacheTTL, func() (SystemResponse, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), systemTimeout)
		defer cancel()
		return s.readSystem(ctx), nil
	})
	writeJSON(w, http.StatusOK, system)
}

// readSystem asks the runtime about itself and about donkey's own
// workloads. Either answer may be missing; the other is still worth showing.
func (s *Server) readSystem(ctx context.Context) SystemResponse {
	system := SystemResponse{Version: Version, Domain: s.cfg.Domain, Components: []SystemComponent{}}
	// A runtime that does not say its version is still named.
	info, _ := s.rt.Info(ctx)
	system.Runtime = SystemRuntime{Name: info.Name, Version: info.Version}
	components, err := s.rt.Components(ctx)
	if err != nil {
		system.ComponentsError = err.Error()
		return system
	}
	for _, c := range components {
		component := SystemComponent{
			Name: c.Name, Role: c.Role, Image: c.Image, Version: imageTag(c.Image),
			Healthy: c.Healthy(), Ready: c.Ready, Want: c.Want, Restarts: c.Restarts, Message: c.Message,
		}
		if !c.StartedAt.IsZero() {
			component.StartedAt = c.StartedAt.UTC().Format(time.RFC3339)
		}
		system.Components = append(system.Components, component)
	}
	return system
}

// imageTag is the tag of an image reference, or "" for one without (a bare
// name, or one pinned by digest alone).
func imageTag(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		return image[colon+1:]
	}
	return ""
}

// handleNodes: GET /v1/nodes.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.nodes.get(nodesCacheTTL, func() (NodesResponse, error) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), systemTimeout)
		defer cancel()
		return s.readNodes(ctx)
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, nodes)
}

func (s *Server) readNodes(ctx context.Context) (NodesResponse, error) {
	response := NodesResponse{Nodes: []SystemNode{}}
	nodes, err := s.rt.Nodes(ctx)
	if unavailable := (runtime.Unavailable{}); errors.As(err, &unavailable) {
		response.Note = unavailable.Reason
		return response, nil
	}
	if err != nil {
		return response, err
	}
	for _, n := range nodes {
		node := SystemNode{
			Name: n.Name, Ready: n.Ready, Schedulable: n.Schedulable, Problems: n.Problems, Roles: n.Roles,
			Version: n.Version, OS: n.OS, Arch: n.Arch,
			CPU:       SystemNodeResource{Capacity: n.CPU.Capacity, Used: n.CPU.Used, Reserved: n.CPU.Reserved},
			Memory:    SystemNodeResource{Capacity: n.Memory.Capacity, Used: n.Memory.Used, Reserved: n.Memory.Reserved},
			Instances: n.Instances, InstanceCapacity: n.InstanceCapacity,
		}
		if !n.CreatedAt.IsZero() {
			node.CreatedAt = n.CreatedAt.UTC().Format(time.RFC3339)
		}
		response.Nodes = append(response.Nodes, node)
	}
	return response, nil
}
