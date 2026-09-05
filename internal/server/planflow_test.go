package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/pkg/runtime"
)

// The plan is the one thing the console reviews and `donkey init` prints, so
// what matters is the whole round trip: upload, read, answer, re-check. These
// tests drive the real handlers with the two dependencies stubbed — the
// assistant, and the parts of the runtime the plan actually asks.

// planAssistant answers with a fixed reading of an app.
type planAssistant struct{ analysis *ai.Analysis }

func (planAssistant) Check(context.Context) error { return nil }

func (a planAssistant) Analyze(context.Context, *ai.AnalyzeInput) (*ai.Analysis, error) {
	return a.analysis, nil
}

func (planAssistant) WriteDockerfile(context.Context, *ai.DockerfileInput) (*ai.DockerfileResult, error) {
	return &ai.DockerfileResult{Dockerfile: "FROM scratch\n"}, nil
}

func (planAssistant) DiagnoseFailure(context.Context, *ai.FailureContext) (*ai.FixProposal, error) {
	return nil, nil
}

// planRuntime answers the handful of questions the plan asks the cluster.
// Everything else is left embedded and unimplemented: a plan that grows into
// another runtime call should fail loudly here rather than quietly pass.
type planRuntime struct {
	runtime.Provider
	spaces   []string
	services []string
	running  *runtime.AppStatus
}

func (planRuntime) ValidateSpace(string) error                     { return nil }
func (planRuntime) CheckSpaceFree(context.Context, string) error   { return nil }
func (r planRuntime) ListSpaces(context.Context) ([]string, error) { return r.spaces, nil }
func (planRuntime) AppMeta(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (r planRuntime) ServiceBinding(_ context.Context, _, owner, name string) (map[string]string, error) {
	if owner == addons.SharedOwner && slices.Contains(r.services, name) {
		return map[string]string{"DATABASE_URL": "postgres://…"}, nil
	}
	return nil, nil
}

func (r planRuntime) Status(context.Context, string, string) (*runtime.AppStatus, error) {
	if r.running != nil {
		return r.running, nil
	}
	return &runtime.AppStatus{}, nil
}

// planServer wires a server with those stubs. A Dockerfile in the upload
// keeps detection in-process and offline.
func planServer(rt runtime.Provider, analysis *ai.Analysis) *Server {
	return &Server{
		cfg:       &Config{Domain: "example.com", SessionKey: testSessionKey},
		rt:        rt,
		assistant: planAssistant{analysis: analysis},
		plans:     newPlanStore(),
	}
}

func appUpload(t *testing.T, extra map[string]string) []byte {
	t.Helper()
	files := map[string]string{
		"Dockerfile":   "FROM node:22\nCOPY . .\nCMD [\"node\", \"server.js\"]\n",
		"package.json": `{"dependencies":{"express":"^5.1.0"}}`,
		"server.js":    "require('http').createServer().listen(4000)",
	}
	for k, v := range extra {
		files[k] = v
	}
	return zipBytes(t, files)
}

func callPlan(t *testing.T, s *Server, body []byte, query string) *Plan {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/v1/plans?"+query, bytes.NewReader(body))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan: want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var plan Plan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decoding the plan: %v", err)
	}
	return &plan
}

func callUpdate(t *testing.T, s *Server, id string, edit PlanEdit) *Plan {
	t.Helper()
	body, _ := json.Marshal(edit)
	req := httptest.NewRequest(http.MethodPut, "https://donkey.example.com/v1/plans/"+id, bytes.NewReader(body))
	asAdmin(t, req)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var plan Plan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("decoding the plan: %v", err)
	}
	return &plan
}

func codes(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Code)
	}
	return out
}

