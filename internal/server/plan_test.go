package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/adrianliechti/donkey/internal/detect"
)

func planned() *storedPlan {
	app := &config.AppConfig{Name: "web", Space: "acme", Port: 8000}
	app.ApplyDefaults()
	sp := &storedPlan{
		app:      app,
		analysis: analysis(),
		volumes:  []PlanVolume{{Path: "/app/data", Reason: "writes app.db", Selected: true}},
		services: []PlanService{{Type: "postgres", Name: "postgres", Known: true, Selected: true}},
		created:  time.Now(),
	}
	applyAnalysis(app, sp.analysis)
	app.Addons = append(app.Addons, config.AddonConfig{Type: "postgres", Name: "postgres"})
	return sp
}

// The review screen's answers are the app's config — including the answers
// that say no. An unticked disk has to actually mean "no disk", or the deploy
// would helpfully put it back.
func TestPlanEditIsAuthoritative(t *testing.T) {
	sp := planned()
	applyPlanEdit(sp, PlanEdit{
		Name: "shop", Space: "acme", Port: 3000,
		Healthcheck: "/healthz",
		Storage:     []string{}, // unticked
		Services:    []string{}, // unticked
		Exposure:    "public",
	})

	app := sp.app
	if app.Name != "shop" || app.Port != 3000 {
		t.Errorf("name and port should follow the edit, got %s:%d", app.Name, app.Port)
	}
	if app.HasStorage() {
		t.Error("unticking every folder must mean no disk")
	}
	if app.Storage == nil || app.Storage.Paths == nil {
		t.Error("the decision has to be recorded, not left open for the deploy to re-decide")
	}
	if len(app.Addons) != 0 {
		t.Errorf("unticking the service must drop it, got %+v", app.Addons)
	}
	if !app.Public {
		t.Error("exposure should follow the edit")
	}
	if sp.volumes[0].Selected || sp.services[0].Selected {
		t.Error("the choices shown must reflect what was chosen")
	}
	if err := app.Validate(); err != nil {
		t.Errorf("the edited config should still be valid: %v", err)
	}
}

// A service that donkey.yaml declared with its own version and size keeps
// them: the plan may propose services, but it must not rewrite the ones the
// user already described.
func TestPlanEditKeepsDeclaredServiceDetail(t *testing.T) {
	sp := planned()
	sp.app.Addons = []config.AddonConfig{{Type: "postgres", Name: "postgres", Version: "16", Storage: "20Gi"}}

	applyPlanEdit(sp, PlanEdit{
		Name: "web", Space: "acme", Port: 8000,
		Storage:  []string{"/app/data"},
		Services: []string{"postgres"},
		Exposure: "protected",
	})

	if len(sp.app.Addons) != 1 {
		t.Fatalf("the service should stay, got %+v", sp.app.Addons)
	}
	if got := sp.app.Addons[0]; got.Version != "16" || got.Storage != "20Gi" {
		t.Errorf("donkey.yaml's own detail should survive the edit, got %+v", got)
	}
	if !slices.Contains(sp.app.Storage.Paths, "/app/data") {
		t.Error("a ticked folder should keep its disk")
	}
}

// An unknown service type is reported, never offered — so it can never be
// selected into the config, whatever the client sends.
func TestPlanEditIgnoresUnknownService(t *testing.T) {
	sp := planned()
	sp.services = []PlanService{{Type: "cassandra", Name: "cassandra", Known: false}}
	sp.app.Addons = nil

	applyPlanEdit(sp, PlanEdit{
		Name: "web", Space: "acme", Port: 8000, Services: []string{"cassandra"}, Exposure: "protected",
	})

	if len(sp.app.Addons) != 0 {
		t.Errorf("donkey has no recipe for this type, so it must not end up in the config: %+v", sp.app.Addons)
	}
}

// The folders the app writes to are shown either way: what donkey.yaml turned
// down stays visible and unticked, rather than quietly disappearing.
func TestPlanVolumesShowRefusedFolders(t *testing.T) {
	app := &config.AppConfig{Name: "web", Port: 8000, Storage: &config.StorageConfig{Paths: []string{}}}
	app.ApplyDefaults()
	vols := planVolumes(app, analysis())

	if len(vols) != 1 || vols[0].Path != "/app/data" {
		t.Fatalf("the assistant's folder should be listed, got %+v", vols)
	}
	if vols[0].Selected {
		t.Error("a folder donkey.yaml turned down must show up unticked")
	}
	if vols[0].Reason == "" {
		t.Error("the reason is the whole point of showing it")
	}
}

