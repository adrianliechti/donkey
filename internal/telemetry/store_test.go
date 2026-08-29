package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

func TestStoreIngestsOTLPIntoDuckDBReport(t *testing.T) {
	store, err := OpenStore(":memory:", DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Truncate(time.Hour)
	first := exportRequest(now, []histogramPoint{
		{status: 200, count: 10, sum: 0.8, bucket: 5},
		{status: 503, count: 5, sum: 1.0, bucket: 7},
	})
	accepted, rejected, err := store.IngestMetrics(context.Background(), first, true)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != 2 || rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
	// A lost OTLP response may cause an identical delta batch to be retried.
	accepted, rejected, err = store.IngestMetrics(context.Background(), first, true)
	if err != nil || accepted != 2 || rejected != 0 {
		t.Fatalf("retry: accepted=%d rejected=%d err=%v", accepted, rejected, err)
	}
	second := exportRequest(now.Add(time.Minute), []histogramPoint{
		{status: 200, count: 3, sum: 0.12, bucket: 4},
	})
	if _, _, err := store.IngestMetrics(context.Background(), second, true); err != nil {
		t.Fatal(err)
	}

	one := strings.Repeat("1", 64)
	two := strings.Repeat("2", 64)
	batch := ViewBatch{Events: []ViewEvent{
		{TimeUnixNano: uint64(now.UnixNano()), Space: "team", App: "site", AppVisitorHash: one, SpaceVisitorHash: one},
		{TimeUnixNano: uint64(now.Add(time.Minute).UnixNano()), Space: "team", App: "site", AppVisitorHash: one, SpaceVisitorHash: one},
		{TimeUnixNano: uint64(now.Add(2 * time.Minute).UnixNano()), Space: "team", App: "site", AppVisitorHash: two, SpaceVisitorHash: two},
		{TimeUnixNano: uint64(now.Add(3 * time.Minute).UnixNano()), Space: "team", App: "site"},
	}}
	acceptedViews, rejectedViews, err := store.InsertViews(context.Background(), batch, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if acceptedViews != 4 || rejectedViews != 0 {
		t.Fatalf("views accepted=%d rejected=%d", acceptedViews, rejectedViews)
	}

	report, err := store.Traffic(context.Background(), "team", "site", SourceEdge, Range30Days, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals.Requests != 18 || report.Totals.Successful != 13 || report.Totals.ServerErrors != 5 {
		t.Fatalf("unexpected HTTP totals: %+v", report.Totals)
	}
	if report.Totals.Views != 4 || report.Totals.UniqueVisitors != 2 || report.Totals.IdentifiedViews != 3 {
		t.Fatalf("unexpected view totals: %+v", report.Totals)
	}
	if report.Totals.AverageLatencyMS != 106.7 || report.Totals.P95LatencyMS != 500 {
		t.Fatalf("unexpected latency totals: %+v", report.Totals)
	}
	if len(report.Series) != 30 || report.Series[29].Requests != 18 || report.Series[29].Views != 4 {
		t.Fatalf("unexpected series: %#v", report.Series)
	}
	hourly, err := store.Traffic(context.Background(), "team", "site", SourceEdge, Range48Hours, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if hourly.Granularity != "hour" || len(hourly.Series) != 48 || hourly.Series[47].Requests != 18 || hourly.Series[47].Views != 4 {
		t.Fatalf("unexpected hourly series: %#v", hourly)
	}
}

func TestStoreKeepsAppTelemetrySeparateAndRejectsSpoofedEdge(t *testing.T) {
	store, err := OpenStore(":memory:", DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC().Truncate(time.Hour)

	appRequest := exportRequest(now, []histogramPoint{{status: 404, count: 2, sum: 0.1, bucket: 5}})
	appRequest.ResourceMetrics[0].Resource.Attributes = []*commonpb.KeyValue{
		stringAttribute("service.namespace", "team"),
		stringAttribute("service.name", "site"),
	}
	if accepted, rejected, err := store.IngestMetrics(context.Background(), appRequest, false); err != nil || accepted != 1 || rejected != 0 {
		t.Fatalf("app ingest: accepted=%d rejected=%d err=%v", accepted, rejected, err)
	}
	if accepted, rejected, err := store.IngestMetrics(context.Background(), appRequest, true); err != nil || accepted != 0 || rejected != 1 {
		t.Fatalf("spoofed edge ingest: accepted=%d rejected=%d err=%v", accepted, rejected, err)
	}

	appReport, err := store.Traffic(context.Background(), "team", "site", SourceApp, Range7Days, now)
	if err != nil {
		t.Fatal(err)
	}
	edgeReport, err := store.Traffic(context.Background(), "team", "site", SourceEdge, Range7Days, now)
	if err != nil {
		t.Fatal(err)
	}
	if appReport.Totals.Requests != 2 || appReport.Totals.ClientErrors != 2 || edgeReport.Totals.Requests != 0 {
		t.Fatalf("app=%+v edge=%+v", appReport.Totals, edgeReport.Totals)
	}
}

func TestPurgeSpaceRemovesEveryTelemetryTableAndKeepsOtherSpaces(t *testing.T) {
	store, err := OpenStore(":memory:", DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)

	for i, scope := range []struct{ space, app string }{{"team", "site"}, {"other", "api"}} {
		httpMetrics := exportRequest(now.Add(time.Duration(i)*time.Second), []histogramPoint{{
			status: 200, count: 2, sum: 0.1, bucket: 5,
		}})
		httpMetrics.ResourceMetrics[0].Resource.Attributes = []*commonpb.KeyValue{
			stringAttribute("service.namespace", scope.space),
			stringAttribute("service.name", scope.app),
		}
		if _, _, err := store.IngestMetrics(ctx, httpMetrics, false); err != nil {
			t.Fatal(err)
		}

		resources, err := resourceMetrics(ResourceObservation{
			Space: scope.space, App: scope.app, Instance: "one", SampledAt: now,
			CPUCores: 0.2, MemoryBytes: 64,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.IngestResourceMetrics(ctx, &collectormetricspb.ExportMetricsServiceRequest{
			ResourceMetrics: []*metricspb.ResourceMetrics{resources},
		}); err != nil {
			t.Fatal(err)
		}

		hash := strings.Repeat(fmt.Sprintf("%d", i+1), 64)
		if _, _, err := store.InsertViews(ctx, ViewBatch{Events: []ViewEvent{{
			TimeUnixNano: uint64(now.UnixNano()), Space: scope.space, App: scope.app,
			AppVisitorHash: hash, SpaceVisitorHash: hash,
		}}}, now); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.PurgeSpace(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"http_rollups", "metric_stream_state", "resource_rollups", "resource_recent_rollups",
		"resource_sample_state", "view_rollups", "view_visitors",
	} {
		var teamRows, otherRows int
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE space = ?", "team").Scan(&teamRows); err != nil {
			t.Fatalf("counting %s team rows: %v", table, err)
		}
		if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE space = ?", "other").Scan(&otherRows); err != nil {
			t.Fatalf("counting %s other rows: %v", table, err)
		}
		if teamRows != 0 || otherRows == 0 {
			t.Errorf("%s rows after purge: team=%d other=%d", table, teamRows, otherRows)
		}
	}
}

type histogramPoint struct {
	status int64
	count  uint64
	sum    float64
	bucket int
}

func exportRequest(at time.Time, points []histogramPoint) *collectormetricspb.ExportMetricsServiceRequest {
	dataPoints := make([]*metricspb.HistogramDataPoint, 0, len(points))
	for _, point := range points {
		counts := make([]uint64, len(HTTPDurationBounds)+1)
		counts[point.bucket] = point.count
		sum := point.sum
		dataPoints = append(dataPoints, &metricspb.HistogramDataPoint{
			Attributes: []*commonpb.KeyValue{
				stringAttribute("http.request.method", "GET"),
				intAttribute("http.response.status_code", point.status),
				stringAttribute("donkey.space.name", "team"),
				stringAttribute("donkey.app.name", "site"),
			},
			StartTimeUnixNano: uint64(at.Add(-time.Minute).UnixNano()),
			TimeUnixNano:      uint64(at.UnixNano()),
			Count:             point.count,
			Sum:               &sum,
			BucketCounts:      counts,
			ExplicitBounds:    append([]float64(nil), HTTPDurationBounds...),
		})
	}
	return &collectormetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				stringAttribute("service.name", "donkey-gateway"),
			}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: []*metricspb.Metric{{
				Name: HTTPServerRequestDuration,
				Unit: "s",
				Data: &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{
					AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA,
					DataPoints:             dataPoints,
				}},
			}}}},
		}},
	}
}

func stringAttribute(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func intAttribute(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
}