// An upload with no donkey.yaml comes back fully answered: that is the whole
// point — the console never has to ask for a port it could read.
func TestPlanAnswersEverything(t *testing.T) {
	s := planServer(planRuntime{spaces: []string{"default"}}, &ai.Analysis{
		AppType:         "an Express API",
		Port:            4000,
		PortReason:      "server.js listens on 4000",
		HealthcheckPath: "/healthz",
		Volumes:         []ai.VolumeSuggestion{{Path: "/app/data", Reason: "writes uploads there"}},
		Services: []ai.ServiceSuggestion{
			{Type: "postgres", Reason: "pg import"},
			{Type: "rustfs", Reason: "boto3 creates an S3 client"},
			{Type: "openai", Reason: "server.js creates an OpenAI client"},
		},
	})
	plan := callPlan(t, s, appUpload(t, nil), "space=acme&name=My%20Cool%20App")

	if plan.ID == "" || !plan.Ready {
		t.Fatalf("a plan that can be deployed should say so: %+v", plan)
	}
	if plan.HasConfig {
		t.Error("this upload has no donkey.yaml")
	}
	if plan.App.Name != "my-cool-app" {
		t.Errorf("the folder's name should become the app's, got %q", plan.App.Name)
	}
	if plan.App.Space != "acme" {
		t.Errorf("the requested space should hold, got %q", plan.App.Space)
	}
	if plan.App.Port != 4000 || plan.App.PortSource != PortFromCode {
		t.Errorf("the port should come out of the code, got %d/%s", plan.App.Port, plan.App.PortSource)
	}
	if plan.App.Healthcheck != "/healthz" {
		t.Errorf("the health route should be adopted, got %q", plan.App.Healthcheck)
	}
	if plan.App.URL != "https://my-cool-app.acme.example.com" {
		t.Errorf("the plan should say where the app will answer, got %q", plan.App.URL)
	}
	if !strings.Contains(plan.Build.Summary, "Dockerfile") {
		t.Errorf("the build summary should name the Dockerfile, got %q", plan.Build.Summary)
	}
	if !slices.Contains(plan.Build.BaseImages, "node:22") {
		t.Errorf("the plan should report the Dockerfile's base image, got %v", plan.Build.BaseImages)
	}
	if len(plan.Build.Runtimes) == 0 || plan.Build.Runtimes[0].Value != "Node.js" {
		t.Errorf("the plan should carry detected runtime evidence, got %+v", plan.Build.Runtimes)
	}
	if len(plan.Build.PackageManagers) == 0 || !strings.Contains(plan.Build.PackageManagers[0].Value, "npm") {
		t.Errorf("the plan should carry package-manager evidence, got %+v", plan.Build.PackageManagers)
	}
	if len(plan.Volumes) != 1 || !plan.Volumes[0].Selected || plan.Volumes[0].Reason == "" {
		t.Errorf("the data folder should be offered, ticked, with its reason: %+v", plan.Volumes)
	}
	if len(plan.Services) != 3 {
		t.Fatalf("every detected catalog service should be offered: %+v", plan.Services)
	}
	for _, service := range plan.Services {
		if !service.Selected || service.Exists || !service.Known {
			t.Errorf("%s should be offered as one to create: %+v", service.Type, service)
		}
	}
	if !strings.Contains(plan.Config, "storage:") || !strings.Contains(plan.Config, "port: 4000") {
		t.Errorf("the rendered donkey.yaml should carry the decisions:\n%s", plan.Config)
	}
	got := codes(plan.Findings)
	for _, want := range []string{"space-new", "service-create", "no-config"} {
		if !slices.Contains(got, want) {
			t.Errorf("expected a %q finding, got %v", want, got)
		}
	}
}

// The upload's own donkey.yaml decides, and the plan says where it disagrees
// with the code instead of overruling it.
func TestPlanRespectsAnUploadedConfig(t *testing.T) {
	s := planServer(planRuntime{spaces: []string{"acme"}}, &ai.Analysis{
		Port:       4000,
		PortReason: "server.js listens on 4000",
	})
	plan := callPlan(t, s, appUpload(t, map[string]string{
		"donkey.yaml": "name: shop\nspace: acme\nport: 3000\n",
	}), "")

	if !plan.HasConfig {
		t.Error("the upload carries a donkey.yaml")
	}
	if plan.App.Name != "shop" || plan.App.Port != 3000 || plan.App.PortSource != PortFromConfig {
		t.Errorf("the file decides: %+v", plan.App)
	}
	if !slices.Contains(codes(plan.Findings), "port-mismatch") {
		t.Errorf("the disagreement with the code should be reported, got %v", codes(plan.Findings))
	}
	if !plan.Ready {
		t.Error("a mismatch is a warning, not a blocker")
	}
}

