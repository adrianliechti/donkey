package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

var _ rt.ResourceSampler = (*Runtime)(nil)

// ResourceSamples reads near-realtime Docker Engine stats for every running
// app container. Docker primes a non-streaming stats response with two CPU
// snapshots, which gives us cores used over that interval rather than a
// lifetime average. Calls are bounded so a large installation cannot fan out
// an unlimited number of socket requests.
func (r *Runtime) ResourceSamples(ctx context.Context) ([]rt.ResourceSample, error) {
	containers, err := r.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", labelManaged+"=true"),
			filters.Arg("label", labelInstallation+"="+dockerInstallation),
			filters.Arg("label", labelComponent+"="+componentWeb),
		),
	})
	if err != nil {
		return nil, err
	}

	const parallel = 8
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var samples []rt.ResourceSample
	var failures []error
	for _, summary := range containers {
		summary := summary
		space, app := summary.Labels[labelSpace], summary.Labels[labelApp]
		if space == "" || app == "" || !sameIdentity(summary.Labels, appLabels(space, app, componentWeb)) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			sample, sampleErr := r.containerResourceSample(ctx, summary)
			mu.Lock()
			defer mu.Unlock()
			if sampleErr != nil {
				failures = append(failures, sampleErr)
				return
			}
			samples = append(samples, sample)
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return samples, err
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Space != samples[j].Space {
			return samples[i].Space < samples[j].Space
		}
		return samples[i].App < samples[j].App
	})
	return samples, errors.Join(failures...)
}

func (r *Runtime) containerResourceSample(ctx context.Context, summary container.Summary) (rt.ResourceSample, error) {
	space, app := summary.Labels[labelSpace], summary.Labels[labelApp]
	reader, err := r.cli.ContainerStats(ctx, summary.ID, false)
	if err != nil {
		return rt.ResourceSample{}, fmt.Errorf("reading Docker stats for %s/%s: %w", space, app, err)
	}
	defer reader.Body.Close()
	var stats container.StatsResponse
	if err := json.NewDecoder(reader.Body).Decode(&stats); err != nil {
		return rt.ResourceSample{}, fmt.Errorf("decoding Docker stats for %s/%s: %w", space, app, err)
	}
	at := stats.Read.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	memory := dockerWorkingSet(stats.MemoryStats, reader.OSType)
	containerName := strings.TrimPrefix(stats.Name, "/")
	if containerName == "" && len(summary.Names) > 0 {
		containerName = strings.TrimPrefix(summary.Names[0], "/")
	}
	containerID := stats.ID
	if containerID == "" {
		containerID = summary.ID
	}
	sample := rt.ResourceSample{
		Space: space, App: app, SampledAt: at,
		Instance: containerID, ContainerID: containerID, ContainerName: containerName,
		CPUCores: dockerCPUCores(stats), CPUModesAvailable: true, MemoryBytes: boundedBytes(memory),
	}
	// The container conventions require cpu.mode when the source is Docker
	// Stats. Zero is still a real user/system sample, not evidence that the
	// breakdown is unavailable.
	sample.CPUUserCores = dockerCPUCoreDelta(
		stats.CPUStats.CPUUsage.UsageInUsermode, stats.PreCPUStats.CPUUsage.UsageInUsermode, stats)
	sample.CPUSystemCores = dockerCPUCoreDelta(
		stats.CPUStats.CPUUsage.UsageInKernelmode, stats.PreCPUStats.CPUUsage.UsageInKernelmode, stats)
	return sample, nil
}

// dockerCPUCores is Docker's documented CPU percentage calculation without
// the final *100: the result is directly expressed as cores (0.25 means a
// quarter of one CPU, 2 means two fully busy CPUs).
func dockerCPUCores(stats container.StatsResponse) float64 {
	return dockerCPUCoreDelta(stats.CPUStats.CPUUsage.TotalUsage, stats.PreCPUStats.CPUUsage.TotalUsage, stats)
}

func dockerCPUCoreDelta(cpuNow, cpuBefore uint64, stats container.StatsResponse) float64 {
	systemNow, systemBefore := stats.CPUStats.SystemUsage, stats.PreCPUStats.SystemUsage
	if cpuNow < cpuBefore || systemNow <= systemBefore {
		return 0
	}
	cores := stats.CPUStats.OnlineCPUs
	if cores == 0 {
		cores = uint32(len(stats.CPUStats.CPUUsage.PercpuUsage))
	}
	if cores == 0 {
		cores = 1
	}
	usage := float64(cpuNow-cpuBefore) / float64(systemNow-systemBefore) * float64(cores)
	if math.IsNaN(usage) || math.IsInf(usage, 0) || usage < 0 {
		return 0
	}
	return usage
}

func dockerWorkingSet(stats container.MemoryStats, osType string) uint64 {
	if osType == "windows" && stats.PrivateWorkingSet > 0 {
		return stats.PrivateWorkingSet
	}
	cache := stats.Stats["inactive_file"] // cgroup v2
	if cache == 0 {
		cache = stats.Stats["total_inactive_file"] // cgroup v1
	}
	if cache < stats.Usage {
		return stats.Usage - cache
	}
	return stats.Usage
}

func boundedBytes(value uint64) int64 {
	if value > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(value)
}
