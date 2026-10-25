package ai

import (
	"strings"
	"testing"
)

// The answer is kept honest against the question: one verdict per finding
// asked about, known values only, secrets blanked, and no waiving
// confidence unless the source was actually read.
func TestTriageSanitize(t *testing.T) {
	in := &TriageInput{
		Findings:     []TriageFinding{{ID: "CVE-1"}, {ID: "CVE-2"}},
		KnownSecrets: []string{"hunter2-secret-value"},
	}
	answer := Triage{Verdicts: []Verdict{
		{ID: "CVE-1", Verdict: VerdictNotAffected, Confidence: ConfidenceHigh, Reason: "Never imported; the db password hunter2-secret-value is unrelated."},
		{ID: "CVE-9", Verdict: VerdictAffected, Confidence: ConfidenceHigh, Reason: "nobody asked about this one"},
	}}

	out := answer.sanitize(in, true)
	if len(out.Verdicts) != 2 || out.Verdicts[0].ID != "CVE-1" || out.Verdicts[1].ID != "CVE-2" {
		t.Fatalf("verdicts = %+v", out.Verdicts)
	}
	if !out.Verdicts[0].Waives() {
		t.Error("a high-confidence not-affected verdict from an exploration waives")
	}
	if strings.Contains(out.Verdicts[0].Reason, "hunter2") {
		t.Errorf("a known secret survived in the reason: %q", out.Verdicts[0].Reason)
	}
	if v := out.Verdicts[1]; v.Verdict != VerdictUnsure || v.Confidence != ConfidenceLow || v.Evidence == nil {
		t.Errorf("a finding without a verdict must come back unsure: %+v", v)
	}

	blind := answer.sanitize(in, false)
	if blind.Verdicts[0].Waives() || blind.Verdicts[0].Confidence != ConfidenceMedium {
		t.Errorf("without reading the source, confidence is capped: %+v", blind.Verdicts[0])
	}

	bogus := Triage{Verdicts: []Verdict{{ID: "CVE-1", Verdict: "fine", Confidence: "certain"}}}
	if v := bogus.sanitize(in, true).Verdicts[0]; v.Verdict != VerdictUnsure || v.Confidence != ConfidenceLow {
		t.Errorf("unknown values must not be trusted: %+v", v)
	}
}

func TestTriageTask(t *testing.T) {
	task := triageTask(&TriageInput{
		AppDir: "services/api", Exposure: "public", Recipe: "FROM node:22-alpine\n",
		Findings: []TriageFinding{{ID: "CVE-2019-10744", Severity: "critical", Package: "lodash", Installed: "4.17.11", Fixed: "4.17.12", Class: "lang-pkgs", Type: "node-pkg", Path: "app/node_modules/lodash/package.json", Title: "prototype pollution"}},
	})
	for _, want := range []string{"services/api", "reached: public", "FROM node:22-alpine", "1. CVE-2019-10744 — lodash 4.17.11 (fixed in 4.17.12)", "critical; kind: lang-pkgs/node-pkg", "app/node_modules/lodash/package.json", "prototype pollution"} {
		if !strings.Contains(task, want) {
			t.Errorf("task lacks %q:\n%s", want, task)
		}
	}
}
