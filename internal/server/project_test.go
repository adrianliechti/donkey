package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

type projectAssistant struct {
	planAssistant
	project *ai.Project
}

func (a projectAssistant) ReadProject(context.Context, string) (*ai.Project, error) {
	data, _ := json.Marshal(a.project)
	var out ai.Project
	json.Unmarshal(data, &out)
	return &out, nil
}

type projectRuntime struct{ planRuntime }

func (projectRuntime) InternalURL(space, app string, port int) string {
	return fmt.Sprintf("http://%s.%s.svc", app, space)
}

func (projectRuntime) Render(spec runtime.DeploySpec) (string, error) {
	data, err := spec.App.Render()
	return string(data), err
}

func projectFixture(t *testing.T) (*Server, *ai.Project, []byte) {
	t.Helper()
	reading := &ai.Project{
		Apps: []ai.ProjectApp{{Path: "frontend", Context: "."}, {Path: "backend", Context: "backend"}},
		Connections: []ai.ProjectConnection{
			{App: "frontend", Target: "backend", Kind: "browser", Env: "VITE_API_URL", Suffix: "/api", Reason: "frontend fetches the API"},
			{App: "frontend", Target: "backend", Kind: "browser", Suffix: "/api", File: "frontend/client.js", Old: `"http://localhost:8080/api"`, New: `"{{url}}"`},
			{App: "frontend", Target: "backend", Kind: "server", Env: "API_URL", Suffix: "/api"},
			{App: "backend", Target: "postgres", Kind: "service", Env: "DB_URL", Key: "DATABASE_URL"},
			{App: "backend", Target: "openai", Kind: "service", Env: "AI_ENDPOINT", Key: "OPENAI_BASE_URL"},
			{App: "backend", Target: "rustfs", Kind: "service", Env: "S3_ENDPOINT", Key: "AWS_ENDPOINT_URL_S3"},
		},
	}
	s := planServer(projectRuntime{planRuntime{spaces: []string{"default"}}}, &ai.Analysis{Port: 8080})
	s.assistant = projectAssistant{planAssistant{analysis: &ai.Analysis{Port: 8080}}, reading}
	body := zipBytes(t, map[string]string{
		"repo/package.json":                 `{"private":true,"workspaces":["frontend","packages/*"]}`,
		"repo/packages/shared/package.json": `{"name":"shared"}`,
		"repo/frontend/package.json":        `{"scripts":{"build":"vite build"}}`,
		"repo/frontend/Dockerfile":          "FROM node:22\nCOPY . .\nRUN npm run build\n",
		"repo/frontend/client.js":           `fetch("http://localhost:8080/api")`,
		"repo/backend/Dockerfile":           "FROM node:22\nCOPY . .\nCMD [\"node\",\"server.js\"]\n",
		"repo/backend/donkey.yaml":          "name: backend\nport: 8080\nenv:\n  DATABASE_URL: postgres://old:old@localhost/app\n",
		"repo/backend/server.js":            `require('http').createServer().listen(8080)`,
	})
	return s, reading, body
}

func callProject(t *testing.T, s *Server, body []byte) *ProjectPlan {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/plans?name=repo", bytes.NewReader(body))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("project: %d %s", rec.Code, rec.Body.String())
	}
	var p ProjectPlan
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return &p
}

func projectEdits(p *ProjectPlan) []PlanEdit {
	var edits []PlanEdit
	for _, app := range p.Apps {
		edit := PlanEdit{Name: app.App.Name, Space: app.App.Space, Port: app.App.Port, Exposure: app.App.Exposure, Storage: app.App.Storage}
		for _, service := range app.Services {
			if service.Selected {
				edit.Services = append(edit.Services, service.Name)
			}
		}
		edits = append(edits, edit)
	}
	return edits
}

