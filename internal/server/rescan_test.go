package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type rescanRuntime struct {
	repairRuntime
	status    runtime.AppStatus
	updates   int
	updateErr error
}

func (r *rescanRuntime) ListApps(context.Context) ([]runtime.AppRef, error) {
	return []runtime.AppRef{{Space: "team", App: "app"}}, nil
}
func (r *rescanRuntime) Status(context.Context, string, string) (*runtime.AppStatus, error) {
	copy := r.status
	return &copy, nil
}
func (r *rescanRuntime) UpdateSecurity(_ context.Context, _, _, image, security string) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	if image != r.status.Image {
		return errors.New("wrong image")
	}
	r.updates++
	r.status.Security = security
	return nil
}

func rescanFixture(t *testing.T) (*Server, *rescanRuntime, *AppScan) {
	t.Helper()
	record := scanRecord(&build.ScanReport{}, defaultPolicy(), scanEvaluation{}, nil)
	record.SBOM = encodeSBOM([]byte(`{"spdxVersion":"SPDX-2.3"}`))
	record.ScannedAt = time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	rt := &rescanRuntime{status: runtime.AppStatus{Found: true, Image: "registry/app:1", Security: securityJSON(record), StartedAt: time.Now().Add(-time.Hour)}}
	s := &Server{cfg: &Config{DataDir: t.TempDir()}, rt: rt, scanner: &build.Scanner{Dir: t.TempDir()}}
	s.cfg.Build.Scan.Triage = config.ScanTriageOff
	if err := s.storeScan(context.Background(), "team", "app", rt.status.Image, record); err != nil {
		t.Fatal(err)
	}
	return s, rt, record
}

func TestPeriodicRescanFindsNewVulnerabilitiesAndKeepsInventory(t *testing.T) {
	s, rt, original := rescanFixture(t)
	calls := 0
	s.scanner.Run = func(_ context.Context, dir string, args, env []string) ([]byte, error) {
		calls++
		if args[0] != "sbom" {
			t.Fatal("periodic scan pulled an image")
		}
		return nil, os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-new","Severity":"CRITICAL","PkgName":"lib","InstalledVersion":"1","FixedVersion":"2"}]}]}`), 0600)
	}
	s.rescanApps(context.Background(), time.Now())
	security := parseSecurity(rt.status.Security)
	if calls != 1 || rt.updates != 1 || security.State != SecurityNeedsAction || security.ScannedAt == original.ScannedAt {
		t.Fatalf("rescan: calls=%d updates=%d security=%+v", calls, rt.updates, security)
	}
	stored, err := s.loadScan(context.Background(), "team", "app", rt.status.Image)
	if err != nil || stored.SBOM != original.SBOM || len(stored.Vulnerabilities) != 1 {
		t.Fatalf("stored findings: %+v %v", stored, err)
	}
	s.rescanApps(context.Background(), time.Now())
	if calls != 1 {
		t.Fatal("fresh inventory matched again")
	}
}

func TestRescanFailurePreservesFindingsAndRetries(t *testing.T) {
	s, rt, original := rescanFixture(t)
	now := time.Now()
	calls := 0
	s.scanner.Run = func(_ context.Context, dir string, args, env []string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("database offline")
		}
		return nil, os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"Results":[]}`), 0600)
	}
	s.rescanApps(context.Background(), now)
	failed := parseSecurity(rt.status.Security)
	if failed.RefreshError == "" || failed.ScannedAt != original.ScannedAt {
		t.Fatalf("failure hid stale data: %+v", failed)
	}
	s.rescanApps(context.Background(), now.Add(10*time.Minute))
	if calls != 1 {
		t.Fatal("retried before backoff")
	}
	s.rescanApps(context.Background(), now.Add(16*time.Minute))
	if calls != 2 || parseSecurity(rt.status.Security).RefreshError != "" {
		t.Fatal("failed to recover")
	}
}

func TestRescanBackoffStartsAfterAttemptCompletes(t *testing.T) {
	s, rt, _ := rescanFixture(t)
	var completed time.Time
	s.scanner.Run = func(context.Context, string, []string, []string) ([]byte, error) {
		time.Sleep(1100 * time.Millisecond) // cross the timestamp's one-second precision
		completed = time.Now().Truncate(time.Second)
		return nil, errors.New("slow database failure")
	}
	s.rescanApps(context.Background(), time.Now())
	failed := parseSecurity(rt.status.Security)
	attempted, err := time.Parse(time.RFC3339, failed.AttemptedAt)
	if err != nil || attempted.Before(completed) {
		t.Fatalf("backoff started before the failed match completed: %s < %s (%v)", failed.AttemptedAt, completed, err)
	}
}

func TestRescanDoesNotOverwriteDeployOrRunWhileLocked(t *testing.T) {
	s, rt, original := rescanFixture(t)
	calls := 0
	s.scanner.Run = func(_ context.Context, dir string, args, env []string) ([]byte, error) {
		calls++
		rt.status.Image = "registry/app:new"
		return nil, os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"Results":[]}`), 0600)
	}
	unlock := s.lockApp("team", "app")
	s.rescanApps(context.Background(), time.Now())
	unlock()
	if calls != 0 {
		t.Fatal("scan blocked foreground deploy")
	}
	s.rescanApps(context.Background(), time.Now())
	if calls != 1 || rt.updates != 0 {
		t.Fatal("scan published over another image")
	}
	stored, err := s.loadScan(context.Background(), "team", "app", "registry/app:1")
	if err != nil || stored.ScannedAt != original.ScannedAt {
		t.Fatal("superseded scan was persisted", err)
	}
}

