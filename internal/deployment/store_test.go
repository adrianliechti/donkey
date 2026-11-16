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

// A redeploy of an image is a record of its own: every record of the image
// is found, newest first, and no other image's.
func TestBuildsByImageListsEveryRecordOfTheImageNewestFirst(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	ctx := context.Background()
	for i, image := range []string{"registry/web:1", "registry/web:2", "registry/web:1"} {
		if _, err := s.RecordBuild(ctx, "team", "web", image, true, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	records, err := s.BuildsByImage(ctx, "team", "web", "registry/web:1")
	if err != nil || len(records) != 2 || string(records[0]) != `{"n":2}` || string(records[1]) != `{"n":0}` {
		t.Fatalf("records: %s %v", records, err)
	}
	if records, err := s.BuildsByImage(ctx, "team", "web", "registry/web:9"); err != nil || len(records) != 0 {
		t.Fatalf("another image: %s %v", records, err)
	}
	if records, err := s.BuildsByImage(ctx, "team", "never-deployed", "registry/web:1"); err != nil || len(records) != 0 {
		t.Fatalf("an app without history: %s %v", records, err)
	}
	if _, err := s.BuildsByImage(ctx, "../outside", "web", "registry/web:1"); err == nil {
		t.Fatal("unsafe scope accepted")
	}
}

// A reader that kept a scan can tell whether it is still the one stored
// without reading it again.
func TestScanStampChangesWhenTheScanIsRewritten(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	ctx := context.Background()
	if stamp, err := s.ScanStamp("team", "web", "registry/web:1"); err != nil || !stamp.IsZero() {
		t.Fatalf("no scan stored: %+v %v", stamp, err)
	}
	if err := s.PutScan(ctx, "team", "web", "registry/web:1", json.RawMessage(`{"findings":1}`)); err != nil {
		t.Fatal(err)
	}
	first, err := s.ScanStamp("team", "web", "registry/web:1")
	if err != nil || first.IsZero() {
		t.Fatalf("stored scan: %+v %v", first, err)
	}
	if again, _ := s.ScanStamp("team", "web", "registry/web:1"); !again.Is(first) {
		t.Fatalf("an unchanged scan changed its stamp: %+v then %+v", first, again)
	}
	if other, _ := s.ScanStamp("team", "web", "registry/web:2"); !other.IsZero() {
		t.Fatalf("another image's stamp: %+v", other)
	}
	if err := s.PutScan(ctx, "team", "web", "registry/web:1", json.RawMessage(`{"findings":12}`)); err != nil {
		t.Fatal(err)
	}
	if second, _ := s.ScanStamp("team", "web", "registry/web:1"); second.Is(first) {
		t.Fatalf("a rewritten scan kept its stamp: %+v", second)
	}
	if _, err := s.ScanStamp("../outside", "web", "registry/web:1"); err == nil {
		t.Fatal("unsafe scope accepted")
	}
}
