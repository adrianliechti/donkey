package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

type openAI struct {
	client openai.Client
	model  string
}

// newOpenAI builds the client. The SDK honors OPENAI_BASE_URL from the
// environment, so local OpenAI-compatible servers work out of the box. The
// generous-but-finite timeout keeps a dead server from stalling a deploy —
// AI failures always fall back to defaults.
func newOpenAI(key, model string) *openAI {
	return &openAI{
		client: openai.NewClient(
			option.WithAPIKey(key),
			option.WithMaxRetries(1),
			option.WithRequestTimeout(2*time.Minute),
		),
		model: model,
	}
}

func (o *openAI) Enabled() bool { return true }

func (o *openAI) SuggestBuildConfig(ctx context.Context, sum *RepoSummary) (*BuildSuggestion, error) {
	var out BuildSuggestion
	if err := o.structured(ctx, suggestSystemPrompt, sum.Render(), "build_suggestion", suggestionSchema, &out); err != nil {
		return nil, err
	}
	out.Strategy = strings.ToLower(out.Strategy)
	if out.Strategy != "railpack" && out.Strategy != "dockerfile" {
		return nil, fmt.Errorf("the AI answer wasn't usable (unknown strategy %q)", out.Strategy)
	}
	return &out, nil
}

func (o *openAI) SuggestAppSettings(ctx context.Context, sum *RepoSummary) (*AppSettings, error) {
	var out AppSettings
	if err := o.structured(ctx, settingsSystemPrompt, sum.Render(), "app_settings", settingsSchema, &out); err != nil {
		return nil, err
	}
	if out.Port < 0 || out.Port > 65535 {
		out.Port = 0
	}
	if out.HealthcheckPath != "" && !strings.HasPrefix(out.HealthcheckPath, "/") {
		out.HealthcheckPath = ""
	}
	return &out, nil
}

func (o *openAI) DiagnoseFailure(ctx context.Context, fc *FailureContext) (*FixProposal, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Failure stage: %s\n", fc.Stage)
	if fc.Detection != "" {
		fmt.Fprintf(&b, "Detected app: %s\n", fc.Detection)
	}
	if fc.DonkeyYAML != "" {
		fmt.Fprintf(&b, "\ndonkey.yaml:\n%s\n", fc.DonkeyYAML)
	}
	for _, e := range fc.Events {
		fmt.Fprintf(&b, "Cluster event: %s\n", e)
	}
	if fc.Logs != "" {
		logs := fc.Logs
		if len(logs) > 15*1024 {
			logs = logs[len(logs)-15*1024:]
		}
		fmt.Fprintf(&b, "\nLogs (tail):\n%s\n", logs)
	}
	var out FixProposal
	if err := o.structured(ctx, diagnoseSystemPrompt, b.String(), "fix_proposal", proposalSchema, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (o *openAI) structured(ctx context.Context, system, user, name string, schema map[string]any, out any) error {
	resp, err := o.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: shared.ChatModel(o.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   name,
					Strict: openai.Bool(true),
					Schema: schema,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("asking OpenAI didn't work: %w", err)
	}
	if len(resp.Choices) == 0 {
		return fmt.Errorf("OpenAI sent an empty answer")
	}
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), out); err != nil {
		return fmt.Errorf("the AI answer wasn't usable: %w", err)
	}
	return nil
}
