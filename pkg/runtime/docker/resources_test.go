package docker

import (
	"math"
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestDockerCPUCores(t *testing.T) {
	stats := container.StatsResponse{
		CPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 250},
			SystemUsage: 2000,
			OnlineCPUs:  4,
		},
		PreCPUStats: container.CPUStats{
			CPUUsage:    container.CPUUsage{TotalUsage: 100},
			SystemUsage: 1000,
		},
	}
	if got := dockerCPUCores(stats); math.Abs(got-0.6) > 1e-9 {
		t.Fatalf("CPU cores = %v, want 0.6", got)
	}
}

func TestDockerWorkingSetSupportsBothCgroupVersions(t *testing.T) {
	for name, key := range map[string]string{"v1": "total_inactive_file", "v2": "inactive_file"} {
		t.Run(name, func(t *testing.T) {
			stats := container.MemoryStats{Usage: 500, Stats: map[string]uint64{key: 125}}
			if got := dockerWorkingSet(stats, "linux"); got != 375 {
				t.Fatalf("working set = %d, want 375", got)
			}
		})
	}
	if got := dockerWorkingSet(container.MemoryStats{PrivateWorkingSet: 42}, "windows"); got != 42 {
		t.Fatalf("Windows working set = %d, want 42", got)
	}
}
