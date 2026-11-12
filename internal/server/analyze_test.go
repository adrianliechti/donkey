package server

import (
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/ai"
	"github.com/adrianliechti/donkey/internal/config"
)

func analysis() *ai.Analysis {
	return &ai.Analysis{
		Port:            8000,
		HealthcheckPath: "/healthz",
		Volumes:         []ai.VolumeSuggestion{{Path: "/app/data", Reason: "main.py writes app.db there"}},
		Services:        []ai.ServiceSuggestion{{Type: "postgres", Reason: "psycopg import"}},
	}
}

// findingText is how a terminal (and this test) reads a set of findings.
func findingText(findings []Finding) string {
	var lines []string
	for _, f := range findings {
		lines = append(lines, f.Text())
	}
	return strings.Join(lines, "\n")
}

// What donkey.yaml doesn't say, the assistant may fill in.
func TestMergeFillsWhatIsOpen(t *testing.T) {
	app := &config.AppConfig{Name: "web", Port: 8000}
	app.ApplyDefaults()
	findings := mergeAnalysis(app, analysis())

	if !app.HasStorage() || app.Storage.Paths[0] != "/app/data" {
		t.Fatalf("volume suggestion should become storage: %+v", app.Storage)
	}
	if app.Storage.Size != config.DefaultStorage {
		t.Errorf("storage should get the default size, got %q", app.Storage.Size)
	}
	if app.Healthcheck.Path != "/healthz" {
		t.Errorf("health path should be adopted, got %q", app.Healthcheck.Path)
	}
	if len(findings) == 0 {
		t.Fatal("a deploy must say what it decided")
	}
	if !strings.Contains(findingText(findings), "/app/data") {
		t.Error("the reported findings should name the folder that gets a disk")
	}
	if Blocking(findings) {
		t.Error("nothing here is a reason not to deploy")
	}
}

// What the user wrote is never overwritten — including "no disk at all".
func TestMergeRespectsTheUser(t *testing.T) {
	app := &config.AppConfig{
		Name:        "web",
		Port:        3000,
		Healthcheck: config.Healthcheck{Path: "/ping"},
		Storage:     &config.StorageConfig{Paths: []string{}},
		Addons:      []config.AddonConfig{{Type: "postgres", Name: "postgres"}},
	}
	app.ApplyDefaults()
	findings := mergeAnalysis(app, analysis())

	if app.HasStorage() {
		t.Error("an empty paths list means the user said no")
	}
	if app.Healthcheck.Path != "/ping" {
		t.Error("a configured health path must stand")
	}
	if app.Port != 3000 {
		t.Error("the port in donkey.yaml is never overwritten")
	}
	joined := findingText(findings)
	if !strings.Contains(joined, "port 3000") {
		t.Error("a port that disagrees with the code should be pointed out")
	}
	if strings.Contains(joined, "donkey addon add postgres") {
		t.Error("an addon the app already has should not be suggested")
	}
}

// A port mismatch is a warning, not a decision: it changes nothing and has to
// be visible as something to act on.
func TestPortMismatchIsAWarning(t *testing.T) {
	app := &config.AppConfig{Name: "web", Port: 3000}
	app.ApplyDefaults()
	for _, f := range mergeAnalysis(app, analysis()) {
		if f.Code != "port-mismatch" {
			continue
		}
		if f.Level != LevelWarn {
			t.Errorf("a port mismatch should warn, got level %q", f.Level)
		}
		if f.Fix == "" {
			t.Error("a warning the user can act on should say how")
		}
		return
	}
	t.Fatal("the port mismatch should be reported")
}

// No assistant, no changes.
func TestMergeWithoutAnalysis(t *testing.T) {
	app := &config.AppConfig{Name: "web", Port: 8000}
	app.ApplyDefaults()
	if findings := mergeAnalysis(app, nil); findings != nil {
		t.Fatalf("nothing to merge should report nothing, got %v", findings)
	}
	if app.Storage != nil {
		t.Error("nothing to merge should not invent storage")
	}
}
