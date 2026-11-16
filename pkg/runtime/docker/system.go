package docker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

func (r *Runtime) Info(ctx context.Context) (rt.Info, error) {
	info := rt.Info{Name: "Docker"}
	version, err := r.cli.ServerVersion(ctx)
	if err != nil {
		return info, fmt.Errorf("reading the Docker engine's version: %w", err)
	}
	info.Version = version.Version
	return info, nil
}

// Nodes is the one machine a Docker engine runs on. The engine says what
// the machine has; what it uses is read off the machine itself, when this
// process runs on it (see hostMeter).
func (r *Runtime) Nodes(ctx context.Context) ([]rt.Node, error) {
	info, err := r.cli.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("asking the Docker engine about its host: %w", err)
	}
	node := rt.Node{
		Name: info.Name, Ready: true, Schedulable: true,
		Version: info.ServerVersion, OS: info.OperatingSystem, Arch: info.Architecture,
		Instances: info.ContainersRunning,
	}
	node.CPU.Capacity = float64(info.NCPU)
	node.Memory.Capacity = float64(info.MemTotal)
	if cpu, memory, ok := r.host.usage(ctx, info.NCPU, info.MemTotal); ok {
		node.CPU.Used, node.Memory.Used = &cpu, &memory
	}
	return []rt.Node{node}, nil
}

