package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// Only the orchestrator observes maintenance conditions. Persisting their
// onset lets every gateway replica use the same deadline after a restart.
func (s *Server) maintenanceLoop(ctx context.Context) {
	store, ok := s.rt.(runtime.MaintenanceStore)
	if !ok {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// Sweep once after a restart or a disable, then leave idle installations alone.
	cleared := false
	for {
		platform, err := s.rt.PlatformConfig(ctx)
		if err == nil {
			off := platform.GatewayMaintenance == (config.GatewayMaintenance{})
			if !off || !cleared {
				err = s.reconcileMaintenance(ctx, store, platform)
				cleared = off && err == nil
			}
		}
		if err != nil && !errors.Is(err, runtime.ErrPlatformNotFound) && ctx.Err() == nil {
			log.Printf("gateway maintenance: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) reconcileMaintenance(ctx context.Context, store runtime.MaintenanceStore, platform config.PlatformConfig) error {
	spaces, err := s.rt.ListSpaces(ctx)
	if err != nil {
		return err
	}
	bySpace := make(map[string]map[string]*runtime.AppStatus)
	rules := platform.GatewayMaintenance
	var errs []error
	var appErr error
	if rules.CriticalDays > 0 || rules.UnmaintainedDays > 0 {
		var apps []inventoryApp
		apps, appErr = s.runningApps(ctx)
		if appErr != nil {
			errs = append(errs, fmt.Errorf("listing apps: %w", appErr))
		}
		for i := range apps {
			app := &apps[i]
			if bySpace[app.Space] == nil {
				bySpace[app.Space] = make(map[string]*runtime.AppStatus)
			}
			bySpace[app.Space][app.App] = &app.Status
		}
	}
	now := time.Now()
	for _, space := range spaces {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		apps := bySpace[space]
		if apps == nil && appErr == nil {
			apps = map[string]*runtime.AppStatus{}
		}
		if err := s.observeSpaceMaintenance(ctx, store, space, apps, platform, now); err != nil && !errors.Is(err, runtime.ErrSpaceNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", space, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Server) observeSpaceMaintenance(ctx context.Context, store runtime.MaintenanceStore, space string, apps map[string]*runtime.AppStatus, platform config.PlatformConfig, now time.Time) error {
	mu := s.spaceLock(space)
	mu.RLock()
	defer mu.RUnlock()
	before, err := store.SpaceMaintenance(ctx, space)
	if err != nil {
		return err
	}
	rules := platform.GatewayMaintenance
	next := config.SpaceMaintenance{Apps: make(map[string]config.MaintenanceTimes)}
	// A failed fleet read says nothing about app removal. Preserve observations
	// for enabled app rules and still evaluate the independent space policy.
	if apps == nil {
		apps = make(map[string]*runtime.AppStatus, len(before.Apps))
		for name := range before.Apps {
			apps[name] = nil
		}
	}
	for name, status := range apps {
		times := before.Apps[name]
		if rules.CriticalDays == 0 {
			times.CriticalSince = 0
		}
		if rules.UnmaintainedDays == 0 {
			times.UnmaintainedSince = 0
		}
		if status != nil && status.Found {
			if rules.UnmaintainedDays > 0 && status.Image != "" {
				_, stale := unmaintainedBuild(status.Image, platform.UnmaintainedAfter(), now)
				times.UnmaintainedSince = conditionSince(times.UnmaintainedSince, stale, now)
			}
			if rules.CriticalDays > 0 {
				if scan := parseSecurity(status.Security); scan != nil {
					times.CriticalSince = conditionSince(times.CriticalSince, scan.Summary.Critical > 0, now)
				}
			}
		}
		if times != (config.MaintenanceTimes{}) {
			next.Apps[name] = times
		}
	}
	var policyErr error
	if rules.NoncompliantDays > 0 {
		var reason string
		reason, policyErr = runtime.CurrentServicePolicy(ctx, s.rt, s.cfg.ServicePolicy, space)
		next.NoncompliantSince = before.NoncompliantSince
		if policyErr == nil {
			next.NoncompliantSince = conditionSince(before.NoncompliantSince, reason != "", now)
		}
	}
	if next.NoncompliantSince != before.NoncompliantSince || !maps.Equal(next.Apps, before.Apps) {
		if err := store.SetSpaceMaintenance(ctx, space, next); err != nil {
			return err
		}
	}
	if policyErr != nil {
		return fmt.Errorf("checking service policy: %w", policyErr)
	}
	return nil
}

func conditionSince(previous int64, active bool, now time.Time) int64 {
	if !active {
		return 0
	}
	if previous != 0 {
		return previous
	}
	return now.Unix()
}
