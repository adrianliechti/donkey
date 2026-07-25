package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
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
// generous-but-finite timeout keeps a dead endpoint from stalling a deploy
// forever — the failure is then reported, never silently papered over.
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

// Check sends a minimal request: enough to prove the endpoint, the key and
// the model name all work together. The token cap stays at 16 because
// providers reject smaller budgets.
func (o *openAI) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := o.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:     shared.ChatModel(o.model),
		Messages:  []openai.ChatCompletionMessageParamUnion{openai.UserMessage("ping")},
		MaxTokens: openai.Int(16),
	})
	if err != nil {
		return fmt.Errorf("%s did not answer: %w", o.model, err)
	}
	return nil
}

func (o *openAI) Analyze(ctx context.Context, in *AnalyzeInput) (*Analysis, error) {
	var b strings.Builder
	if in.Detected != "" {
		if in.DetectionFailed {
			fmt.Fprintf(&b, "donkey could NOT build this app on its own: %s\nPropose a Dockerfile or a railpack config.\n\n", in.Detected)
		} else {
			fmt.Fprintf(&b, "donkey's detector says: %s\n\n", in.Detected)
		}
	}
	if in.CurrentConfig != "" {
		fmt.Fprintf(&b, "Existing donkey.yaml:\n%s\n\n", in.CurrentConfig)
	}
	if in.Summary != nil {
		b.WriteString(in.Summary.Render())
	}

	var out Analysis
	if err := o.structured(ctx, analyzeSystemPrompt, b.String(), "app_analysis", analysisSchema, &out); err != nil {
		return nil, err
	}
	out.sanitize()
	if in.DetectionFailed && out.Strategy != "railpack" && out.Strategy != "dockerfile" {
		return nil, fmt.Errorf("the AI assistant couldn't propose a way to build this app")
	}
	return &out, nil
}

// sanitize drops answers donkey would refuse anyway, so callers can trust
// what they get: a model that invents /tmp volumes or a bare health path
// shouldn't be able to break a deploy.
func (a *Analysis) sanitize() {
	a.Strategy = strings.ToLower(strings.TrimSpace(a.Strategy))
	if a.Port < 0 || a.Port > 65535 {
		a.Port = 0
	}
	if a.HealthcheckPath != "" && !strings.HasPrefix(a.HealthcheckPath, "/") {
		a.HealthcheckPath = ""
	}
	volumes := a.Volumes[:0]
	seen := map[string]bool{}
	for _, v := range a.Volumes {
		p := path.Clean(strings.TrimSpace(v.Path))
		if !path.IsAbs(p) || p == "/" || seen[p] || excludedVolume(p) {
			continue
		}
		seen[p] = true
		v.Path = p
		volumes = append(volumes, v)
	}
	a.Volumes = volumes
	services := a.Services[:0]
	for _, s := range a.Services {
		s.Type = strings.ToLower(strings.TrimSpace(s.Type))
		if s.Type != "" {
			services = append(services, s)
		}
	}
	a.Services = services

	// A finding donkey can't attribute to a place in the code is noise, and
	// only these two run-in answers carry a meaning donkey knows how to
	// explain.
	urls := a.LocalURLs[:0]
	for _, u := range a.LocalURLs {
		u.URL = strings.TrimSpace(u.URL)
		u.RunsIn = strings.ToLower(strings.TrimSpace(u.RunsIn))
		if u.URL == "" || (u.RunsIn != "browser" && u.RunsIn != "server") {
			continue
		}
		urls = append(urls, u)
	}
	a.LocalURLs = urls

	// preDeploy commands run in the cluster, so they are held to what a
	// migration looks like: one line, no shell chaining, and nothing that
	// would sit there forever and block the deploy behind it.
	steps := a.PreDeploy[:0]
	seenCmd := map[string]bool{}
	for _, s := range a.PreDeploy {
		s.Command = strings.TrimSpace(s.Command)
		if s.Command == "" || seenCmd[s.Command] || len(s.Command) > 300 ||
			strings.ContainsAny(s.Command, "\n\r") || unsafePreDeploy(s.Command) {
			continue
		}
		seenCmd[s.Command] = true
		steps = append(steps, s)
	}
	a.PreDeploy = steps

	if h := a.HostRestriction; h != nil {
		h.Setting = strings.TrimSpace(h.Setting)
		h.EnvVar = strings.TrimSpace(h.EnvVar)
		if h.Setting == "" {
			a.HostRestriction = nil
		}
	}
}

// unsafePreDeploy rejects commands that are not the "run the migrations and
// exit" shape: shell operators (which turn one reviewed command into several
// unreviewed ones) and anything that serves rather than finishes.
func unsafePreDeploy(cmd string) bool {
	if strings.ContainsAny(cmd, "&|;`$><") {
		return true
	}
	fields := strings.Fields(strings.ToLower(cmd))
	for _, f := range fields {
		switch f {
		case "runserver", "serve", "start", "up", "watch", "tail", "sleep":
			return true
		}
	}
	return false
}

// excludedVolume rejects folders that must never become persistent storage:
// scratch space, caches and build output would only grow, and system paths
// would break the container.
func excludedVolume(p string) bool {
	for _, bad := range []string{"/tmp", "/var/tmp", "/dev", "/proc", "/sys", "/etc", "/usr", "/bin", "/sbin", "/lib", "/root", "/var/log"} {
		if p == bad || strings.HasPrefix(p, bad+"/") {
			return true
		}
	}
	base := path.Base(p)
	switch base {
	case "node_modules", "dist", "build", ".next", "__pycache__", ".venv", "venv", "target", "vendor", "cache", ".cache", "logs":
		return true
	}
	return false
}

func (o *openAI) DiagnoseFailure(ctx context.Context, fc *FailureContext) (*FixProposal, error) {
	// Logs and events are the one part of donkey's traffic to the model that
	// can contain real credentials — an app that cannot reach its database
	// prints the connection string it tried. They are scrubbed here, on the
	// way out, so that no caller can forget to do it. See redact.go.
	fc = fc.redact()

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
