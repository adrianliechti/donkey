package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func operationStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(":memory:", DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mcpMetrics(at time.Time, code, errorType string, count uint64) *metricsv1.ExportMetricsServiceRequest {
	req := exportRequest(at, []histogramPoint{{count: count, sum: float64(count) * 30, bucket: 10}})
	req.ResourceMetrics[0].Resource.Attributes = []*commonpb.KeyValue{
		stringAttribute("service.namespace", "team"), stringAttribute("service.name", "site"),
	}
	m := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0]
	m.Name = MCPServerOperationDuration
	p := m.GetHistogram().DataPoints[0]
	p.ExplicitBounds = append([]float64(nil), MCPDurationBounds...)
	p.Attributes = []*commonpb.KeyValue{
		stringAttribute("mcp.method.name", "tools/call"), stringAttribute("gen_ai.tool.name", "weather"),
	}
	if code != "" {
		p.Attributes = append(p.Attributes, stringAttribute("rpc.response.status_code", code))
	}
	if errorType != "" {
		p.Attributes = append(p.Attributes, stringAttribute("error.type", errorType))
	}
	return req
}

func TestMCPMetricsOutcomesRetriesAndSourceSeparation(t *testing.T) {
	s := operationStore(t)
	ctx, now := context.Background(), time.Now().UTC().Add(-time.Minute)
	for _, sample := range []struct {
		code, errorType string
		count           uint64
	}{
		{"", "", 5}, {"", "tool_error", 2}, {"-32602", "", 3}, {"-32603", "", 1},
	} {
		req := mcpMetrics(now, sample.code, sample.errorType, sample.count)
		for range 2 {
			accepted, rejected, err := s.IngestMetrics(ctx, req, false)
			if err != nil || accepted != 1 || rejected != 0 {
				t.Fatalf("ingest=%d/%d: %v", accepted, rejected, err)
			}
		}
	}
	for _, source := range []string{SourceEdge, SourceApp} {
		r, err := s.Traffic(ctx, "team", "site", source, Range48Hours, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if r.Totals.Requests != 0 || r.MCP.Source != SourceApp || r.MCP.Totals.Operations != 11 || r.MCP.Totals.Errors != 3 || r.MCP.Totals.CallerErrors != 3 {
			t.Fatalf("HTTP and MCP outcomes: %+v", r)
		}
		if r.MCP.Totals.P95LatencyMS != 30000 || r.MCP.Totals.AverageLatencyMS != 30000 || len(r.MCP.Operations) != 1 || r.MCP.Operations[0].Target != "weather" {
			t.Fatalf("MCP buckets or grouping: %+v", r.MCP)
		}
	}
	if a, r, err := s.IngestMetrics(ctx, mcpMetrics(now, "", "", 1), true); err != nil || a != 0 || r != 1 {
		t.Fatalf("gateway must not forge MCP operations: %d/%d %v", a, r, err)
	}
}

func TestMCPCumulativeAndInvalidMetrics(t *testing.T) {
	s := operationStore(t)
	ctx, now := context.Background(), time.Now().UTC().Add(-time.Minute)
	first := mcpMetrics(now, "", "", 10)
	first.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().AggregationTemporality = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
	second := proto.Clone(first).(*metricsv1.ExportMetricsServiceRequest)
	p := second.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[0]
	p.TimeUnixNano += uint64(time.Second)
	p.Count, p.BucketCounts[10] = 13, 13
	*p.Sum = 390
	for _, req := range []*metricsv1.ExportMetricsServiceRequest{first, second, second} {
		if a, r, err := s.IngestMetrics(ctx, req, false); err != nil || a != 1 || r != 0 {
			t.Fatalf("cumulative=%d/%d %v", a, r, err)
		}
	}
	r, err := s.Traffic(ctx, "team", "site", SourceApp, Range7Days, time.Now())
	if err != nil || r.MCP.Totals.Operations != 3 {
		t.Fatalf("cumulative report=%+v %v", r.MCP, err)
	}
	for _, mutate := range []func(*metricspb.Metric){
		func(m *metricspb.Metric) { m.GetHistogram().DataPoints[0].Attributes = nil },
		func(m *metricspb.Metric) { m.GetHistogram().DataPoints[0].Count = 100 },
		func(m *metricspb.Metric) { m.GetHistogram().DataPoints[0].TimeUnixNano = 0 },
		func(m *metricspb.Metric) { *m.GetHistogram().DataPoints[0].Sum = math.Inf(1) },
		func(m *metricspb.Metric) { m.Unit = "By" },
		func(m *metricspb.Metric) {
			m.GetHistogram().AggregationTemporality = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_UNSPECIFIED
		},
	} {
		req := mcpMetrics(now, "", "", 1)
		mutate(req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0])
		if a, r, err := s.IngestMetrics(ctx, req, false); err != nil || a != 0 || r != 1 {
			t.Fatalf("invalid metric=%d/%d %v", a, r, err)
		}
	}
}

