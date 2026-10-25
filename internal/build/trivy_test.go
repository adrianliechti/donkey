package build

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
)

func TestScannerQueueCancellationAndRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started, unblock := make(chan struct{}), make(chan struct{})
	failure := errors.New("database offline")
	s := &Scanner{Dir: t.TempDir()}
	s.Run = func(ctx context.Context, dir string, args, env []string) ([]byte, error) {
		if args[0] == "image" {
			close(started)
			select {
			case <-unblock:
			case <-ctx.Done():
			}
			return nil, failure
		}
		return nil, os.WriteFile(filepath.Join(dir, scanReportFile), []byte(`{"Results":[]}`), 0600)
	}
	finished := make(chan error, 1)
	go func() { finished <- s.Update(ctx) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitCtx, stop := context.WithCancel(ctx)
	waiting := make(chan error, 1)
	go func() { waiting <- s.Match(waitCtx, &ScanReport{SBOM: []byte("{}")}) }()
	stop()
	select {
	case err := <-waiting:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled queued scan: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("cancelled scan did not leave the queue")
	}
	close(unblock)
	if err := <-finished; !errors.Is(err, failure) {
		t.Fatalf("database failure: %v", err)
	}
	if err := s.Match(ctx, &ScanReport{SBOM: []byte("{}")}); err != nil {
		t.Fatalf("retry after database recovery: %v", err)
	}
	entries, err := os.ReadDir(s.workDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("scan left work directories: %v, %v", entries, err)
	}
}

