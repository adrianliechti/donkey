package telemetry

import (
	"context"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

type resourceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f resourceRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

func TestResourceExporterDoesNotDropValidPodsBesideInvalidSample(t *testing.T) {
	var exported collectormetricspb.ExportMetricsServiceRequest
	exporter := NewResourceExporter("http://telemetry.test", make([]byte, 32))
	exporter.http = &http.Client{Transport: resourceRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.Unmarshal(body, &exported); err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header),
		}, nil
	})}

	err := exporter.Export(context.Background(), []ResourceObservation{
		{Space: "not valid", App: "site", Instance: "bad", CPUCores: 0.1, MemoryBytes: 1},
		{Space: "team", App: "site", Instance: "pod-uid", CPUCores: 0.25, MemoryBytes: 64},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid resource telemetry scope") {
		t.Fatalf("invalid observation should be reported after exporting its valid peer: %v", err)
	}
	if len(exported.ResourceMetrics) != 1 {
		t.Fatalf("exported %d resource records, want the one valid pod", len(exported.ResourceMetrics))
	}
	resources := resourceMap(exported.ResourceMetrics[0].Resource)
	if resources["service.namespace"] != "team" || resources["service.name"] != "site" ||
		resources["service.instance.id"] != "pod-uid" {
		t.Fatalf("exported scope=%v", resources)
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

	report, err := store.Resources(context.Background(), "team", "site", ResourceRange48Hours, at.Add(time.Second))
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

func TestStoreKeepsRecentResourceMovementByMinute(t *testing.T) {
	store, err := OpenStore(":memory:", 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	firstAt := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	for _, observation := range []ResourceObservation{
		{Space: "team", App: "worker", Instance: "one", SampledAt: firstAt, CPUCores: 0.1, MemoryBytes: 100},
		{Space: "team", App: "worker", Instance: "one", SampledAt: firstAt.Add(time.Minute), CPUCores: 0.8, MemoryBytes: 250},
	} {
		metrics, err := resourceMetrics(observation)
		if err != nil {
			t.Fatal(err)
		}
		accepted, rejected, err := store.IngestResourceMetrics(context.Background(), &collectormetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{metrics},
		})
		if err != nil {
			t.Fatal(err)
		}
		if accepted != 2 || rejected != 0 {
			t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
		}
	}

	report, err := store.Resources(context.Background(), "team", "worker", ResourceRange1Hour, firstAt.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Granularity != "minute" || len(report.Series) != 60 {
		t.Fatalf("granularity=%q points=%d", report.Granularity, len(report.Series))
	}
	var measured []ResourcePoint
	for _, point := range report.Series {
		if point.Samples > 0 {
			measured = append(measured, point)
		}
	}
	if len(measured) != 2 || measured[0].AverageCPUCores != 0.1 || measured[0].AverageMemoryBytes != 100 ||
		measured[1].AverageCPUCores != 0.8 || measured[1].AverageMemoryBytes != 250 {
		t.Fatalf("minute movement=%+v", measured)
	}

	fourHours, err := store.Resources(context.Background(), "team", "worker", ResourceRange4Hours, firstAt.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if fourHours.Granularity != "5m" || len(fourHours.Series) != 48 {
		t.Fatalf("four-hour granularity=%q points=%d", fourHours.Granularity, len(fourHours.Series))
	}
}

func TestResourceRangesAreIndependentFromTrafficRanges(t *testing.T) {
	if got, err := ParseResourceRange(""); err != nil || got != ResourceRange4Hours {
		t.Fatalf("default resource range=%q err=%v", got, err)
	}
	if _, err := ParseTrafficRange("4h"); err == nil {
		t.Fatal("HTTP traffic unexpectedly accepted the resource-only 4h range")
	}
}

func TestCurrentHTTPMethodConventionIncludesQuery(t *testing.T) {
	if got := canonicalMetricMethod("QUERY"); got != "QUERY" {
		t.Fatalf("QUERY method=%q", got)
	}
}
