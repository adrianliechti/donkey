package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// webScope is the relation every report query starts from: the scope's events
// in a window, cut into visits, narrowed to the filtered visits. Visits are
// read out of the events with window functions rather than stored, so a
// filter can select them by any attribute without a second copy of the data.
//
//	marked       events with their visit number per visitor
//	visits       one row per visit, attributed to its first page view
//	kept         the visits the filters select
//	kept_events  their events; path and event filters also narrow these
type webScope struct {
	sql  string
	args []any
}

func newWebScope(space, app string, from, to int64, filters []AnalyticsFilter) webScope {
	var b strings.Builder
	args := []any{space, app, from, to}
	b.WriteString(`WITH marked AS MATERIALIZED (
  SELECT *, sum(fresh) OVER (PARTITION BY visitor ORDER BY seq ROWS UNBOUNDED PRECEDING) AS visit
  FROM (
    SELECT *, CASE WHEN ts - lag(ts) OVER (PARTITION BY visitor ORDER BY seq) <= `)
	fmt.Fprintf(&b, "%d", webVisitGap.Milliseconds())
	b.WriteString(` THEN 0 ELSE 1 END AS fresh
    FROM (
      SELECT *, row_number() OVER (PARTITION BY visitor ORDER BY ts) AS seq
      FROM web_events WHERE space = ? AND app = ? AND ts >= ? AND ts < ?
    )
  )
),
visits AS MATERIALIZED (
  SELECT visitor, visit, min(ts) AS started, max(ts) AS ended,
    count(*) FILTER (WHERE kind = 'pageview') AS pageviews,
    count(*) FILTER (WHERE kind = 'event') AS events,
    arg_min(path, seq) FILTER (WHERE kind = 'pageview') AS entry_page,
    arg_max(path, seq) FILTER (WHERE kind = 'pageview') AS exit_page`)
	for _, column := range []string{"referrer", "channel", "utm_source", "utm_medium", "utm_campaign", "browser", "os", "device", "country", "language"} {
		fmt.Fprintf(&b, ",\n    arg_min(%s, seq) FILTER (WHERE kind = 'pageview') AS %s", column, column)
	}
	var path, event *AnalyticsFilter
	for i := range filters {
		switch filters[i].Dimension {
		case DimPath:
			path = &filters[i]
		case DimEvent:
			event = &filters[i]
		}
	}
	if path != nil {
		b.WriteString(",\n    bool_or(kind = 'pageview' AND path = ?) AS has_path")
		args = append(args, path.Value)
	}
	if event != nil {
		b.WriteString(",\n    bool_or(kind = 'event' AND name = ?) AS has_event")
		args = append(args, event.Value)
	}
	b.WriteString(`
  FROM marked GROUP BY visitor, visit
  HAVING count(*) FILTER (WHERE kind = 'pageview') > 0
),
kept AS MATERIALIZED (
  SELECT * FROM visits WHERE true`)
	for _, filter := range filters {
		if column, ok := filter.Dimension.visitColumn(); ok {
			fmt.Fprintf(&b, " AND %s = ?", column)
			args = append(args, filter.Value)
		}
	}
	if path != nil {
		b.WriteString(" AND has_path")
	}
	if event != nil {
		b.WriteString(" AND has_event")
	}
	b.WriteString(`
),
kept_events AS MATERIALIZED (
  SELECT m.ts, m.visitor, m.kind, m.path, m.name, m.props
  FROM marked m JOIN kept k ON m.visitor = k.visitor AND m.visit = k.visit
  WHERE true`)
	if path != nil {
		b.WriteString(" AND (m.kind <> 'pageview' OR m.path = ?)")
		args = append(args, path.Value)
	}
	if event != nil {
		b.WriteString(" AND (m.kind <> 'event' OR m.name = ?)")
		args = append(args, event.Value)
	}
	b.WriteString("\n)\n")
	return webScope{sql: b.String(), args: args}
}

