package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestHistorySurvivesRestartAndRetainsNewest(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	ctx := context.Background()
	var wg sync.WaitGroup
	for range RetainBuilds + 4 {
		wg.Go(func() {
			if _, err := s.RecordBuild(ctx, "team", "web", "registry/web:1", true, json.RawMessage(`{"result":{"ok":true}}`)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	reopened := &Store{Dir: s.Dir}
	builds, err := reopened.Builds(ctx, "team", "web")
	if err != nil || len(builds) != RetainBuilds || builds[0].Sequence != RetainBuilds+4 || builds[len(builds)-1].Sequence != 5 {
		t.Fatalf("retention: %+v, %v", builds, err)
	}
	if _, err := reopened.Build(ctx, "team", "web", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired record: %v", err)
	}
	if _, err := reopened.BuildByImage(ctx, "team", "web", "registry/web:2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong image matched: %v", err)
	}
	if raw, err := reopened.BuildByImage(ctx, "team", "web", "registry/web:1"); err != nil || !json.Valid(raw) {
		t.Fatalf("stored image: %s %v", raw, err)
	}
}

func TestScanArtifactsRemainAvailableAndAreScoped(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	ctx := context.Background()
	for _, app := range []string{"web", "api"} {
		if err := s.PutScan(ctx, "team", app, "registry/app:1", json.RawMessage(`{"sbom":"inventory"}`)); err != nil {
			t.Fatal(err)
		}
	}
	for i := range RetainBuilds + 1 {
		if _, err := s.RecordBuild(ctx, "team", "web", fmt.Sprint("image:", i), false, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Scan(ctx, "team", "web", "registry/app:1"); err != nil {
		t.Fatal("retention removed inventory", err)
	}
	if _, err := s.Scan(ctx, "team", "web", "registry/app:2"); !errors.Is(err, ErrNotFound) {
		t.Fatal("wrong image matched", err)
	}
	if err := s.RemoveApp("team", "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, "team", "web", "registry/app:1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("app removal left inventory", err)
	}
	if _, err := s.Scan(ctx, "team", "api", "registry/app:1"); err != nil {
		t.Fatal("app removal crossed scope", err)
	}
	if err := s.RemoveSpace("team"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, "team", "api", "registry/app:1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("space removal left inventory", err)
	}
	if err := s.RemoveSpace("../outside"); err == nil {
		t.Fatal("unsafe scope accepted")
	}
}
