package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// CallerEvent contains only gateway-derived HMACs of a verified identity.
// Anonymous requests do not get an identifier. Repeated events are harmless:
// the store retains one observation per caller, app and hour, not a request log.
type CallerEvent struct {
	TimeUnixNano  uint64 `json:"timeUnixNano"`
	Space         string `json:"space"`
	App           string `json:"app"`
	UserHash      string `json:"user.hash"`
	SpaceUserHash string `json:"spaceUserHash"`
}

type CallerBatch struct {
	Events []CallerEvent `json:"events"`
}

func (s *Service) handleCallers(w http.ResponseWriter, r *http.Request) {
	if !tokenMatches(r.Header.Get(TelemetryTokenHeader), GatewayToken(s.cfg.Key)) {
		http.Error(w, "gateway token required", http.StatusUnauthorized)
		return
	}
	var batch CallerBatch
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&batch); err != nil {
		http.Error(w, "invalid caller batch", http.StatusBadRequest)
		return
	}
	accepted, rejected, err := s.store.InsertCallers(r.Context(), batch, time.Now())
	if err != nil {
		http.Error(w, "storing callers failed", http.StatusInternalServerError)
		return
	}
	writeServiceJSON(w, http.StatusOK, map[string]int{"accepted": accepted, "rejected": rejected})
}

const callersSchema = `
CREATE TABLE IF NOT EXISTS caller_observations (
  hour BIGINT NOT NULL,
  space TEXT NOT NULL,
  app TEXT NOT NULL,
  user_hash TEXT NOT NULL,
  space_user_hash TEXT NOT NULL,
  PRIMARY KEY (hour, space, app, user_hash)
);
CREATE INDEX IF NOT EXISTS caller_observations_scope_time
  ON caller_observations (space, app, hour);
`

func (s *Store) InsertCallers(ctx context.Context, batch CallerBatch, now time.Time) (accepted, rejected int, err error) {
	if len(batch.Events) > 5000 {
		return 0, len(batch.Events), errors.New("caller batch exceeds 5000 events")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	for _, event := range batch.Events {
		if !config.ValidName(event.Space) || !config.ValidName(event.App) || event.TimeUnixNano == 0 || event.TimeUnixNano > math.MaxInt64 ||
			event.UserHash == "" || event.SpaceUserHash == "" || !validVisitorHash(event.UserHash) || !validVisitorHash(event.SpaceUserHash) {
			rejected++
			continue
		}
		at := time.Unix(0, int64(event.TimeUnixNano)).UTC()
		if at.Before(now.AddDate(0, 0, -s.retentionDays)) || at.After(now.Add(5*time.Minute)) {
			rejected++
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO caller_observations (hour,space,app,user_hash,space_user_hash)
VALUES (?,?,?,?,?) ON CONFLICT(hour,space,app,user_hash) DO NOTHING`,
			at.Truncate(time.Hour).Unix(), event.Space, event.App, event.UserHash, event.SpaceUserHash); err != nil {
			return 0, 0, err
		}
		accepted++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return accepted, rejected, nil
}

func (s *Store) callerCounts(ctx context.Context, space, app string, window trafficWindow, total *trafficAggregate, series map[string]*trafficAggregate) error {
	column, where := "space_user_hash", "space = ? AND hour >= ? AND hour <= ?"
	args := []any{space, window.from.Unix(), window.through.Unix()}
	if app != "" {
		column = "user_hash"
		where += " AND app = ?"
		args = append(args, app)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT "+column+") FROM caller_observations WHERE "+where, args...).Scan(&total.AuthenticatedCallers); err != nil {
		return err
	}
	// Group in SQL so a long reporting window does not load every caller
	// into the query service's memory.
	bucket := "hour"
	if window.granularity == "day" {
		bucket = "CAST(FLOOR(hour / 86400.0) AS BIGINT) * 86400"
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+bucket+",COUNT(DISTINCT "+column+") FROM caller_observations WHERE "+where+" GROUP BY 1", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var at, count int64
		if err := rows.Scan(&at, &count); err != nil {
			return err
		}
		if agg := series[window.key(time.Unix(at, 0))]; agg != nil {
			agg.AuthenticatedCallers = count
		}
	}
	return rows.Err()
}
