// Package deployment stores release history and image scan artifacts beside
// source history on the orchestrator's data volume. Writes are atomic renames
// within one directory, including when that volume is backed by NFS.
package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

var ErrNotFound = errors.New("no matching deployment record")

const RetainBuilds = 20

type BuildSummary struct {
	Sequence int64  `json:"sequence"`
	Created  string `json:"created"`
	Image    string `json:"image,omitempty"`
	OK       bool   `json:"ok"`
}

type record struct {
	BuildSummary
	Record json.RawMessage `json:"record"`
}

type Store struct {
	Dir string
	mu  sync.Mutex
}

func (s *Store) appDir(space, app string) (string, error) {
	if s == nil || s.Dir == "" {
		return "", errors.New("deployment data directory is not configured")
	}
	if !config.ValidName(space) || !config.ValidName(app) {
		return "", fmt.Errorf("invalid deployment scope %q/%q", space, app)
	}
	return filepath.Join(s.Dir, space, app), nil
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	// Persist the rename before reporting success to a migration that may
	// then delete its source record.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *Store) builds(space, app string) ([]record, error) {
	dir, err := s.appDir(space, app)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, "releases"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	rows := []record{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "releases", entry.Name()))
		if os.IsNotExist(err) {
			continue
		} // retention raced a reader
		if err != nil {
			return nil, err
		}
		var row record
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("read release %s: %w", entry.Name(), err)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Sequence > rows[j].Sequence })
	return rows, nil
}

func (s *Store) Builds(ctx context.Context, space, app string) ([]BuildSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.builds(space, app)
	if err != nil {
		return nil, err
	}
	result := make([]BuildSummary, len(rows))
	for i, row := range rows {
		result[i] = row.BuildSummary
	}
	return result, nil
}

func (s *Store) Build(ctx context.Context, space, app string, sequence int64) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := s.appDir(space, app)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "releases", fmt.Sprintf("%020d.json", sequence)))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var row record
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return row.Record, nil
}

func (s *Store) BuildByImage(ctx context.Context, space, app, image string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := s.builds(space, app)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Image == image {
			return row.Record, nil
		}
	}
	return nil, ErrNotFound
}

func (s *Store) RecordBuild(ctx context.Context, space, app, image string, ok bool, raw json.RawMessage) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.builds(space, app)
	if err != nil {
		return 0, err
	}
	sequence := int64(1)
	if len(rows) > 0 {
		sequence = rows[0].Sequence + 1
	}
	summary := BuildSummary{Sequence: sequence, Created: time.Now().UTC().Format(time.RFC3339), Image: image, OK: ok}
	if err := s.importBuild(ctx, space, app, summary, raw); err != nil {
		return 0, err
	}
	dir, _ := s.appDir(space, app)
	for i := RetainBuilds - 1; i < len(rows); i++ {
		if err := os.Remove(filepath.Join(dir, "releases", fmt.Sprintf("%020d.json", rows[i].Sequence))); err != nil && !os.IsNotExist(err) {
			return sequence, err
		}
	}
	return sequence, nil
}

// ImportBuild preserves build numbers, timestamps and Git's build-N links.
// It is idempotent, so an interrupted startup migration can resume.
func (s *Store) ImportBuild(ctx context.Context, space, app string, summary BuildSummary, raw json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.importBuild(ctx, space, app, summary, raw)
}

func (s *Store) importBuild(ctx context.Context, space, app string, summary BuildSummary, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := s.appDir(space, app)
	if err != nil {
		return err
	}
	if summary.Sequence < 1 || !json.Valid(raw) {
		return fmt.Errorf("invalid deployment record")
	}
	data, err := json.Marshal(record{BuildSummary: summary, Record: raw})
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "releases", fmt.Sprintf("%020d.json", summary.Sequence)), data)
}

func (s *Store) Scan(ctx context.Context, space, app, image string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := s.appDir(space, app)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "scans", fmt.Sprintf("%x.json", sha256.Sum256([]byte(image)))))
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return raw, err
}

func (s *Store) PutScan(ctx context.Context, space, app, image string, raw json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := s.appDir(space, app)
	if err != nil {
		return err
	}
	if image == "" || !json.Valid(raw) {
		return fmt.Errorf("image and a valid scan record are required")
	}
	return atomicWrite(filepath.Join(dir, "scans", fmt.Sprintf("%x.json", sha256.Sum256([]byte(image)))), raw)
}

func (s *Store) RemoveApp(space, app string) error {
	dir, err := s.appDir(space, app)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func (s *Store) RemoveSpace(space string) error {
	dir, err := s.appDir(space, "unused")
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Dir(dir))
}

func (s *Store) Migrated() (bool, error) {
	if s == nil || s.Dir == "" {
		return false, errors.New("deployment data directory is not configured")
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, ".telemetry-migrated"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return string(raw) == "1", err
}

func (s *Store) MarkMigrated() error {
	return atomicWrite(filepath.Join(s.Dir, ".telemetry-migrated"), []byte("1"))
}
