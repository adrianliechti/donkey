package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	metricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/adrianliechti/donkey/internal/config"
)

type storedResourceObservation struct {
	ResourceObservation
	timeNano int64
}

type resourceGauge struct {
	value   float64
	at      uint64
	present bool
}

// IngestResourceMetrics accepts only the gauges written with RuntimeToken.
// One ResourceMetrics message becomes one atomic container observation; app
// reports aggregate the fresh container identities after ingestion.
func (s *Store) IngestResourceMetrics(ctx context.Context, req *metricsv1.ExportMetricsServiceRequest) (accepted, rejected int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	for _, rm := range req.ResourceMetrics {
		observation, known, unknown, ok := parseResourceMetrics(rm)
		if !ok || !validResourceObservation(observation, now, s.retentionDays) {
			rejected += known + unknown
			if known+unknown == 0 {
				rejected++
			}
			continue
		}
		if err := ingestResourceObservation(ctx, tx, observation, now.Unix()); err != nil {
			return accepted, rejected, err
		}
		accepted += known
		rejected += unknown
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return accepted, rejected, nil
}

func parseResourceMetrics(rm *metricspb.ResourceMetrics) (storedResourceObservation, int64, int64, bool) {
	if rm == nil {
		return storedResourceObservation{}, 0, 0, false
	}
	resources := resourceMap(rm.Resource)
	observation := storedResourceObservation{ResourceObservation: ResourceObservation{
		Space: resources["service.namespace"], App: resources["service.name"],
		Instance: resources["service.instance.id"], ContainerID: resources["container.id"],
		ContainerName: resources["container.name"], KubeNamespace: resources["k8s.namespace.name"],
		KubePodName: resources["k8s.pod.name"], KubePodUID: resources["k8s.pod.uid"],
	}}
	knownNames := map[string]string{
		ContainerCPUUsage:         "{cpu}",
		ContainerMemoryWorkingSet: "By",
	}
	gauges := make(map[string]resourceGauge, len(knownNames))
	var known, unknown int64
	valid := true
	for _, sm := range rm.ScopeMetrics {
		if sm == nil {
			valid = false
			continue
		}
		for _, metric := range sm.Metrics {
			if metric == nil {
				valid = false
				continue
			}
			count := int64(metricDataPointCount(metric))
			expectedUnit, recognized := knownNames[metric.Name]
			if !recognized {
				unknown += count
				continue
			}
			known += count
			value, ok := resourceMetricGauge(metric, expectedUnit, metric.Name == ContainerCPUUsage)
			if !ok || gauges[metric.Name].present {
				valid = false
				continue
			}
			gauges[metric.Name] = value
		}
	}
	cpu, memory := gauges[ContainerCPUUsage], gauges[ContainerMemoryWorkingSet]
	if !valid || !cpu.present || !memory.present || cpu.at == 0 || cpu.at != memory.at {
		return observation, known, unknown, false
	}
	for _, gauge := range gauges {
		if gauge.present && gauge.at != cpu.at {
			return observation, known, unknown, false
		}
	}
	if cpu.at > math.MaxInt64 || memory.value > math.MaxInt64 || memory.value != math.Trunc(memory.value) {
		return observation, known, unknown, false
	}
	observation.timeNano = int64(cpu.at)
	observation.SampledAt = time.Unix(0, observation.timeNano).UTC()
	observation.CPUCores = cpu.value
	observation.MemoryBytes = int64(memory.value)
	return observation, known, unknown, true
}

func resourceMetricGauge(metric *metricspb.Metric, expectedUnit string, cpuUsage bool) (resourceGauge, bool) {
	if metric.Unit != expectedUnit {
		return resourceGauge{}, false
	}
	if cpuUsage {
		gauge := metric.GetGauge()
		if gauge == nil || len(gauge.DataPoints) == 0 {
			return resourceGauge{}, false
		}
		var result resourceGauge
		modes := map[string]bool{}
		modePoints, totalPoints := false, false
		for _, point := range gauge.DataPoints {
			value, at, ok := resourceNumberPoint(point)
			if !ok || result.present && result.at != at {
				return resourceGauge{}, false
			}
			attrs := attributesMap(point.Attributes)
			mode, hasMode := attrs["cpu.mode"]
			if hasMode {
				modePoints = true
				if (mode != "user" && mode != "system") || modes[mode] {
					return resourceGauge{}, false
				}
				modes[mode] = true
			} else {
				totalPoints = true
			}
			if modePoints && totalPoints {
				return resourceGauge{}, false
			}
			result.value += value
			result.at, result.present = at, true
		}
		if totalPoints && len(gauge.DataPoints) != 1 {
			return resourceGauge{}, false
		}
		return result, true
	}
	sum := metric.GetSum()
	if sum == nil || sum.IsMonotonic ||
		sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE ||
		len(sum.DataPoints) != 1 {
		return resourceGauge{}, false
	}
	value, at, ok := resourceNumberPoint(sum.DataPoints[0])
	return resourceGauge{value: value, at: at, present: ok}, ok
}

func resourceNumberPoint(point *metricspb.NumberDataPoint) (float64, uint64, bool) {
	if point == nil {
		return 0, 0, false
	}
	var value float64
	switch number := point.Value.(type) {
	case *metricspb.NumberDataPoint_AsDouble:
		value = number.AsDouble
	case *metricspb.NumberDataPoint_AsInt:
		value = float64(number.AsInt)
	default:
		return 0, 0, false
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || point.TimeUnixNano == 0 {
		return 0, 0, false
	}
	return value, point.TimeUnixNano, true
}

func validResourceObservation(observation storedResourceObservation, now time.Time, retentionDays int) bool {
	return config.ValidName(observation.Space) && config.ValidName(observation.App) &&
		observation.Instance != "" &&
		observation.CPUCores >= 0 && observation.MemoryBytes >= 0 &&
		!observation.SampledAt.Before(now.AddDate(0, 0, -retentionDays)) &&
		!observation.SampledAt.After(now.Add(5*time.Minute))
}

func ingestResourceObservation(ctx context.Context, tx *sql.Tx, observation storedResourceObservation, seen int64) error {
	var previous int64
	err := tx.QueryRowContext(ctx,
		"SELECT time_nano FROM resource_sample_state WHERE space = ? AND app = ? AND instance = ?",
		observation.Space, observation.App, observation.Instance,
	).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && observation.timeNano <= previous {
		_, updateErr := tx.ExecContext(ctx,
			"UPDATE resource_sample_state SET last_seen = ? WHERE space = ? AND app = ? AND instance = ?",
			seen, observation.Space, observation.App, observation.Instance)
		return updateErr
	}

	hour := observation.SampledAt.Truncate(time.Hour).Unix()
	_, err = tx.ExecContext(ctx, `
INSERT INTO resource_rollups (
  hour, space, app, instance, sample_count, cpu_sum, cpu_max, memory_sum, memory_max,
  last_nano, last_cpu, last_memory
) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(hour, space, app, instance) DO UPDATE SET
  sample_count=resource_rollups.sample_count+1,
  cpu_sum=resource_rollups.cpu_sum+excluded.cpu_sum,
  cpu_max=greatest(resource_rollups.cpu_max, excluded.cpu_max),
  memory_sum=resource_rollups.memory_sum+excluded.memory_sum,
  memory_max=greatest(resource_rollups.memory_max, excluded.memory_max),
  last_nano=excluded.last_nano,
  last_cpu=excluded.last_cpu,
  last_memory=excluded.last_memory`,
		hour, observation.Space, observation.App, observation.Instance,
		observation.CPUCores, observation.CPUCores, float64(observation.MemoryBytes), observation.MemoryBytes,
		observation.timeNano, observation.CPUCores, observation.MemoryBytes,
	)
	if err != nil {
		return err
	}
	minute := observation.SampledAt.Truncate(time.Minute).Unix()
	_, err = tx.ExecContext(ctx, `
INSERT INTO resource_recent_rollups (
  minute, space, app, instance, sample_count, cpu_sum, cpu_max, memory_sum, memory_max
) VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)
ON CONFLICT(minute, space, app, instance) DO UPDATE SET
  sample_count=resource_recent_rollups.sample_count+1,
  cpu_sum=resource_recent_rollups.cpu_sum+excluded.cpu_sum,
  cpu_max=greatest(resource_recent_rollups.cpu_max, excluded.cpu_max),
  memory_sum=resource_recent_rollups.memory_sum+excluded.memory_sum,
  memory_max=greatest(resource_recent_rollups.memory_max, excluded.memory_max)`,
		minute, observation.Space, observation.App, observation.Instance,
		observation.CPUCores, observation.CPUCores, float64(observation.MemoryBytes), observation.MemoryBytes,
	)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO resource_sample_state (space, app, instance, time_nano, last_seen)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(space, app, instance) DO UPDATE SET
  time_nano=excluded.time_nano,
  last_seen=excluded.last_seen`, observation.Space, observation.App, observation.Instance, observation.timeNano, seen)
	return err
}

type resourceAggregate struct {
	samples   int64
	cpuSum    float64
	cpuMax    float64
	memorySum float64
	memoryMax int64
}

const resourceFreshFor = 2 * time.Minute
const resourceCollectionWindow = 45 * time.Second

type latestResourceInstance struct {
	nano   int64
	cpu    float64
	memory int64
}

type resourceWindow struct {
	selected    ResourceRange
	granularity string
	from        time.Time
	to          time.Time
	lastBucket  time.Time
	through     time.Time
	step        time.Duration
	recent      bool
	buckets     []string
}

func newResourceWindow(selected ResourceRange, now time.Time, retentionDays int) (resourceWindow, error) {
	parsed, err := ParseResourceRange(string(selected))
	if err != nil {
		return resourceWindow{}, err
	}
	now = now.UTC()
	window := resourceWindow{selected: parsed, to: now}
	var count int
	switch parsed {
	case ResourceRange1Hour:
		window.granularity, window.step, count = "minute", time.Minute, 60
	case ResourceRange4Hours:
		window.granularity, window.step, count = "5m", 5*time.Minute, 48
	default:
		longTerm, err := newTrafficWindow(TrafficRange(parsed), now, retentionDays)
		if err != nil {
			return resourceWindow{}, err
		}
		window.granularity = longTerm.granularity
		window.from = longTerm.from
		window.lastBucket = longTerm.lastBucket
		window.through = longTerm.through
		window.buckets = longTerm.buckets
		return window, nil
	}
	window.recent = true
	window.lastBucket = now.Truncate(window.step)
	window.from = window.lastBucket.Add(-time.Duration(count-1) * window.step)
	window.through = now.Truncate(time.Minute)
	window.buckets = make([]string, 0, count)
	for at := window.from; !at.After(window.lastBucket); at = at.Add(window.step) {
		window.buckets = append(window.buckets, window.key(at))
	}
	return window, nil
}

func (w resourceWindow) key(at time.Time) string {
	if w.recent {
		return at.UTC().Truncate(w.step).Format(time.RFC3339)
	}
	if w.granularity == "hour" {
		return at.UTC().Truncate(time.Hour).Format(time.RFC3339)
	}
	return at.UTC().Format(time.DateOnly)
}

func (s *Store) Resources(ctx context.Context, space, app string, selected ResourceRange, now time.Time) (ResourceReport, error) {
	if !config.ValidName(space) || !config.ValidName(app) {
		return ResourceReport{}, errors.New("invalid resource telemetry scope")
	}
	window, err := newResourceWindow(selected, now, s.retentionDays)
	if err != nil {
		return ResourceReport{}, err
	}
	report := ResourceReport{
		Space: space, App: app, Range: window.selected, Granularity: window.granularity,
		From: window.from, To: window.to,
		Series: make([]ResourcePoint, 0, len(window.buckets)),
	}

	latestRows, err := s.db.QueryContext(ctx, `
SELECT last_nano, last_cpu, last_memory
FROM (
  SELECT *, row_number() OVER (PARTITION BY instance ORDER BY last_nano DESC) AS newest
  FROM resource_rollups
  WHERE space = ? AND app = ?
)
WHERE newest = 1`, space, app)
	if err != nil {
		return ResourceReport{}, err
	}
	var latest []latestResourceInstance
	var latestNano int64
	for latestRows.Next() {
		var instance latestResourceInstance
		if err := latestRows.Scan(
			&instance.nano, &instance.cpu, &instance.memory,
		); err != nil {
			latestRows.Close()
			return ResourceReport{}, err
		}
		latest = append(latest, instance)
		if instance.nano > latestNano {
			latestNano = instance.nano
		}
	}
	if err := latestRows.Close(); err != nil {
		return ResourceReport{}, err
	}
	if latestNano > 0 {
		current := ResourceCurrent{SampledAt: time.Unix(0, latestNano).UTC()}
		cutoff := latestNano - resourceCollectionWindow.Nanoseconds()
		for _, instance := range latest {
			if instance.nano < cutoff {
				continue
			}
			current.Instances++
			current.CPUCores += instance.cpu
			current.MemoryBytes += instance.memory
		}
		age := now.UTC().Sub(current.SampledAt)
		current.Fresh = age >= -5*time.Minute && age <= resourceFreshFor
		current.CPUCores = rounded(current.CPUCores, 4)
		report.Current = &current
		report.Available = true
	}

	series := make(map[string]*resourceAggregate, len(window.buckets))
	for _, bucket := range window.buckets {
		series[bucket] = &resourceAggregate{}
	}
	perInterval := make(map[int64]*resourceAggregate)
	query := `
SELECT hour, sample_count, cpu_sum, cpu_max, memory_sum, memory_max
FROM resource_rollups
WHERE space = ? AND app = ? AND hour >= ? AND hour <= ?`
	if window.recent {
		query = `
SELECT minute, sample_count, cpu_sum, cpu_max, memory_sum, memory_max
FROM resource_recent_rollups
WHERE space = ? AND app = ? AND minute >= ? AND minute <= ?`
	}
	rows, err := s.db.QueryContext(ctx, query,
		space, app, window.from.Unix(), window.through.Unix())
	if err != nil {
		return ResourceReport{}, err
	}
	for rows.Next() {
		var interval int64
		var row resourceAggregate
		if err := rows.Scan(&interval, &row.samples, &row.cpuSum, &row.cpuMax, &row.memorySum, &row.memoryMax); err != nil {
			rows.Close()
			return ResourceReport{}, err
		}
		if row.samples <= 0 {
			continue
		}
		if series[window.key(time.Unix(interval, 0))] == nil {
			continue
		}
		aggregate := perInterval[interval]
		if aggregate == nil {
			aggregate = &resourceAggregate{}
			perInterval[interval] = aggregate
		}
		if row.samples > aggregate.samples {
			aggregate.samples = row.samples
		}
		aggregate.cpuSum += row.cpuSum
		// Per-instance maxima are not guaranteed to be simultaneous; their
		// sum is a safe upper bound for the app's peak within this interval.
		aggregate.cpuMax += row.cpuMax
		aggregate.memorySum += row.memorySum
		aggregate.memoryMax += row.memoryMax
		report.Available = true
	}
	if err := rows.Close(); err != nil {
		return ResourceReport{}, err
	}
	for interval, aggregate := range perInterval {
		bucket := series[window.key(time.Unix(interval, 0))]
		bucket.samples += aggregate.samples
		bucket.cpuSum += aggregate.cpuSum
		bucket.cpuMax = math.Max(bucket.cpuMax, aggregate.cpuMax)
		bucket.memorySum += aggregate.memorySum
		if aggregate.memoryMax > bucket.memoryMax {
			bucket.memoryMax = aggregate.memoryMax
		}
	}
	for _, bucket := range window.buckets {
		aggregate := series[bucket]
		point := ResourcePoint{Bucket: bucket, Samples: aggregate.samples}
		if aggregate.samples > 0 {
			point.AverageCPUCores = rounded(aggregate.cpuSum/float64(aggregate.samples), 4)
			point.PeakCPUCores = rounded(aggregate.cpuMax, 4)
			point.AverageMemoryBytes = int64(math.Round(aggregate.memorySum / float64(aggregate.samples)))
			point.PeakMemoryBytes = aggregate.memoryMax
		}
		report.Series = append(report.Series, point)
	}
	return report, nil
}
