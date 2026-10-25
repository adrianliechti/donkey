package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/deployment"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

const scanRetryInterval = 15 * time.Minute
const rescanTimeout = 5 * time.Minute

// Only the orchestrator starts this loop. Due times live on the workloads,
// so a restart resumes overdue work without rescanning the entire fleet.
func (s *Server) scanLoop(ctx context.Context) {
	interval := s.cfg.Build.Scan.RescanInterval()
	if interval == 0 || s.scanner == nil || s.cfg.DataDir == "" {
		return
	}
	if _, ok := s.rt.(runtime.SecurityProvider); !ok {
		return
	}
	ticker := time.NewTicker(min(interval, scanRetryInterval))
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.rescanApps(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func scanDue(security *AppSecurity, now time.Time, interval time.Duration) bool {
	if security == nil || interval <= 0 {
		return false
	}
	if last, err := time.Parse(time.RFC3339, security.AttemptedAt); err == nil && now.Sub(last) < scanRetryInterval {
		return false
	}
	last, err := time.Parse(time.RFC3339, security.ScannedAt)
	return err != nil || now.Sub(last) >= interval
}

func (s *Server) rescanApps(ctx context.Context, now time.Time) {
	started := time.Now()
	if s.cfg.Build.Scan.RescanInterval() == 0 || s.draining.Load() || s.board.active() {
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	apps, err := s.rt.ListApps(listCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("list apps for vulnerability rescan: %v", err)
		}
		return
	}
	for _, app := range apps {
		if ctx.Err() != nil || s.draining.Load() || s.board.active() {
			return
		}
		appCtx, cancel := context.WithTimeout(ctx, rescanTimeout+30*time.Second)
		err := s.rescanApp(appCtx, app, now.Add(time.Since(started)))
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("rescan app=%s space=%s: %v", app.App, app.Space, err)
		}
	}
}

// Brief locks protect the snapshot and publication. Matching happens outside
// them; deployments and deletions can proceed while the scanner is working.
func (s *Server) tryScanLock(app runtime.AppRef) (func(), bool) {
	space := s.spaceLock(app.Space)
	if !space.TryRLock() {
		return nil, false
	}
	mu := s.appLock(app.Space, app.App)
	if !mu.TryLock() {
		space.RUnlock()
		return nil, false
	}
	return func() { mu.Unlock(); space.RUnlock() }, true
}

func (s *Server) rescanApp(ctx context.Context, app runtime.AppRef, now time.Time) error {
	started := time.Now()
	updater, ok := s.rt.(runtime.SecurityProvider)
	if !ok {
		return nil
	}
	unlock, ok := s.tryScanLock(app)
	if !ok {
		return nil
	}
	before, err := s.rt.Status(ctx, app.Space, app.App)
	unlock()
	if err != nil {
		return err
	}
	if before == nil || !before.Found || before.Image == "" || !scanDue(parseSecurity(before.Security), now, s.cfg.Build.Scan.RescanInterval()) {
		return nil
	}

	stored, scanErr := s.loadScan(ctx, app.Space, app.App, before.Image)
	var refreshed *AppScan
	if scanErr == nil && stored != nil && !scanDue(securityOf(stored), now, s.cfg.Build.Scan.RescanInterval()) {
		// The record may already be fresh if publishing its badge failed.
		refreshed = stored
	} else if scanErr == nil && stored != nil && stored.SBOM != "" {
		var sbom []byte
		sbom, scanErr = stored.SBOMBytes()
		if scanErr == nil {
			matchCtx, cancel := context.WithTimeout(ctx, rescanTimeout)
			report, err := s.scanner.Rescan(matchCtx, sbom)
			cancel()
			scanErr = err
			if err == nil {
				// Secrets require the image's files. Preserve the build-time
				// secret scan; an SBOM refresh cannot clear those findings.
				report.Secrets = stored.Secrets
				report.SBOM = nil
				refreshed = scanRecord(report, stored.Policy, evaluateScan(report, stored.Policy), scanVerdicts(stored))
				refreshed.SBOM = stored.SBOM
				refreshed.ScannedAt = time.Now().UTC().Format(time.RFC3339)
			}
		}
	} else if scanErr == nil {
		// An app with a security badge was scanned before. Missing inventory
		// is a storage error; silently skipping it would leave a clean badge
		// forever. Legacy apps without a badge were excluded above.
		scanErr = fmt.Errorf("the running image's stored inventory is missing; rebuild the app to restore it")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	unlock, ok = s.tryScanLock(app)
	if !ok {
		return nil
	}
	defer unlock()
	after, err := s.rt.Status(ctx, app.Space, app.App)
	if err != nil {
		return err
	}
	if after == nil || !after.Found || after.Image != before.Image || after.Security != before.Security || !after.StartedAt.Equal(before.StartedAt) {
		return nil // another deploy, restart or deletion superseded this scan
	}
	if scanErr == nil {
		// Waivers may have changed while matching. Re-read them under the
		// space lock, including expirations, before publishing the badge.
		refreshed, scanErr = s.reevaluateScan(ctx, app.Space, app.App, refreshed)
	}
	if scanErr == nil {
		scanErr = s.storeScan(ctx, app.Space, app.App, before.Image, refreshed)
	}
	security := securityJSON(refreshed)
	if scanErr != nil {
		last := parseSecurity(before.Security)
		last.RefreshError = "The latest vulnerability check failed; the previous findings are still shown."
		// Back off from completion, even when earlier apps or this match
		// took minutes. A timestamp from the start of the pass can already
		// be older than the retry interval when the failure is published.
		last.AttemptedAt = now.Add(time.Since(started)).UTC().Format(time.RFC3339)
		raw, _ := json.Marshal(last)
		security = string(raw)
	}
	if err := updater.UpdateSecurity(ctx, app.Space, app.App, before.Image, security); err != nil {
		return err
	}
	return scanErr
}

func scanVerdicts(stored *AppScan) map[string]ai.Verdict {
	verdicts := make(map[string]ai.Verdict, len(stored.Verdicts))
	for _, verdict := range stored.Verdicts {
		verdicts[verdict.ID] = verdict
	}
	return verdicts
}

// loadScan prefers the latest runtime check and falls back to deployment
// history for images that have not been picked up by the periodic worker.
func (s *Server) loadScan(ctx context.Context, space, app, image string) (*AppScan, error) {
	raw, err := s.deploymentStore().Scan(ctx, space, app, image)
	if err == nil {
		var record *AppScan
		if err := json.Unmarshal(raw, &record); err != nil {
			return nil, err
		}
		if err := record.validate(); err != nil {
			return nil, err
		}
		return record, nil
	}
	if !errors.Is(err, deployment.ErrNotFound) {
		return nil, err
	}
	raw, err = s.deploymentStore().BuildByImage(ctx, space, app, image)
	if errors.Is(err, deployment.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var release AppRelease
	if err := json.Unmarshal(raw, &release); err != nil {
		return nil, err
	}
	if release.Scan != nil {
		if err := release.Scan.validate(); err != nil {
			return nil, err
		}
	}
	return release.Scan, nil
}

func (s *Server) storeScan(ctx context.Context, space, app, image string, record *AppScan) error {
	if err := record.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.deploymentStore().PutScan(ctx, space, app, image, raw)
}

func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	space, app := r.PathValue("space"), r.PathValue("app")
	status, err := s.rt.Status(r.Context(), space, app)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if status == nil || !status.Found {
		writeErr(w, http.StatusNotFound, fmt.Errorf("app not found"))
		return
	}
	record, err := s.loadScan(r.Context(), space, app, status.Image)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scan": record.Shown()})
}
