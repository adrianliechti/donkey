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

// What donkey.yaml doesn't say, the assistant may fill in.
func TestMergeFillsWhatIsOpen(t *testing.T) {
	app := &config.AppConfig{Name: "web", Port: 8000}
	app.ApplyDefaults()
	lines := mergeAnalysis(app, analysis())

	if !app.HasStorage() || app.Storage.Paths[0] != "/app/data" {
		t.Fatalf("volume suggestion should become storage: %+v", app.Storage)
	}
	if app.Storage.Size != config.DefaultStorage {
		t.Errorf("storage should get the default size, got %q", app.Storage.Size)
	}
	if app.Healthcheck.Path != "/healthz" {
		t.Errorf("health path should be adopted, got %q", app.Healthcheck.Path)
	}
	if len(lines) == 0 {
		t.Fatal("a deploy must say what it decided")
	}
	if !strings.Contains(strings.Join(lines, "\n"), "/app/data") {
		t.Error("the reported lines should name the folder that gets a disk")
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
	lines := mergeAnalysis(app, analysis())

	if app.HasStorage() {
		t.Error("an empty paths list means the user said no")
	}
	if app.Healthcheck.Path != "/ping" {
		t.Error("a configured health path must stand")
	}
	if app.Port != 3000 {
		t.Error("the port in donkey.yaml is never overwritten")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "port 3000") {
		t.Error("a port that disagrees with the code should be pointed out")
	}
	if strings.Contains(joined, "donkey addon add postgres") {
		t.Error("an addon the app already has should not be suggested")
	}
}

// No assistant, no changes.
func TestMergeWithoutAnalysis(t *testing.T) {
	app := &config.AppConfig{Name: "web", Port: 8000}
	app.ApplyDefaults()
	if lines := mergeAnalysis(app, nil); lines != nil {
		t.Fatalf("nothing to merge should report nothing, got %v", lines)
	}
	if app.Storage != nil {
		t.Error("nothing to merge should not invent storage")
	}
}
