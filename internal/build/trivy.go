package build

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"

	"github.com/adrianliechti/donkey/internal/config"
)

// Scanner matches inventories against the vulnerability database with a
// local scanner binary (Trivy). It belongs to the orchestrator: the database
// lives under Dir on its data volume — durable storage donkey already owns —
// fetched by the scanner itself from its public source or the installation's
// mirror, with the orchestrator's own proxy, credentials and trust. Nothing
// here depends on the build engine: the inventory came out of it once, and
// every later reading of the same image is a lookup against this database.
type Scanner struct {
	once sync.Once
	gate chan struct{}
	// Binary is the scanner executable; empty means "trivy" on PATH.
	Binary string
	// Dir holds the database (db/), the scanner's docker config and CA
	// bundle, and the work directories of running matches.
	Dir string
	// DBRepository and JavaDBRepository point the database pulls at a
	// mirror; empty means the scanner's public defaults.
	DBRepository     string
	JavaDBRepository string
	// Registry supplies credentials and the certificate authority for the
	// mirror hosts, the same way builds get theirs (auth.go).
	Registry config.Registry
	// Run executes the scanner in dir with args and env and returns its
	// combined output; nil runs Binary. Tests substitute one.
	Run func(ctx context.Context, dir string, args []string, env []string) ([]byte, error)
}

// DBStatus is what the scanner's database says about itself.
type DBStatus struct {
	Present    bool
	UpdatedAt  time.Time
	NextUpdate time.Time
}

func (s *Scanner) binary() string {
	if s.Binary == "" {
		return "trivy"
	}
	return s.Binary
}

func (s *Scanner) cacheDir() string { return filepath.Join(s.Dir, "cache") }
func (s *Scanner) workDir() string  { return filepath.Join(s.Dir, "work") }

