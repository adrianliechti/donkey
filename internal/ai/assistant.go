// Package ai is donkey's assistant, and donkey requires it: it reads every
// app and reports what it is, how to build it, which port it binds, which
// folders must survive a restart. Without a reachable model donkey stops
// rather than half-reading an app. Planning is read-only; build repairs may
// edit an isolated workspace and return the resulting source patches.
package ai

import (
	"context"
	"fmt"
	"os"
	"time"
)

type Assistant interface {
	Check(ctx context.Context) error
	Analyze(ctx context.Context, in *AnalyzeInput) (*Analysis, error)
	DiagnoseFailure(ctx context.Context, fc *FailureContext) (*FixProposal, error)
	// WriteDockerfile is also how the builder recovers: failed Dockerfiles and
	// their failures come back in, a corrected one goes out.
	WriteDockerfile(ctx context.Context, in *DockerfileInput) (*DockerfileResult, error)
	// TriageVulnerabilities reads the app to say, per scanner finding,
	// whether this app is actually affected. It is the one reading that may
	// let a refused image through (see Verdict.Waives), so it only trusts
	// itself when it has read the source.
	TriageVulnerabilities(ctx context.Context, in *TriageInput) (*Triage, error)
}

// TriageInput is what the assistant needs to judge scanner findings.
type TriageInput struct {
	// Root is the build workspace the image was built from (or a checkout
	// of it). Empty means the assistant answers from the findings alone —
	// and then never with enough confidence to waive anything.
	Root string
	// AppDir is the app's folder inside Root, when the workspace holds more
	// than the app (a monorepo); empty means Root is the app.
	AppDir string
	// Recipe is the Dockerfile that produced the image.
	Recipe string
	// Exposure is how the app is reached: public, protected or internal.
	Exposure string
	Findings []TriageFinding
	// KnownSecrets are blanked from everything the assistant says back.
	KnownSecrets []string
	Progress     func(string)
}