func (s *Store) Analytics(ctx context.Context, space, app string, selected TrafficRange, filters []AnalyticsFilter, now time.Time) (AnalyticsReport, error) {
	if !config.ValidName(space) || !config.ValidName(app) {
		return AnalyticsReport{}, errors.New("invalid analytics scope")
	}
	for _, filter := range filters {
		if !filter.Dimension.valid() {
			return AnalyticsReport{}, fmt.Errorf("unknown analytics dimension %q", filter.Dimension)
		}
	}
	window, err := newTrafficWindow(selected, now, s.retentionDays)
	if err != nil {
		return AnalyticsReport{}, err
	}
	from, to := window.from.UnixMilli(), window.to.UnixMilli()+1
	scope := newWebScope(space, app, from, to, filters)

	report := AnalyticsReport{
		Space: space, App: app, Range: window.selected, Granularity: window.granularity,
		From: window.from, To: window.to,
		Filters:     append([]AnalyticsFilter{}, filters...),
		Breakdowns:  map[AnalyticsDimension][]AnalyticsRow{},
		Definitions: analyticsDefinitions,
	}
	if report.Totals, err = s.webTotals(ctx, scope); err != nil {
		return AnalyticsReport{}, err
	}
	// The comparison window has the same length and ends where this one
	// starts; it exists only while its oldest event is still retained.
	span := to - from
	if retained := now.UTC().AddDate(0, 0, -s.retentionDays).UnixMilli(); from-span >= retained {
		previous, err := s.webTotals(ctx, newWebScope(space, app, from-span, from, filters))
		if err != nil {
			return AnalyticsReport{}, err
		}
		report.Previous = &previous
	}
	if report.Series, err = s.webSeries(ctx, scope, window); err != nil {
		return AnalyticsReport{}, err
	}
	if err := s.webBreakdowns(ctx, scope, report.Breakdowns); err != nil {
		return AnalyticsReport{}, err
	}
	for _, filter := range filters {
		if filter.Dimension == DimEvent {
			if report.EventProperties, err = s.webEventProperties(ctx, scope); err != nil {
				return AnalyticsReport{}, err
			}
		}
	}
	return report, nil
}

func (s *Store) webTotals(ctx context.Context, scope webScope) (AnalyticsTotals, error) {
	var totals AnalyticsTotals
	var bounce, duration float64
	err := s.db.QueryRowContext(ctx, scope.sql+`SELECT
  (SELECT count(DISTINCT visitor) FROM kept),
  (SELECT count(*) FROM kept),
  (SELECT count(*) FROM kept_events WHERE kind = 'pageview'),
  (SELECT count(*) FROM kept_events WHERE kind = 'event'),
  (SELECT coalesce(avg(CASE WHEN pageviews = 1 AND events = 0 THEN 100.0 ELSE 0.0 END), 0) FROM kept),
  (SELECT coalesce(avg(ended - started), 0) FROM kept)`, scope.args...).Scan(
		&totals.Visitors, &totals.Visits, &totals.Pageviews, &totals.Events, &bounce, &duration)
	if err != nil {
		return AnalyticsTotals{}, err
	}
	totals.BounceRate = rounded(bounce, 1)
	totals.VisitDurationSeconds = math.Round(duration / 1000)
	if totals.Visits > 0 {
		totals.ViewsPerVisit = rounded(float64(totals.Pageviews)/float64(totals.Visits), 1)
	}
	return totals, nil
}

func (s *Store) webSeries(ctx context.Context, scope webScope, window trafficWindow) ([]AnalyticsPoint, error) {
	width := time.Hour.Milliseconds()
	if window.granularity == "day" {
		width = (24 * time.Hour).Milliseconds()
	}
	points := make(map[string]*AnalyticsPoint, len(window.buckets))
	series := make([]AnalyticsPoint, len(window.buckets))
	for i, bucket := range window.buckets {
		series[i].Bucket = bucket
		points[bucket] = &series[i]
	}
	// Visits count where they started; page views and visitors where they
	// happened, so a visit across midnight adds its visitor to both days.
	rows, err := s.db.QueryContext(ctx, scope.sql+fmt.Sprintf(`
SELECT false, ts // %[1]d AS slot, count(*) FILTER (WHERE kind = 'pageview'), count(DISTINCT visitor)
FROM kept_events GROUP BY slot
UNION ALL
SELECT true, started // %[1]d AS slot, count(*), 0 FROM kept GROUP BY slot`, width), scope.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var visits bool
		var slot, count, visitors int64
		if err := rows.Scan(&visits, &slot, &count, &visitors); err != nil {
			return nil, err
		}
		point := points[window.key(time.UnixMilli(slot*width))]
		if point == nil {
			continue
		}
		if visits {
			point.Visits += count
		} else {
			point.Pageviews += count
			point.Visitors += visitors
		}
	}
	return series, rows.Err()
}

