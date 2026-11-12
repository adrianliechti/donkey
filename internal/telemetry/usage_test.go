package telemetry

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestUsageCountsPeoplePerDay(t *testing.T) {
	store, err := OpenStore(":memory:", DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	key := bytes.Repeat([]byte{7}, 32)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	empty, err := store.Usage(ctx, now)
	if err != nil || empty.Days != UsageDays || len(empty.Apps) != 0 {
		t.Fatalf("empty usage = %+v, %v", empty, err)
	}

	// Behind the login: ada on three days (twice on one of them), bob once,
	// and a caller from before the window.
	var callers CallerBatch
	caller := func(app, subject string, at time.Time) {
		appHash, spaceHash := VisitorHashes(key, "team", app, subject)
		callers.Events = append(callers.Events, CallerEvent{uint64(at.UnixNano()), "team", app, appHash, spaceHash})
	}
	for _, ago := range []time.Duration{time.Hour, 3 * time.Hour, 25 * time.Hour, 49 * time.Hour} {
		caller("tool", "ada", now.Add(-ago))
	}
	caller("tool", "bob", now.Add(-2*time.Hour))
	caller("tool", "eve", now.AddDate(0, 0, -9))
	// Their browsers count the same people again; the callers already have.
	var web WebBatch
	visit := func(app, subject, address string, at time.Time) {
		web.Events = append(web.Events, WebEvent{
			TimeUnixMilli: at.UnixMilli(), Space: "team", App: app, Kind: WebKindPageview, Path: "/",
			Visitor: WebVisitor(key, "team", app, subject, address, "Mozilla/5.0", at),
		})
	}
	visit("tool", "ada", "", now.Add(-time.Hour))
	// A public site: two browsers today, one of them yesterday as well.
	visit("site", "", "192.0.2.1", now.Add(-time.Hour))
	visit("site", "", "192.0.2.1", now.Add(-2*time.Hour))
	visit("site", "", "192.0.2.2", now.Add(-time.Hour))
	visit("site", "", "192.0.2.1", now.Add(-25*time.Hour))
	if _, _, err := store.InsertCallers(ctx, callers, now); err != nil {
		t.Fatal(err)
	}
	if _, rejected, err := store.InsertWebEvents(ctx, web, now); err != nil || rejected != 0 {
		t.Fatalf("web events rejected=%d %v", rejected, err)
	}

	report, err := store.Usage(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	people := map[string]float64{}
	for _, usage := range report.Apps {
		people[usage.App] = usage.People
	}
	// ada on three days and bob on one: four person-days over the week.
	if got, want := people["tool"], 4.0/7; got != want {
		t.Errorf("tool people = %v, want %v", got, want)
	}
	if got, want := people["site"], 3.0/7; got != want {
		t.Errorf("site people = %v, want %v", got, want)
	}

	short, err := OpenStore(":memory:", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()
	if report, err := short.Usage(ctx, now); err != nil || report.Days != 3 {
		t.Errorf("a three-day store reports %+v, %v", report, err)
	}
}