// TriageFinding is one scanner finding, as the assistant sees it.
type TriageFinding struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Package   string `json:"package"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed,omitempty"`
	// Class is os-pkgs or lang-pkgs; Type the ecosystem; Path the package
	// file inside the image, when the scanner named one.
	Class       string `json:"class"`
	Type        string `json:"type"`
	Path        string `json:"path,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// What a verdict can say, and how sure it is.
const (
	VerdictAffected    = "affected"
	VerdictNotAffected = "not-affected"
	VerdictUnsure      = "unsure"

	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// Verdict is the assistant's reading of one finding.
type Verdict struct {
	ID         string `json:"id"`
	Verdict    string `json:"verdict"`
	Confidence string `json:"confidence"`
	// Reason is one to three sentences a developer can check.
	Reason string `json:"reason"`
	// Evidence names the files the reason rests on.
	Evidence []string `json:"evidence"`
	// Fix is the one change that makes the finding go away, when known.
	Fix string `json:"fix,omitempty"`
}

// Waives says whether this verdict alone may lift a block: only a
// high-confidence "not affected" does, and only one that came from reading
// the source (see TriageVulnerabilities).
func (v Verdict) Waives() bool {
	return v.Verdict == VerdictNotAffected && v.Confidence == ConfidenceHigh
}

// Triage is the set of verdicts, one per finding asked about.
type Triage struct {
	Verdicts []Verdict `json:"verdicts"`
}

// DockerfileInput is what the assistant needs to write an app's Dockerfile.
type DockerfileInput struct {
	Summary *RepoSummary `json:"summary"`
	// Root lets the assistant read the app itself before writing anything —
	// the vite config's output directory, a Django project's module path, the
	// workspace package actually being deployed. Empty falls back to the
	// summary alone.
	Root string `json:"root"`
	// Port is what donkey will tell the app to listen on, through PORT.
	Port int `json:"port"`
	// Instructions are installation-specific operator guidance appended to
	// donkey's built-in builder instructions.
	Instructions string `json:"instructions,omitempty"`
	// Attempts is every Dockerfile already tried, oldest first. The whole
	// history goes back, not just the last: with only one attempt to look at,
	// a model oscillates between two broken variants.
	Attempts []DockerfileAttempt `json:"attempts"`
	// Runtime is the reviewed deployment contract. Secret values are removed
	// before it reaches the model; variable names remain useful evidence.
	Runtime      string   `json:"runtime,omitempty"`
	KnownSecrets []string `json:"-"`
	// AllowEdits is only enabled for an isolated, disposable build workspace.
	AllowEdits bool         `json:"-"`
	Progress   func(string) `json:"-"`
	// CheckImage supplies current registry and vulnerability evidence.
	CheckImage func(context.Context, string) (string, error) `json:"-"`
}

type DockerfileAttempt struct {
	Dockerfile   string `json:"dockerfile"`
	Dockerignore string `json:"dockerignore,omitempty"`
	Stage        string `json:"stage,omitempty"`
	Failure      string `json:"failure"`
}

type DockerfileResult struct {
	Dockerfile string `json:"dockerfile"`
	// Dockerignore is used only when the app ships none of its own.
	Dockerignore string `json:"dockerignore"`
	Notes        string `json:"notes"`
	// FilesRead is what the assistant opened, so the build log can show what
	// the answer was based on.
	FilesRead []string       `json:"filesRead"`
	Changes   []SourceChange `json:"changes,omitempty"`
}

// SourceChange records an edit made only to the disposable build workspace.
type SourceChange struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

// AnalyzeInput is what the assistant gets to look at.
type AnalyzeInput struct {
	Summary *RepoSummary `json:"summary"`
	// Root lets the assistant read the app. Nearly every Analysis field is a
	// question about source that a summary can only hint at.
	Root string `json:"root"`
	// Detected is what donkey worked out, for the assistant to confirm or
	// correct rather than start from nothing.
	Detected      string `json:"detected"`
	CurrentConfig string `json:"currentConfig"`
}

// Analysis is the assistant's full picture of an app.
type Analysis struct {
	AppType string `json:"appType"`
	// Port is 0 when the app honors PORT and any port works.
	Port       int    `json:"port"`
	PortReason string `json:"portReason"`
	// BindsLoopback: the app answers only itself — it starts, logs a URL, and
	// no probe or visitor ever reaches it.
	BindsLoopback   bool   `json:"bindsLoopback"`
	BindReason      string `json:"bindReason"`
	HealthcheckPath string `json:"healthcheckPath"`
	// Volumes are container folders whose contents must survive a restart.
	Volumes  []VolumeSuggestion  `json:"volumes"`
	Services []ServiceSuggestion `json:"services"`
	// LocalURLs are baked-in addresses that stop meaning what the author
	// intended once the app runs on a cluster.
	LocalURLs []LocalURL `json:"localURLs"`
	// HostRestriction: the app rejects unrecognized Host headers, so behind a
	// gateway it refuses every visitor while looking perfectly healthy.
	HostRestriction *HostRestriction `json:"hostRestriction"`
	Notes           string           `json:"notes"`
}

// HostRestriction is a host allowlist the app enforces itself. Django's
// ALLOWED_HOSTS is the canonical case: left empty, it 400s every request.
type HostRestriction struct {
	Setting string `json:"setting"`
	File    string `json:"file"`
	// EnvVar overrides it without editing code, where the framework has one.
	EnvVar string `json:"envVar"`
	Reason string `json:"reason"`
}

// LocalURL is a hardcoded address the code tries to reach. Whether that is a
// mistake depends on where the code runs, which is why RunsIn exists.
type LocalURL struct {
	URL  string `json:"url"`
	File string `json:"file"`
	// RunsIn "browser": localhost is the visitor's machine — always wrong.
	// "server": localhost is the container — right only for a sidecar.
	RunsIn string `json:"runsIn"`
	Reason string `json:"reason"`
}

type VolumeSuggestion struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type ServiceSuggestion struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// VolumePaths returns just the paths, for writing into donkey.yaml.
func (a *Analysis) VolumePaths() []string {
	var out []string
	for _, v := range a.Volumes {
		out = append(out, v.Path)
	}
	return out
}

type FailureContext struct {
	// Stage is build | push | deploy | rollout.
	Stage      string
	Logs       string
	Events     []string
	DonkeyYAML string
	// Detection describes what donkey detected (providers, strategy).
	Detection string
	// KnownSecrets are values the caller knows are secret. They are blanked
	// literally, which catches what Redact's patterns cannot: a password that
	// simply looks like a word. Empty is safe but weaker.
	KnownSecrets []string
	// Root is the uploaded copy of the failed app, never the user's working
	// tree. Given one, the assistant reads the code before proposing anything.
	Root string
}

type FixAction struct {
	// Type is edit_file | set_env | rerun.
	Type        string `json:"type"`
	Path        string `json:"path"`
	NewContent  string `json:"newContent"`
	Key         string `json:"key"`
	Value       string `json:"value"`
	Explanation string `json:"explanation"`
	// Diff is computed by donkey from the two texts; the model is never asked
	// for one, since a hand-written diff cannot be trusted to apply.
	Diff string `json:"diff,omitempty"`
}

type FixProposal struct {
	Diagnosis string      `json:"diagnosis"`
	Actions   []FixAction `json:"actions"`
}

// New fails loudly: a donkey without an assistant cannot read apps.
func New(model string) (Assistant, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("donkey's assistant needs an API key — set ai.apiKey (or the OPENAI_API_KEY environment variable) in the install settings and run `donkey server install` again")
	}
	if model == "" {
		return nil, fmt.Errorf("donkey's assistant needs a model — set ai.model in the install settings")
	}
	return newOpenAI(key, model), nil
}

// Reachable proves endpoint, key and model work together. Called at install
// and again at startup — the alternative is a server that looks healthy and
// fails on its first deploy.
func Reachable(ctx context.Context, model string) error {
	a, err := New(model)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.Check(ctx); err != nil {
		endpoint := os.Getenv("OPENAI_BASE_URL")
		if endpoint == "" {
			endpoint = "the OpenAI API"
		}
		return fmt.Errorf("donkey's assistant is not answering at %s: %w\n"+
			"donkey reads every app with it, so nothing can be deployed until this works. Check that the\n"+
			"endpoint is running and reachable from here, and that ai.model (%q) is a model it serves",
			endpoint, err, model)
	}
	return nil
}
