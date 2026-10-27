package server

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type repairAssistant struct {
	ai.Assistant
	write func(*ai.DockerfileInput) (*ai.DockerfileResult, error)
}

func (a repairAssistant) WriteDockerfile(_ context.Context, in *ai.DockerfileInput) (*ai.DockerfileResult, error) {
	return a.write(in)
}

type repairBuilder func(build.Request) (*build.Result, error)

func (b repairBuilder) Build(_ context.Context, r build.Request) (*build.Result, error) { return b(r) }

type repairRuntime struct{ runtime.Provider }

func (repairRuntime) GetEnv(context.Context, string, string) (map[string]string, error) {
	return map[string]string{"TOKEN": "runtime-secret-value"}, nil
}
func (repairRuntime) ListServices(context.Context, string, string) ([]runtime.ServiceBindingInfo, error) {
	return nil, nil
}
func (repairRuntime) SpaceConfig(context.Context, string) (config.SpaceConfig, error) {
	return config.SpaceConfig{}, nil
}

func TestScanRepairLifecycle(t *testing.T) {
	clean := &build.ScanReport{}
	blocked := &build.ScanReport{Vulnerabilities: []build.Vulnerability{criticalFixable}}
	unavailable := &build.ScanError{Err: errors.New("database unavailable")}
	type attempt struct {
		report  *build.ScanReport
		err     error
		rollout bool
	}
	for _, tc := range []struct {
		name             string
		supplied         bool
		savedGenerated   bool
		attempts         []attempt
		writes, rollouts int
		ok               bool
	}{
		{name: "generated refusal repairs and deploys", attempts: []attempt{{report: blocked}, {report: clean}}, writes: 2, rollouts: 1, ok: true},
		{name: "supplied refusal leaves runtime alone", supplied: true, attempts: []attempt{{report: blocked}}},
		{name: "saved generated refusal repairs and deploys", supplied: true, savedGenerated: true, attempts: []attempt{{report: blocked}, {report: clean}}, writes: 1, rollouts: 1, ok: true},
		{name: "scanner outage leaves source and runtime alone", supplied: true, attempts: []attempt{{err: unavailable}}},
		{name: "retry outage clears earlier refusal", attempts: []attempt{{report: blocked}, {err: unavailable}}, writes: 2},
		{name: "failed rollout then refused repair has no stale image", attempts: []attempt{{report: clean, rollout: true}, {report: blocked}}, writes: 2, rollouts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := &Server{cfg: &Config{}, rt: repairRuntime{}}
			s.cfg.Build.Attempts = len(tc.attempts)
			s.cfg.Build.Scan.Triage = config.ScanTriageOff
			writes, builds, rollouts := 0, 0, 0
			s.assistant = repairAssistant{write: func(in *ai.DockerfileInput) (*ai.DockerfileResult, error) {
				writes++
				if builds > 0 && tc.attempts[builds-1].report == blocked {
					last := in.Attempts[len(in.Attempts)-1]
					if last.Stage != "scan" || !strings.Contains(last.Failure, criticalFixable.Fixed) {
						t.Fatalf("repair lost scan evidence: %+v", last)
					}
				}
				return &ai.DockerfileResult{Dockerfile: fmt.Sprintf("FROM app:%d\n", writes)}, nil
			}}
			response := httptest.NewRecorder()
			recorder := newReleaseRecorder(time.Now(), nil)
			out := newLineStreamer(response, recorder)
			builder := repairBuilder(func(req build.Request) (*build.Result, error) {
				if builds >= len(tc.attempts) {
					t.Fatal("unexpected repair after terminal failure")
				}
				if out.lastScan != nil || recorder.release.Scan != nil {
					t.Fatal("new attempt retained the previous scan")
				}
				a := tc.attempts[builds]
				builds++
				if a.err != nil {
					return nil, a.err
				}
				if err := req.Scan.Judge(context.Background(), a.report); err != nil {
					return nil, err
				}
				return &build.Result{ImageRef: req.ImageRef, Scan: a.report}, nil
			})
			req := build.Request{ContextDir: dir, ImageRef: "registry/app:initial", Scan: &build.ScanSpec{}}
			if tc.supplied {
				req.Strategy, req.DockerfilePath = config.BuildDockerfile, "Dockerfile"
				if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM supplied\n"), 0600); err != nil {
					t.Fatal(err)
				}
				req.GeneratedRecipe = tc.savedGenerated
			}
			outcome, err := s.buildGenerated(context.Background(), out, builder, dir, &config.AppConfig{Name: "app", Space: "team", Port: 8080}, req, func(result *build.Result, _ string) error {
				rollouts++
				if out.lastScan == nil || out.lastScan.Blocked {
					t.Fatal("releasing without a passing scan")
				}
				if tc.attempts[builds-1].rollout {
					return &runtime.RolloutError{Reason: "startup failed"}
				}
				return nil
			})
			if (err == nil) != tc.ok || writes != tc.writes || rollouts != tc.rollouts || builds != len(tc.attempts) {
				t.Fatalf("err=%v writes=%d builds=%d rollouts=%d", err, writes, builds, rollouts)
			}
			if !tc.ok && outcome.Result != nil {
				t.Fatalf("failure carries another attempt's image: %+v", outcome.Result)
			}
			last := tc.attempts[len(tc.attempts)-1]
			if last.err != nil && (out.lastScan != nil || recorder.release.Scan != nil) {
				t.Fatal("scanner failure retained old findings")
			}
			if last.err != nil && !errors.Is(err, last.err) {
				t.Fatalf("lost scanner error: %v", err)
			}
		})
	}
}