func TestRescanCancellationStopsWorker(t *testing.T) {
	s, rt, _ := rescanFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.scanner.Run = func(ctx context.Context, _ string, _, _ []string) ([]byte, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan struct{})
	go func() { defer close(done); s.scanLoop(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	if rt.updates != 0 {
		t.Fatal("cancelled scan published a result")
	}
}

func TestRescanDiscardsDeletedOrRestartedApp(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-image restart", true: "deleted"}[deleted], func(t *testing.T) {
			s, rt, original := rescanFixture(t)
			s.scanner.Run = func(_ context.Context, dir string, _, _ []string) ([]byte, error) {
				if deleted {
					rt.status.Found = false
				} else {
					rt.status.StartedAt = time.Now()
				}
				return nil, os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"Results":[]}`), 0600)
			}
			s.rescanApps(context.Background(), time.Now())
			stored, err := s.loadScan(context.Background(), "team", "app", rt.status.Image)
			if err != nil || rt.updates != 0 || stored.ScannedAt != original.ScannedAt {
				t.Fatal("superseded scan was published", err)
			}
		})
	}
}

func TestRescanUsesCurrentWaivers(t *testing.T) {
	s, rt, original := rescanFixture(t)
	// The previous scan was waived; that waiver no longer exists in the
	// current space settings. Matching must not keep the old exemption.
	original.Policy.Waivers = []config.SecurityWaiver{{ID: "CVE-new", Reason: "temporary"}}
	if err := s.storeScan(context.Background(), "team", "app", rt.status.Image, original); err != nil {
		t.Fatal(err)
	}
	s.scanner.Run = func(_ context.Context, dir string, _, _ []string) ([]byte, error) {
		return nil, os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"Results":[{"Vulnerabilities":[{"VulnerabilityID":"CVE-new","Severity":"CRITICAL","PkgName":"lib","InstalledVersion":"1","FixedVersion":"2"}]}]}`), 0600)
	}
	s.rescanApps(context.Background(), time.Now())
	if parseSecurity(rt.status.Security).State != SecurityNeedsAction {
		t.Fatal("retained a removed waiver")
	}
}

func TestRescanPreservesSecretsAndRetriesBadgeWithoutMatching(t *testing.T) {
	s, rt, original := rescanFixture(t)
	original.Secrets = []build.Secret{awsKey}
	if err := s.storeScan(context.Background(), "team", "app", rt.status.Image, original); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.scanner.Run = func(_ context.Context, dir string, args, env []string) ([]byte, error) {
		calls++
		return nil, os.WriteFile(filepath.Join(dir, "report.json"), []byte(`{"Results":[]}`), 0600)
	}
	rt.updateErr = errors.New("runtime unavailable")
	s.rescanApps(context.Background(), time.Now())
	rt.updateErr = nil
	s.rescanApps(context.Background(), time.Now())
	if calls != 1 || parseSecurity(rt.status.Security).Summary.Secrets != 1 || parseSecurity(rt.status.Security).State != SecurityNeedsAction {
		t.Fatal("badge retry rescanned or cleared secret findings")
	}
}

func TestRescanReportsMissingOrIncompleteInventory(t *testing.T) {
	for _, mode := range []string{"missing record", "missing SBOM", "null", "{}"} {
		t.Run(mode, func(t *testing.T) {
			s, rt, original := rescanFixture(t)
			ctx := context.Background()
			switch mode {
			case "missing record":
				if err := s.deploymentStore().RemoveApp("team", "app"); err != nil {
					t.Fatal(err)
				}
			case "missing SBOM":
				original.SBOM = ""
				if err := s.storeScan(ctx, "team", "app", rt.status.Image, original); err != nil {
					t.Fatal(err)
				}
			default:
				if err := s.deploymentStore().PutScan(ctx, "team", "app", rt.status.Image, json.RawMessage(mode)); err != nil {
					t.Fatal(err)
				}
			}
			s.rescanApps(ctx, time.Now())
			failed := parseSecurity(rt.status.Security)
			if rt.updates != 1 || failed.RefreshError == "" || failed.ScannedAt != original.ScannedAt {
				t.Fatalf("lost inventory silently skipped: %+v", failed)
			}
			s.rescanApps(ctx, time.Now())
			if rt.updates != 1 {
				t.Fatal("storage error retried without backoff")
			}
		})
	}
}
