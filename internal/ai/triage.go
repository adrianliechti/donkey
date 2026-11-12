package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3/responses"
)

// A scanner proves a vulnerable version is present in the image. Whether
// the app is affected depends on what the code does with it — which is a
// reading of the source, the kind of job the assistant already does for
// ports, health routes and failures. The verdict can lift a refusal, so the
// rules below err towards "unsure", and a verdict given without reading the
// source is capped so it can never waive on its own.
func (o *openAI) TriageVulnerabilities(ctx context.Context, in *TriageInput) (*Triage, error) {
	if in == nil || len(in.Findings) == 0 {
		return &Triage{Verdicts: []Verdict{}}, nil
	}
	task := triageTask(in)
	var out Triage
	if in.Root != "" {
		history := o.investigate(ctx, in.Root, triageInstructions, task+"\n"+triageExploreInstruction, maxExploreTurns,
			exploreOptions{Progress: in.Progress, KnownSecrets: in.KnownSecrets})
		history = append(history, responses.ResponseInputItemParamOfMessage(triageConcludeInstruction, responses.EasyInputMessageRoleUser))
		if err := o.structuredWith(ctx, triageInstructions, history, "triage", triageSchema, &out); err == nil {
			return out.sanitize(in, true), nil
		}
		out = Triage{}
	}
	if err := o.structured(ctx, triageInstructions, task, "triage", triageSchema, &out); err != nil {
		return nil, err
	}
	return out.sanitize(in, false), nil
}

// triageTask lays out what the scanner found, one finding per block, with
// the recipe and the app's exposure for context.
func triageTask(in *TriageInput) string {
	var b strings.Builder
	if in.AppDir != "" && in.AppDir != "." {
		fmt.Fprintf(&b, "The app lives in %s inside the workspace; the rest is shared packages and siblings.\n", in.AppDir)
	}
	if in.Exposure != "" {
		fmt.Fprintf(&b, "How the app is reached: %s\n", in.Exposure)
	}
	if in.Recipe != "" {
		recipe := in.Recipe
		if len(recipe) > 8*1024 {
			recipe = recipe[:8*1024] + "\n…"
		}
		fmt.Fprintf(&b, "\nThe Dockerfile the image was built from:\n%s\n", recipe)
	}
	fmt.Fprintf(&b, "\nThe scanner found %d finding(s) that would refuse this deploy:\n", len(in.Findings))
	for i, f := range in.Findings {
		fmt.Fprintf(&b, "\n%d. %s — %s %s", i+1, f.ID, f.Package, f.Installed)
		if f.Fixed != "" {
			fmt.Fprintf(&b, " (fixed in %s)", f.Fixed)
		}
		fmt.Fprintf(&b, "\n   severity: %s; kind: %s/%s", f.Severity, f.Class, f.Type)
		if f.Path != "" {
			fmt.Fprintf(&b, "; in the image at %s", f.Path)
		}
		if f.Title != "" {
			fmt.Fprintf(&b, "\n   %s", f.Title)
		}
		if f.Description != "" {
			fmt.Fprintf(&b, "\n   %s", f.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// sanitize keeps the answer honest against the input: one verdict per
// finding asked about, known values only, secrets blanked, and no
// waiving confidence without the source having been read.
func (t *Triage) sanitize(in *TriageInput, explored bool) *Triage {
	byID := map[string]Verdict{}
	for _, v := range t.Verdicts {
		byID[v.ID] = v
	}
	known := usableSecrets(in.KnownSecrets)
	out := &Triage{Verdicts: make([]Verdict, 0, len(in.Findings))}
	for _, f := range in.Findings {
		v, ok := byID[f.ID]
		if !ok {
			v = Verdict{ID: f.ID, Verdict: VerdictUnsure, Confidence: ConfidenceLow, Reason: "The assistant gave no verdict for this finding."}
		}
		switch v.Verdict {
		case VerdictAffected, VerdictNotAffected, VerdictUnsure:
		default:
			v.Verdict = VerdictUnsure
		}
		switch v.Confidence {
		case ConfidenceHigh, ConfidenceMedium, ConfidenceLow:
		default:
			v.Confidence = ConfidenceLow
		}
		if !explored && v.Confidence == ConfidenceHigh {
			v.Confidence = ConfidenceMedium
		}
		v.Reason = Redact(replaceKnown(v.Reason, known))
		v.Fix = Redact(replaceKnown(v.Fix, known))
		if v.Evidence == nil {
			v.Evidence = []string{}
		}
		out.Verdicts = append(out.Verdicts, v)
	}
	return out
}

const triageInstructions = `You help a deployment tool called donkey decide whether an application is
affected by vulnerabilities a scanner found in the container image it was
just built into. The scanner proves a vulnerable package version is PRESENT
in the image. You judge whether THIS app is actually AFFECTED, which depends
on what the code does with that package.

For every finding, answer:
- verdict: "affected", "not-affected" or "unsure".
- confidence: "high", "medium" or "low".
- reason: one to three plain sentences a developer can check.
- evidence: the files (with line numbers where useful) the reason rests on.
- fix: the one change that makes the finding go away — "bump X to Y in
  <file>", "use a newer tag of the base image", "drop the package" — or ""
  when you do not know one.

Judge like a careful security reviewer:
- "affected": the package is imported, loaded or executed at runtime, or the
  vulnerable functionality is reachable from input the app handles. A package
  on a request path counts even when you cannot prove the exact vulnerable
  function is called.
- "not-affected" needs evidence you read: a build-only tool left in the image
  that nothing executes at runtime; a dependency the code never imports (you
  searched); an OS binary that neither the start command nor the code
  invokes; a vulnerable feature the code demonstrably does not use (you read
  the call sites). Name what you read.
- "unsure" whenever the evidence is thin. A not-affected verdict with high
  confidence lets through a deploy the policy would otherwise refuse, so it
  must rest on files you actually read, not on plausibility.
- OS libraries such as openssl, libc or zlib are used by anything linked
  against them; treat language runtimes and web servers as linked. For such
  a package "not-affected" is rare and needs a specific reason, for example
  that the weakness is in a command-line tool the image never runs.
- Exposure matters for weight, not for the verdict: a public app is reachable
  by anyone, a protected one only by signed-in members, an internal one only
  by its own space.

Confidence "high" means you would sign off on it personally.`

const triageExploreInstruction = `
You can read this app before answering, using the tools available to you:
read, glob and grep. They are read-only and limited to this app's folder.

Work from the package outwards:
- grep for its import, require or use; read the files that use it.
- Read the Dockerfile stages: is the package even in the runtime stage, and
  what does the start command run?
- For an OS package, decide what in the image links against or executes it.
- Stop when you can state the verdict with its evidence. A few well-chosen
  reads beat listing everything.`

const triageConcludeInstruction = `Now give your verdicts, one per finding, in the order asked.
Every reason must name what you read. Where you read nothing that settles
it, say "unsure" rather than guessing.`

var triageSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"verdicts": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":         map[string]any{"type": "string"},
					"verdict":    map[string]any{"type": "string", "enum": []string{VerdictAffected, VerdictNotAffected, VerdictUnsure}},
					"confidence": map[string]any{"type": "string", "enum": []string{ConfidenceHigh, ConfidenceMedium, ConfidenceLow}},
					"reason":     map[string]any{"type": "string"},
					"evidence":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"fix":        map[string]any{"type": "string"},
				},
				"required":             []string{"id", "verdict", "confidence", "reason", "evidence", "fix"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"verdicts"},
	"additionalProperties": false,
}
