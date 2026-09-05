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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

func TestProjectWithOneApp(t *testing.T) {
	s, reading, _ := projectFixture(t)
	reading.Apps = []ai.ProjectApp{{Path: ".", Context: "."}}
	reading.Connections = []ai.ProjectConnection{{App: ".", Target: "postgres", Kind: "service", Env: "DB_URL", Key: "DATABASE_URL"}}
	p := callProject(t, s, appUpload(t, nil))
	if !p.Ready || len(p.Apps) != 1 || p.Apps[0].App.Name != "repo" || len(p.Connections) != 1 {
		t.Fatalf("a normal repository must use the same project flow: %+v", p)
	}
}

func TestProjectKeepsExistingBrowserRoute(t *testing.T) {
	for _, tc := range []struct {
		name, path, routeApp string
		public, ready        bool
	}{
		{"exact path", "/api", "frontend", true, true},
		{"child path", "/api/tickets", "frontend", true, true},
		{"different prefix", "/apix", "frontend", true, false},
		{"unknown path", "", "frontend", true, false},
		{"different caller", "/api", "other", true, false},
		{"login mismatch", "/api", "frontend", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, reading, body := projectFixture(t)
			reading.Connections = []ai.ProjectConnection{{App: "frontend", Target: "backend", Kind: "browser", Suffix: tc.path}}
			dir := t.TempDir()
			if _, err := extractContext(bytes.NewReader(body), dir); err != nil {
				t.Fatal(err)
			}
			state, err := s.readProject(context.Background(), dir, reading, planInput{})
			if err != nil {
				t.Fatal(err)
			}
			state.apps[0].app.Public = true
			state.apps[1].app.Internal = true
			state.apps[1].app.Public = tc.public
			routes := []config.RouteConfig{{App: tc.routeApp, Path: "/api"}}
			state.apps[1].app.Routes = routes
			plan, apps, patches := s.resolveProject(context.Background(), "test", state)
			if plan.Ready != tc.ready {
				t.Fatalf("ready=%v, want %v: %+v", plan.Ready, tc.ready, plan.Findings)
			}
			if !reflect.DeepEqual(apps[1].app.Routes, routes) || len(patches) != 0 {
				t.Fatal("existing routing must not be changed")
			}
			if tc.ready && (len(plan.Connections) != 1 || plan.Connections[0].Value != tc.path) {
				t.Fatalf("existing connection missing from review: %+v", plan.Connections)
			}
		})
	}
}

func TestProjectValidatesConfiguredBuildContext(t *testing.T) {
	for _, test := range []struct {
		name, configured, guessed string
		valid                     bool
	}{
		{"workspace", "..", "backend", true},
		{"explicit setting wins", ".", "../outside", true},
		{"escape", "../..", "backend", false},
		{"sibling", "../frontend", "backend", false},
		{"absolute", "/", "backend", false},
		{"missing guess", "", "missing", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, reading, body := projectFixture(t)
			dir := t.TempDir()
			if _, err := extractContext(bytes.NewReader(body), dir); err != nil {
				t.Fatal(err)
			}
			data := "name: backend\nport: 8080\nbuild:\n  context: " + fmt.Sprintf("%q", test.configured) + "\n"
			if err := os.WriteFile(filepath.Join(dir, "backend", config.AppFileName), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			reading.Apps[1].Context = test.guessed
			_, err := s.readProject(context.Background(), dir, reading, planInput{})
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v: %v", test.valid, err)
			}
		})
	}
}

func TestProjectUsesSpaceFromConfiguredSibling(t *testing.T) {
	s, reading, body := projectFixture(t)
	dir := t.TempDir()
	if _, err := extractContext(bytes.NewReader(body), dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frontend", config.AppFileName), []byte("name: frontend\nport: 8080\nspace: team\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := s.readProject(context.Background(), dir, reading, planInput{})
	if err != nil {
		t.Fatal(err)
	}
	p, apps, _ := s.resolveProject(context.Background(), "test", state)
	if !p.Ready || apps[0].app.Space != "team" || apps[1].app.Space != "team" {
		t.Fatalf("an omitted space must inherit the sibling's choice: %+v", p)
	}
}

func TestProjectRejectsConflictingNamesAndServices(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func([]*storedPlan)
	}{
		{"app names", func(apps []*storedPlan) { apps[0].app.Name = apps[1].app.Name }},
		{"app and service", func(apps []*storedPlan) { apps[0].app.Name = "postgres" }},
		{"service types", func(apps []*storedPlan) {
			apps[0].app.Addons = []config.AddonConfig{{Name: "postgres", Type: "rustfs"}}
		}},
		{"service versions", func(apps []*storedPlan) {
			apps[0].app.Addons = []config.AddonConfig{{Name: "postgres", Type: "postgres", Version: "16"}}
			apps[1].app.Addons[0].Version = "17"
		}},
		{"service storage", func(apps []*storedPlan) {
			apps[0].app.Addons = []config.AddonConfig{{Name: "postgres", Type: "postgres", Storage: "10Gi"}}
			apps[1].app.Addons[0].Storage = "20Gi"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _, body := projectFixture(t)
			p := callProject(t, s, body)
			stored, _ := s.plans.get(p.ID)
			test.edit(stored.project.apps)
			p, _, _ = s.resolveProject(context.Background(), p.ID, stored.project)
			if p.Ready || !Blocking(p.Findings) {
				t.Fatalf("conflict was accepted: %+v", p)
			}
		})
	}
}

