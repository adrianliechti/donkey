package apiclient

import (
	"context"

	"monobox.ch/donkey/internal/ai"
)

// Assistant adapts the server's AI endpoints to the ai.Assistant interface
// the init flow uses — the CLI itself never talks to OpenAI.
type Assistant struct{ c *Client }

func (c *Client) Assistant() *Assistant { return &Assistant{c} }

func (a *Assistant) Enabled() bool { return true }

func (a *Assistant) SuggestBuildConfig(ctx context.Context, sum *ai.RepoSummary) (*ai.BuildSuggestion, error) {
	return a.c.SuggestBuildConfig(ctx, sum)
}

func (a *Assistant) SuggestAppSettings(ctx context.Context, sum *ai.RepoSummary) (*ai.AppSettings, error) {
	return a.c.SuggestAppSettings(ctx, sum)
}

func (a *Assistant) DiagnoseFailure(ctx context.Context, fc *ai.FailureContext) (*ai.FixProposal, error) {
	// Failure diagnosis happens server-side during deploys.
	return nil, nil
}
