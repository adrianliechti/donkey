package server

// Temporary startup migration; remove in the next release. The marker lives
// on donkey-data. A failed copy never deletes the source, and retries preserve
// build numbers so Git's build tags and rollback references keep working.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"

	"github.com/adrianliechti/donkey/internal/deployment"
)

func (s *Server) migrateTelemetryDeployments(ctx context.Context) error {
	if s.cfg.DataDir == "" {
		return nil
	}
	store := s.deploymentStore()
	if done, err := store.Migrated(); err != nil || done {
		return err
	}
	scopes, err := s.telemetry.BuildScopes(ctx)
	if err != nil {
		return fmt.Errorf("migrate deployment data: %w", err)
	}
	for _, scope := range scopes {
		builds, err := s.telemetry.Builds(ctx, scope.Space, scope.App)
		if err != nil {
			return err
		}
		if len(builds) == 0 {
			return fmt.Errorf("migrate %s/%s: telemetry listed a scope without build records", scope.Space, scope.App)
		}
		// Oldest first makes the newest record win for reused image tags.
		for _, summary := range slices.Backward(builds) {
			raw, err := s.telemetry.Build(ctx, scope.Space, scope.App, summary.Sequence)
			if err != nil {
				return err
			}
			var release AppRelease
			if err := json.Unmarshal(raw, &release); err != nil {
				return fmt.Errorf("migrate %s/%s build %d: %w", scope.Space, scope.App, summary.Sequence, err)
			}
			if err := store.ImportBuild(ctx, scope.Space, scope.App, deployment.BuildSummary(summary), raw); err != nil {
				return err
			}
			copied, err := store.Build(ctx, scope.Space, scope.App, summary.Sequence)
			canonical, canonicalErr := json.Marshal(raw)
			if err != nil || canonicalErr != nil || !bytes.Equal(copied, canonical) {
				return fmt.Errorf("verify migrated build %s/%s/%d", scope.Space, scope.App, summary.Sequence)
			}
			if release.Scan != nil && summary.Image != "" {
				if err := s.storeScan(ctx, scope.Space, scope.App, summary.Image, release.Scan); err != nil {
					return err
				}
			}
		}
		if err := s.telemetry.PurgeBuilds(ctx, scope.Space, scope.App); err != nil {
			return err
		}
		log.Printf("migrated deployment history space=%s app=%s builds=%d", scope.Space, scope.App, len(builds))
	}
	return store.MarkMigrated()
}
