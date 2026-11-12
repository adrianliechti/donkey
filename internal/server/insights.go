package server

import (
	"cmp"
	"context"
	"log"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/telemetry"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// Insights are what donkey has noticed about an app, good or bad. Nobody
// sets them: they are worked out from what donkey observes, so the next
// deploy or a busier week changes them. The console labels apps with them
// and words them; the API carries only the kind and the facts behind it.
const (
	// InsightUnmaintained: the running build is older than the platform allows.
	InsightUnmaintained = "unmaintained"
	// InsightPopular: among the platform's most used apps.
	InsightPopular = "popular"
)

type AppInsight struct {
	Kind string `json:"kind"`
	// Since is when the running build was made (unmaintained).
	Since string `json:"since,omitempty"`
	// People is how many people use the app on an average day (popular).
	People int `json:"people,omitempty"`
}

const (
	// Popularity moves in days while every open console polls the fleet
	// every ten seconds, so the usage behind it is read every few minutes.
	usageCacheTTL = 5 * time.Minute
	// A telemetry outage costs the popular insight, not the overview; it is
	// asked again a minute later.
	usageRetryTTL = time.Minute
	usageTimeout  = 3 * time.Second
	// The busiest tenth of the platform's apps are popular (at least the
	// busiest one), as long as a handful of people use each on an average
	// day: a small platform's busiest app is not popular for two users.
	popularShare     = 0.1
	minPopularPeople = 5
)

type usageCache struct {
	mu     sync.Mutex
	report *telemetry.UsageReport
	until  time.Time
}

// appUsage is how many people use each app, or nil while telemetry cannot
// say. The lock coalesces concurrent fleet reads into one query.
func (s *Server) appUsage(ctx context.Context) *telemetry.UsageReport {
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	if time.Now().Before(s.usage.until) {
		return s.usage.report
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageTimeout)
	report, err := s.telemetry.Usage(readCtx)
	cancel()
	if err != nil {
		if s.usage.report != nil || s.usage.until.IsZero() {
			log.Printf("app usage unavailable, so no app is marked popular: %v", err)
		}
		s.usage.report, s.usage.until = nil, time.Now().Add(usageRetryTTL)
		return nil
	}
	s.usage.report, s.usage.until = &report, time.Now().Add(usageCacheTTL)
	return &report
}

// popularApps ranks the apps that exist — all of them, so every viewer sees
// the same ranking whichever spaces they can open — and returns the popular
// ones with their people on an average day.
func popularApps(refs []runtime.AppRef, usage *telemetry.UsageReport) map[runtime.AppRef]int {
	if usage == nil || len(refs) == 0 {
		return nil
	}
	exists := make(map[runtime.AppRef]bool, len(refs))
	for _, ref := range refs {
		exists[ref] = true
	}
	people := map[runtime.AppRef]float64{}
	var ranked []float64
	for _, app := range usage.Apps {
		ref := runtime.AppRef{Space: app.Space, App: app.App}
		if exists[ref] && app.People > 0 {
			people[ref] = app.People
			ranked = append(ranked, app.People)
		}
	}
	if len(ranked) == 0 {
		return nil
	}
	slices.SortFunc(ranked, func(a, b float64) int { return cmp.Compare(b, a) })
	top := min(int(math.Ceil(float64(len(refs))*popularShare)), len(ranked))
	threshold := max(ranked[top-1], minPopularPeople)
	popular := map[runtime.AppRef]int{}
	for ref, n := range people {
		if n >= threshold {
			popular[ref] = int(math.Round(n))
		}
	}
	return popular
}

// appInsights works out one app's insights. The build's age is read off the
// image tag, so a rollback to an old image counts as old: its base image is.
func appInsights(st *runtime.AppStatus, limit time.Duration, people int, now time.Time) []AppInsight {
	var insights []AppInsight
	if built, ok := build.ImageTime(st.Image); ok && now.Sub(built) > limit {
		insights = append(insights, AppInsight{Kind: InsightUnmaintained, Since: built.UTC().Format(time.RFC3339)})
	}
	if people > 0 {
		insights = append(insights, AppInsight{Kind: InsightPopular, People: people})
	}
	return insights
}