func TestProjectSharesDeclaredServiceOptions(t *testing.T) {
	s, _, body := projectFixture(t)
	p := callProject(t, s, body)
	stored, _ := s.plans.get(p.ID)
	stored.project.apps[0].app.Addons = []config.AddonConfig{{Name: "postgres", Type: "postgres"}}
	stored.project.apps[1].app.Addons[0].Version = "16"
	stored.project.apps[1].app.Addons[0].Storage = "20Gi"
	p, apps, _ := s.resolveProject(context.Background(), p.ID, stored.project)
	if !p.Ready {
		t.Fatal(p.Findings)
	}
	for _, app := range apps {
		if service := app.app.Addons[0]; service.Version != "16" || service.Storage != "20Gi" {
			t.Fatalf("deploy order could change the shared service: %+v", service)
		}
	}
}

func TestProjectRejectsUnsafeURLPaths(t *testing.T) {
	for _, suffix := range []string{"/../admin", "/%2e%2e/admin", "/a/./b", "/%2f../admin", "//elsewhere", "/api?override=1", "/api#fragment", "/api%00", "/api%0d", "/api%5c", "/api%22", "/api%27", "/%zz"} {
		t.Run(suffix, func(t *testing.T) {
			s, reading, body := projectFixture(t)
			reading.Connections[0].Suffix = suffix
			p := callProject(t, s, body)
			if p.Ready || !Blocking(p.Findings) {
				t.Fatalf("unsafe path %q passed review", suffix)
			}
		})
	}
	for _, suffix := range []string{"", "/", "/api/v1/", "/items/a%2Fb", "/api/a%20b"} {
		if !validURLPath(suffix) {
			t.Fatalf("ordinary API path %q was rejected", suffix)
		}
	}
}

func TestProjectUpdateIsAtomic(t *testing.T) {
	s, _, body := projectFixture(t)
	p := callProject(t, s, body)
	stored, _ := s.plans.get(p.ID)
	before := stored.project.apps[0].appCopy()
	edits := projectEdits(p)
	edits[0].Name, edits[1].Port = "changed", 0
	raw, _ := json.Marshal(edits)
	req := httptest.NewRequest(http.MethodPut, "/v1/projects/plans/"+p.ID, bytes.NewReader(raw))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !reflect.DeepEqual(before, stored.project.apps[0].app) {
		t.Fatalf("an invalid sibling partially changed the project: %d %s", rec.Code, rec.Body.String())
	}
}

type reviewRuntime struct {
	projectRuntime
	bindings map[string]map[string]string
	choices  map[string]consoleChoices
}

func (r reviewRuntime) ServiceBinding(_ context.Context, _, _, name string) (map[string]string, error) {
	return r.bindings[name], nil
}

func (r reviewRuntime) AppMeta(_ context.Context, _, app, _ string) (string, error) {
	data, err := json.Marshal(r.choices[app])
	return string(data), err
}

func TestProjectKeepsReviewedConsoleChoices(t *testing.T) {
	s, _, body := projectFixture(t)
	s.rt = reviewRuntime{projectRuntime: s.rt.(projectRuntime), choices: map[string]consoleChoices{
		"frontend": {HostOverride: "custom.example.com", Exposure: "public"},
		"backend":  {Exposure: "public"},
	}}
	p := callProject(t, s, body)
	if !p.Ready || p.Apps[0].App.URL != "https://custom.example.com" || p.Apps[0].App.Exposure != "public" {
		t.Fatalf("review missed existing console choices: %+v", p)
	}
	stored, _ := s.plans.get(p.ID)
	edits := projectEdits(p)
	for i := range edits {
		edits[i].Exposure = "protected"
		applyPlanEdit(stored.project.apps[i], edits[i])
	}
	p, _, _ = s.resolveProject(context.Background(), p.ID, stored.project)
	if !p.Ready || p.Apps[0].App.Exposure != "protected" {
		t.Fatal("a recheck overrode the user's reviewed choice")
	}
}

