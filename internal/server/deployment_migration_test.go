package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/telemetry"
)

func TestDeploymentMigrationResumesAndPreservesBuildNumbers(t *testing.T) {
	var requests atomic.Int32
	var purged, failPurge atomic.Bool
	failPurge.Store(true)
	release := releaseFixture()
	release.Scan = scanRecord(&build.ScanReport{Scanner: "trivy", SBOM: []byte("inventory")}, defaultPolicy(), scanEvaluation{}, nil)
	raw, _ := json.Marshal(release)
	summaries := []telemetry.BuildSummary{{Sequence: 8, Created: "2026-10-25T07:00:00Z", Image: release.Result.ImageRef, OK: true}, {Sequence: 7, Created: "2026-10-24T07:00:00Z", Image: release.Result.ImageRef}}
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/builds/scopes":
			scopes := []telemetry.BuildScope{}
			if !purged.Load() {
				scopes = append(scopes, telemetry.BuildScope{Space: "acme", App: "web"})
			}
			writeJSON(w, http.StatusOK, scopes)
		case "GET /v1/builds":
			writeJSON(w, http.StatusOK, map[string]any{"builds": summaries})
		case "GET /v1/builds/record":
			_, _ = w.Write(raw)
		case "DELETE /v1/builds":
			if failPurge.Load() {
				http.Error(w, "try again", http.StatusServiceUnavailable)
				return
			}
			purged.Store(true)
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		default:
			t.Errorf("unexpected legacy request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer legacy.Close()
	s := &Server{cfg: &Config{DataDir: t.TempDir()}, telemetry: telemetry.NewClient(legacy.URL, nil)}
	ctx := context.Background()
	if err := s.migrateTelemetryDeployments(ctx); err == nil {
		t.Fatal("purge failure was ignored")
	}
	if done, _ := s.deploymentStore().Migrated(); done {
		t.Fatal("incomplete migration marked finished")
	}
	if _, err := s.deploymentStore().Build(ctx, "acme", "web", 8); err != nil {
		t.Fatal("source purge attempted before copying", err)
	}
	failPurge.Store(false)
	if err := s.migrateTelemetryDeployments(ctx); err != nil {
		t.Fatal(err)
	}
	builds, err := s.deploymentStore().Builds(ctx, "acme", "web")
	if err != nil || len(builds) != 2 || builds[0].Sequence != 8 || builds[0].Created != summaries[0].Created || !purged.Load() {
		t.Fatalf("migration changed history: %+v %v", builds, err)
	}
	if scan, err := s.loadScan(ctx, "acme", "web", release.Result.ImageRef); err != nil || scan.SBOM != release.Scan.SBOM {
		t.Fatalf("lost SBOM: %+v %v", scan, err)
	}
	count := requests.Load()
	restarted := &Server{cfg: s.cfg, telemetry: s.telemetry}
	if err := restarted.migrateTelemetryDeployments(ctx); err != nil || requests.Load() != count {
		t.Fatal("completed migration contacted telemetry", err)
	}
	if sequence, err := restarted.deploymentStore().RecordBuild(ctx, "acme", "web", "registry/web:2", true, raw); err != nil || sequence != 9 {
		t.Fatalf("build sequence after migration: %d %v", sequence, err)
	}
}

func TestDeploymentMigrationKeepsSourceOnCopyFailure(t *testing.T) {
	for _, mode := range []string{"unwritable destination", "corrupt record", "incomplete scan", "missing builds"} {
		t.Run(mode, func(t *testing.T) {
			var purged atomic.Bool
			legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/builds/scopes":
					writeJSON(w, http.StatusOK, []telemetry.BuildScope{{Space: "acme", App: "web"}})
				case "GET /v1/builds":
					if mode == "missing builds" {
						writeJSON(w, http.StatusOK, map[string]any{})
					} else {
						writeJSON(w, http.StatusOK, map[string]any{"builds": []telemetry.BuildSummary{{Sequence: 1, Image: "registry/app:1"}}})
					}
				case "GET /v1/builds/record":
					if mode == "corrupt record" {
						_, _ = w.Write([]byte(`{"result":`))
					} else if mode == "incomplete scan" {
						_, _ = w.Write([]byte(`{"scan":{}}`))
					} else {
						writeJSON(w, http.StatusOK, releaseFixture())
					}
				case "DELETE /v1/builds":
					purged.Store(true)
					writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
				default:
					http.NotFound(w, r)
				}
			}))
			defer legacy.Close()
			s := &Server{cfg: &Config{DataDir: t.TempDir()}, telemetry: telemetry.NewClient(legacy.URL, nil)}
			if mode == "unwritable destination" {
				if err := os.MkdirAll(s.deploymentStore().Dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(s.deploymentStore().Dir, "acme"), []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.migrateTelemetryDeployments(context.Background()); err == nil || purged.Load() {
				t.Fatalf("failed copy deleted its source: %v", err)
			}
			if done, _ := s.deploymentStore().Migrated(); done {
				t.Fatal("failed copy marked complete")
			}
		})
	}
}
