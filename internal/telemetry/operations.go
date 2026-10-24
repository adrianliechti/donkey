package telemetry

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

// MCP conventions are Development. Keep these buckets separate from HTTP:
// tool execution regularly takes longer than an HTTP page or API response.
// Convention snapshot: semantic-conventions-genai 9ffc7e00c639be7712e544e3e54331e885dccf84,
// docs/gen-ai/mcp.md (reviewed 2026-10-24).
var MCPDurationBounds = []float64{0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300}

type OperationTotals struct {
	Operations       int64   `json:"operations"`
	Errors           int64   `json:"errors"`
	CallerErrors     int64   `json:"callerErrors"`
	AverageLatencyMS float64 `json:"averageLatencyMs"`
	P50LatencyMS     float64 `json:"p50LatencyMs"`
	P95LatencyMS     float64 `json:"p95LatencyMs"`
}

type TrafficEndpoint struct {
	App    string `json:"app"`
	Method string `json:"method"`
	Route  string `json:"route"`
	TrafficTotals
}

type MCPOperation struct {
	App    string `json:"app"`
	Method string `json:"method"`
	Target string `json:"target"`
	OperationTotals
}

// MCP operations and HTTP transport requests are separate measurements.
// Both use the selected source, so legacy app metrics cannot double-count
// the gateway's automatic protocol observations.
type MCPReport struct {
	Source     string          `json:"source"`
	Totals     OperationTotals `json:"totals"`
	Operations []MCPOperation  `json:"operations"`
}

const operationsSchema = `
CREATE TABLE IF NOT EXISTS operation_rollups (
  hour BIGINT NOT NULL,
  kind TEXT NOT NULL,
  source TEXT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  method TEXT NOT NULL,
  target TEXT NOT NULL,
  status TEXT NOT NULL,
  error TEXT NOT NULL,
  request_count BIGINT NOT NULL,
  duration_sum DOUBLE NOT NULL,
  %s,
  PRIMARY KEY (hour, kind, source, space, app, method, target, status, error)
);
CREATE INDEX IF NOT EXISTS operation_rollups_scope_time
  ON operation_rollups (space, app, hour);
`

// Only templates supplied by instrumentation are retained. We never derive
// routes from raw URLs or use resource URIs, arguments or results as targets.
func boundedDimension(value string) string {
	if len(value) > 256 || strings.ContainsFunc(value, unicode.IsControl) {
		return "_OTHER"
	}
	return value
}

func mcpCallerError(code string) bool {
	switch code {
	case "-32700", "-32600", "-32601", "-32602", "-32002":
		return true
	}
	return false
}

func (s *Store) ingestMCPPoint(ctx context.Context, tx *sql.Tx, resources map[string]string, metric *metricspb.Metric,
	histogram *metricspb.Histogram, point *metricspb.HistogramDataPoint, trustedGateway bool, now time.Time) (bool, error) {
	attrs := attributesMap(point.Attributes)
	source, space, app := SourceApp, resources["service.namespace"], resources["service.name"]
	if trustedGateway {
		if resources["service.name"] != "donkey-gateway" {
			return false, nil
		}
		source, space, app = SourceEdge, attrs["donkey.space.name"], attrs["donkey.app.name"]
	}
	if !config.ValidName(space) || !config.ValidName(app) {
		return false, nil
	}
	method := boundedDimension(attrs["mcp.method.name"])
	if method == "" || point.TimeUnixNano == 0 || point.TimeUnixNano > math.MaxInt64 || point.StartTimeUnixNano > math.MaxInt64 ||
		point.Count > math.MaxInt64 || point.Sum == nil || point.Flags&1 != 0 {
		return false, nil
	}
	at := time.Unix(0, int64(point.TimeUnixNano)).UTC()
	if at.Before(now.AddDate(0, 0, -s.retentionDays)) || at.After(now.Add(5*time.Minute)) {
		return false, nil
	}
	factor := metricUnitSecondsFactor(metric.Unit, false)
	if factor == 0 {
		return false, nil
	}
	buckets, valid := histogramBuckets(point.ExplicitBounds, point.BucketCounts, factor, MCPDurationBounds)
	sum := point.GetSum() * factor
	if !valid || bucketTotal(buckets) != point.Count || math.IsNaN(sum) || math.IsInf(sum, 0) || sum < 0 {
		return false, nil
	}
	status := attrs["rpc.response.status_code"]
	if status != "" {
		if _, err := strconv.ParseInt(status, 10, 64); err != nil {
			return false, nil
		}
	}
	errorType := boundedDimension(attrs["error.type"])
	if errorType == "" && status != "" && !mcpCallerError(status) {
		errorType = status
	}
	target := attrs["gen_ai.tool.name"]
	if target == "" {
		target = attrs["gen_ai.prompt.name"]
	}
	target = boundedDimension(target)
	key := MCPServerOperationDuration + ":" + metricStreamKey(source, space, app, method, 0, resources, attrs)
	count, sum, buckets, keep, err := normalizeTemporality(ctx, tx, key, source, space, app,
		histogram.AggregationTemporality, point.StartTimeUnixNano, point.TimeUnixNano, point.Count, sum, buckets, now.Unix())
	if err != nil || !keep || count == 0 {
		return keep, err
	}
	err = upsertOperation(ctx, tx, at.Truncate(time.Hour).Unix(), "mcp", source, space, app, method, target, status, errorType, count, sum, buckets)
	return err == nil, err
}

