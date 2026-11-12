package server

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/telemetry"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

func TestAppInsights(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	limit := config.PlatformConfig{}.UnmaintainedAfter()
	for _, tc := range []struct {
		image  string
		people int
		want   []AppInsight
	}{
		{"registry.example.com/donkey/team/web:4f2a9c1b03de-20260601120000", 0,
			[]AppInsight{{Kind: InsightUnmaintained, Since: "2026-06-01T12:00:00Z"}}},
		{"registry.example.com/donkey/team/web:20261001120000-a1b2", 0, nil},
		// No timestamp in the tag: no age, so never unmaintained.
		{"registry.example.com/donkey/team/web:latest", 12, []AppInsight{{Kind: InsightPopular, People: 12}}},
		{"registry.example.com/donkey/team/web:20260101000000", 40, []AppInsight{
			{Kind: InsightUnmaintained, Since: "2026-01-01T00:00:00Z"}, {Kind: InsightPopular, People: 40},
		}},
	} {
		if got := appInsights(&runtime.AppStatus{Image: tc.image}, limit, tc.people, now); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: insights = %+v, want %+v", tc.image, got, tc.want)
		}
	}

	strict := config.PlatformConfig{UnmaintainedAfterDays: 14}.UnmaintainedAfter()
	if got := appInsights(&runtime.AppStatus{Image: "registry.example.com/donkey/team/web:20261001120000"}, strict, 0, now); len(got) != 1 {
		t.Errorf("a 29-day-old build under a 14-day limit: %+v", got)
	}
}

func TestPopularApps(t *testing.T) {
	var refs []runtime.AppRef
	for i := range 20 {
		refs = append(refs, runtime.AppRef{Space: "team", App: fmt.Sprintf("app-%d", i)})
	}
	usage := func(people ...float64) *telemetry.UsageReport {
		report := &telemetry.UsageReport{Days: 7}
		for i, n := range people {
			report.Apps = append(report.Apps, telemetry.AppUsage{Space: "team", App: fmt.Sprintf("app-%d", i), People: n})
		}
		// Usage of an app that no longer exists ranks nothing.
		report.Apps = append(report.Apps, telemetry.AppUsage{Space: "team", App: "removed", People: 900})
		return report
	}

	// Twenty apps: the busiest two are popular.
	got := popularApps(refs, usage(120, 80.4, 60, 30, 8))
	if want := map[runtime.AppRef]int{refs[0]: 120, refs[1]: 80}; !reflect.DeepEqual(got, want) {
		t.Errorf("popular = %v, want %v", got, want)
	}
	// A tie at the edge takes both.
	if got := popularApps(refs, usage(50, 30, 30, 10)); len(got) != 3 {
		t.Errorf("tied popular = %v", got)
	}
	// The busiest app of a quiet platform is not popular for three people a day.
	if got := popularApps(refs, usage(3, 2)); len(got) != 0 {
		t.Errorf("quiet popular = %v", got)
	}
	// One app alone can be popular.
	if got := popularApps(refs[:1], usage(6)); !reflect.DeepEqual(got, map[runtime.AppRef]int{refs[0]: 6}) {
		t.Errorf("single popular = %v", got)
	}
	if got := popularApps(refs, nil); got != nil {
		t.Errorf("without telemetry = %v", got)
	}
}
