//go:build e2e

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/build"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
	"github.com/adrianliechti/donkey/internal/packages"
	"github.com/adrianliechti/donkey/internal/testenv"
)

// Exercise the production repair loop with a live model, BuildKit, Trivy and
// startup verification. The fixture starts with a saved generated recipe and
// a vulnerable dependency; the first refusal must feed a real repair. Runtime
// policy reads use an empty test space. No registry push or deployment is made.
//
//	OPENAI_BASE_URL=http://localhost:4242/v1 DONKEY_E2E_REPAIR=true \
//	  go test -tags e2e -run '^TestLiveScanRepair$' -v -timeout 20m ./internal/server
func TestLiveScanRepair(t *testing.T) {
	if os.Getenv("DONKEY_E2E_REPAIR") != "true" || os.Getenv("OPENAI_BASE_URL") == "" {
		t.Skip("set DONKEY_E2E_REPAIR=true and OPENAI_BASE_URL to run the live scan repair")
	}
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Setenv("OPENAI_API_KEY", "local")
	}
	model := os.Getenv("DONKEY_MODEL")
	if model == "" {
		model = config.DefaultAIModel
	}
	assistant, err := ai.New(model)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 18*time.Minute)
	defer cancel()
	addr, host := testenv.Buildkit(t, ctx)
	packagesURL := testenv.PackageProxy(t, ctx, host, packages.Config{
		NPMUpstream: config.DefaultNPMUpstream, PyPIUpstream: config.DefaultPyPIUpstream,
		GoUpstream: config.DefaultGoUpstream, MavenUpstream: config.DefaultMavenUpstream, NuGetUpstream: config.DefaultNuGetUpstream,
	})
	scannerDir, run := testenv.Trivy(t)
	scanner := &build.Scanner{Dir: scannerDir, Run: run}
	s := &Server{cfg: &Config{}, rt: repairRuntime{}, assistant: assistant, scanner: scanner}
	s.cfg.Build.Attempts = 4
	s.cfg.Build.Scan.Triage = config.ScanTriageOff

	dir := t.TempDir()
	files := map[string]string{
		"package.json": `{"name":"scan-repair-fixture","version":"1.0.0","private":true,"scripts":{"start":"node server.js"},"dependencies":{"lodash":"4.17.11"}}`,
		"server.js": `const http = require('http');
const _ = require('lodash');
http.createServer((req, res) => {
  let body = '';
  req.on('data', chunk => body += chunk);
  req.on('end', () => {
    const settings = _.defaultsDeep(body ? JSON.parse(body) : {}, {theme: 'light', pageSize: 20});
    res.setHeader('content-type', 'application/json');
    res.end(JSON.stringify(settings));
  });
}).listen(Number(process.env.PORT) || 8080, '0.0.0.0');
`,
		detect.AIDockerfilePath: `FROM node:18.0.0-alpine3.15
WORKDIR /app
COPY package.json ./
RUN npm install --no-audit --no-fund --ignore-scripts
COPY server.js ./
CMD ["node", "server.js"]
`,
	}
	for name, content := range files {
		if err := writeGeneratedFile(dir, name, content); err != nil {
			t.Fatal(err)
		}
	}
	detected, err := detect.Detect(dir, &config.AppConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !detected.GeneratedRecipe || detected.Strategy != config.BuildDockerfile {
		t.Fatalf("saved recipe ownership lost: %+v", detected)
	}
	builder := &liveRepairBuilder{BuildKitBuilder: &build.BuildKitBuilder{BuildkitAddr: addr, PackagesURL: packagesURL,
		Registry: config.Registry{URL: "example.invalid/donkey"}, Settings: config.BuildSettings{Provenance: "off"}}, t: t}
	response := &liveRepairResponse{ResponseRecorder: httptest.NewRecorder(), t: t}
	out := newLineStreamer(response, nil)
	app := &config.AppConfig{Name: "scan-repair", Space: "test", Port: 8080, Healthcheck: config.Healthcheck{Path: "/"}}
	logPath := filepath.Join(t.TempDir(), "build.log")
	result, err := s.buildGenerated(ctx, out, builder, dir, app, build.Request{
		ContextDir: dir, Strategy: detected.Strategy, DockerfilePath: detected.DockerfilePath, GeneratedRecipe: detected.GeneratedRecipe,
		ImageRef: "example.invalid/donkey/scan-repair:test", Platform: "linux/" + runtime.GOARCH,
		Scan: s.scanSpec(), Verify: s.verifySpec(app), Progress: build.ProgressSink{LogPath: logPath},
	}, nil)
	if err != nil {
		log, _ := os.ReadFile(logPath)
		t.Fatalf("live repair failed: %v\n%s", err, tailBytes(log, 12*1024))
	}
	if builder.refusals == 0 || result.Attempts < 2 || builder.checks == 0 {
		t.Fatalf("missing scan refusal, repair or image inspection: attempts=%d refusals=%d checks=%d", result.Attempts, builder.refusals, builder.checks)
	}
	if result.Result == nil || result.Result.Scan == nil || out.lastScan == nil || out.lastScan.Blocked || out.lastScan.Summary.Blocking != 0 {
		t.Fatal("repair completed without a passing final scan")
	}
	if result.Dockerfile == files[detect.AIDockerfilePath] || len(result.Changes) == 0 {
		t.Fatal("repair did not update both the saved recipe and its vulnerable dependency")
	}
	t.Logf("model=%s attempts=%d refusals=%d image checks=%d; final scan and startup passed\n%s", model, result.Attempts, builder.refusals, builder.checks, result.Dockerfile)
}

type liveRepairBuilder struct {
	*build.BuildKitBuilder
	t                *testing.T
	builds, refusals int
	checks           int
}

func (b *liveRepairBuilder) Build(ctx context.Context, req build.Request) (*build.Result, error) {
	b.builds++
	b.t.Logf("building attempt %d", b.builds)
	result, err := b.BuildKitBuilder.Build(ctx, req)
	var blocked *build.ScanBlockedError
	if errors.As(err, &blocked) {
		b.refusals++
		b.t.Logf("scan refused attempt %d: %s", b.builds, blocked.Message)
	} else if err != nil {
		log, _ := os.ReadFile(req.Progress.LogPath)
		b.t.Logf("build attempt %d: %v\n%s", b.builds, err, tailBytes(log, 12*1024))
	}
	if b.builds == 1 && blocked == nil {
		b.t.Fatalf("fixture must first build and fail scanning: %v", err)
	}
	return result, err
}

func (b *liveRepairBuilder) InspectBaseImage(ctx context.Context, image, platform string, spec build.ScanSpec) (*build.BaseImage, error) {
	b.checks++
	return b.BuildKitBuilder.InspectBaseImage(ctx, image, platform, spec)
}

type liveRepairResponse struct {
	*httptest.ResponseRecorder
	t *testing.T
}

func (w *liveRepairResponse) Write(p []byte) (int, error) {
	var msg struct{ Line string }
	if json.Unmarshal(p, &msg) == nil && msg.Line != "" && !strings.Contains(msg.Line, "\n") {
		w.t.Log(msg.Line)
	}
	return w.ResponseRecorder.Write(p)
}
