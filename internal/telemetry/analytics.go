package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adrianliechti/donkey/internal/config"
)

// Web analytics is the client-side stream donkey-gateway collects from the
// script it adds to web apps' HTML pages: page views (including SPA route
// changes), custom events, and how long a page stayed visible. The gateway
// derives every dimension before export — browser, OS and device from the
// user agent, the country from the browser's time zone, the referrer's host
// from its URL — so this service never sees an IP address, a user agent or a
// query string. Visits are not stored; they are read out of the events.
const (
	WebKindPageview   = "pageview"
	WebKindEvent      = "event"
	WebKindEngagement = "engagement"

	// webVisitGap ends a visit: the next event after this much silence starts
	// a new one, the common definition GA, umami and rybbit share.
	webVisitGap = 30 * time.Minute

	webBreakdownLimit = 25
	webLiveWindow     = 5 * time.Minute
	webMaxFilters     = 8
	webMaxProps       = 10
)

// WebEvent is one page view, custom event or engagement ping. Visitor is a
// gateway-made HMAC: per app and per day for anonymous visitors, per app for
// signed-in ones.
type WebEvent struct {
	TimeUnixMilli int64             `json:"time"`
	Space         string            `json:"space"`
	App           string            `json:"app"`
	Visitor       string            `json:"visitor"`
	Kind          string            `json:"kind"`
	Path          string            `json:"path"`
	Name          string            `json:"name,omitempty"`
	Referrer      string            `json:"referrer,omitempty"`
	Channel       string            `json:"channel,omitempty"`
	UTMSource     string            `json:"utmSource,omitempty"`
	UTMMedium     string            `json:"utmMedium,omitempty"`
	UTMCampaign   string            `json:"utmCampaign,omitempty"`
	Browser       string            `json:"browser,omitempty"`
	OS            string            `json:"os,omitempty"`
	Device        string            `json:"device,omitempty"`
	Country       string            `json:"country,omitempty"`
	Language      string            `json:"language,omitempty"`
	EngagedMS     int64             `json:"engagedMs,omitempty"`
	Props         map[string]string `json:"props,omitempty"`
}

type WebBatch struct {
	Events []WebEvent `json:"events"`
}

// AnalyticsDimension names one breakdown and one filter. Path and event are
// page-level: filtering by them keeps the visits that contain the page or
// event, and narrows the pages or events table to it. Every other dimension
// describes a visit.
type AnalyticsDimension string

const (
	DimPath        AnalyticsDimension = "path"
	DimEntry       AnalyticsDimension = "entry"
	DimExit        AnalyticsDimension = "exit"
	DimReferrer    AnalyticsDimension = "referrer"
	DimChannel     AnalyticsDimension = "channel"
	DimUTMSource   AnalyticsDimension = "utm_source"
	DimUTMMedium   AnalyticsDimension = "utm_medium"
	DimUTMCampaign AnalyticsDimension = "utm_campaign"
	DimBrowser     AnalyticsDimension = "browser"
	DimOS          AnalyticsDimension = "os"
	DimDevice      AnalyticsDimension = "device"
	DimCountry     AnalyticsDimension = "country"
	DimLanguage    AnalyticsDimension = "language"
	DimEvent       AnalyticsDimension = "event"
)

// visitDimensions are the per-visit attributes, in report order, each with
// its column in the visits relation the report builds. Attribution is the
// visit's first page view: where it came from, and on what.
var visitDimensions = []struct {
	dimension AnalyticsDimension
	column    string
}{
	{DimEntry, "entry_page"}, {DimExit, "exit_page"}, {DimReferrer, "referrer"}, {DimChannel, "channel"},
	{DimUTMSource, "utm_source"}, {DimUTMMedium, "utm_medium"}, {DimUTMCampaign, "utm_campaign"},
	{DimBrowser, "browser"}, {DimOS, "os"}, {DimDevice, "device"}, {DimCountry, "country"}, {DimLanguage, "language"},
}

func (d AnalyticsDimension) visitColumn() (string, bool) {
	for _, v := range visitDimensions {
		if d == v.dimension {
			return v.column, true
		}
	}
	return "", false
}

func (d AnalyticsDimension) valid() bool {
	_, visit := d.visitColumn()
	return visit || d == DimPath || d == DimEvent
}

// omitsEmpty: an empty referrer is a direct visit (the channel says so), and
// an empty campaign or event name is no campaign or event at all. For the
// other dimensions empty means unknown and stays in the table.
func (d AnalyticsDimension) omitsEmpty() bool {
	switch d {
	case DimReferrer, DimUTMSource, DimUTMMedium, DimUTMCampaign, DimEvent:
		return true
	}
	return false
}