// Components are donkey's own containers: the ones carrying a system role
// and no space, app, service or machine of their own.
func (r *Runtime) Components(ctx context.Context) ([]rt.Component, error) {
	containers, err := r.cli.ContainerList(ctx, container.ListOptions{
		// A stopped part of the control plane is exactly what this is for.
		All: true,
		Filters: filters.NewArgs(
			filters.Arg("label", labelManaged+"=true"),
			filters.Arg("label", labelInstallation+"="+dockerInstallation),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("listing the control plane's containers: %w", err)
	}
	components := []rt.Component{}
	for _, summary := range containers {
		role := summary.Labels[labelRole]
		if role == "" || !MatchesSystemLabels(summary.Labels, role) {
			continue
		}
		component := rt.Component{Role: role, Image: summary.Image, Want: 1}
		if len(summary.Names) > 0 {
			component.Name = strings.TrimPrefix(summary.Names[0], "/")
		}
		info, err := r.cli.ContainerInspect(ctx, summary.ID)
		if cerrdefs.IsNotFound(err) {
			continue // replaced between the list and this look
		}
		if err != nil {
			component.Message = err.Error()
			components = append(components, component)
			continue
		}
		component.Restarts = int32(info.RestartCount)
		if state := info.State; state != nil {
			if t, err := time.Parse(time.RFC3339Nano, state.StartedAt); err == nil && t.Unix() > 0 {
				component.StartedAt = t
			}
			if problem := containerProblem(state); problem == "" {
				component.Ready = 1
			} else {
				component.Message = problem
			}
		}
		components = append(components, component)
	}
	rt.SortComponents(components)
	return components, nil
}

// containerProblem says why a container is not serving, or "" when it is.
func containerProblem(state *container.State) string {
	switch {
	case state.Restarting:
		return fmt.Sprintf("restarting (exit code %d)", state.ExitCode)
	case state.OOMKilled && !state.Running:
		return "killed — out of memory"
	case !state.Running:
		return fmt.Sprintf("%s (exit code %d)", state.Status, state.ExitCode)
	case state.Health != nil && state.Health.Status == container.Unhealthy:
		return "running, but its health check fails"
	}
	return ""
}

// hostMeter measures the machine this process runs on, from /proc. A
// container sees the whole machine's processor and memory counters there —
// the kernel does not scope them to a container — which is the same reading
// a cluster's metrics give for a node: everything on it, not only donkey's.
//
// The engine can be somewhere else (DOCKER_HOST), so the reading only counts
// when the machine it describes is the engine's: the same number of
// processors and the same memory. Anywhere else — a server run from a
// laptop, a remote engine — usage is left out rather than made up.
type hostMeter struct {
	// proc is where the counters are read; "" means /proc.
	proc string

	mu sync.Mutex
	// from is the reading the next one is compared with; share is the last
	// answer, kept for the moment after it was worked out.
	from    cpuSample
	share   float64
	shareAt time.Time
}

// cpuSample is the processors' counters since boot, at one instant.
type cpuSample struct {
	at          time.Time
	busy, total uint64
}

const (
	// A first reading has nothing to compare with, so it takes a short
	// window of its own.
	cpuFirstWindow = 250 * time.Millisecond
	// An answer stands for this long: two people on the page must not turn
	// each other's reading into the blink between their requests.
	cpuAnswerFor = time.Second
	// The window slides once it is this wide, so a page that polls reads
	// the time since its last poll.
	cpuSlideAfter = 5 * time.Second
	// A reading older than this says how busy the machine was, not is.
	cpuMaxWindow = 2 * time.Minute
)

// usage is the machine's processors in use (in cores) and memory in use (in
// bytes), if this process can tell.
func (m *hostMeter) usage(ctx context.Context, cores int, memTotal int64) (cpu, memory float64, ok bool) {
	total, available, ok := m.memory()
	if !ok || total <= 0 || memTotal <= 0 || math.Abs(float64(total-memTotal)) > 0.01*float64(memTotal) {
		return 0, 0, false
	}
	now, seen, ok := m.cpu()
	if !ok || cores <= 0 || seen != cores {
		return 0, 0, false
	}
	share, ok := m.busyShare(ctx, now)
	if !ok {
		return 0, 0, false
	}
	return share * float64(cores), float64(total - available), true
}

// busyShare is the part of the processors' time spent busy over the window
// that ends with now.
func (m *hostMeter) busyShare(ctx context.Context, now cpuSample) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.shareAt.IsZero() && now.at.Sub(m.shareAt) < cpuAnswerFor {
		return m.share, true
	}
	from := m.from
	age := now.at.Sub(from.at)
	if from.at.IsZero() || age > cpuMaxWindow || now.total <= from.total || now.busy < from.busy {
		// Nothing usable to compare with (the first look, a long pause, or
		// counters that started over): read twice, a moment apart.
		select {
		case <-ctx.Done():
			return 0, false
		case <-time.After(cpuFirstWindow):
		}
		from = now
		var ok bool
		if now, _, ok = m.cpu(); !ok || now.total <= from.total || now.busy < from.busy {
			return 0, false
		}
		m.from = now
	} else if age >= cpuSlideAfter {
		m.from = now
	}
	m.share = min(float64(now.busy-from.busy)/float64(now.total-from.total), 1)
	m.shareAt = now.at
	return m.share, true
}

func (m *hostMeter) path(name string) string {
	if m.proc != "" {
		return filepath.Join(m.proc, name)
	}
	return filepath.Join("/proc", name)
}

// memory reads the machine's memory and how much of it is still to be had,
// in bytes.
func (m *hostMeter) memory() (total, available int64, ok bool) {
	raw, err := os.ReadFile(m.path("meminfo"))
	if err != nil {
		return 0, 0, false
	}
	found := 0
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		key, rest, _ := strings.Cut(scanner.Text(), ":")
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total, found = kb*1024, found+1
		case "MemAvailable":
			available, found = kb*1024, found+1
		}
	}
	return total, available, found == 2 && available <= total
}

// cpu reads the processors' counters since boot — time spent busy, and all
// time — and how many processors there are.
func (m *hostMeter) cpu() (sample cpuSample, cores int, ok bool) {
	raw, err := os.ReadFile(m.path("stat"))
	if err != nil {
		return cpuSample{}, 0, false
	}
	sample.at = time.Now()
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			if _, err := strconv.Atoi(strings.TrimPrefix(fields[0], "cpu")); err == nil {
				cores++
			}
			continue
		}
		// user nice system idle iowait irq softirq steal — guest time is
		// already inside user and nice, so the fields after steal are skipped.
		for i, field := range fields[1:min(len(fields), 9)] {
			ticks, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return cpuSample{}, 0, false
			}
			sample.total += ticks
			if i != 3 && i != 4 { // idle, iowait
				sample.busy += ticks
			}
		}
		ok = true
	}
	return sample, cores, ok && sample.total > 0
}