func TestRepairLoopIncludesBuildAndRolloutEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM supplied\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte("listen(3000)"), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: &Config{}, rt: repairRuntime{}}
	app := &config.AppConfig{Name: "app", Space: "team", Port: 8080, Command: []string{"node", "server.js"}}
	writes := 0
	s.assistant = repairAssistant{write: func(in *ai.DockerfileInput) (*ai.DockerfileResult, error) {
		writes++
		if !in.AllowEdits || in.Root != dir || len(in.Attempts) != writes || !strings.Contains(in.Runtime, "server.js") {
			t.Fatalf("missing repair contract: %+v", in)
		}
		last := in.Attempts[len(in.Attempts)-1]
		if strings.Contains(last.Failure, "runtime-secret-value") {
			t.Fatal("secret reached repair evidence")
		}
		if writes == 1 && (last.Stage != "build" || last.Dockerfile != "FROM supplied\n") {
			t.Fatalf("lost supplied recipe: %+v", last)
		}
		if writes == 2 {
			if last.Stage != "rollout" || !strings.Contains(last.Failure, "wrong port") {
				t.Fatalf("lost runtime evidence: %+v", last)
			}
			if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte("listen(8080)"), 0644); err != nil {
				t.Fatal(err)
			}
		}
		return &ai.DockerfileResult{Dockerfile: fmt.Sprintf("FROM fixed-%d\n", writes), Dockerignore: fmt.Sprintf("cache-%d\n", writes)}, nil
	}}
	builds, rollouts := 0, 0
	seen := map[string]bool{}
	builder := repairBuilder(func(req build.Request) (*build.Result, error) {
		builds++
		if seen[req.ImageRef] {
			t.Fatal("retry reused a runtime generation")
		}
		seen[req.ImageRef] = true
		if builds == 1 {
			if req.DockerfilePath != "Dockerfile" || writes != 0 {
				t.Fatal("supplied recipe wasn't tried unchanged")
			}
			return nil, errors.New("compile failure runtime-secret-value")
		}
		ignore, err := os.ReadFile(filepath.Join(dir, req.DockerfilePath+".dockerignore"))
		if err != nil {
			t.Fatal(err)
		}
		if string(ignore) != fmt.Sprintf("cache-%d\n", writes) {
			t.Fatalf("stale ignore: %s", ignore)
		}
		return &build.Result{ImageRef: req.ImageRef}, nil
	})
	release := func(result *build.Result, _ string) error {
		rollouts++
		if rollouts == 1 {
			return &runtime.RolloutError{App: "app", Reason: "wrong port", Evidence: runtime.Evidence{Logs: "token=runtime-secret-value"}}
		}
		data, _ := os.ReadFile(filepath.Join(dir, "server.js"))
		if string(data) != "listen(8080)" {
			t.Fatal("source repair not built")
		}
		return nil
	}
	res, err := s.buildGenerated(context.Background(), newLineStreamer(httptest.NewRecorder(), nil), builder, dir, app, build.Request{ContextDir: dir, ImageRef: "registry/app:initial", Strategy: config.BuildDockerfile, DockerfilePath: "Dockerfile"}, release)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 3 || writes != 2 || rollouts != 2 {
		t.Fatalf("incorrect attempts: %+v, writes=%d rollouts=%d", res, writes, rollouts)
	}
}

