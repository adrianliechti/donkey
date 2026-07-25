// Package ai is donkey's assistant, and donkey requires it. It runs on every
// look at an app: it reads the repository alongside donkey's own detector and
// reports what the app is, how it should be built, which port it listens on,
// which folders hold data that must survive a restart, and which services it
// expects. Without a reachable model there is no half-service and no silent
// fallback — donkey says what is missing and stops.
//
// It never applies changes on its own: the CLI confirms, and deploys only
// fill in what donkey.yaml left open.
package ai

import (
	"context"
	"fmt"
	"os"
)

type Assistant interface {
	// Check verifies the model answers at all — donkey requires it, so
	// `donkey doctor` reports it like any other dependency.
	Check(ctx context.Context) error
	// Analyze reads a repository (with donkey's own detection as evidence)
	// and describes how to run it.
	Analyze(ctx context.Context, in *AnalyzeInput) (*Analysis, error)
	// DiagnoseFailure explains a build/deploy failure and proposes fixes.
	DiagnoseFailure(ctx context.Context, fc *FailureContext) (*FixProposal, error)
}

// AnalyzeInput is what the assistant gets to look at.
type AnalyzeInput struct {
	Summary *RepoSummary `json:"summary"`
	// Detected is what donkey's own detector already worked out ("railpack:
	// node, python"), or a description of why it failed. The assistant is
	// asked to confirm or correct it rather than start from nothing.
	Detected string `json:"detected"`
	// DetectionFailed tells the assistant that donkey cannot build this app
	// on its own, so it must propose a Dockerfile or a railpack config.
	DetectionFailed bool `json:"detectionFailed"`
	// CurrentConfig is the existing donkey.yaml, if any.
	CurrentConfig string `json:"currentConfig"`
}

// Analysis is the assistant's full picture of an app.
type Analysis struct {
	AppType string `json:"appType"`
	// Strategy is "railpack", "dockerfile" or "" (keep what donkey detected);
	// the matching file content is only filled in when donkey needs it.
	Strategy       string `json:"strategy"`
	Dockerfile     string `json:"dockerfile"`
	RailpackConfig string `json:"railpackConfig"`
	// Port the app listens on; 0 means "it honors the PORT env var, any port
	// works".
	Port            int    `json:"port"`
	PortReason      string `json:"portReason"`
	HealthcheckPath string `json:"healthcheckPath"`
	// Volumes are folders inside the container whose contents must survive a
	// restart (uploads, databases on disk, generated content).
	Volumes []VolumeSuggestion `json:"volumes"`
	// Services are backing services the app expects (postgres, …).
	Services []ServiceSuggestion `json:"services"`
	// LocalURLs are addresses baked into the code that will not resolve to
	// what the author meant once the app runs on a cluster.
	LocalURLs []LocalURL `json:"localURLs"`
	Notes     string     `json:"notes"`
}

// LocalURL is a hardcoded address (localhost, 127.0.0.1, a dev port) the code
// tries to reach. Whether that is a mistake depends entirely on where the code
// runs, which is why the assistant is asked to say.
type LocalURL struct {
	URL  string `json:"url"`
	File string `json:"file"`
	// RunsIn is "browser" when the code ships to a browser as part of a
	// frontend bundle, "server" when it executes inside the container.
	// Browser code's localhost is the visitor's own machine — always wrong.
	// Server code's localhost is its own container — right only when the
	// thing it calls runs in that same container.
	RunsIn string `json:"runsIn"`
	Reason string `json:"reason"`
}

// VolumeSuggestion is one folder that needs to keep its contents.
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
	// KnownSecrets are values the caller knows are secret — the app's env
	// values and its addon binding values. DiagnoseFailure blanks every
	// occurrence of them (from 8 characters up) before anything is sent,
	// which catches what the patterns in Redact cannot: a password that
	// simply looks like a word, printed by a library in a shape nobody
	// anticipated. Leaving this empty is safe but weaker.
	KnownSecrets []string
}

type FixAction struct {
	// Type is edit_file | set_env | rerun.
	Type        string `json:"type"`
	Path        string `json:"path"`
	NewContent  string `json:"newContent"`
	Key         string `json:"key"`
	Value       string `json:"value"`
	Explanation string `json:"explanation"`
}

type FixProposal struct {
	Diagnosis string      `json:"diagnosis"`
	Actions   []FixAction `json:"actions"`
}

// New builds the assistant from the environment. It fails loudly: a donkey
// without an assistant cannot read apps, so this is a setup error to fix, not
// a mode to run in.
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
