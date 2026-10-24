//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/apiclient"
	"github.com/adrianliechti/donkey/internal/server"
)

// Exercise the same upload -> review -> deploy flow as the UI, with a live
// assistant and real builds. Check the application at the gateway, including
// its assets and writes, rather than treating a healthy pod as the result.
// Explicitly opt in: this corpus is considerably larger than the platform
// smoke test. Run with DONKEY_E2E_BUILDER=true hack/verify.sh kind -run TestBuilderSamples.
func TestBuilderSamples(t *testing.T) {
	if os.Getenv("DONKEY_E2E_BUILDER") != "true" {
		t.Skip("set DONKEY_E2E_BUILDER=true to build the sample corpus with the live assistant")
	}
	s := newSuite(t)
	if s.edge == nil {
		t.Fatal("DONKEY_E2E_GATEWAY is required to verify the deployed applications")
	}
	for _, tc := range []struct {
		dir, path, body string
	}{
		{"static-site", "/", "Field notes"},
		{"fastapi-sample", "/", "Deployed by donkey"},
		{"dockerfile-sample", "/", "Deployed by donkey"},
		{"express-api", "/todos", "Write the API"},
		{"nextjs-app", "/", "It works"},
		{"My Cool App", "/", "My Cool App"},
		{"odd-port-sample", "/ping", "pong"},
		{"sqlite-notes", "/", "sqlite-notes"},
		{"local-habits", "/", "Todos"},
		{"monorepo-root", "/", "donkey found me below the repository root"},
		{"addons-sample", "/", "notes in Postgres"},
		{"flask-beginner", "/", "It works"},
		{"streamlit-report", "/_stcore/health", "ok"},
		{"django-blog", "/", "django-blog"},
		{"kitchen-sink-sample/api", "/", `"service":"api"`},
		{"kitchen-sink-sample/web", "/", "kitchen sink web"},
		{"kitchen-sink-sample/worker", "/", "worker ok"},
		{"kitchen-sink-sample/gateway", "/", "gateway: Dockerfile wins"},
		{"kitchen-sink-sample/legacy", "/", "legacy tcl app ok"},
		{"broken/express-loopback", "/", "It works"},
		{"broken/flask-loopback", "/", "It works"},
		{"broken/streamlit-bare", "/_stcore/health", "ok"},
		{"broken/no-manifest", "/", "no requirements.txt anywhere"},
	} {
		t.Run(tc.dir, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), filepath.Base(tc.dir))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			copyDir(t, filepath.Join("..", "testdata", tc.dir), dir)
			// Test inference, without writing generated config into the corpus.
			// The addon declaration and the nested monorepo config are intentional
			// parts of those two fixtures and stay in place.
			if tc.dir != "addons-sample" {
				if err := os.Remove(filepath.Join(dir, "donkey.yaml")); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			plan, base := s.deployBuilderSample(t, dir, "e2e-"+strings.ToLower(strings.ReplaceAll(filepath.Base(tc.dir), " ", "-")), "", "web", "public")
			s.waitBody(t, base+tc.path, tc.body, time.Minute)
			switch tc.dir {
			case "static-site", "My Cool App", "nextjs-app", "local-habits", "kitchen-sink-sample/web", "streamlit-report", "broken/streamlit-bare":
				s.assertSampleAssets(t, base)
				if os.Getenv("DONKEY_E2E_BROWSER") == "true" {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
					defer cancel()
					cmd := exec.CommandContext(ctx, "node", "builder-browser.mjs", tc.dir, base)
					output, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("browser verification: %v\n%s", err, output)
					}
					t.Log(string(output))
				}
			}
			if tc.dir == "sqlite-notes" || tc.dir == "addons-sample" {
				s.assertSamplePersistence(t, plan, base, tc.dir == "sqlite-notes")
			}
		})
	}
}

