package telemetry

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func TestResourceMetricsUseOnlyOfficialContainerConventions(t *testing.T) {
	at := time.Date(2026, 8, 28, 12, 34, 56, 0, time.UTC)
	rm, err := resourceMetrics(ResourceObservation{
		Space: "team", App: "site", Instance: "container-1",
		ContainerID: "container-1", ContainerName: "site-web", SampledAt: at,
		CPUCores: 0.3, CPUUserCores: 0.2, CPUSystemCores: 0.1, CPUModesAvailable: true,
		MemoryBytes: 96 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := resourceMap(rm.Resource)
	for key, want := range map[string]string{
		"service.namespace":   "team",
		"service.name":        "site",
		"service.instance.id": "container-1",
		"container.id":        "container-1",
		"container.name":      "site-web",
	} {
		if got := resources[key]; got != want {
			t.Errorf("resource %s=%q, want %q", key, got, want)
		}
	}
	for key := range resources {
		if strings.HasPrefix(key, "donkey.") {
			t.Errorf("custom resource attribute leaked into OTLP: %s", key)
		}
	}

	metrics := rm.ScopeMetrics[0].Metrics
	if len(metrics) != 2 {
		t.Fatalf("metrics=%d, want 2", len(metrics))
	}
	cpu := metrics[0]
	if cpu.Name != ContainerCPUUsage || cpu.Unit != "{cpu}" || cpu.GetGauge() == nil {
		t.Fatalf("CPU metric is not the official Gauge: %+v", cpu)
	}
	if len(cpu.GetGauge().DataPoints) != 2 {
		t.Fatalf("CPU mode points=%d, want 2", len(cpu.GetGauge().DataPoints))
	}
	modes := map[string]float64{}
	for _, point := range cpu.GetGauge().DataPoints {
		modes[attributesMap(point.Attributes)["cpu.mode"]] = point.GetAsDouble()
	}
	if modes["user"] != 0.2 || modes["system"] != 0.1 {
		t.Fatalf("CPU modes=%v", modes)
	}

	memory := metrics[1]
	sum := memory.GetSum()
	if memory.Name != ContainerMemoryWorkingSet || memory.Unit != "By" || sum == nil || sum.IsMonotonic ||
		sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Fatalf("memory metric is not the official UpDownCounter encoding: %+v", memory)
	}
	if got := sum.DataPoints[0].GetAsInt(); got != 96*1024*1024 {
		t.Fatalf("memory=%d", got)
	}
	for _, metric := range metrics {
		if strings.HasPrefix(metric.Name, "donkey.") {
			t.Errorf("custom metric leaked into OTLP: %s", metric.Name)
		}
	}
}

func TestStoreAggregatesContainerResourcesPerApp(t *testing.T) {
	store, err := OpenStore(":memory:", 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Now().UTC().Truncate(time.Second)
	first, err := resourceMetrics(ResourceObservation{
		Space: "team", App: "site", Instance: "one", SampledAt: at,
		CPUCores: 0.3, CPUUserCores: 0.2, CPUSystemCores: 0.1, CPUModesAvailable: true,
		MemoryBytes: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := resourceMetrics(ResourceObservation{
		Space: "team", App: "site", Instance: "two", SampledAt: at,
		CPUCores: 0.4, MemoryBytes: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := &collectormetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{first, second}}
	accepted, rejected, err := store.IngestResourceMetrics(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != 5 || rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
	// An OTLP retry of the same snapshot must not add another history sample.
	if _, _, err := store.IngestResourceMetrics(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	report, err := store.Resources(context.Background(), "team", "site", Range48Hours, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Available || report.Current == nil || report.Current.Instances != 2 {
		t.Fatalf("current=%+v available=%v", report.Current, report.Available)
	}
	if math.Abs(report.Current.CPUCores-0.7) > 1e-9 || report.Current.MemoryBytes != 300 {
		t.Fatalf("current=%+v", report.Current)
	}
	var populated *ResourcePoint
	for i := range report.Series {
		if report.Series[i].Samples > 0 {
			populated = &report.Series[i]
			break
		}
	}
	if populated == nil || populated.Samples != 1 || math.Abs(populated.AverageCPUCores-0.7) > 1e-9 || populated.AverageMemoryBytes != 300 {
		t.Fatalf("history point=%+v", populated)
	}
}

func TestCurrentHTTPMethodConventionIncludesQuery(t *testing.T) {
	if got := canonicalMetricMethod("QUERY"); got != "QUERY" {
		t.Fatalf("QUERY method=%q", got)
	}
}
