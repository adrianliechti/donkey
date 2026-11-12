package telemetry

import (
	"context"
	"net/http"
	"time"
)

// UsageDays is the window usage is measured over: a week shows an app's
// habits, where a day would show a launch or a quiet Sunday.
const UsageDays = 7

// UsageReport is how many people use each app, for every app at once, so
// the console can tell which apps are popular without a report per app.
type UsageReport struct {
	// Days is the window the counts cover: UsageDays, or the store's
	// retention when that is shorter.
	Days int        `json:"days"`
	Apps []AppUsage `json:"apps"`
}

// AppUsage is one app's people on an average day of the window. A person is
// a signed-in caller where the app has any — behind the login, crawlers
// never are one — and otherwise a browser visitor of its web analytics,
// which drops bots and headless browsers. Anonymous visitors are counted
// once per day anyway (their hash changes with the day), so both kinds are
// counted per day and averaged, which keeps the two comparable.
type AppUsage struct {
	Space  string  `json:"space"`
	App    string  `json:"app"`
	People float64 `json:"people"`
}

func (s *Store) Usage(ctx context.Context, now time.Time) (UsageReport, error) {
	days := min(UsageDays, s.retentionDays)
	report := UsageReport{Days: days, Apps: []AppUsage{}}
	since := now.UTC().AddDate(0, 0, -days)
	rows, err := s.db.QueryContext(ctx, `
SELECT space, app, GREATEST(MAX(callers), MAX(visitors)) FROM (
  SELECT space, app, COUNT(*) AS callers, 0 AS visitors FROM (
    SELECT DISTINCT space, app, hour // 86400, user_hash FROM caller_observations WHERE hour >= ?
  ) GROUP BY space, app
  UNION ALL
  SELECT space, app, 0, COUNT(*) FROM (
    SELECT DISTINCT space, app, ts // 86400000, visitor FROM web_events WHERE ts >= ?
  ) GROUP BY space, app
) GROUP BY space, app ORDER BY space, app`, since.Unix(), since.UnixMilli())
	if err != nil {
		return report, err
	}
	defer rows.Close()
	for rows.Next() {
		var usage AppUsage
		var personDays int64
		if err := rows.Scan(&usage.Space, &usage.App, &personDays); err != nil {
			return report, err
		}
		usage.People = float64(personDays) / float64(days)
		report.Apps = append(report.Apps, usage)
	}
	return report, rows.Err()
}

func (s *Service) handleUsage(w http.ResponseWriter, r *http.Request) {
	report, err := s.store.Usage(r.Context(), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeServiceJSON(w, http.StatusOK, report)
}

// Usage reads how many people use each app.
func (c *Client) Usage(ctx context.Context) (UsageReport, error) {
	var report UsageReport
	return report, c.getJSON(ctx, "/v1/usage", &report)
}