type AnalyticsFilter struct {
	Dimension AnalyticsDimension `json:"dimension"`
	Value     string             `json:"value"`
}

// ParseAnalyticsFilters reads repeated dimension:value query values.
func ParseAnalyticsFilters(values []string) ([]AnalyticsFilter, error) {
	if len(values) > webMaxFilters {
		return nil, fmt.Errorf("at most %d filters", webMaxFilters)
	}
	out := make([]AnalyticsFilter, 0, len(values))
	seen := map[AnalyticsDimension]bool{}
	for _, raw := range values {
		dimension, value, ok := strings.Cut(raw, ":")
		filter := AnalyticsFilter{Dimension: AnalyticsDimension(dimension), Value: value}
		if !ok || !filter.Dimension.valid() {
			return nil, fmt.Errorf("filter %q must be <dimension>:<value> with a known dimension", raw)
		}
		if len(value) > 512 || !utf8.ValidString(value) {
			return nil, fmt.Errorf("filter %q is too long", dimension)
		}
		if seen[filter.Dimension] {
			return nil, fmt.Errorf("one filter per dimension (%s)", dimension)
		}
		seen[filter.Dimension] = true
		out = append(out, filter)
	}
	return out, nil
}

type AnalyticsTotals struct {
	Visitors             int64   `json:"visitors"`
	Visits               int64   `json:"visits"`
	Pageviews            int64   `json:"pageviews"`
	Events               int64   `json:"events"`
	ViewsPerVisit        float64 `json:"viewsPerVisit"`
	BounceRate           float64 `json:"bounceRate"`
	VisitDurationSeconds float64 `json:"visitDurationSeconds"`
}

type AnalyticsPoint struct {
	Bucket    string `json:"bucket"`
	Visitors  int64  `json:"visitors"`
	Visits    int64  `json:"visits"`
	Pageviews int64  `json:"pageviews"`
}

// AnalyticsRow is one value of a breakdown. Count is page views for path,
// occurrences for event, and visits for every other dimension.
type AnalyticsRow struct {
	Value    string `json:"value"`
	Visitors int64  `json:"visitors"`
	Count    int64  `json:"count"`
}

type AnalyticsPropertyValue struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

type AnalyticsProperty struct {
	Key    string                   `json:"key"`
	Values []AnalyticsPropertyValue `json:"values"`
}

type AnalyticsDefinitions struct {
	Visitor  string `json:"visitor"`
	Visit    string `json:"visit"`
	Bounce   string `json:"bounce"`
	Duration string `json:"duration"`
}

type AnalyticsReport struct {
	Space       string            `json:"space"`
	App         string            `json:"app"`
	Range       TrafficRange      `json:"range"`
	Granularity string            `json:"granularity"`
	From        time.Time         `json:"from"`
	To          time.Time         `json:"to"`
	Filters     []AnalyticsFilter `json:"filters"`
	Totals      AnalyticsTotals   `json:"totals"`
	// Previous is the same-length window just before From, when it is still
	// retained.
	Previous        *AnalyticsTotals                      `json:"previous,omitempty"`
	Series          []AnalyticsPoint                      `json:"series"`
	Breakdowns      map[AnalyticsDimension][]AnalyticsRow `json:"breakdowns"`
	EventProperties []AnalyticsProperty                   `json:"eventProperties,omitempty"`
	Definitions     AnalyticsDefinitions                  `json:"definitions"`
}

type AnalyticsLive struct {
	Visitors      int64          `json:"visitors"`
	WindowMinutes int            `json:"windowMinutes"`
	Pages         []AnalyticsRow `json:"pages"`
}

var analyticsDefinitions = AnalyticsDefinitions{
	Visitor: "anonymous visitors are a per-app hash of IP address and browser that changes every day, so one person " +
		"returning on another day counts again; signed-in visitors are a per-app hash of their sign-in. Nothing is stored in the browser",
	Visit:    "one visitor's page views and events without a 30-minute pause",
	Bounce:   "visits with a single page view and no custom event",
	Duration: "from a visit's first page view to its last activity, including the time its last page stayed visible",
}

// webColumns are the stored columns, in insert order.
const webColumns = "ts, space, app, visitor, kind, path, name, referrer, channel, utm_source, utm_medium, utm_campaign, " +
	"browser, os, device, country, language, engaged_ms, props"

const webColumnCount = 19

