package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"
)

func webTestStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	store, err := OpenStore(":memory:", DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	start := now.Add(-3 * time.Hour)
	at := func(t time.Time) int64 { return t.UnixMilli() }
	chrome := WebEvent{Space: "team", App: "site", Visitor: a, Browser: "Chrome", OS: "macOS", Device: "desktop", Country: "CH", Language: "de-CH"}
	firefox := WebEvent{Space: "team", App: "site", Visitor: b, Browser: "Firefox", OS: "Android", Device: "mobile", Country: "US", Language: "en-US"}
	safari := WebEvent{Space: "team", App: "site", Visitor: c, Browser: "Safari", OS: "iOS", Device: "mobile", Country: "DE", Language: "de-DE"}
	with := func(base WebEvent, edit func(*WebEvent)) WebEvent {
		edit(&base)
		return base
	}
	events := []WebEvent{
		// a: one visit of two pages, a signup and a minute of reading…
		with(chrome, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path, e.Referrer, e.Channel = at(start), WebKindPageview, "/", "google.com", "Search"
		}),
		with(chrome, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path = at(start.Add(time.Minute)), WebKindPageview, "/pricing"
		}),
		with(chrome, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path, e.Name, e.Props = at(start.Add(2*time.Minute)), WebKindEvent, "/pricing", "signup", map[string]string{"plan": "pro"}
		}),
		with(chrome, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path, e.EngagedMS = at(start.Add(3*time.Minute)), WebKindEngagement, "/pricing", 60000
		}),
		// …and, two hours later, a second visit that bounces.
		with(chrome, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path, e.Channel = at(start.Add(2*time.Hour)), WebKindPageview, "/docs", "Direct"
		}),
		// b: a bounce from a link, read for half a minute.
		with(firefox, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path, e.Referrer, e.Channel = at(now.Add(-time.Hour)), WebKindPageview, "/", "news.ycombinator.com", "Referral"
		}),
		with(firefox, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path, e.EngagedMS = at(now.Add(-time.Hour+30*time.Second)), WebKindEngagement, "/", 30000
		}),
		// c: on the site right now.
		with(safari, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path, e.Channel = at(now.Add(-2*time.Minute)), WebKindPageview, "/live", "Direct"
		}),
	}
	rejected := []WebEvent{
		with(chrome, func(e *WebEvent) { e.TimeUnixMilli, e.Kind, e.Path = at(now), WebKindEvent, "/" }),                    // no name
		with(chrome, func(e *WebEvent) { e.TimeUnixMilli, e.Kind, e.Path, e.Visitor = at(now), WebKindPageview, "/", "x" }), // no hash
		with(chrome, func(e *WebEvent) { e.TimeUnixMilli, e.Kind, e.Path = at(now.AddDate(0, 0, -40)), WebKindPageview, "/" }),
		with(chrome, func(e *WebEvent) { e.TimeUnixMilli, e.Kind, e.Path = at(now), WebKindPageview, "relative" }),
		with(chrome, func(e *WebEvent) {
			e.TimeUnixMilli, e.Kind, e.Path, e.Props = at(now), WebKindPageview, "/", map[string]string{"a": "b"}
		}),
	}
	accepted, refused, err := store.InsertWebEvents(context.Background(), WebBatch{Events: append(events, rejected...)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if accepted != len(events) || refused != len(rejected) {
		t.Fatalf("accepted=%d rejected=%d", accepted, refused)
	}
	return store
}

func TestAnalyticsReportReadsVisitsOutOfEvents(t *testing.T) {
	now := time.Date(2026, 10, 24, 15, 20, 0, 0, time.UTC)
	store := webTestStore(t, now)

	report, err := store.Analytics(context.Background(), "team", "site", Range48Hours, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	want := AnalyticsTotals{
		Visitors: 3, Visits: 4, Pageviews: 5, Events: 1, ViewsPerVisit: 1.3,
		// a's second visit, b and c bounce; a's first visit does not.
		BounceRate: 75,
		// (3 min + 0 + 30 s + 0) / 4, the engagement ping included.
		VisitDurationSeconds: 53,
	}
	if report.Totals != want {
		t.Fatalf("totals = %+v, want %+v", report.Totals, want)
	}
	if report.Previous == nil || *report.Previous != (AnalyticsTotals{}) {
		t.Fatalf("the previous 48 hours are retained and empty, got %+v", report.Previous)
	}
	var pageviews, visits int64
	for _, point := range report.Series {
		pageviews += point.Pageviews
		visits += point.Visits
	}
	if len(report.Series) != 48 || pageviews != 5 || visits != 4 {
		t.Fatalf("series: %d buckets, %d page views, %d visits", len(report.Series), pageviews, visits)
	}

	expect := func(dimension AnalyticsDimension, rows ...AnalyticsRow) {
		t.Helper()
		got := report.Breakdowns[dimension]
		if len(got) != len(rows) {
			t.Fatalf("%s = %+v, want %+v", dimension, got, rows)
		}
		for i := range rows {
			if got[i] != rows[i] {
				t.Fatalf("%s = %+v, want %+v", dimension, got, rows)
			}
		}
	}
	expect(DimPath, AnalyticsRow{"/", 2, 2}, AnalyticsRow{"/docs", 1, 1}, AnalyticsRow{"/live", 1, 1}, AnalyticsRow{"/pricing", 1, 1})
	expect(DimEntry, AnalyticsRow{"/", 2, 2}, AnalyticsRow{"/docs", 1, 1}, AnalyticsRow{"/live", 1, 1})
	expect(DimExit, AnalyticsRow{"/", 1, 1}, AnalyticsRow{"/docs", 1, 1}, AnalyticsRow{"/live", 1, 1}, AnalyticsRow{"/pricing", 1, 1})
	expect(DimChannel, AnalyticsRow{"Direct", 2, 2}, AnalyticsRow{"Referral", 1, 1}, AnalyticsRow{"Search", 1, 1})
	// Direct visits have no referrer row; the channel already says so.
	expect(DimReferrer, AnalyticsRow{"google.com", 1, 1}, AnalyticsRow{"news.ycombinator.com", 1, 1})
	expect(DimCountry, AnalyticsRow{"CH", 1, 2}, AnalyticsRow{"DE", 1, 1}, AnalyticsRow{"US", 1, 1})
	expect(DimEvent, AnalyticsRow{"signup", 1, 1})
	expect(DimUTMSource)

	byCountry, err := store.Analytics(context.Background(), "team", "site", Range48Hours,
		[]AnalyticsFilter{{Dimension: DimCountry, Value: "CH"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := byCountry.Totals; got.Visitors != 1 || got.Visits != 2 || got.Pageviews != 3 || got.BounceRate != 50 || got.VisitDurationSeconds != 90 {
		t.Fatalf("filtered by country: %+v", got)
	}

	// A page filter keeps the visits that saw the page and narrows the page
	// table to it; the visit's other pages still decide entry and bounce.
	byPage, err := store.Analytics(context.Background(), "team", "site", Range48Hours,
		[]AnalyticsFilter{{Dimension: DimPath, Value: "/pricing"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := byPage.Totals; got.Visits != 1 || got.Pageviews != 1 || got.BounceRate != 0 {
		t.Fatalf("filtered by page: %+v", got)
	}
	if rows := byPage.Breakdowns[DimPath]; len(rows) != 1 || rows[0].Value != "/pricing" {
		t.Fatalf("pages filtered by page: %+v", rows)
	}
	if rows := byPage.Breakdowns[DimEntry]; len(rows) != 1 || rows[0].Value != "/" {
		t.Fatalf("entries filtered by page: %+v", rows)
	}

	byEvent, err := store.Analytics(context.Background(), "team", "site", Range48Hours,
		[]AnalyticsFilter{{Dimension: DimEvent, Value: "signup"}, {Dimension: DimBrowser, Value: "Chrome"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if byEvent.Totals.Visits != 1 || len(byEvent.EventProperties) != 1 || byEvent.EventProperties[0].Key != "plan" ||
		len(byEvent.EventProperties[0].Values) != 1 || byEvent.EventProperties[0].Values[0] != (AnalyticsPropertyValue{"pro", 1}) {
		t.Fatalf("filtered by event: %+v %+v", byEvent.Totals, byEvent.EventProperties)
	}

	month, err := store.Analytics(context.Background(), "team", "site", Range30Days, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if month.Previous != nil || month.Totals.Visits != 4 || len(month.Series) != 30 {
		t.Fatalf("30 days: previous %+v, totals %+v, %d buckets", month.Previous, month.Totals, len(month.Series))
	}
}

func TestAnalyticsLiveAndPurge(t *testing.T) {
	now := time.Date(2026, 10, 24, 15, 20, 0, 0, time.UTC)
	store := webTestStore(t, now)

	live, err := store.AnalyticsLive(context.Background(), "team", "site", now)
	if err != nil {
		t.Fatal(err)
	}
	if live.Visitors != 1 || len(live.Pages) != 1 || live.Pages[0].Value != "/live" {
		t.Fatalf("live = %+v", live)
	}

	if err := store.PurgeSpace(context.Background(), "team"); err != nil {
		t.Fatal(err)
	}
	report, err := store.Analytics(context.Background(), "team", "site", Range7Days, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Totals != (AnalyticsTotals{}) {
		t.Fatalf("a purged space keeps no analytics: %+v", report.Totals)
	}
}

func TestParseAnalyticsFilters(t *testing.T) {
	filters, err := ParseAnalyticsFilters([]string{"path:/a:b", "country:CH"})
	if err != nil || len(filters) != 2 || filters[0] != (AnalyticsFilter{DimPath, "/a:b"}) {
		t.Fatalf("filters = %+v, %v", filters, err)
	}
	for _, bad := range [][]string{{"nope:x"}, {"path"}, {"path:/a", "path:/b"}, {"time:1; DROP TABLE web_events"}} {
		if _, err := ParseAnalyticsFilters(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