func TestRepairStopsWithoutChangedInputs(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: &Config{}, rt: repairRuntime{}}
	s.assistant = repairAssistant{write: func(*ai.DockerfileInput) (*ai.DockerfileResult, error) {
		return &ai.DockerfileResult{Dockerfile: "FROM unchanged\n"}, nil
	}}
	builds := 0
	builder := repairBuilder(func(req build.Request) (*build.Result, error) {
		builds++
		return nil, fmt.Errorf("failure %d for %s", builds, req.ImageRef)
	})
	res, err := s.buildGenerated(context.Background(), newLineStreamer(httptest.NewRecorder(), nil), builder, dir, &config.AppConfig{Name: "app", Port: 8080}, build.Request{ContextDir: dir, ImageRef: "registry/app:initial"}, nil)
	if err == nil || !strings.Contains(err.Error(), "without progress") || res.Attempts != 2 {
		t.Fatalf("didn't stop duplicate inputs: %+v %v", res, err)
	}
}

func TestRepairDoesNotRewriteForPlatformErrors(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: &Config{}, rt: repairRuntime{}}
	s.assistant = repairAssistant{write: func(*ai.DockerfileInput) (*ai.DockerfileResult, error) {
		return &ai.DockerfileResult{Dockerfile: "FROM app\n"}, nil
	}}
	platformErr := errors.New("runtime API unavailable")
	builder := repairBuilder(func(req build.Request) (*build.Result, error) { return &build.Result{ImageRef: req.ImageRef}, nil })
	res, err := s.buildGenerated(context.Background(), newLineStreamer(httptest.NewRecorder(), nil), builder, dir, &config.AppConfig{Name: "app", Port: 8080}, build.Request{ContextDir: dir, ImageRef: "registry/app:initial"}, func(*build.Result, string) error { return platformErr })
	if err != platformErr || res.Attempts != 1 {
		t.Fatalf("misclassified platform error: %+v %v", res, err)
	}
}

func TestRecipeIgnorePreservesAuthorAndAllowsCorrections(t *testing.T) {
	dir := t.TempDir()
	recipe := ".donkey/Dockerfile"
	for _, ignore := range []string{"src\n", "cache\n", ""} {
		if err := writeGeneratedDockerignore(dir, recipe, ignore, ""); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(dir, recipe+".dockerignore"))
		if string(data) != ignore {
			t.Fatalf("ignore not updated: %q", data)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte("author-root\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.dockerignore"), []byte("author-specific\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeGeneratedDockerignore(dir, recipe, "generated\n", "Dockerfile.dockerignore"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, recipe+".dockerignore"))
	if string(data) != "author-specific\n" {
		t.Fatalf("lost author exclusions: %q", data)
	}
}

func TestVerifyCarriesRuntimeContractAndDefersSecrets(t *testing.T) {
	s := &Server{cfg: &Config{Domain: "example.com"}}
	app := &config.AppConfig{Name: "app", Space: "team", Port: 8080, Env: map[string]string{"MODE": "production"}, Command: []string{"server"}, Storage: &config.StorageConfig{Paths: []string{"/data"}}, Healthcheck: config.Healthcheck{Path: "/ready"}}
	got := s.verifySpec(app)
	if got == nil || got.Env["MODE"] != "production" || len(got.Command) != 1 || len(got.StoragePaths) != 1 || got.HealthcheckPath != "/ready" || got.Host != "app.team.example.com" {
		t.Fatalf("lost runtime settings: %+v", got)
	}
	app.Env["API_KEY"] = "secret-token-value"
	if s.verifySpec(app) != nil {
		t.Fatal("secrets copied into BuildKit verification")
	}
	app.Env = map[string]string{"DATABASE_URL": "${postgres.DATABASE_URL}"}
	app.Addons = []config.AddonConfig{{Name: "postgres", Type: "postgres"}}
	if s.verifySpec(app) != nil {
		t.Fatal("service-dependent app verified without its service")
	}
}