// webEventsSchema keeps raw events rather than rollups: visits, bounces and
// entry pages only exist across events, and 30 days of a small installation's
// page views are well within what DuckDB scans per report. Rows arrive in time
// order, so DuckDB's per-block min/max already prunes by time without an
// index that every insert would have to maintain.
const webEventsSchema = `
CREATE TABLE IF NOT EXISTS web_events (
  ts BIGINT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  visitor TEXT NOT NULL,
  kind TEXT NOT NULL,
  path TEXT NOT NULL,
  name TEXT NOT NULL,
  referrer TEXT NOT NULL,
  channel TEXT NOT NULL,
  utm_source TEXT NOT NULL,
  utm_medium TEXT NOT NULL,
  utm_campaign TEXT NOT NULL,
  browser TEXT NOT NULL,
  os TEXT NOT NULL,
  device TEXT NOT NULL,
  country TEXT NOT NULL,
  language TEXT NOT NULL,
  engaged_ms BIGINT NOT NULL,
  props TEXT NOT NULL
);`

// InsertWebEvents stores one gateway batch. Anything malformed is counted as
// rejected rather than failing the batch: the gateway already validated it,
// so a rejection here is a version skew worth seeing, not worth retrying.
func (s *Store) InsertWebEvents(ctx context.Context, batch WebBatch, now time.Time) (accepted, rejected int, err error) {
	if len(batch.Events) > 5000 {
		return 0, len(batch.Events), errors.New("web event batch exceeds 5000 events")
	}
	cutoff := now.UTC().AddDate(0, 0, -s.retentionDays).UnixMilli()
	future := now.UTC().Add(5 * time.Minute).UnixMilli()
	args := make([]any, 0, len(batch.Events)*webColumnCount)
	for _, event := range batch.Events {
		row, ok := webEventRow(event, cutoff, future)
		if !ok {
			rejected++
			continue
		}
		args = append(args, row...)
		accepted++
	}
	if accepted == 0 {
		return 0, rejected, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	placeholder := "(" + strings.TrimSuffix(strings.Repeat("?,", webColumnCount), ",") + ")"
	// One statement per batch; DuckDB's parameter limit is far above the
	// gateway's batch size times the column count.
	for start := 0; start < accepted; start += 200 {
		end := min(start+200, accepted)
		values := strings.TrimSuffix(strings.Repeat(placeholder+",", end-start), ",")
		if _, err := tx.ExecContext(ctx, "INSERT INTO web_events ("+webColumns+") VALUES "+values,
			args[start*webColumnCount:end*webColumnCount]...); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return accepted, rejected, nil
}

func webEventRow(e WebEvent, cutoff, future int64) ([]any, bool) {
	if e.TimeUnixMilli < cutoff || e.TimeUnixMilli > future ||
		!config.ValidName(e.Space) || !config.ValidName(e.App) || !validVisitorHash(e.Visitor) || e.Visitor == "" {
		return nil, false
	}
	switch e.Kind {
	case WebKindPageview, WebKindEngagement:
		if e.Name != "" || e.Kind == WebKindPageview && e.EngagedMS != 0 {
			return nil, false
		}
	case WebKindEvent:
		if e.Name == "" || e.EngagedMS != 0 {
			return nil, false
		}
	default:
		return nil, false
	}
	if !strings.HasPrefix(e.Path, "/") || e.EngagedMS < 0 || e.EngagedMS > int64(24*time.Hour/time.Millisecond) {
		return nil, false
	}
	for _, field := range []struct {
		value string
		max   int
	}{
		{e.Path, 512}, {e.Name, 100}, {e.Referrer, 253}, {e.Channel, 32},
		{e.UTMSource, 100}, {e.UTMMedium, 100}, {e.UTMCampaign, 100},
		{e.Browser, 32}, {e.OS, 32}, {e.Device, 16}, {e.Country, 2}, {e.Language, 35},
	} {
		if len(field.value) > field.max || !printable(field.value) {
			return nil, false
		}
	}
	props := ""
	if len(e.Props) > 0 {
		if e.Kind != WebKindEvent || len(e.Props) > webMaxProps {
			return nil, false
		}
		for key, value := range e.Props {
			if key == "" || len(key) > 50 || len(value) > 200 || !printable(key) || !printable(value) {
				return nil, false
			}
		}
		raw, err := json.Marshal(e.Props) // map keys marshal sorted
		if err != nil {
			return nil, false
		}
		props = string(raw)
	}
	return []any{
		e.TimeUnixMilli, e.Space, e.App, e.Visitor, e.Kind, e.Path, e.Name, e.Referrer, e.Channel,
		e.UTMSource, e.UTMMedium, e.UTMCampaign, e.Browser, e.OS, e.Device, e.Country, e.Language,
		e.EngagedMS, props,
	}, true
}

func printable(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
