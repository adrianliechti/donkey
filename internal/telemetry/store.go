package telemetry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

type Store struct {
	db            *sql.DB
	retentionDays int
}

func OpenStore(path string, retentionDays int) (*Store, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultRetentionDays
	}
	if path == "" {
		return nil, errors.New("telemetry database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("creating telemetry data directory: %w", err)
		}
	}
	dsn := path
	if path == ":memory:" {
		dsn = ""
	}
	db, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, err
	}
	// A dedicated, single-replica service owns the DuckDB file. One connection
	// also serializes short ingestion and report transactions and keeps an
	// in-memory test database coherent.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening telemetry database: %w", err)
	}
	s := &Store{db: db, retentionDays: retentionDays}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.Cleanup(context.Background(), time.Now()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ready(ctx context.Context) error { return s.db.PingContext(ctx) }

func bucketColumns(definition bool) string {
	cols := make([]string, httpDurationBucketCount)
	for i := range cols {
		if definition {
			cols[i] = fmt.Sprintf("b%d BIGINT NOT NULL DEFAULT 0", i)
		} else {
			cols[i] = fmt.Sprintf("b%d", i)
		}
	}
	return strings.Join(cols, ", ")
}

func (s *Store) migrate(ctx context.Context) error {
	schema := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS http_rollups (
  hour BIGINT NOT NULL,
  source TEXT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  method TEXT NOT NULL,
  status INTEGER NOT NULL,
  request_count BIGINT NOT NULL,
  duration_sum REAL NOT NULL,
  %s,
  PRIMARY KEY (hour, source, space, app, method, status)
);
CREATE INDEX IF NOT EXISTS http_rollups_scope_time
  ON http_rollups (source, space, app, hour);

CREATE TABLE IF NOT EXISTS metric_stream_state (
  stream_key TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  temporality INTEGER NOT NULL,
  start_nano BIGINT NOT NULL,
  time_nano BIGINT NOT NULL,
  request_count BIGINT NOT NULL,
  duration_sum REAL NOT NULL,
  %s,
  last_seen BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS metric_stream_state_seen
  ON metric_stream_state (last_seen);

CREATE TABLE IF NOT EXISTS resource_rollups (
  hour BIGINT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  instance TEXT NOT NULL,
  sample_count BIGINT NOT NULL,
  cpu_sum DOUBLE NOT NULL,
  cpu_max DOUBLE NOT NULL,
  memory_sum DOUBLE NOT NULL,
  memory_max BIGINT NOT NULL,
  last_nano BIGINT NOT NULL,
  last_cpu DOUBLE NOT NULL,
  last_memory BIGINT NOT NULL,
  PRIMARY KEY (hour, space, app, instance)
);
CREATE INDEX IF NOT EXISTS resource_rollups_scope_time
  ON resource_rollups (space, app, hour);

CREATE TABLE IF NOT EXISTS resource_recent_rollups (
  minute BIGINT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  instance TEXT NOT NULL,
  sample_count BIGINT NOT NULL,
  cpu_sum DOUBLE NOT NULL,
  cpu_max DOUBLE NOT NULL,
  memory_sum DOUBLE NOT NULL,
  memory_max BIGINT NOT NULL,
  PRIMARY KEY (minute, space, app, instance)
);
CREATE INDEX IF NOT EXISTS resource_recent_rollups_scope_time
  ON resource_recent_rollups (space, app, minute);

CREATE TABLE IF NOT EXISTS resource_sample_state (
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  instance TEXT NOT NULL,
  time_nano BIGINT NOT NULL,
  last_seen BIGINT NOT NULL,
  PRIMARY KEY (space, app, instance)
);
CREATE INDEX IF NOT EXISTS resource_sample_state_seen
  ON resource_sample_state (last_seen);

CREATE TABLE IF NOT EXISTS view_rollups (
  hour BIGINT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  view_count BIGINT NOT NULL,
  identified_views BIGINT NOT NULL,
  PRIMARY KEY (hour, space, app)
);
CREATE INDEX IF NOT EXISTS view_rollups_scope_time
  ON view_rollups (space, app, hour);

CREATE TABLE IF NOT EXISTS view_visitors (
  hour BIGINT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  app_visitor_hash TEXT NOT NULL,
  space_visitor_hash TEXT NOT NULL,
  PRIMARY KEY (hour, space, app, app_visitor_hash)
);
CREATE INDEX IF NOT EXISTS view_visitors_app_time
  ON view_visitors (space, app, hour);
CREATE INDEX IF NOT EXISTS view_visitors_space_time
  ON view_visitors (space, hour);
`, bucketColumns(true), bucketColumns(true))
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("creating telemetry schema: %w", err)
	}
	// Early telemetry databases did not retain stream scope. Add it in place,
	// then discard only those opaque baselines; rollups remain intact.
	for _, q := range []string{
		"ALTER TABLE metric_stream_state ADD COLUMN IF NOT EXISTS source TEXT DEFAULT ''",
		"ALTER TABLE metric_stream_state ADD COLUMN IF NOT EXISTS space TEXT DEFAULT ''",
		"ALTER TABLE metric_stream_state ADD COLUMN IF NOT EXISTS app TEXT DEFAULT ''",
		"DELETE FROM metric_stream_state WHERE source = '' OR space = '' OR app = ''",
		"CREATE INDEX IF NOT EXISTS metric_stream_state_scope ON metric_stream_state (space, app)",
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("updating telemetry stream scope: %w", err)
		}
	}
	return nil
}

// PurgeSpace atomically removes every retained metric and normalization
// baseline for a logical space. Reusing its name then starts with clean
// reports rather than mixing a new application with up to 30 days of history.
func (s *Store) PurgeSpace(ctx context.Context, space string) error {
	if !config.ValidName(space) {
		return fmt.Errorf("invalid telemetry space %q", space)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM http_rollups WHERE space = ?",
		"DELETE FROM metric_stream_state WHERE space = ?",
		"DELETE FROM resource_rollups WHERE space = ?",
		"DELETE FROM resource_recent_rollups WHERE space = ?",
		"DELETE FROM resource_sample_state WHERE space = ?",
		"DELETE FROM view_rollups WHERE space = ?",
		"DELETE FROM view_visitors WHERE space = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, space); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Cleanup(ctx context.Context, now time.Time) error {
	cutoff := now.UTC().AddDate(0, 0, -s.retentionDays).Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		"DELETE FROM http_rollups WHERE hour < ?",
		"DELETE FROM view_rollups WHERE hour < ?",
		"DELETE FROM view_visitors WHERE hour < ?",
		"DELETE FROM metric_stream_state WHERE last_seen < ?",
		"DELETE FROM resource_rollups WHERE hour < ?",
		"DELETE FROM resource_sample_state WHERE last_seen < ?",
	} {
		if _, err := tx.ExecContext(ctx, q, cutoff); err != nil {
			return err
		}
	}
	// One-minute resource rollups exist only for the short, movement-oriented
	// charts. Long-term hourly rollups above retain the configured 30 days.
	recentCutoff := now.UTC().Add(-48 * time.Hour).Truncate(time.Minute).Unix()
	if _, err := tx.ExecContext(ctx, "DELETE FROM resource_recent_rollups WHERE minute < ?", recentCutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// IngestMetrics retains the stable HTTP server duration histogram and
// deliberately ignores unrelated instruments. This endpoint is an OTLP
// boundary for donkey's traffic analytics, not a general-purpose metrics
// warehouse.
func (s *Store) IngestMetrics(ctx context.Context, req *collectormetricspb.ExportMetricsServiceRequest, trustedGateway bool) (accepted, rejected int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for _, rm := range req.ResourceMetrics {
		if rm == nil {
			rejected++
			continue
		}
		resources := attributesMap(nil)
		if rm.Resource != nil {
			resources = attributesMap(rm.Resource.Attributes)
		}
		for _, sm := range rm.ScopeMetrics {
			if sm == nil {
				rejected++
				continue
			}
			for _, metric := range sm.Metrics {
				if metric == nil {
					rejected++
					continue
				}
				legacy := metric.Name == "http.server.duration"
				if metric.Name != HTTPServerRequestDuration && !legacy {
					continue
				}
				histogram := metric.GetHistogram()
				if histogram == nil {
					rejected += int64(metricDataPointCount(metric))
					continue
				}
				for _, point := range histogram.DataPoints {
					if point == nil {
						rejected++
						continue
					}
					ok, pointErr := s.ingestHTTPPoint(ctx, tx, resources, metric, histogram, point, trustedGateway, legacy, now)
					if pointErr != nil {
						return accepted, rejected, pointErr
					}
					if ok {
						accepted++
					} else {
						rejected++
					}
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return accepted, rejected, nil
}

func metricDataPointCount(metric *metricspb.Metric) int {
	switch data := metric.Data.(type) {
	case *metricspb.Metric_Gauge:
		return len(data.Gauge.DataPoints)
	case *metricspb.Metric_Sum:
		return len(data.Sum.DataPoints)
	case *metricspb.Metric_Histogram:
		return len(data.Histogram.DataPoints)
	case *metricspb.Metric_ExponentialHistogram:
		return len(data.ExponentialHistogram.DataPoints)
	case *metricspb.Metric_Summary:
		return len(data.Summary.DataPoints)
	default:
		return 0
	}
}

func (s *Store) ingestHTTPPoint(
	ctx context.Context,
	tx *sql.Tx,
	resources map[string]string,
	metric *metricspb.Metric,
	histogram *metricspb.Histogram,
	point *metricspb.HistogramDataPoint,
	trustedGateway, legacy bool,
	now time.Time,
) (bool, error) {
	attrs := attributesMap(point.Attributes)
	source, space, app := SourceApp, resources["service.namespace"], resources["service.name"]
	if trustedGateway {
		// Possession of the installation key makes a stream trusted, but it
		// still has to identify itself as the edge. This keeps accidental use
		// of the key by another control-plane component out of app charts.
		if resources["service.name"] != "donkey-gateway" {
			return false, nil
		}
		source = SourceEdge
		space, app = attrs["donkey.space.name"], attrs["donkey.app.name"]
	}
	if !config.ValidName(space) || !config.ValidName(app) {
		return false, nil
	}
	methodKey, statusKey := "http.request.method", "http.response.status_code"
	if legacy {
		methodKey, statusKey = "http.method", "http.status_code"
	}
	method := attrs[methodKey]
	method = canonicalMetricMethod(method)
	status, _ := strconv.Atoi(attrs[statusKey])
	if status < 0 || status > 999 || point.TimeUnixNano == 0 || point.TimeUnixNano > math.MaxInt64 || point.Count > math.MaxInt64 {
		return false, nil
	}
	pointTime := time.Unix(0, int64(point.TimeUnixNano)).UTC()
	if pointTime.Before(now.AddDate(0, 0, -s.retentionDays)) || pointTime.After(now.Add(5*time.Minute)) {
		return false, nil
	}
	factor := metricUnitSecondsFactor(metric.Unit, legacy)
	if factor == 0 || point.Sum == nil {
		return false, nil
	}
	buckets, valid := canonicalBuckets(point.ExplicitBounds, point.BucketCounts, factor)
	if !valid || bucketTotal(buckets) != point.Count {
		return false, nil
	}
	count := point.Count
	sum := point.GetSum() * factor
	if math.IsNaN(sum) || math.IsInf(sum, 0) || sum < 0 {
		return false, nil
	}
	streamKey := metricStreamKey(source, space, app, method, status, resources, attrs)
	deltaCount, deltaSum, deltaBuckets, keep, err := normalizeTemporality(
		ctx, tx, streamKey, source, space, app, histogram.AggregationTemporality,
		point.StartTimeUnixNano, point.TimeUnixNano, count, sum, buckets, now.Unix(),
	)
	if err != nil || !keep {
		return err == nil, err
	}
	if deltaCount == 0 {
		return true, nil
	}
	hour := pointTime.Truncate(time.Hour).Unix()
	if err := upsertHTTPRollup(ctx, tx, hour, source, space, app, method, status, deltaCount, deltaSum, deltaBuckets); err != nil {
		return false, err
	}
	return true, nil
}

func canonicalMetricMethod(method string) string {
	switch method {
	case "CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "QUERY", "TRACE", "_OTHER":
		return method
	default:
		return "_OTHER"
	}
}

func bucketTotal(buckets [httpDurationBucketCount]uint64) uint64 {
	var total uint64
	for _, count := range buckets {
		if math.MaxUint64-total < count {
			return math.MaxUint64
		}
		total += count
	}
	return total
}

func metricUnitSecondsFactor(unit string, legacy bool) float64 {
	switch unit {
	case "s", "":
		if legacy && unit == "" {
			return 0.001
		}
		return 1
	case "ms":
		return 0.001
	default:
		return 0
	}
}

func canonicalBuckets(bounds []float64, counts []uint64, factor float64) ([httpDurationBucketCount]uint64, bool) {
	var out [httpDurationBucketCount]uint64
	if len(bounds) == 0 && len(counts) == 0 {
		return out, true
	}
	if len(counts) != len(bounds)+1 {
		return out, false
	}
	previous := math.Inf(-1)
	for i, count := range counts {
		upper := math.Inf(1)
		if i < len(bounds) {
			upper = bounds[i] * factor
			if math.IsNaN(upper) || upper <= previous {
				return out, false
			}
			previous = upper
		}
		target := len(HTTPDurationBounds)
		if !math.IsInf(upper, 1) {
			target = sort.SearchFloat64s(HTTPDurationBounds, upper)
			if target >= len(HTTPDurationBounds) {
				target = len(HTTPDurationBounds)
			}
		}
		if math.MaxUint64-out[target] < count {
			return out, false
		}
		out[target] += count
	}
	return out, true
}

type streamState struct {
	start   uint64
	time    uint64
	count   uint64
	sum     float64
	buckets [httpDurationBucketCount]uint64
}

func normalizeTemporality(
	ctx context.Context,
	tx *sql.Tx,
	key, source, space, app string,
	temporality metricspb.AggregationTemporality,
	start, end, count uint64,
	sum float64,
	buckets [httpDurationBucketCount]uint64,
	seen int64,
) (uint64, float64, [httpDurationBucketCount]uint64, bool, error) {
	if temporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA &&
		temporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		return 0, 0, [httpDurationBucketCount]uint64{}, false, nil
	}
	current := streamState{start: start, time: end, count: count, sum: sum, buckets: buckets}
	previous, found, err := readStreamState(ctx, tx, key)
	if err != nil {
		return 0, 0, [httpDurationBucketCount]uint64{}, false, err
	}
	if found && end <= previous.time {
		// OTLP exporters retry a delta when the response is lost. End time is
		// monotonic for one writer, so this makes the retry idempotent.
		return 0, 0, [httpDurationBucketCount]uint64{}, true, nil
	}
	if err := writeStreamState(ctx, tx, key, source, space, app, temporality, current, seen); err != nil {
		return 0, 0, [httpDurationBucketCount]uint64{}, false, err
	}
	if temporality == metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
		return count, sum, buckets, true, nil
	}
	// The first cumulative snapshot establishes a baseline. Counting it would
	// place an arbitrary process lifetime into the current hour. A reset does
	// the same; at most one export interval is omitted.
	if !found || previous.start != start || count < previous.count || sum < previous.sum {
		return 0, 0, [httpDurationBucketCount]uint64{}, true, nil
	}
	var delta [httpDurationBucketCount]uint64
	for i := range buckets {
		if buckets[i] < previous.buckets[i] {
			return 0, 0, [httpDurationBucketCount]uint64{}, true, nil
		}
		delta[i] = buckets[i] - previous.buckets[i]
	}
	return count - previous.count, sum - previous.sum, delta, true, nil
}

func readStreamState(ctx context.Context, tx *sql.Tx, key string) (streamState, bool, error) {
	columns := "start_nano, time_nano, request_count, duration_sum, " + bucketColumns(false)
	row := tx.QueryRowContext(ctx, "SELECT "+columns+" FROM metric_stream_state WHERE stream_key = ?", key)
	var state streamState
	var start, end, count int64
	dest := []any{&start, &end, &count, &state.sum}
	for i := range state.buckets {
		dest = append(dest, &state.buckets[i])
	}
	if err := row.Scan(dest...); errors.Is(err, sql.ErrNoRows) {
		return state, false, nil
	} else if err != nil {
		return state, false, err
	}
	if start < 0 || end < 0 || count < 0 {
		return streamState{}, false, errors.New("invalid negative metric stream state")
	}
	state.start, state.time, state.count = uint64(start), uint64(end), uint64(count)
	return state, true, nil
}

func writeStreamState(ctx context.Context, tx *sql.Tx, key, source, space, app string, temporality metricspb.AggregationTemporality, state streamState, seen int64) error {
	if state.start > math.MaxInt64 || state.time > math.MaxInt64 || state.count > math.MaxInt64 {
		return errors.New("metric stream value exceeds DuckDB BIGINT range")
	}
	columns := "stream_key, source, space, app, temporality, start_nano, time_nano, request_count, duration_sum, " + bucketColumns(false) + ", last_seen"
	placeholders := strings.TrimSuffix(strings.Repeat("?,", 9+httpDurationBucketCount+1), ",")
	updates := []string{
		"source=excluded.source", "space=excluded.space", "app=excluded.app",
		"temporality=excluded.temporality", "start_nano=excluded.start_nano", "time_nano=excluded.time_nano",
		"request_count=excluded.request_count", "duration_sum=excluded.duration_sum", "last_seen=excluded.last_seen",
	}
	for i := 0; i < httpDurationBucketCount; i++ {
		updates = append(updates, fmt.Sprintf("b%d=excluded.b%d", i, i))
	}
	q := "INSERT INTO metric_stream_state (" + columns + ") VALUES (" + placeholders + ") " +
		"ON CONFLICT(stream_key) DO UPDATE SET " + strings.Join(updates, ",")
	args := []any{key, source, space, app, int(temporality), int64(state.start), int64(state.time), int64(state.count), state.sum}
	for _, n := range state.buckets {
		if n > math.MaxInt64 {
			return errors.New("metric bucket exceeds DuckDB BIGINT range")
		}
		args = append(args, int64(n))
	}
	args = append(args, seen)
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}

func upsertHTTPRollup(
	ctx context.Context,
	tx *sql.Tx,
	hour int64,
	source, space, app, method string,
	status int,
	count uint64,
	sum float64,
	buckets [httpDurationBucketCount]uint64,
) error {
	if count > math.MaxInt64 {
		return errors.New("request count exceeds DuckDB BIGINT range")
	}
	columns := "hour, source, space, app, method, status, request_count, duration_sum, " + bucketColumns(false)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", 8+httpDurationBucketCount), ",")
	updates := []string{
		"request_count=http_rollups.request_count+excluded.request_count",
		"duration_sum=http_rollups.duration_sum+excluded.duration_sum",
	}
	for i := 0; i < httpDurationBucketCount; i++ {
		updates = append(updates, fmt.Sprintf("b%d=http_rollups.b%d+excluded.b%d", i, i, i))
	}
	q := "INSERT INTO http_rollups (" + columns + ") VALUES (" + placeholders + ") " +
		"ON CONFLICT(hour,source,space,app,method,status) DO UPDATE SET " + strings.Join(updates, ",")
	args := []any{hour, source, space, app, method, status, int64(count), sum}
	for _, n := range buckets {
		if n > math.MaxInt64 {
			return errors.New("request bucket exceeds DuckDB BIGINT range")
		}
		args = append(args, int64(n))
	}
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}

func metricStreamKey(source, space, app, method string, status int, resources, attrs map[string]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\x00", source, space, app, method, status)
	writeCanonicalMap(h, resources)
	writeCanonicalMap(h, attrs)
	return hex.EncodeToString(h.Sum(nil))
}

type stringWriter interface{ Write([]byte) (int, error) }

func writeCanonicalMap(w stringWriter, values map[string]string) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(w, "%s=%s\x00", key, values[key])
	}
}

func attributesMap(attrs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		if attr == nil || attr.Key == "" || attr.Value == nil {
			continue
		}
		out[attr.Key] = anyValueString(attr.Value)
	}
	return out
}

func anyValueString(value *commonpb.AnyValue) string {
	switch v := value.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return v.StringValue
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(v.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(v.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(v.BoolValue)
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(v.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, 0, len(v.ArrayValue.Values))
		for _, item := range v.ArrayValue.Values {
			parts = append(parts, anyValueString(item))
		}
		return strings.Join(parts, ",")
	case *commonpb.AnyValue_KvlistValue:
		return canonicalAttributes(v.KvlistValue.Values)
	default:
		return ""
	}
}

func canonicalAttributes(attrs []*commonpb.KeyValue) string {
	m := attributesMap(attrs)
	var b strings.Builder
	writeCanonicalMap(&b, m)
	return b.String()
}

func resourceMap(resource *resourcepb.Resource) map[string]string {
	if resource == nil {
		return map[string]string{}
	}
	return attributesMap(resource.Attributes)
}

func (s *Store) InsertViews(ctx context.Context, batch ViewBatch, now time.Time) (accepted, rejected int, err error) {
	if len(batch.Events) > 5000 {
		return 0, len(batch.Events), errors.New("view batch exceeds 5000 events")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	cutoff := now.UTC().AddDate(0, 0, -s.retentionDays).Unix()
	future := now.UTC().Add(5 * time.Minute).Unix()
	for _, event := range batch.Events {
		if event.TimeUnixNano > math.MaxInt64 {
			rejected++
			continue
		}
		happened := time.Unix(0, int64(event.TimeUnixNano)).UTC().Unix()
		if event.TimeUnixNano == 0 {
			happened = now.UTC().Unix()
		}
		if !config.ValidName(event.Space) || !config.ValidName(event.App) || happened < cutoff || happened > future ||
			!validVisitorHash(event.AppVisitorHash) || !validVisitorHash(event.SpaceVisitorHash) {
			rejected++
			continue
		}
		hour := time.Unix(happened, 0).UTC().Truncate(time.Hour).Unix()
		identified := 0
		if event.AppVisitorHash != "" {
			identified = 1
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO view_rollups (hour, space, app, view_count, identified_views)
VALUES (?, ?, ?, 1, ?)
ON CONFLICT(hour, space, app) DO UPDATE SET
  view_count=view_rollups.view_count+excluded.view_count,
  identified_views=view_rollups.identified_views+excluded.identified_views`, hour, event.Space, event.App, identified); err != nil {
			return accepted, rejected, err
		}
		if identified == 1 {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO view_visitors (hour, space, app, app_visitor_hash, space_visitor_hash)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(hour, space, app, app_visitor_hash) DO NOTHING`, hour, event.Space, event.App, event.AppVisitorHash, event.SpaceVisitorHash); err != nil {
				return accepted, rejected, err
			}
		}
		accepted++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return accepted, rejected, nil
}

func validVisitorHash(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type trafficWindow struct {
	selected    TrafficRange
	granularity string
	from        time.Time
	to          time.Time
	lastBucket  time.Time
	through     time.Time
	buckets     []string
}

func newTrafficWindow(selected TrafficRange, now time.Time, retentionDays int) (trafficWindow, error) {
	parsed, err := ParseTrafficRange(string(selected))
	if err != nil {
		return trafficWindow{}, err
	}
	now = now.UTC()
	window := trafficWindow{selected: parsed, to: now, through: now.Truncate(time.Hour)}
	var count int
	switch parsed {
	case Range48Hours:
		if retentionDays < 2 {
			return trafficWindow{}, fmt.Errorf("traffic history is retained for %d days", retentionDays)
		}
		window.granularity = "hour"
		window.lastBucket = now.Truncate(time.Hour)
		window.from = window.lastBucket.Add(-47 * time.Hour)
		count = 48
	case Range7Days, Range30Days:
		days := 7
		if parsed == Range30Days {
			days = 30
		}
		if retentionDays < days {
			return trafficWindow{}, fmt.Errorf("traffic history is retained for %d days", retentionDays)
		}
		window.granularity = "day"
		window.lastBucket = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		window.from = window.lastBucket.AddDate(0, 0, -(days - 1))
		count = days
	}
	window.buckets = make([]string, 0, count)
	for at := window.from; !at.After(window.lastBucket); {
		window.buckets = append(window.buckets, window.key(at))
		if window.granularity == "hour" {
			at = at.Add(time.Hour)
		} else {
			at = at.AddDate(0, 0, 1)
		}
	}
	return window, nil
}

func (w trafficWindow) key(at time.Time) string {
	if w.granularity == "hour" {
		return at.UTC().Truncate(time.Hour).Format(time.RFC3339)
	}
	return at.UTC().Format(time.DateOnly)
}

func (s *Store) Traffic(ctx context.Context, space, app, source string, selected TrafficRange, now time.Time) (TrafficReport, error) {
	if !config.ValidName(space) || app != "" && !config.ValidName(app) {
		return TrafficReport{}, errors.New("invalid traffic scope")
	}
	if source == "" {
		source = SourceEdge
	}
	if source != SourceEdge && source != SourceApp {
		return TrafficReport{}, fmt.Errorf("unknown traffic source %q", source)
	}
	window, err := newTrafficWindow(selected, now, s.retentionDays)
	if err != nil {
		return TrafficReport{}, err
	}

	series := make(map[string]*trafficAggregate, len(window.buckets))
	for _, bucket := range window.buckets {
		series[bucket] = &trafficAggregate{}
	}
	total := &trafficAggregate{}
	where, args := "source = ? AND space = ? AND hour >= ? AND hour <= ?", []any{
		source, space, window.from.Unix(), window.through.Unix(),
	}
	if app != "" {
		where += " AND app = ?"
		args = append(args, app)
	}
	q := "SELECT hour, status, request_count, duration_sum, " + bucketColumns(false) + " FROM http_rollups WHERE " + where
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return TrafficReport{}, err
	}
	for rows.Next() {
		var hour, count int64
		var status int
		var sum float64
		var buckets [httpDurationBucketCount]uint64
		dest := []any{&hour, &status, &count, &sum}
		for i := range buckets {
			dest = append(dest, &buckets[i])
		}
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return TrafficReport{}, err
		}
		if count < 0 {
			continue
		}
		bucket := window.key(time.Unix(hour, 0))
		agg := series[bucket]
		if agg == nil {
			continue
		}
		agg.addHTTP(status, uint64(count), sum, buckets)
		total.addHTTP(status, uint64(count), sum, buckets)
	}
	if err := rows.Close(); err != nil {
		return TrafficReport{}, err
	}

	if source == SourceEdge {
		viewWhere, viewArgs := "space = ? AND hour >= ? AND hour <= ?", []any{
			space, window.from.Unix(), window.through.Unix(),
		}
		if app != "" {
			viewWhere += " AND app = ?"
			viewArgs = append(viewArgs, app)
		}
		viewRows, err := s.db.QueryContext(ctx,
			"SELECT hour, view_count, identified_views FROM view_rollups WHERE "+viewWhere, viewArgs...)
		if err != nil {
			return TrafficReport{}, err
		}
		for viewRows.Next() {
			var hour, views, identified int64
			if err := viewRows.Scan(&hour, &views, &identified); err != nil {
				viewRows.Close()
				return TrafficReport{}, err
			}
			if agg := series[window.key(time.Unix(hour, 0))]; agg != nil {
				agg.Views += views
				agg.IdentifiedViews += identified
			}
			total.Views += views
			total.IdentifiedViews += identified
		}
		if err := viewRows.Close(); err != nil {
			return TrafficReport{}, err
		}

		visitorColumn := "space_visitor_hash"
		if app != "" {
			visitorColumn = "app_visitor_hash"
		}
		uniqueRows, err := s.db.QueryContext(ctx,
			"SELECT hour, "+visitorColumn+" FROM view_visitors WHERE "+viewWhere, viewArgs...)
		if err != nil {
			return TrafficReport{}, err
		}
		perBucket := make(map[string]map[string]struct{}, len(window.buckets))
		all := make(map[string]struct{})
		for uniqueRows.Next() {
			var hour int64
			var visitor string
			if err := uniqueRows.Scan(&hour, &visitor); err != nil {
				uniqueRows.Close()
				return TrafficReport{}, err
			}
			bucket := window.key(time.Unix(hour, 0))
			if series[bucket] == nil {
				continue
			}
			if perBucket[bucket] == nil {
				perBucket[bucket] = make(map[string]struct{})
			}
			perBucket[bucket][visitor] = struct{}{}
			all[visitor] = struct{}{}
		}
		if err := uniqueRows.Close(); err != nil {
			return TrafficReport{}, err
		}
		for bucket, visitors := range perBucket {
			series[bucket].UniqueVisitors = int64(len(visitors))
		}
		total.UniqueVisitors = int64(len(all))
	}

	total.finish()
	report := TrafficReport{
		Space: space, App: app, Source: source, Range: window.selected, Granularity: window.granularity,
		From: window.from, To: window.to,
		Totals: total.TrafficTotals,
		Series: make([]TrafficPoint, 0, len(window.buckets)),
		Definitions: Definitions{
			Success:        "HTTP status 200–399; 4xx and 5xx are shown separately",
			View:           "a successful top-level HTML document request observed at donkey-gateway",
			UniqueVisitors: "distinct signed-in users, HMAC-pseudonymized per app and per space; anonymous visitors receive no tracking cookie",
		},
	}
	for _, bucket := range window.buckets {
		agg := series[bucket]
		agg.finish()
		report.Series = append(report.Series, pointFromAggregate(bucket, agg.TrafficTotals))
	}
	return report, nil
}

func pointFromAggregate(bucket string, totals TrafficTotals) TrafficPoint {
	return TrafficPoint{
		Bucket:   bucket,
		Requests: totals.Requests, Successful: totals.Successful,
		ClientErrors: totals.ClientErrors, ServerErrors: totals.ServerErrors, OtherResponses: totals.OtherResponses,
		AverageLatencyMS: totals.AverageLatencyMS, P50LatencyMS: totals.P50LatencyMS, P95LatencyMS: totals.P95LatencyMS,
		Views: totals.Views, UniqueVisitors: totals.UniqueVisitors, IdentifiedViews: totals.IdentifiedViews,
		VisitorCoveragePC: totals.VisitorCoveragePC,
	}
}