func TestProjectPlansEveryAppAndResolvesConnections(t *testing.T) {
	s, _, body := projectFixture(t)
	p := callProject(t, s, body)
	if !p.Ready || len(p.Apps) != 2 {
		t.Fatalf("want both apps ready: %+v", p)
	}
	if p.Apps[0].HasConfig || !p.Apps[1].HasConfig {
		t.Fatal("one donkey.yaml must not hide its unconfigured sibling")
	}
	stored, _ := s.plans.get(p.ID)
	_, apps, patches := s.resolveProject(context.Background(), p.ID, stored.project)
	front, back := apps[0].app, apps[1].app
	if front.Build.Context != ".." {
		t.Fatalf("lost workspace context: %+v", front.Build)
	}
	if front.Build.Env["VITE_API_URL"] != "/_donkey/backend/api" || front.Env["VITE_API_URL"] != front.Build.Env["VITE_API_URL"] {
		t.Fatal("browser URL must reach build and runtime")
	}
	if front.Env["API_URL"] != "http://backend.default.svc/api" {
		t.Fatal("server URL must use runtime DNS")
	}
	if len(back.Routes) != 1 || back.Routes[0] != (config.RouteConfig{App: "frontend", Path: "/_donkey/backend", StripPrefix: true}) {
		t.Fatalf("bad browser route: %+v", back.Routes)
	}
	for key, value := range map[string]string{"DATABASE_URL": "${postgres.DATABASE_URL}", "DB_URL": "${postgres.DATABASE_URL}", "AI_ENDPOINT": "${openai.OPENAI_BASE_URL}", "OPENAI_API_KEY": "${openai.OPENAI_API_KEY}", "OPENAI_MODEL": "${openai.OPENAI_MODEL}", "S3_ENDPOINT": "${rustfs.AWS_ENDPOINT_URL_S3}", "AWS_SECRET_ACCESS_KEY": "${rustfs.AWS_SECRET_ACCESS_KEY}"} {
		if back.Env[key] != value {
			t.Errorf("%s = %q, want %q", key, back.Env[key], value)
		}
		if _, exists := back.Build.Env[key]; exists {
			t.Errorf("service variable %s leaked into build", key)
		}
	}
	if len(patches) != 1 || patches[0].after != `fetch("/_donkey/backend/api")` {
		t.Fatalf("source URL not repaired: %+v", patches)
	}
	if len(p.Changes) != 1 || !strings.Contains(p.Changes[0].Diff, "+fetch") {
		t.Fatal("source fix must be reviewable")
	}
	if strings.Contains(p.Apps[1].Config, "old:old") {
		t.Fatal("stale database URL survived review")
	}
}

func TestProjectUsesAppNameForNestedSourceDirectory(t *testing.T) {
	s, reading, _ := projectFixture(t)
	reading.Apps = []ai.ProjectApp{
		{Name: "cartservice", Path: "cartservice/src", Context: "cartservice/src", Env: []ai.ProjectEnv{{Name: "DISABLE_PROFILER", Value: "1"}, {Name: "PORT", Value: "9999"}}},
		{Name: "suggested", Path: "api", Context: "api", Env: []ai.ProjectEnv{{Name: "MODE", Value: "suggested"}}},
	}
	reading.Connections = nil
	body := zipBytes(t, map[string]string{
		"repo/cartservice/src/Dockerfile": "FROM scratch\n",
		"repo/api/Dockerfile":             "FROM scratch\n",
		"repo/api/donkey.yaml":            "name: configured\nport: 8080\nenv:\n  MODE: configured\n",
	})
	p := callProject(t, s, body)
	if p.Apps[0].App.Name != "cartservice" || p.Apps[1].App.Name != "configured" {
		t.Fatalf("names = %s, %s", p.Apps[0].App.Name, p.Apps[1].App.Name)
	}
	stored, _ := s.plans.get(p.ID)
	cart, api := stored.project.apps[0].app, stored.project.apps[1].app
	if cart.Env["DISABLE_PROFILER"] != "1" || cart.Env["PORT"] != "" || cart.Port == 9999 || api.Env["MODE"] != "configured" {
		t.Fatalf("deployment defaults must preserve explicit settings and the chosen port: %v, %v", cart.Env, api.Env)
	}
}

func TestProjectRecheckRewiresNamesAndSpace(t *testing.T) {
	s, _, body := projectFixture(t)
	p := callProject(t, s, body)
	edits := projectEdits(p)
	edits[0].Name, edits[1].Name = "web", "api"
	edits[0].Space, edits[1].Space = "team", "team"
	raw, _ := json.Marshal(edits)
	req := httptest.NewRequest(http.MethodPut, "/v1/projects/plans/"+p.ID, bytes.NewReader(raw))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	stored, _ := s.plans.get(p.ID)
	plan, apps, patches := s.resolveProject(context.Background(), p.ID, stored.project)
	if !plan.Ready {
		t.Fatalf("rechecked plan blocked: %+v", plan.Findings)
	}
	if apps[0].app.Env["API_URL"] != "http://api.team.svc/api" || apps[0].app.Build.Env["VITE_API_URL"] != "/_donkey/api/api" {
		t.Fatal("renamed peer left stale URLs")
	}
	if len(apps[1].app.Routes) != 1 || apps[1].app.Routes[0].App != "web" {
		t.Fatalf("old routes survived: %+v", apps[1].app.Routes)
	}
	if strings.Contains(patches[0].after, "backend") {
		t.Fatal("source patch kept the previous name")
	}
}