func TestProjectChecksActualServiceKeys(t *testing.T) {
	s, reading, body := projectFixture(t)
	s.rt = reviewRuntime{projectRuntime: s.rt.(projectRuntime), bindings: map[string]map[string]string{
		"openai": {"OPENAI_BASE_URL": "https://external.example.com/v1"},
	}}
	p := callProject(t, s, body)
	if !p.Ready || strings.Contains(p.Apps[1].Config, "${openai.OPENAI_API_KEY}") || strings.Contains(p.Apps[1].Config, "${openai.OPENAI_MODEL}") {
		t.Fatal("an endpoint-only service must not invent credentials or a model")
	}
	reading.Connections[4].Key = "OPENAI_API_KEY"
	p = callProject(t, s, body)
	if p.Ready || !strings.Contains(p.Apps[1].Config, "${openai.OPENAI_API_KEY}") {
		t.Fatal("an alias to a missing key must block review")
	}
}

type waitingRuntime struct {
	runtime.Provider
	status func(string) (*runtime.AppStatus, error)
}

func (r waitingRuntime) Status(_ context.Context, _, app string) (*runtime.AppStatus, error) {
	return r.status(app)
}

func TestProjectWaitKeepsIndividualResults(t *testing.T) {
	s := &Server{rt: waitingRuntime{status: func(app string) (*runtime.AppStatus, error) {
		switch app {
		case "ready":
			return &runtime.AppStatus{Found: true, UpToDate: true, WantReplica: 1, ReadyReplica: 1}, nil
		case "old":
			return &runtime.AppStatus{Found: true, WantReplica: 1, ReadyReplica: 1}, nil
		case "missing":
			return nil, nil
		case "failed-build":
			t.Error("must not wait on an app that failed before rollout")
		}
		return nil, fmt.Errorf("unavailable")
	}}}
	results := []DeployResult{{App: "ready", OK: true}, {App: "old", OK: true}, {App: "missing", OK: true}, {App: "unavailable", OK: true}, {App: "failed-build", Error: "build failed"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.waitProject(ctx, results)
	if !results[0].OK || results[4].Error != "build failed" {
		t.Fatal("waiting overwrote a completed result")
	}
	for _, result := range results[1:4] {
		if result.OK || result.Error == "" {
			t.Fatalf("unfinished app reported success: %+v", result)
		}
	}
}

func TestOverlappingProjectsDoNotDeadlock(t *testing.T) {
	s := &Server{}
	apps := []*storedPlan{{app: &config.AppConfig{Name: "api", Space: "team"}}, {app: &config.AppConfig{Name: "web", Space: "team"}}}
	start, done := make(chan struct{}), make(chan struct{})
	var group sync.WaitGroup
	for _, order := range [][]*storedPlan{apps, {apps[1], apps[0]}} {
		group.Go(func() {
			<-start
			for range 20 {
				unlock := s.lockProject(order)
				unlock()
			}
		})
	}
	close(start)
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("opposite project orders deadlocked")
	}
}

func TestProjectOriginsReachBrowserBuilds(t *testing.T) {
	s, reading, body := projectFixture(t)
	reading.Connections = append(reading.Connections,
		ai.ProjectConnection{App: "frontend", Target: "backend", Kind: "origin", Env: "PUBLIC_API_ORIGIN"},
		ai.ProjectConnection{App: "backend", Target: "frontend", Kind: "origin", Env: "ALLOWED_ORIGIN"},
	)
	p := callProject(t, s, body)
	stored, _ := s.plans.get(p.ID)
	stored.project.apps[0].app.Host = "custom.example.com"
	stored.project.apps[1].app.Build.Env = map[string]string{"ALLOWED_ORIGIN": "http://localhost:3000"}
	p, apps, _ := s.resolveProject(context.Background(), p.ID, stored.project)
	if !p.Ready || apps[0].app.Build.Env["PUBLIC_API_ORIGIN"] != "https://backend.default.example.com" || apps[1].app.Env["ALLOWED_ORIGIN"] != "https://custom.example.com" || apps[1].app.Build.Env["ALLOWED_ORIGIN"] != "https://custom.example.com" {
		t.Fatalf("an origin setting kept its development URL: %+v", p)
	}
}

type dockerAddressRuntime struct{ projectRuntime }

func (dockerAddressRuntime) InternalURL(space, app string, port int) string {
	return fmt.Sprintf("http://%s:%d", app, port)
}

func TestProjectGRPCAddressesUseRuntimePorts(t *testing.T) {
	for _, docker := range []bool{false, true} {
		t.Run(fmt.Sprint(docker), func(t *testing.T) {
			s, reading, body := projectFixture(t)
			want := "backend.default.svc:80"
			if docker {
				s.rt = dockerAddressRuntime{s.rt.(projectRuntime)}
				want = "backend:8080"
			}
			reading.Connections[2].Kind, reading.Connections[2].Suffix = "address", ""
			p := callProject(t, s, body)
			if !p.Ready {
				t.Fatal(p.Findings)
			}
			for _, connection := range p.Connections {
				if connection.Env == "API_URL" && connection.Value != want {
					t.Fatalf("gRPC address = %q, want %q", connection.Value, want)
				}
			}
		})
	}
}