func TestHTTPRouteBreakdownsKeepTemplatesAndSourcesSeparate(t *testing.T) {
	s := operationStore(t)
	ctx, now := context.Background(), time.Now().UTC().Add(-time.Minute)
	for i, route := range []string{"/users/{id}", "/health", ""} {
		req := exportRequest(now, []histogramPoint{{status: 200, count: uint64(i + 1), sum: 0.1, bucket: 5}})
		p := req.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[0]
		p.Attributes = append(p.Attributes, stringAttribute("http.route", route), stringAttribute("url.path", "/users/private-person"))
		for range 2 {
			if _, _, err := s.IngestMetrics(ctx, req, true); err != nil {
				t.Fatal(err)
			}
		}
		req.ResourceMetrics[0].Resource.Attributes = []*commonpb.KeyValue{stringAttribute("service.namespace", "team"), stringAttribute("service.name", "site")}
		if _, _, err := s.IngestMetrics(ctx, req, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{SourceEdge, SourceApp} {
		r, err := s.Traffic(ctx, "team", "site", source, Range48Hours, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if r.Totals.Requests != 6 || len(r.Endpoints) != 3 || r.Endpoints[0].Route != "" || r.Endpoints[2].Route != "/users/{id}" {
			t.Fatalf("route report=%+v", r)
		}
	}
	var leaked int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM operation_rollups WHERE target LIKE '%private-person%'").Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("raw path leaked: %d %v", leaked, err)
	}
}

func TestCallerDeduplicationValidationRetentionAndPurge(t *testing.T) {
	s := operationStore(t)
	ctx, now := context.Background(), time.Now().UTC().Add(-time.Minute)
	key := bytes.Repeat([]byte{7}, 32)
	batch := CallerBatch{}
	for _, app := range []string{"site", "api"} {
		for _, at := range []time.Time{now.Add(-time.Hour), now} {
			a, b := VisitorHashes(key, "team", app, "ada")
			batch.Events = append(batch.Events, CallerEvent{uint64(at.UnixNano()), "team", app, a, b})
		}
	}
	for range 2 {
		if a, r, err := s.InsertCallers(ctx, batch, now); err != nil || a != 4 || r != 0 {
			t.Fatalf("callers=%d/%d %v", a, r, err)
		}
	}
	for _, app := range []string{"", "site", "api"} {
		r, err := s.Traffic(ctx, "team", app, SourceEdge, Range48Hours, now)
		if err != nil || r.Totals.AuthenticatedCallers != 1 || r.Series[47].AuthenticatedCallers != 1 || r.Series[46].AuthenticatedCallers != 1 {
			t.Fatalf("caller report=%+v %v", r, err)
		}
		r, err = s.Traffic(ctx, "team", app, SourceEdge, Range7Days, now)
		if err != nil || r.Series[6].AuthenticatedCallers != 1 {
			t.Fatalf("daily callers=%+v %v", r, err)
		}
	}
	invalid := batch.Events[0]
	invalid.UserHash = ""
	if a, r, err := s.InsertCallers(ctx, CallerBatch{Events: []CallerEvent{invalid}}, now); err != nil || a != 0 || r != 1 {
		t.Fatalf("anonymous=%d/%d %v", a, r, err)
	}
	if err := s.Cleanup(ctx, now.AddDate(0, 0, 31)); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM caller_observations").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("cleanup=%d %v", remaining, err)
	}
	if _, _, err := s.InsertCallers(ctx, batch, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IngestMetrics(ctx, mcpMetrics(now, "", "", 1), false); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeSpace(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"caller_observations", "operation_rollups", "metric_stream_state"} {
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&remaining); err != nil || remaining != 0 {
			t.Fatalf("purge %s=%d %v", table, remaining, err)
		}
	}
}

func TestCallerEndpointRequiresGatewayTokenAndMCPUsesAppScope(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	service, err := NewService(&Config{DatabasePath: ":memory:", Key: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.store.Close() })
	now := time.Now().UTC()
	a, b := VisitorHashes(key, "team", "site", "ada")
	body, _ := json.Marshal(CallerBatch{Events: []CallerEvent{{uint64(now.UnixNano()), "team", "site", a, b}}})
	for _, tc := range []struct {
		token  string
		status int
	}{{AppToken(key, "team", "site"), 401}, {GatewayToken(key), 200}} {
		r := httptest.NewRequest(http.MethodPost, "/v1/donkey/callers", bytes.NewReader(body))
		r.Header.Set(TelemetryTokenHeader, tc.token)
		w := httptest.NewRecorder()
		service.ingestHandler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("callers status=%d: %s", w.Code, w.Body.String())
		}
	}
	body, _ = proto.Marshal(mcpMetrics(now, "", "", 1))
	for _, tc := range []struct {
		app    string
		status int
	}{{"other", 401}, {"site", 200}} {
		r := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/x-protobuf")
		r.Header.Set(TelemetryTokenHeader, AppToken(key, "team", tc.app))
		w := httptest.NewRecorder()
		service.ingestHandler().ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("MCP status=%d: %s", w.Code, w.Body.String())
		}
		if tc.status == 200 {
			var response metricsv1.ExportMetricsServiceResponse
			if err := proto.Unmarshal(w.Body.Bytes(), &response); err != nil || response.PartialSuccess != nil {
				t.Fatalf("MCP response=%+v %v", &response, err)
			}
		}
	}
}