func TestBuilderRecovery(t *testing.T) {
	if os.Getenv("DONKEY_E2E_BUILDER") != "true" {
		t.Skip("set DONKEY_E2E_BUILDER=true to exercise live automatic repair")
	}
	s := newSuite(t)
	for _, stage := range []string{"build", "rollout"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
			copyDir(t, "../testdata/broken/express-loopback", dir)
			// Author-provided recipes are tried unchanged first. This one starts
			// the actual loopback-only source, so the first attempt must fail.
			writeFile(t, filepath.Join(dir, "Dockerfile"), "FROM node:22-alpine\nWORKDIR /app\nCOPY package.json ./\nRUN npm install --omit=dev\nCOPY index.js ./\nUSER node\nEXPOSE 3000\nCMD [\"node\",\"index.js\"]\n")
			configuration := "name: recovery-" + stage + "\nport: 3000\nhealthcheck:\n  path: /\n"
			if stage == "rollout" {
				// A runtime-only credential makes verification wait for the real
				// cluster. This harmless fixture value is never needed by the app.
				configuration += "env:\n  FIXTURE_API_TOKEN: runtime-only-fixture-value\n"
			}
			writeFile(t, filepath.Join(dir, "donkey.yaml"), configuration)
			_, base := s.deployBuilderSample(t, dir, "e2e-recovery-"+stage, stage, "web", "public")
			s.waitBody(t, base+"/", "It works", time.Minute)
		})
	}
}

