// Package ai is donkey's optional OpenAI-backed assistant. Everything works
// without it; when active it helps in exactly two places: recognizing apps
// detection couldn't, and explaining/fixing failures. It never applies
// changes on its own — proposals go through diff + confirmation in the CLI.
package ai

import (
	"context"
	"os"
)

type Assistant interface {
	Enabled() bool
	// SuggestBuildConfig proposes how to build an unrecognized app.
	SuggestBuildConfig(ctx context.Context, sum *RepoSummary) (*BuildSuggestion, error)
	// SuggestAppSettings reads the source to determine runtime facts (the
	// port the app really listens on, a usable health path) for apps the
	// detector already recognized.
	SuggestAppSettings(ctx context.Context, sum *RepoSummary) (*AppSettings, error)
	// DiagnoseFailure explains a build/deploy failure and proposes fixes.
	DiagnoseFailure(ctx context.Context, fc *FailureContext) (*FixProposal, error)
}

type AppSettings struct {
	// Port the app listens on; 0 means "the app honors the PORT env var, any
	// port works".
	Port            int    `json:"port"`
	HealthcheckPath string `json:"healthcheckPath"`
	Reason          string `json:"reason"`
}

type BuildSuggestion struct {
	AppType string `json:"appType"`
	// Strategy is "railpack" (with RailpackConfig) or "dockerfile" (with Dockerfile).
	Strategy        string `json:"strategy"`
	Dockerfile      string `json:"dockerfile"`
	RailpackConfig  string `json:"railpackConfig"`
	Port            int    `json:"port"`
	StartCommand    string `json:"startCommand"`
	HealthcheckPath string `json:"healthcheckPath"`
	Notes           string `json:"notes"`
}

type FailureContext struct {
	// Stage is build | push | deploy | rollout.
	Stage      string
	Logs       string
	Events     []string
	DonkeyYAML string
	// Detection describes what donkey detected (providers, strategy).
	Detection string
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

// New returns the OpenAI assistant when allowed and configured, otherwise a
// disabled one. Call sites only ever check Enabled().
func New(allowed bool, model string) Assistant {
	key := os.Getenv("OPENAI_API_KEY")
	if !allowed || key == "" {
		return disabled{}
	}
	return newOpenAI(key, model)
}

// NewDisabled returns the inert assistant (used when AI is off or no server
// is reachable).
func NewDisabled() Assistant { return disabled{} }

type disabled struct{}

func (disabled) Enabled() bool { return false }
func (disabled) SuggestBuildConfig(context.Context, *RepoSummary) (*BuildSuggestion, error) {
	return nil, errDisabled
}
func (disabled) SuggestAppSettings(context.Context, *RepoSummary) (*AppSettings, error) {
	return nil, errDisabled
}
func (disabled) DiagnoseFailure(context.Context, *FailureContext) (*FixProposal, error) {
	return nil, errDisabled
}

type disabledError struct{}

func (disabledError) Error() string { return "AI assistance is not active" }

var errDisabled = disabledError{}
