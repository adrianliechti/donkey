package ai

import (
	"strings"
	"testing"
)

// The model's answer is a suggestion, not an instruction: anything donkey
// would refuse anyway is dropped before a caller ever sees it.
func TestSanitizeDropsUnusableVolumes(t *testing.T) {
	a := &Analysis{
		Port:            70000,
		HealthcheckPath: "healthz",
		Volumes: []VolumeSuggestion{
			{Path: "/app/uploads"},
			{Path: "/app/uploads/"}, // the same folder twice
			{Path: "/tmp/scratch"},
			{Path: "/var/log/app"},
			{Path: "/app/node_modules"},
			{Path: "relative/data"},
			{Path: "/"},
		},
		Services: []ServiceSuggestion{{Type: " Postgres "}, {Type: ""}},
	}
	a.sanitize()

	if len(a.Volumes) != 1 || a.Volumes[0].Path != "/app/uploads" {
		t.Fatalf("only the real data folder should survive, got %+v", a.Volumes)
	}
	if a.Port != 0 {
		t.Errorf("an impossible port should be dropped, got %d", a.Port)
	}
	if a.HealthcheckPath != "" {
		t.Errorf("a path that isn't a path should be dropped, got %q", a.HealthcheckPath)
	}
	if len(a.Services) != 1 || a.Services[0].Type != "postgres" {
		t.Errorf("service types should be normalized, got %+v", a.Services)
	}
}

// A host restriction that names no setting is unusable, so it never reaches a
// caller as if it were a finding.
func TestSanitizeDropsEmptyHostRestriction(t *testing.T) {
	a := &Analysis{HostRestriction: &HostRestriction{Setting: "  ", File: "settings.py"}}
	a.sanitize()

	if a.HostRestriction != nil {
		t.Errorf("a restriction that names no setting is unusable, got %+v", a.HostRestriction)
	}
}

func TestVolumePaths(t *testing.T) {
	a := &Analysis{Volumes: []VolumeSuggestion{{Path: "/a"}, {Path: "/b"}}}
	got := a.VolumePaths()
	if len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("VolumePaths() = %v", got)
	}
}

// Every service donkey can provision has to be named in the analysis
// contract; otherwise it exists in the catalog but can never appear in a
// source-derived deploy plan.
func TestAnalyzePromptOffersEveryService(t *testing.T) {
	for _, service := range []string{"postgres", "redis", "rustfs", "openai"} {
		if !strings.Contains(analyzeInstructions, `"`+service+`"`) {
			t.Errorf("analysis instructions do not offer %s", service)
		}
	}
}
