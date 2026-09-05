package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// Build records are the versioned deploy history: one bounded JSON record per
// build, success or failure, written by donkey-server and rendered by the
// console. They live here because this is donkey's one durable, single-writer
// store — the record's shape belongs to the server; this side only versions,
// lists and retires it.

// buildRetainPerApp bounds the history per app. Old records leave when new
// ones arrive; deleting the space deletes them all (PurgeSpace).
const buildRetainPerApp = 20

// ErrNoBuild reports that no matching build record is stored.
var ErrNoBuild = errors.New("no matching build record")

// BuildSummary is one line of an app's build history.
type BuildSummary struct {
	Sequence int64  `json:"sequence"`
	Created  string `json:"created"`
	Image    string `json:"image,omitempty"`
	OK       bool   `json:"ok"`
}

func validBuildScope(space, app string) error {
	if !config.ValidName(space) || !config.ValidName(app) {
		return fmt.Errorf("invalid build record scope %q/%q", space, app)
	}
	return nil
}

// InsertBuild appends one record to the app's history and retires records
// beyond the retention window. It returns the record's sequence number.
func (s *Store) InsertBuild(ctx context.Context, space, app, image string, ok bool, record string, now time.Time) (int64, error) {
	if err := validBuildScope(space, app); err != nil {
		return 0, err
	}
	if len(record) == 0 {
		return 0, fmt.Errorf("build record must not be empty")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var sequence int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(sequence), 0) + 1 FROM build_records WHERE space = ? AND app = ?",
		space, app,
	).Scan(&sequence); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO build_records (space, app, sequence, created, image, ok, record) VALUES (?, ?, ?, ?, ?, ?, ?)",
		space, app, sequence, now.UTC().Unix(), image, ok, record,
	); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM build_records WHERE space = ? AND app = ? AND sequence <= ?",
		space, app, sequence-buildRetainPerApp,
	); err != nil {
		return 0, err
	}
	return sequence, tx.Commit()
}

// Builds lists an app's retained history, newest first.
func (s *Store) Builds(ctx context.Context, space, app string) ([]BuildSummary, error) {
	if err := validBuildScope(space, app); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT sequence, created, image, ok FROM build_records WHERE space = ? AND app = ? ORDER BY sequence DESC",
		space, app,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	builds := []BuildSummary{}
	for rows.Next() {
		var b BuildSummary
		var created int64
		if err := rows.Scan(&b.Sequence, &created, &b.Image, &b.OK); err != nil {
			return nil, err
		}
		b.Created = time.Unix(created, 0).UTC().Format(time.RFC3339)
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

// Build returns one record by sequence.
func (s *Store) Build(ctx context.Context, space, app string, sequence int64) (string, error) {
	if err := validBuildScope(space, app); err != nil {
		return "", err
	}
	var record string
	err := s.db.QueryRowContext(ctx,
		"SELECT record FROM build_records WHERE space = ? AND app = ? AND sequence = ?",
		space, app, sequence,
	).Scan(&record)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoBuild
	}
	return record, err
}

// BuildByImage returns the newest record that produced or released the given
// image reference — how a `--no-build --tag` redeploy recovers the decisions
// that tag was actually verified with.
func (s *Store) BuildByImage(ctx context.Context, space, app, image string) (string, error) {
	if err := validBuildScope(space, app); err != nil {
		return "", err
	}
	var record string
	err := s.db.QueryRowContext(ctx,
		"SELECT record FROM build_records WHERE space = ? AND app = ? AND image = ? ORDER BY sequence DESC LIMIT 1",
		space, app, image,
	).Scan(&record)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoBuild
	}
	return record, err
}
