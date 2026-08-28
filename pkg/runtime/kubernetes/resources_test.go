package kubernetes

import (
	"math"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPodMetricUsage(t *testing.T) {
	metric := &unstructured.Unstructured{Object: map[string]any{
		"timestamp": "2026-08-28T12:34:56Z",
		"containers": []any{
			map[string]any{"name": "sidecar", "usage": map[string]any{"cpu": "1", "memory": "1Gi"}},
			map[string]any{"name": "web", "usage": map[string]any{"cpu": "125m", "memory": "96Mi"}},
		},
	}}
	cpu, memory, at, ok := podMetricUsage(metric, "web")
	if !ok {
		t.Fatal("web usage was not parsed")
	}
	if math.Abs(cpu-0.125) > 1e-9 || memory != 96*1024*1024 {
		t.Fatalf("usage CPU=%v memory=%d", cpu, memory)
	}
	want := time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC)
	if !at.Equal(want) {
		t.Fatalf("timestamp=%s, want %s", at, want)
	}
}