func (s *suite) deployBuilderSample(t *testing.T, dir, space, failureStage, mode, exposure string) (*server.Plan, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	s.rawTry(http.MethodDelete, "/v1/spaces/"+space)
	s.raw(t, http.MethodPost, "/v1/spaces", map[string]any{"name": space, "allowPublic": true, "viewers": map[string]any{"users": []string{"demo@example.com"}}}, nil)
	if os.Getenv("DONKEY_E2E_KEEP") != "true" {
		t.Cleanup(func() { s.rawTry(http.MethodDelete, "/v1/spaces/"+space) })
	}
	var archive bytes.Buffer
	if _, err := apiclient.TarContext(dir, &archive); err != nil {
		t.Fatal(err)
	}
	plan, err := s.api.Plan(ctx, bytes.NewReader(archive.Bytes()), apiclient.PlanOpts{
		Space: space, Name: strings.ToLower(strings.ReplaceAll(filepath.Base(dir), " ", "-")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if exposure == "" {
		exposure = plan.App.Exposure
	}
	if mode == "" {
		mode = plan.App.Mode
	}
	edit := server.PlanEdit{Name: plan.App.Name, Space: space, Port: plan.App.Port, Healthcheck: plan.App.Healthcheck, Storage: plan.App.Storage, Mode: mode, Exposure: exposure, CORS: plan.App.CORS}
	for _, service := range plan.Services {
		if service.Selected {
			edit.Services = append(edit.Services, service.Name)
		}
	}
	plan, err = s.api.UpdatePlan(ctx, plan.ID, edit)
	if err != nil {
		t.Fatal(err)
	}
	if reports := os.Getenv("DONKEY_E2E_REPORTS"); reports != "" {
		if err := os.MkdirAll(reports, 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(reports, space+"-plan.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !plan.Ready {
		t.Fatalf("sample could not be planned: %+v", plan.Findings)
	}
	t.Logf("%s: root=%q, port=%d, storage=%v, build=%s", plan.App.Name, plan.Root, plan.App.Port, plan.App.Storage, plan.Build.Strategy)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.server+"/v1/deploys?plan="+url.QueryEscape(plan.ID), bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
		t.Fatalf("deploy: HTTP %d: %s", res.StatusCode, body)
	}
	var stream io.Reader = res.Body
	if reports := os.Getenv("DONKEY_E2E_REPORTS"); reports != "" {
		f, err := os.OpenFile(filepath.Join(reports, space+"-deploy.ndjson"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		stream = io.TeeReader(stream, f)
	}
	decoder := json.NewDecoder(stream)
	var recipe string
	failureObserved := false
	for {
		var msg struct {
			Line   string               `json:"line"`
			Update *server.DeployUpdate `json:"update"`
			Result *server.DeployResult `json:"result"`
		}
		if err := decoder.Decode(&msg); err != nil {
			t.Fatalf("deployment stream ended without a result: %v", err)
		}
		if msg.Update != nil && msg.Update.Recipe != "" {
			recipe = msg.Update.Recipe
		}
		if msg.Line != "" && !strings.Contains(msg.Line, "\n") {
			t.Log(msg.Line)
		}
		if failureStage != "" && strings.Contains(msg.Line, "failed during "+failureStage+".") {
			failureObserved = true
		}
		if msg.Result != nil {
			if !msg.Result.OK {
				t.Fatalf("deploy failed: %s\n%s", msg.Result.Error, msg.Result.Diagnosis)
			}
			if recipe == "" {
				t.Error("successful build did not expose its build recipe path")
			}
			if failureStage != "" && (!failureObserved || !strings.Contains(msg.Result.ImageRef, "-repair-")) {
				t.Errorf("expected recovery from a real %s failure, saw failure=%v, final image=%s", failureStage, failureObserved, msg.Result.ImageRef)
			}
			break
		}
	}
	status, err := s.api.Status(ctx, space, plan.App.Name)
	if err != nil || !status.Healthy {
		t.Fatalf("deployed app is not healthy: %+v, %v", status, err)
	}
	return plan, "https://" + plan.App.Name + "." + space + "." + s.domain
}

// Follow the app's own HTML references, including filenames with spaces.
// A 200 fallback page for a missing script must not count as a working asset.
func (s *suite) assertSampleAssets(t *testing.T, base string) {
	t.Helper()
	_, html := s.get(t, base+"/", true)
	refs := regexp.MustCompile(`(?:src|href)=["']([^"']+)["']`).FindAllStringSubmatch(html, -1)
	count := 0
	root, _ := url.Parse(base + "/")
	for _, ref := range refs {
		u, err := url.Parse(ref[1])
		if err != nil || u.IsAbs() || u.Host != "" {
			continue
		}
		ext := strings.ToLower(filepath.Ext(u.Path))
		if ext != ".js" && ext != ".css" && ext != ".svg" {
			continue
		}
		res, body := s.get(t, root.ResolveReference(u).String(), true)
		if res.StatusCode != http.StatusOK || body == "" || strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "text/html") {
			t.Errorf("asset %q: HTTP %d, type %q, %d bytes", ref[1], res.StatusCode, res.Header.Get("Content-Type"), len(body))
		}
		count++
	}
	if count == 0 {
		t.Fatal("no application assets found in the served HTML")
	}
	t.Logf("verified %d application assets", count)
}

func (s *suite) assertSamplePersistence(t *testing.T, plan *server.Plan, base string, sqlite bool) {
	t.Helper()
	marker := fmt.Sprintf("builder-e2e-%d", time.Now().UnixNano())
	body, contentType, readPath := url.Values{"text": {marker}}.Encode(), "application/x-www-form-urlencoded", "/"
	if sqlite {
		if len(plan.App.Storage) == 0 {
			t.Fatal("SQLite data directory was not selected for persistent storage")
		}
		body, contentType, readPath = `{"text":"`+marker+`"}`, "application/json", "/notes"
	}
	req, err := http.NewRequest(http.MethodPost, base+"/notes", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", contentType)
	res, err := s.edge.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(io.LimitReader(res.Body, 8192))
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		t.Fatalf("write note: HTTP %d: %s", res.StatusCode, data)
	}
	s.waitBody(t, base+readPath, marker, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	before, err := s.api.Status(ctx, plan.App.Space, plan.App.Name)
	if err != nil || before.StartedAt == "" {
		t.Fatalf("cannot identify the instance before restart: %+v, %v", before, err)
	}
	s.raw(t, http.MethodPost, "/v1/spaces/"+plan.App.Space+"/apps/"+plan.App.Name+"/restart", nil, nil)
	// Restart is asynchronous on Kubernetes. A response from the previous
	// pod would falsely prove persistence; wait for a fresh healthy instance.
	for {
		after, err := s.api.Status(ctx, plan.App.Space, plan.App.Name)
		if err == nil && after.Healthy && after.StartedAt != "" && after.StartedAt != before.StartedAt {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no fresh healthy instance after restart: %+v, %v", after, err)
		case <-time.After(time.Second):
		}
	}
	s.waitBody(t, base+readPath, marker, time.Minute)
	t.Log("application write survived a runtime restart")
}