func TestProjectRejectsInvalidConnections(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ai.Project)
	}{
		{"unknown target", func(p *ai.Project) { p.Connections[0].Target = "missing" }},
		{"browser credential", func(p *ai.Project) { p.Connections[3].Env = "VITE_DATABASE_URL" }},
		{"unmatched edit", func(p *ai.Project) { p.Connections[1].Old = "not in source" }},
		{"unknown binding", func(p *ai.Project) { p.Connections[3].Key = "MISSING" }},
		{"invalid kind", func(p *ai.Project) { p.Connections[0].Kind = "made-up" }},
		{"internal browser URL", func(p *ai.Project) { p.Connections[0].Kind = "server" }},
		{"no destination", func(p *ai.Project) { p.Connections[0].Env = "" }},
		{"conflicting env", func(p *ai.Project) { p.Connections[2].Env = "VITE_API_URL" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, reading, body := projectFixture(t)
			test.change(reading)
			p := callProject(t, s, body)
			if p.Ready || !Blocking(p.Findings) {
				t.Fatalf("unsafe plan was accepted: %+v", p)
			}
		})
	}
}

func TestProjectDeploymentRejectsChangedUpload(t *testing.T) {
	s, _, body := projectFixture(t)
	p := callProject(t, s, body)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/deploys?plan="+p.ID, bytes.NewReader(appUpload(t, nil)))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed since review") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestProjectDryRunUsesEachReviewedApp(t *testing.T) {
	s, _, body := projectFixture(t)
	p := callProject(t, s, body)
	req := httptest.NewRequest(http.MethodPost, "/v1/projects/deploys?plan="+p.ID+"&dryRun=true", bytes.NewReader(body))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	var result DeployResult
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		var msg struct {
			Result *DeployResult `json:"result"`
		}
		json.Unmarshal([]byte(line), &msg)
		if msg.Result != nil {
			result = *msg.Result
		}
	}
	if !result.OK || len(result.Apps) != 2 {
		t.Fatalf("not every app deployed: %+v\n%s", result, rec.Body.String())
	}
	if result.Apps[0].App != "backend" {
		t.Fatal("dependency must deploy first")
	}
	if !strings.Contains(result.Apps[1].Manifests, "/_donkey/backend/api") {
		t.Fatal("reviewed browser URL was not deployed")
	}
}

func TestProjectPathsAndPatchPreconditions(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "frontend")
	os.Mkdir(app, 0755)
	if root, err := buildContext(dir, app, ".."); err != nil || root != dir {
		t.Fatalf("workspace parent rejected: %s %v", root, err)
	}
	if _, err := buildContext(dir, app, "../.."); err == nil {
		t.Fatal("escaped the upload")
	}
	if _, _, err := projectPath(dir, "../outside"); err == nil {
		t.Fatal("escaped project path")
	}
	os.WriteFile(filepath.Join(dir, "a.js"), []byte("old"), 0644)
	os.WriteFile(filepath.Join(dir, "b.js"), []byte("changed"), 0644)
	err := applyProjectPatches(dir, []projectPatch{{"a.js", "old", "new"}, {"b.js", "old", "new"}})
	if err == nil {
		t.Fatal("changed source accepted")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "a.js"))
	if string(data) != "old" {
		t.Fatal("an earlier patch was written before all preconditions passed")
	}
}

func TestProjectOrderHandlesCycles(t *testing.T) {
	apps := []*storedPlan{{root: "web"}, {root: "api"}, {root: "worker"}}
	connections := []ai.ProjectConnection{{App: "web", Target: "api", Kind: "browser"}, {App: "api", Target: "worker", Kind: "server"}}
	order, cycle := projectOrder(apps, connections)
	if cycle || !slices.Equal(order, []int{2, 1, 0}) {
		t.Fatalf("bad dependency order: %v %v", order, cycle)
	}
	connections = append(connections, ai.ProjectConnection{App: "worker", Target: "api", Kind: "server"})
	order, cycle = projectOrder(apps, connections)
	if !cycle || len(order) != 3 {
		t.Fatal("cycle must retain every app and postpone rollout waits")
	}
}