// A path only donkey.yaml knows about is still the app's storage.
func TestPlanVolumesIncludeConfiguredPaths(t *testing.T) {
	app := &config.AppConfig{Name: "web", Port: 8000, Storage: &config.StorageConfig{Paths: []string{"/srv/uploads"}}}
	app.ApplyDefaults()
	vols := planVolumes(app, &ai.Analysis{})

	if len(vols) != 1 || vols[0].Path != "/srv/uploads" || !vols[0].Selected {
		t.Fatalf("a configured path should be listed and ticked, got %+v", vols)
	}
}

// The port is explained, not asked for: where the answer came from decides
// what the review screen says about it.
func TestProposedPortSource(t *testing.T) {
	det := &detect.Detection{Strategy: config.BuildAI}

	if port, source := proposedPort(det, &ai.Analysis{Port: 5000, PortReason: "app.run(port=5000)"}); port != 5000 || source != PortFromCode {
		t.Errorf("a port read out of the code wins: got %d/%s", port, source)
	}
	// No stack-by-stack table any more: donkey tells every app its port
	// through PORT, so an app that honors it takes donkey's choice.
	if port, source := proposedPort(det, &ai.Analysis{PortReason: "honors PORT"}); port != 8080 || source != PortFromEnv {
		t.Errorf("an app that honors PORT takes donkey's choice: got %d/%s", port, source)
	}
	if port, source := proposedPort(det, &ai.Analysis{}); port != 8080 || source != PortGuessed {
		t.Errorf("otherwise the default, as a guess: got %d/%s", port, source)
	}
	if port, _ := proposedPort(nil, &ai.Analysis{}); port != 8080 {
		t.Errorf("with nothing to go on, one sane default: got %d", port)
	}
}

// A plan the deploy can no longer produce is worse than none: an expired one
// must be refused, not deployed with stale answers.
func TestExpiredPlanIsRefused(t *testing.T) {
	store := newPlanStore()
	sp := planned()
	sp.created = time.Now().Add(-2 * planTTL)
	id := store.put(sp)
	if _, ok := store.get(id); ok {
		t.Error("an expired plan must not be handed out")
	}

	fresh := planned()
	if _, ok := store.get(store.put(fresh)); !ok {
		t.Error("a fresh plan should be there")
	}
}

// The deploy says so plainly rather than falling back to a different reading
// of the app than the one that was reviewed.
func TestDeployWithUnknownPlan(t *testing.T) {
	s := &Server{cfg: &Config{Domain: "example.com"}, plans: newPlanStore()}
	req := httptest.NewRequest(http.MethodPost, "https://donkey.example.com/v1/deploys?plan=nope", nil)
	req.Header.Set("X-Auth-Request-User", "u1")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("an unknown plan should be 410 Gone, got %d", rec.Code)
	}
}

// The config the console shows for copying is the file the CLI would write —
// that is the only reason it is worth showing.
func TestPlanConfigRendersAsDonkeyYAML(t *testing.T) {
	sp := planned()
	rendered, err := sp.app.Render()
	if err != nil {
		t.Fatalf("rendering the config: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.AppFileName), rendered, 0o644); err != nil {
		t.Fatal(err)
	}
	back, err := config.LoadApp(dir)
	if err != nil {
		t.Fatalf("what the console shows must load as a donkey.yaml: %v", err)
	}
	if back.Name != sp.app.Name || back.Port != sp.app.Port {
		t.Errorf("the rendered file should describe the same app, got %+v", back)
	}
	if !back.HasStorage() {
		t.Error("the disk the plan decided on belongs in the file")
	}
}

// A deploy must not be able to write its choices back into a plan somebody
// might deploy again.
func TestPlanConfigIsCopied(t *testing.T) {
	sp := planned()
	copied := sp.appCopy()
	copied.Public = true
	copied.Name = "somethingelse"

	if sp.app.Public || sp.app.Name != "web" {
		t.Error("the stored plan must not change when a deploy settles its own copy")
	}
}