func upsertOperation(ctx context.Context, tx *sql.Tx, hour int64, kind, source, space, app, method, target, status, errorType string,
	count uint64, sum float64, buckets [httpDurationBucketCount]uint64) error {
	columns := "hour,kind,source,space,app,method,target,status,error,request_count,duration_sum," + bucketColumns(false)
	updates := []string{"request_count=operation_rollups.request_count+excluded.request_count", "duration_sum=operation_rollups.duration_sum+excluded.duration_sum"}
	args := []any{hour, kind, source, space, app, method, target, status, errorType, int64(count), sum}
	for i, n := range buckets {
		updates = append(updates, fmt.Sprintf("b%d=operation_rollups.b%d+excluded.b%d", i, i, i))
		args = append(args, n)
	}
	q := "INSERT INTO operation_rollups (" + columns + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ") " +
		"ON CONFLICT(hour,kind,source,space,app,method,target,status,error) DO UPDATE SET " + strings.Join(updates, ",")
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}

type operationAggregate struct {
	OperationTotals
	sum     float64
	buckets [httpDurationBucketCount]uint64
}

func (a *operationAggregate) add(status, errorType string, count int64, sum float64, buckets [httpDurationBucketCount]uint64) {
	a.Operations += count
	if errorType != "" {
		a.Errors += count
	} else if mcpCallerError(status) {
		a.CallerErrors += count
	}
	a.sum += sum
	for i, n := range buckets {
		a.buckets[i] += n
	}
}

func (a *operationAggregate) finish() OperationTotals {
	if a.Operations > 0 {
		a.AverageLatencyMS = rounded(a.sum/float64(a.Operations)*1000, 1)
		a.P50LatencyMS = durationQuantileMS(a.buckets, MCPDurationBounds, 0.50)
		a.P95LatencyMS = durationQuantileMS(a.buckets, MCPDurationBounds, 0.95)
	}
	return a.OperationTotals
}

func (s *Store) operationBreakdowns(ctx context.Context, report *TrafficReport, window trafficWindow) error {
	where := "space = ? AND hour >= ? AND hour <= ? AND source = ?"
	args := []any{report.Space, window.from.Unix(), window.through.Unix(), report.Source}
	if report.App != "" {
		where += " AND app = ?"
		args = append(args, report.App)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT kind,app,method,target,status,error,request_count,duration_sum,"+bucketColumns(false)+" FROM operation_rollups WHERE "+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct{ app, method, target string }
	endpoints := map[key]*trafficAggregate{}
	operations := map[key]*operationAggregate{}
	total := &operationAggregate{}
	for rows.Next() {
		var kind, app, method, target, status, errorType string
		var count int64
		var sum float64
		var buckets [httpDurationBucketCount]uint64
		dest := []any{&kind, &app, &method, &target, &status, &errorType, &count, &sum}
		for i := range buckets {
			dest = append(dest, &buckets[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		k := key{app, method, target}
		if kind == "http" {
			if endpoints[k] == nil {
				endpoints[k] = &trafficAggregate{}
			}
			code, _ := strconv.Atoi(status)
			endpoints[k].addHTTP(code, uint64(count), sum, buckets)
		} else {
			if operations[k] == nil {
				operations[k] = &operationAggregate{}
			}
			operations[k].add(status, errorType, count, sum, buckets)
			total.add(status, errorType, count, sum, buckets)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	report.Endpoints = make([]TrafficEndpoint, 0, len(endpoints))
	for k, agg := range endpoints {
		agg.finish()
		report.Endpoints = append(report.Endpoints, TrafficEndpoint{App: k.app, Method: k.method, Route: k.target, TrafficTotals: agg.TrafficTotals})
	}
	sort.Slice(report.Endpoints, func(i, j int) bool {
		a, b := report.Endpoints[i], report.Endpoints[j]
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.App+"\x00"+a.Method+"\x00"+a.Route < b.App+"\x00"+b.Method+"\x00"+b.Route
	})
	if len(report.Endpoints) > 25 {
		report.Endpoints = report.Endpoints[:25]
	}
	report.MCP = MCPReport{Source: report.Source, Totals: total.finish(), Operations: make([]MCPOperation, 0, len(operations))}
	for k, agg := range operations {
		report.MCP.Operations = append(report.MCP.Operations, MCPOperation{App: k.app, Method: k.method, Target: k.target, OperationTotals: agg.finish()})
	}
	sort.Slice(report.MCP.Operations, func(i, j int) bool {
		a, b := report.MCP.Operations[i], report.MCP.Operations[j]
		if a.Operations != b.Operations {
			return a.Operations > b.Operations
		}
		return a.App+"\x00"+a.Method+"\x00"+a.Target < b.App+"\x00"+b.Method+"\x00"+b.Target
	})
	if len(report.MCP.Operations) > 25 {
		report.MCP.Operations = report.MCP.Operations[:25]
	}
	return nil
}
