package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBuildRecordsVersionRetainAndPurge(t *testing.T) {
	store, err := OpenStore(":memory:", DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := time.Now().UTC()

	for i := 1; i <= buildRetainPerApp+5; i++ {
		image := fmt.Sprintf("registry/web:%d", i)
		record := fmt.Sprintf(`{"result":{"imageRef":%q}}`, image)
		sequence, err := store.InsertBuild(ctx, "acme", "web", image, i%2 == 0, record, now)
		if err != nil {
			t.Fatal(err)
		}
		if sequence != int64(i) {
			t.Fatalf("sequence = %d, want %d", sequence, i)
		}
	}
	if _, err := store.InsertBuild(ctx, "acme", "other", "registry/other:1", true, `{}`, now); err != nil {
		t.Fatal(err)
	}

	builds, err := store.Builds(ctx, "acme", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != buildRetainPerApp {
		t.Fatalf("retained %d records, want %d", len(builds), buildRetainPerApp)
	}
	if builds[0].Sequence != int64(buildRetainPerApp+5) || builds[len(builds)-1].Sequence != 6 {
		t.Fatalf("history should be newest-first with the oldest retired: %d..%d",
			builds[0].Sequence, builds[len(builds)-1].Sequence)
	}

	// A retired record is gone; a retained one comes back verbatim.
	if _, err := store.Build(ctx, "acme", "web", 3); !errors.Is(err, ErrNoBuild) {
		t.Fatalf("retired record should be ErrNoBuild, got %v", err)
	}
	record, err := store.Build(ctx, "acme", "web", 7)
	if err != nil || record != `{"result":{"imageRef":"registry/web:7"}}` {
		t.Fatalf("record 7 = %q, %v", record, err)
	}

	// The newest record for an exact image reference — the rollback lookup.
	byImage, err := store.BuildByImage(ctx, "acme", "web", "registry/web:10")
	if err != nil || byImage != `{"result":{"imageRef":"registry/web:10"}}` {
		t.Fatalf("by-image record = %q, %v", byImage, err)
	}
	if _, err := store.BuildByImage(ctx, "acme", "web", "registry/web:nope"); !errors.Is(err, ErrNoBuild) {
		t.Fatalf("unknown image should be ErrNoBuild, got %v", err)
	}

	// Deleting the space deletes its history and nobody else's.
	if err := store.PurgeSpace(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if builds, err := store.Builds(ctx, "acme", "web"); err != nil || len(builds) != 0 {
		t.Fatalf("purged space still has %d records (%v)", len(builds), err)
	}
}

func TestBuildRecordsValidateScopeAndKeepLargeRecords(t *testing.T) {
	store, err := OpenStore(":memory:", DefaultRetentionDays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if _, err := store.InsertBuild(ctx, "../etc", "web", "", true, `{}`, time.Now()); err == nil {
		t.Fatal("an invalid space name must be refused")
	}
	if _, err := store.InsertBuild(ctx, "acme", "web", "", true, "", time.Now()); err == nil {
		t.Fatal("an empty record must be refused")
	}
	huge := fmt.Sprintf(`{"buildLog":%q}`, strings.Repeat("large build record ", 600000))
	sequence, err := store.InsertBuild(ctx, "acme", "web", "", true, huge, time.Now())
	if err != nil {
		t.Fatalf("large record rejected: %v", err)
	}
	if got, err := store.Build(ctx, "acme", "web", sequence); err != nil || got != huge {
		t.Fatalf("large record changed when read back: %v", err)
	}
}