// The orchestrator's scanner runs the binary in a work directory with the
// inventory, points it at the database on the volume and at the mirrors,
// and reads the findings back off the report it leaves.
func TestScannerMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "password"), []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\nregistry\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen struct {
		dir  string
		args []string
		env  map[string]string
	}
	s := &Scanner{
		Dir:              filepath.Join(dir, "scanner"),
		DBRepository:     "jfrog.corp/ghcr/aquasecurity/trivy-db",
		JavaDBRepository: "ghcr.io/aquasecurity/trivy-java-db",
		Registry:         config.Registry{URL: "jfrog.corp/donkey", Username: "reader", PasswordFile: filepath.Join(dir, "password"), CAFile: ca},
		Run: func(_ context.Context, work string, args []string, env []string) ([]byte, error) {
			seen.dir, seen.args, seen.env = work, args, map[string]string{}
			for _, kv := range env {
				k, v, _ := strings.Cut(kv, "=")
				seen.env[k] = v
			}
			if _, err := os.Stat(filepath.Join(work, scanSBOMFile)); err != nil {
				t.Errorf("the inventory was not written for the scanner: %v", err)
			}
			return nil, os.WriteFile(filepath.Join(work, scanReportFile), []byte(trivyFixture), 0o600)
		},
	}
	// A database on the volume says how fresh the answer is.
	if err := os.MkdirAll(filepath.Join(s.cacheDir(), "db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cacheDir(), "db", "metadata.json"), []byte(`{"Version":2,"UpdatedAt":"2026-10-25T06:00:00Z","NextUpdate":"2026-10-25T12:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	report := &ScanReport{SBOM: []byte(`{"spdxVersion":"SPDX-2.3"}`), Secrets: []Secret{awsKeyFixture}}
	if err := s.Match(context.Background(), report); err != nil {
		t.Fatal(err)
	}
	if len(report.Vulnerabilities) != 3 || report.OS != "alpine 3.18.0" || !report.EndOfLife {
		t.Errorf("findings not taken from the report: %d vulnerabilities, OS %q", len(report.Vulnerabilities), report.OS)
	}
	if len(report.Secrets) != 1 {
		t.Errorf("matching must leave the secrets alone: %+v", report.Secrets)
	}
	if report.DBUpdatedAt != "2026-10-25T06:00:00Z" {
		t.Errorf("database freshness = %q", report.DBUpdatedAt)
	}
	if !strings.HasPrefix(seen.dir, s.workDir()) {
		t.Errorf("work dir %q is not under the scanner's directory", seen.dir)
	}
	if _, err := os.Stat(seen.dir); !os.IsNotExist(err) {
		t.Errorf("the work directory should be gone after the match")
	}
	want := []string{"sbom", "--cache-dir", s.cacheDir(), "--format", "json", "--output", scanReportFile, scanSBOMFile}
	if strings.Join(seen.args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", seen.args, want)
	}
	for key, value := range map[string]string{
		"TRIVY_CACHE_DIR":          s.cacheDir(),
		"TRIVY_DB_REPOSITORY":      "jfrog.corp/ghcr/aquasecurity/trivy-db",
		"TRIVY_JAVA_DB_REPOSITORY": "ghcr.io/aquasecurity/trivy-java-db",
		"TRIVY_SKIP_VERSION_CHECK": "true",
		"TRIVY_DISABLE_TELEMETRY":  "true",
		"DOCKER_CONFIG":            filepath.Join(s.Dir, "docker"),
		"SSL_CERT_FILE":            filepath.Join(s.Dir, "ca.crt"),
	} {
		if seen.env[key] != value {
			t.Errorf("env %s = %q, want %q", key, seen.env[key], value)
		}
	}

	// The mirror on donkey's own registry host gets its login; a public
	// host donkey has no login for gets none.
	raw, err := os.ReadFile(filepath.Join(s.Dir, "docker", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Auths["jfrog.corp"]["auth"] == "" {
		t.Errorf("no credential for the mirror host: %s", raw)
	}
	if _, ok := cfg.Auths["ghcr.io"]; ok {
		t.Errorf("a host donkey has no login for must not get one: %s", raw)
	}
	// The CA bundle extends the system roots rather than replacing them.
	bundle, err := os.ReadFile(filepath.Join(s.Dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(bundle), "-----BEGIN CERTIFICATE-----\nregistry\n") {
		t.Errorf("the registry's authority is missing from the bundle")
	}
	if roots := systemRoots(); len(roots) > 0 && !strings.HasPrefix(string(bundle), string(roots[:64])) {
		t.Errorf("the system's roots are missing from the bundle")
	}
}

func TestScannerWithoutMirrorsOrCA(t *testing.T) {
	s := &Scanner{Dir: filepath.Join(t.TempDir(), "scanner"), Run: func(_ context.Context, work string, _ []string, env []string) ([]byte, error) {
		for _, kv := range env {
			if strings.HasPrefix(kv, "SSL_CERT_FILE=") || strings.HasPrefix(kv, "TRIVY_DB_REPOSITORY=") {
				t.Errorf("unexpected %s", kv)
			}
		}
		return nil, os.WriteFile(filepath.Join(work, scanReportFile), []byte(`{"Results":[]}`), 0o600)
	}}
	report, err := s.Rescan(context.Background(), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Vulnerabilities) != 0 || len(report.Secrets) != 0 || string(report.SBOM) != "{}" {
		t.Errorf("rescan = %+v", report)
	}
	if raw, _ := os.ReadFile(filepath.Join(s.Dir, "docker", "config.json")); string(raw) != `{"auths":{}}` {
		t.Errorf("empty docker config = %s", raw)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "ca.crt")); !os.IsNotExist(err) {
		t.Error("no CA configured, yet a bundle was written")
	}
	if db := s.DBStatus(); db.Present {
		t.Error("no database on the volume, yet one was reported")
	}
}

func TestScannerFailuresAreExplained(t *testing.T) {
	s := &Scanner{Dir: filepath.Join(t.TempDir(), "scanner"), Run: func(context.Context, string, []string, []string) ([]byte, error) {
		return []byte("FATAL\tinit error: DB error: failed to download vulnerability DB"), os.ErrPermission
	}}
	err := s.Match(context.Background(), &ScanReport{SBOM: []byte("{}")})
	if err == nil || !strings.Contains(err.Error(), "failed to download vulnerability DB") {
		t.Errorf("the scanner's own words should reach the user: %v", err)
	}
	if err := s.Match(context.Background(), &ScanReport{}); err == nil {
		t.Error("a report without an inventory cannot be matched")
	}
	if _, err := (&Scanner{}).Rescan(context.Background(), []byte("{}")); err == nil {
		t.Error("a scanner without a directory cannot keep a database")
	}
}