func (s *Scanner) run(ctx context.Context, dir string, args []string, env []string) ([]byte, error) {
	if s.Run != nil {
		return s.Run(ctx, dir, args, env)
	}
	cmd := exec.CommandContext(ctx, s.binary(), args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

// Version is the scanner's own version line, for doctor.
func (s *Scanner) Version(ctx context.Context) (string, error) {
	out, err := s.run(ctx, "", []string{"--version"}, s.env())
	if err != nil {
		return "", fmt.Errorf("%s is not usable: %w (%s)", s.binary(), err, strings.TrimSpace(string(out)))
	}
	// "Version: 0.76.1" is the first line; the rest describes the database.
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(strings.TrimPrefix(line, "Version:")), nil
}

// DBStatus reads the database's metadata off the volume.
func (s *Scanner) DBStatus() DBStatus {
	raw, err := os.ReadFile(filepath.Join(s.cacheDir(), "db", "metadata.json"))
	if err != nil {
		return DBStatus{}
	}
	var meta struct {
		UpdatedAt  time.Time `json:"UpdatedAt"`
		NextUpdate time.Time `json:"NextUpdate"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return DBStatus{}
	}
	return DBStatus{Present: true, UpdatedAt: meta.UpdatedAt, NextUpdate: meta.NextUpdate}
}

// Update fetches or refreshes the database without scanning anything, so
// the first build after an install does not pay for the download.
func (s *Scanner) Update(ctx context.Context) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if err := s.prepare(); err != nil {
		return err
	}
	args := []string{"image", "--cache-dir", s.cacheDir(), "--download-db-only"}
	if out, err := s.run(ctx, s.Dir, args, s.env()); err != nil {
		return fmt.Errorf("fetching the vulnerability database: %w\n%s", err, tail(out))
	}
	return nil
}

// Match fills the report's findings from its inventory: the scanner reads
// the SPDX document it wrote itself, refreshes the database when it is due,
// and reports every package it knows a weakness for. Secrets are untouched;
// they come off the image, not the inventory.
func (s *Scanner) Match(ctx context.Context, report *ScanReport) error {
	if report == nil || len(report.SBOM) == 0 {
		return fmt.Errorf("nothing to match: the scan left no inventory")
	}
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if err := s.prepare(); err != nil {
		return err
	}
	work, err := os.MkdirTemp(s.workDir(), "match-*")
	if err != nil {
		return fmt.Errorf("preparing the match: %w", err)
	}
	defer os.RemoveAll(work)
	if err := os.WriteFile(filepath.Join(work, scanSBOMFile), report.SBOM, 0o600); err != nil {
		return err
	}
	args := []string{"sbom", "--cache-dir", s.cacheDir(), "--format", "json", "--output", scanReportFile, scanSBOMFile}
	out, err := s.run(ctx, work, args, s.env())
	if err != nil {
		return fmt.Errorf("matching the inventory against the vulnerability database: %w\n%s", err, tail(out))
	}
	raw, err := os.ReadFile(filepath.Join(work, scanReportFile))
	if err != nil {
		return fmt.Errorf("the scanner left no report: %w", err)
	}
	matched, err := parseTrivyReport(raw)
	if err != nil {
		return err
	}
	report.Vulnerabilities = matched.Vulnerabilities
	report.OS, report.EndOfLife = matched.OS, matched.EndOfLife
	if db := s.DBStatus(); db.Present {
		report.DBUpdatedAt = db.UpdatedAt.UTC().Format(time.RFC3339)
		report.DBNextUpdate = db.NextUpdate.UTC().Format(time.RFC3339)
	}
	return nil
}

// Matches share database files, credentials and trust. Keep their updates
// exclusive, and let a cancelled deploy leave the queue immediately.
func (s *Scanner) acquire(ctx context.Context) error {
	s.once.Do(func() { s.gate = make(chan struct{}, 1) })
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scanner) release() { <-s.gate }

// Rescan reads a stored inventory against the current database: what
// re-reading a running app costs, and what the runtime watch calls.
func (s *Scanner) Rescan(ctx context.Context, sbom []byte) (*ScanReport, error) {
	report := &ScanReport{Scanner: s.binary(), Vulnerabilities: []Vulnerability{}, Secrets: []Secret{}, SBOM: sbom}
	if err := s.Match(ctx, report); err != nil {
		return nil, err
	}
	return report, nil
}

// prepare lays out Dir: the cache, the work area, the docker config the
// scanner's registry client reads for the mirrors, and a CA bundle that
// extends the system's roots with the registry's authority rather than
// replacing them.
func (s *Scanner) prepare() error {
	if s.Dir == "" {
		return fmt.Errorf("the scanner has no directory to keep its database in")
	}
	for _, dir := range []string{s.cacheDir(), s.workDir(), filepath.Join(s.Dir, "docker"), filepath.Join(s.Dir, "tmp")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("preparing the scanner's directory: %w", err)
		}
	}
	auths := map[string]map[string]string{}
	for _, repo := range []string{s.DBRepository, s.JavaDBRepository} {
		if repo == "" {
			continue
		}
		named, err := reference.ParseNormalizedNamed(repo)
		if err != nil {
			return fmt.Errorf("scanner database repository %q: %w", repo, err)
		}
		host := reference.Domain(named)
		ac, err := registryAuthConfig(s.Registry, host)
		if err != nil {
			return err
		}
		if ac.Username == "" || ac.Password == "" {
			continue
		}
		key := host
		if host == "docker.io" {
			key = "https://index.docker.io/v1/"
		}
		auths[key] = map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(ac.Username + ":" + ac.Password))}
	}
	cfg, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "docker", "config.json"), cfg, 0o600); err != nil {
		return err
	}
	if s.Registry.CAFile != "" {
		ca, err := os.ReadFile(s.Registry.CAFile)
		if err != nil {
			return fmt.Errorf("reading the registry's certificate authority for the scanner: %w", err)
		}
		bundle := append([]byte{}, systemRoots()...)
		if len(bundle) > 0 && bundle[len(bundle)-1] != '\n' {
			bundle = append(bundle, '\n')
		}
		bundle = append(bundle, ca...)
		if err := os.WriteFile(filepath.Join(s.Dir, "ca.crt"), bundle, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// systemRoots is the system's own CA bundle, wherever this platform keeps
// it; empty when none is found.
func systemRoots() []byte {
	for _, path := range []string{
		"/etc/ssl/certs/ca-certificates.crt", // Debian, Alpine — the server image
		"/etc/pki/tls/certs/ca-bundle.crt",   // Red Hat
		"/etc/ssl/cert.pem",                  // macOS, BSD
	} {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return b
		}
	}
	return nil
}

// env is the scanner's environment: the process's own (so the
// installation's forward proxy reaches it) plus what points it at its
// database, mirrors, credentials and trust. The scanner would otherwise
// call home for a version check and usage telemetry — pointless noise, and
// an error in a network that only reaches the mirrors.
func (s *Scanner) env() []string {
	env := append([]string{}, os.Environ()...)
	set := func(key, value string) {
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	set("TRIVY_CACHE_DIR", s.cacheDir())
	set("TRIVY_NO_PROGRESS", "true")
	set("TRIVY_SKIP_VERSION_CHECK", "true")
	set("TRIVY_DISABLE_TELEMETRY", "true")
	set("TRIVY_DB_REPOSITORY", s.DBRepository)
	set("TRIVY_JAVA_DB_REPOSITORY", s.JavaDBRepository)
	if s.Dir != "" {
		set("DOCKER_CONFIG", filepath.Join(s.Dir, "docker"))
		set("TMPDIR", filepath.Join(s.Dir, "tmp"))
		if s.Registry.CAFile != "" {
			set("SSL_CERT_FILE", filepath.Join(s.Dir, "ca.crt"))
		}
	}
	return env
}

func tail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 4*1024 {
		s = "…" + s[len(s)-4*1024:]
	}
	return s
}