// What the review screen answers is what the deploy gets — including the
// answers that say no, and including the ones that make it refuse.
func TestPlanUpdateRechecks(t *testing.T) {
	s := planServer(planRuntime{
		spaces:   []string{"acme"},
		services: []string{"shared-thing"},
		running:  &runtime.AppStatus{Found: true, Image: "reg/web:old", WantReplica: 1, ReadyReplica: 1, UpToDate: true},
	}, &ai.Analysis{
		Port:     4000,
		Volumes:  []ai.VolumeSuggestion{{Path: "/app/data", Reason: "writes uploads there"}},
		Services: []ai.ServiceSuggestion{{Type: "postgres", Reason: "pg import"}},
	})
	plan := callPlan(t, s, appUpload(t, nil), "space=acme&name=web")

	if !slices.Contains(codes(plan.Findings), "app-update") {
		t.Errorf("replacing a running app should be said out loud, got %v", codes(plan.Findings))
	}

	// Turn both offers down.
	plan = callUpdate(t, s, plan.ID, PlanEdit{
		Name: "web", Space: "acme", Port: 4000, Exposure: "public",
		Storage: []string{}, Services: []string{},
	})
	if len(plan.App.Storage) != 0 {
		t.Errorf("an unticked folder must not keep its disk: %+v", plan.App.Storage)
	}
	if plan.Volumes[0].Selected || plan.Services[0].Selected {
		t.Error("the choices must come back as they were made")
	}
	if !slices.Contains(codes(plan.Findings), "service-missing") {
		t.Errorf("a service the app needs and won't get should warn, got %v", codes(plan.Findings))
	}
	if plan.App.Exposure != "public" || !strings.Contains(plan.Config, "public: true") {
		t.Errorf("the answers should be in the config:\n%s", plan.Config)
	}
	// "No disk" has to be written down as a decision — an omitted storage
	// block means "nobody decided yet", which the next deploy would fill in
	// with the very folders that were just turned down.
	if !strings.Contains(plan.Config, "paths: []") {
		t.Errorf("a refused disk should be recorded as refused:\n%s", plan.Config)
	}
	if !plan.Ready {
		t.Error("turning offers down is allowed")
	}

	// A name the cluster has already given to something else.
	plan = callUpdate(t, s, plan.ID, PlanEdit{
		Name: "shared-thing", Space: "acme", Port: 4000, Exposure: "protected",
		Storage: []string{}, Services: []string{},
	})
	if plan.Ready || !slices.Contains(codes(plan.Findings), "name-service") {
		t.Errorf("a name a service already holds must block the deploy, got %v (ready=%v)",
			codes(plan.Findings), plan.Ready)
	}
}

// There is no such thing as an app donkey cannot build any more: whatever the
// language, the answer is a Dockerfile the assistant writes during the build.
// A plan for a stack nothing recognizes still comes back complete.
func TestPlanForAnUnrecognizedStack(t *testing.T) {
	s := planServer(planRuntime{spaces: []string{"default"}}, &ai.Analysis{
		AppType:         "a Zig service",
		Port:            8080,
		HealthcheckPath: "/healthz",
	})
	plan := callPlan(t, s, zipBytes(t, map[string]string{"main.zig": "pub fn main() void {}"}), "name=zigapp")

	if !plan.Ready {
		t.Fatalf("a plan for an unknown stack should still be deployable: %+v", plan.Findings)
	}
	if plan.Build.Strategy != string(config.BuildAI) {
		t.Errorf("the builder should write this app a Dockerfile, got strategy %q", plan.Build.Strategy)
	}
	if !strings.Contains(plan.Build.Summary, "Dockerfile") {
		t.Errorf("the summary should say a Dockerfile gets written, got %q", plan.Build.Summary)
	}
	if plan.App.Port != 8080 {
		t.Errorf("the assistant's port should be adopted, got %d", plan.App.Port)
	}
}