func (s *Store) webBreakdowns(ctx context.Context, scope webScope, out map[AnalyticsDimension][]AnalyticsRow) error {
	branches := []string{
		"SELECT 'path' AS dimension, path AS value, count(DISTINCT visitor) AS visitors, count(*) AS n FROM kept_events WHERE kind = 'pageview' GROUP BY path",
		"SELECT 'event', name, count(DISTINCT visitor), count(*) FROM kept_events WHERE kind = 'event' AND name <> '' GROUP BY name",
	}
	for _, v := range visitDimensions {
		where := ""
		if v.dimension.omitsEmpty() {
			where = fmt.Sprintf(" WHERE %s <> ''", v.column)
		}
		branches = append(branches, fmt.Sprintf(
			"SELECT '%s', coalesce(%s, ''), count(DISTINCT visitor), count(*) FROM kept%s GROUP BY 2", v.dimension, v.column, where))
	}
	rows, err := s.db.QueryContext(ctx, scope.sql+fmt.Sprintf(`
SELECT dimension, value, visitors, n FROM (%s)
QUALIFY row_number() OVER (PARTITION BY dimension ORDER BY n DESC, visitors DESC, value) <= %d`,
		strings.Join(branches, "\nUNION ALL\n"), webBreakdownLimit), scope.args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var dimension string
		var row AnalyticsRow
		if err := rows.Scan(&dimension, &row.Value, &row.Visitors, &row.Count); err != nil {
			return err
		}
		out[AnalyticsDimension(dimension)] = append(out[AnalyticsDimension(dimension)], row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for dimension, list := range out {
		sortAnalyticsRows(list)
		out[dimension] = list
	}
	return nil
}

func sortAnalyticsRows(rows []AnalyticsRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		if rows[i].Visitors != rows[j].Visitors {
			return rows[i].Visitors > rows[j].Visitors
		}
		return rows[i].Value < rows[j].Value
	})
}

// webEventProperties breaks the filtered event down by its properties. They
// are stored as JSON per event and counted here, bounded to the most recent
// occurrences, rather than given a table of their own.
func (s *Store) webEventProperties(ctx context.Context, scope webScope) ([]AnalyticsProperty, error) {
	rows, err := s.db.QueryContext(ctx, scope.sql+`
SELECT props FROM kept_events WHERE kind = 'event' AND props <> '' ORDER BY ts DESC LIMIT 20000`, scope.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]map[string]int64{}
	totals := map[string]int64{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var props map[string]string
		if json.Unmarshal([]byte(raw), &props) != nil {
			continue
		}
		for key, value := range props {
			if counts[key] == nil {
				counts[key] = map[string]int64{}
			}
			counts[key][value]++
			totals[key]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AnalyticsProperty, 0, len(counts))
	for key, values := range counts {
		property := AnalyticsProperty{Key: key}
		for value, count := range values {
			property.Values = append(property.Values, AnalyticsPropertyValue{Value: value, Count: count})
		}
		sort.Slice(property.Values, func(i, j int) bool {
			if property.Values[i].Count != property.Values[j].Count {
				return property.Values[i].Count > property.Values[j].Count
			}
			return property.Values[i].Value < property.Values[j].Value
		})
		if len(property.Values) > webBreakdownLimit {
			property.Values = property.Values[:webBreakdownLimit]
		}
		out = append(out, property)
	}
	sort.Slice(out, func(i, j int) bool {
		if totals[out[i].Key] != totals[out[j].Key] {
			return totals[out[i].Key] > totals[out[j].Key]
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > webMaxProps {
		out = out[:webMaxProps]
	}
	return out, nil
}

// AnalyticsLive is who is on the app right now: visitors active in the last
// few minutes, and the page each of them is on.
func (s *Store) AnalyticsLive(ctx context.Context, space, app string, now time.Time) (AnalyticsLive, error) {
	if !config.ValidName(space) || !config.ValidName(app) {
		return AnalyticsLive{}, errors.New("invalid analytics scope")
	}
	since := now.Add(-webLiveWindow).UnixMilli()
	live := AnalyticsLive{WindowMinutes: int(webLiveWindow / time.Minute), Pages: []AnalyticsRow{}}
	rows, err := s.db.QueryContext(ctx, `
SELECT path, count(*) FROM (
  SELECT visitor, arg_max(path, ts) AS path FROM web_events
  WHERE space = ? AND app = ? AND ts >= ? AND kind IN ('pageview', 'engagement')
  GROUP BY visitor
) GROUP BY path ORDER BY 2 DESC, 1 LIMIT 10`, space, app, since)
	if err != nil {
		return AnalyticsLive{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var row AnalyticsRow
		if err := rows.Scan(&row.Value, &row.Visitors); err != nil {
			return AnalyticsLive{}, err
		}
		row.Count = row.Visitors
		live.Pages = append(live.Pages, row)
	}
	if err := rows.Err(); err != nil {
		return AnalyticsLive{}, err
	}
	err = s.db.QueryRowContext(ctx,
		"SELECT count(DISTINCT visitor) FROM web_events WHERE space = ? AND app = ? AND ts >= ?",
		space, app, since).Scan(&live.Visitors)
	return live, err
}
