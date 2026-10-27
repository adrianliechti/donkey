package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
)

type checkingBuilder struct {
	repairBuilder
	inspect func(context.Context, string, string, build.ScanSpec) (*build.BaseImage, error)
}

func (b checkingBuilder) InspectBaseImage(ctx context.Context, image, platform string, scan build.ScanSpec) (*build.BaseImage, error) {
	return b.inspect(ctx, image, platform, scan)
}

type unusedMatcher struct{}

func (unusedMatcher) Match(context.Context, *build.ScanReport) error { return nil }

func TestBaseImageEvidenceAndBudget(t *testing.T) {
	s := &Server{cfg: &Config{}, rt: repairRuntime{}}
	app := &config.AppConfig{Name: "web", Space: "team"}
	req := build.Request{Platform: "linux/arm64", Scan: &build.ScanSpec{Image: "scanner:1", Matcher: unusedMatcher{}}}
	calls := 0
	builder := checkingBuilder{inspect: func(ctx context.Context, image, platform string, spec build.ScanSpec) (*build.BaseImage, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Minute || platform != req.Platform || spec.Matcher != req.Scan.Matcher {
			t.Fatal("image check lost its deadline, platform or matcher")
		}
		return &build.BaseImage{Reference: image + "@sha256:" + strings.Repeat("a", 64), Platform: platform,
			Scan: &build.ScanReport{OS: "alpine test", EndOfLife: true, DBUpdatedAt: "2026-10-27T00:00:00Z",
				Vulnerabilities: []build.Vulnerability{criticalFixable}, SBOM: []byte("private inventory"), Secrets: []build.Secret{{Match: "do-not-send"}}}}, nil
	}}
	check := s.baseImageCheck(builder, req, app)
	first, err := check(context.Background(), "alpine:3.20")
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Reference string `json:"reference"`
		Blocking  int    `json:"blockingCount"`
		EOL       bool   `json:"endOfLife"`
		DB        string `json:"databaseUpdatedAt"`
	}
	if json.Unmarshal([]byte(first), &evidence) != nil || evidence.Blocking != 1 || !evidence.EOL || evidence.DB == "" || !strings.Contains(evidence.Reference, "@sha256:") || !strings.Contains(first, criticalFixable.Fixed) {
		t.Fatalf("missing actionable evidence: %s", first)
	}
	if strings.Contains(first, "private inventory") || strings.Contains(first, "do-not-send") {
		t.Fatal("raw inventory or secret evidence sent to model")
	}
	second, err := check(context.Background(), "docker.io/library/alpine:3.20")
	if err != nil || first != second || calls != 1 {
		t.Fatal("equivalent references were checked twice")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, image := range []string{"alpine:3.20", "alpine:cancelled"} {
		if result, err := check(ctx, image); result != "" || !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatal("canceled check returned cached evidence or started a new inspection")
		}
	}
	for i := 1; i < maxBaseImageChecks; i++ {
		if _, err := check(context.Background(), fmt.Sprintf("alpine:test-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := check(context.Background(), "alpine:excess"); err == nil || calls != maxBaseImageChecks {
		t.Fatal("image checks exceeded budget")
	}
	if _, err := check(context.Background(), "alpine:3.20"); err != nil {
		t.Fatal("budget blocked already collected evidence")
	}
	// Another deployment must ask the registry and scanner again.
	if _, err := s.baseImageCheck(builder, req, app)(context.Background(), "alpine:3.20"); err != nil || calls != maxBaseImageChecks+1 {
		t.Fatal("evidence leaked across deployments")
	}
}

func TestBaseImageCheckRequiresScannerAndReportsFailure(t *testing.T) {
	s := &Server{cfg: &Config{}, rt: repairRuntime{}}
	app := &config.AppConfig{Name: "web", Space: "team"}
	want := errors.New("database unavailable")
	builder := checkingBuilder{inspect: func(context.Context, string, string, build.ScanSpec) (*build.BaseImage, error) { return nil, want }}
	if s.baseImageCheck(builder, build.Request{}, app) != nil {
		t.Fatal("image checks enabled with scanning off")
	}
	check := s.baseImageCheck(builder, build.Request{Scan: &build.ScanSpec{Matcher: unusedMatcher{}}}, app)
	if result, err := check(context.Background(), "alpine:3.20"); !errors.Is(err, want) || result != "" {
		t.Fatal("scanner failure became a clean image verdict")
	}
}
