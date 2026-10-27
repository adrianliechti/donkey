//go:build e2e

package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

// The vulnerability scan against a real BuildKit, in the exact shape a
// deploy uses: the dockerfile frontend through the gateway, the scanner
// mounted beside the unexported result writing the inventory and the
// secrets report, the orchestrator-side scanner matching the inventory
// against the database, and the judge's refusal ending the build before
// any export.
//
// The image carries two things the scan must find: an installed lodash
// 4.17.11 (CVE-2019-10744, critical, fixed in 4.17.12 — recognized from
// node_modules alone) and a credential-shaped AWS access key id in a file.
//
// The matcher is the same scanner release, run in its own image: this host
// has no trivy binary, the server image does.
//
//	DONKEY_E2E_SCAN=true go test -tags e2e -run '^TestScan' -v -timeout 15m ./internal/build
func TestScanOnGatewayResult(t *testing.T) {
	if os.Getenv("DONKEY_E2E_SCAN") != "true" {
		t.Skip("set DONKEY_E2E_SCAN=true to run a real scan in Docker")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	addr, _ := osPackageBuildkit(t, ctx)

	dir := t.TempDir()
	lodash := filepath.Join(dir, "node_modules", "lodash")
	if err := os.MkdirAll(lodash, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lodash, "package.json"), []byte(`{"name":"lodash","version":"4.17.11"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Credential-shaped, not a credential: the scanner's rule matches the
	// AKIA prefix plus sixteen upper-case characters.
	if err := os.WriteFile(filepath.Join(dir, "settings.env"), []byte("AWS_ACCESS_KEY_ID=AKIAQWERTYUIOPASDFGH\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recipe := "FROM alpine:3.20\nWORKDIR /app\nCOPY node_modules ./node_modules\nCOPY settings.env /app/settings.env\nCMD [\"sleep\", \"1\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(recipe), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &BuildKitBuilder{BuildkitAddr: addr, Registry: config.Registry{URL: "example.invalid/donkey"}}
	scanner := dockerScanner(t)
	t.Run("base candidate is scanned before building the app", func(t *testing.T) {
		candidate, err := b.InspectBaseImage(ctx, "alpine:3.20", "linux/"+runtime.GOARCH, ScanSpec{Image: config.DefaultScannerImage, Matcher: scanner,
			Judge: func(context.Context, *ScanReport) error {
				t.Error("candidate check invoked final app policy")
				return nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(candidate.Reference, "@sha256:") || candidate.Scan == nil || candidate.Scan.OS == "" || len(candidate.Scan.SBOM) == 0 {
			t.Fatalf("candidate missing resolved identity or scan: %+v", candidate)
		}
	})
	build := func(judge func(context.Context, *ScanReport) error) (*Result, error) {
		logPath := filepath.Join(t.TempDir(), "build.log")
		res, err := b.Build(ctx, Request{
			ContextDir: dir, DockerfilePath: "Dockerfile", Strategy: config.BuildDockerfile,
			ImageRef: "example.invalid/scan:probe", Platform: "linux/" + runtime.GOARCH,
			Scan:     &ScanSpec{Image: config.DefaultScannerImage, Secrets: true, Matcher: scanner, Judge: judge},
			Progress: ProgressSink{LogPath: logPath},
		})
		contents, _ := os.ReadFile(logPath)
		t.Log(string(contents))
		return res, err
	}

	t.Run("the report names the findings", func(t *testing.T) {
		res, err := build(nil)
		if err != nil {
			t.Fatal(err)
		}
		if res.Scan == nil {
			t.Fatal("no report on the result")
		}
		if res.Scan.OS == "" || res.Scan.DBUpdatedAt == "" {
			t.Errorf("report lacks OS or database freshness: %+v", res.Scan)
		}
		var lodash *Vulnerability
		for i := range res.Scan.Vulnerabilities {
			if v := &res.Scan.Vulnerabilities[i]; v.ID == "CVE-2019-10744" && v.Package == "lodash" {
				lodash = v
			}
		}
		if lodash == nil {
			t.Fatalf("CVE-2019-10744 in lodash not found among %d findings", len(res.Scan.Vulnerabilities))
		}
		if lodash.Severity != "critical" || lodash.Installed != "4.17.11" || lodash.Fixed != "4.17.12" || lodash.Type != "node-pkg" {
			t.Errorf("lodash finding = %+v", *lodash)
		}
		found := false
		for _, s := range res.Scan.Secrets {
			if s.Rule == "aws-access-key-id" && s.Target == "app/settings.env" {
				found = true
			}
		}
		if !found {
			t.Errorf("the AWS key id was not found: %+v", res.Scan.Secrets)
		}
	})

	t.Run("a rescan from the stored inventory finds the same, with no image", func(t *testing.T) {
		res, err := build(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Scan.SBOM) == 0 {
			t.Fatal("the build left no inventory")
		}
		t.Logf("inventory: %d bytes", len(res.Scan.SBOM))
		again, err := scanner.Rescan(ctx, res.Scan.SBOM)
		if err != nil {
			t.Fatal(err)
		}
		if again.OS != res.Scan.OS || again.DBUpdatedAt == "" {
			t.Errorf("rescan OS/db = %q/%q, want %q", again.OS, again.DBUpdatedAt, res.Scan.OS)
		}
		found := map[string]bool{}
		for _, v := range again.Vulnerabilities {
			found[v.ID+"/"+v.Package] = true
		}
		for _, v := range res.Scan.Vulnerabilities {
			if !found[v.ID+"/"+v.Package] {
				t.Errorf("the rescan lost %s in %s", v.ID, v.Package)
			}
		}
		if len(again.Secrets) != 0 {
			t.Errorf("an inventory carries no secrets, got %+v", again.Secrets)
		}
		if len(again.SBOM) == 0 {
			t.Error("the rescan should hand the inventory back")
		}
	})

	t.Run("the judge's refusal ends the build", func(t *testing.T) {
		judged := false
		_, err := build(func(_ context.Context, report *ScanReport) error {
			judged = true
			return &ScanBlockedError{Message: fmt.Sprintf("refused: %d findings", len(report.Vulnerabilities)), Report: report}
		})
		if !judged {
			t.Fatal("the judge never ran")
		}
		var blocked *ScanBlockedError
		if !errors.As(err, &blocked) {
			t.Fatalf("want the judge's typed refusal, got %v", err)
		}
		if blocked.Report == nil || len(blocked.Report.Vulnerabilities) == 0 {
			t.Errorf("the refusal should carry the report: %+v", blocked.Report)
		}
	})

	t.Run("a scanner outage stays typed and a retry recovers", func(t *testing.T) {
		run := scanner.Run
		defer func() { scanner.Run = run }()
		unavailable := errors.New("database unavailable")
		scanner.Run = func(context.Context, string, []string, []string) ([]byte, error) {
			return nil, unavailable
		}
		result, err := build(nil)
		var scanErr *ScanError
		if result != nil || !errors.As(err, &scanErr) || !errors.Is(err, unavailable) {
			t.Fatalf("scanner failure lost its type or returned an image: result=%+v err=%v", result, err)
		}
		scanner.Run = run
		if result, err := build(nil); err != nil || result == nil || result.Scan == nil {
			t.Fatalf("retry after scanner recovery failed: result=%+v err=%v", result, err)
		}
	})

	t.Run("a source build failure is not a scanner outage", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine:3.20\nRUN exit 42\n"), 0600); err != nil {
			t.Fatal(err)
		}
		result, err := build(nil)
		var scanErr *ScanError
		if err == nil || result != nil || errors.As(err, &scanErr) {
			t.Fatalf("source failure misclassified: result=%+v err=%v", result, err)
		}
	})
}

// Doctor's probes against the real endpoints: the scanner image resolves
// through the build engine, the database repository answers, and both say
// something useful when they do not.
func TestScanDoctorProbes(t *testing.T) {
	if os.Getenv("DONKEY_E2E_SCAN") != "true" {
		t.Skip("set DONKEY_E2E_SCAN=true to probe real registries")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	addr, _ := osPackageBuildkit(t, ctx)
	reg := config.Registry{URL: "example.invalid/donkey"}

	if err := CheckImageResolves(ctx, addr, reg, nil, config.DefaultScannerImage, "linux/"+runtime.GOARCH); err != nil {
		t.Errorf("the pinned scanner image must resolve: %v", err)
	}
	if err := CheckImageResolves(ctx, addr, reg, nil, "docker.io/aquasec/no-such-image-donkey:1", "linux/"+runtime.GOARCH); err == nil {
		t.Error("a missing image must not resolve")
	}
	if err := CheckOCIArtifact(ctx, reg, DefaultScannerDBRepository); err != nil {
		t.Errorf("the default database repository must answer: %v", err)
	}
	if err := CheckOCIArtifact(ctx, reg, "ghcr.io/aquasecurity/trivy-java-db"); err != nil {
		t.Errorf("the Java database on ghcr.io must answer (bearer flow): %v", err)
	}
	if err := CheckOCIArtifact(ctx, reg, "ghcr.io/aquasecurity/no-such-db-donkey"); err == nil {
		t.Error("a missing repository must not answer")
	} else {
		t.Logf("missing repository: %v", err)
	}
}

// dockerScanner is the orchestrator's scanner with its binary run in the
// scanner image: the work directory at /work and the scanner's own
// directory — database included — at the same path inside, so what the
// scanner reads off the volume (the database's freshness) is what the
// binary wrote.
func dockerScanner(t *testing.T) *Scanner {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scanner")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Scanner{
		Dir: dir,
		Run: func(ctx context.Context, work string, args []string, env []string) ([]byte, error) {
			mapped := make([]string, 0, len(args))
			for i := 0; i < len(args); i++ {
				if args[i] == "--cache-dir" && i+1 < len(args) {
					mapped = append(mapped, "--cache-dir", "/cache")
					i++
					continue
				}
				mapped = append(mapped, args[i])
			}
			docker := []string{"run", "--rm", "--volume", filepath.Join(dir, "cache") + ":/cache"}
			if work != "" {
				docker = append(docker, "--volume", work+":/work", "--workdir", "/work")
			}
			for _, kv := range env {
				if strings.HasPrefix(kv, "TRIVY_") && !strings.HasPrefix(kv, "TRIVY_CACHE_DIR=") {
					docker = append(docker, "--env", kv)
				}
			}
			docker = append(docker, "--env", "TRIVY_CACHE_DIR=/cache", config.DefaultScannerImage)
			docker = append(docker, mapped...)
			return exec.CommandContext(ctx, "docker", docker...).CombinedOutput()
		},
	}
}
