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
	"github.com/openai/openai-go/v3/responses"
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
	_, err := o.client.Responses.New(ctx, responses.ResponseNewParams{
		Model:           shared.ResponsesModel(o.model),
		Input:           responses.ResponseNewParamsInputUnion{OfString: openai.String("ping")},
		MaxOutputTokens: openai.Int(16),
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
	if err := o.structured(ctx, analyzeInstructions, b.String(), "app_analysis", analysisSchema, &out); err != nil {
		return nil, err
	}
	out.sanitize()
	if in.DetectionFailed && out.Strategy != "railpack" && out.Strategy != "dockerfile" {
		return nil, fmt.Errorf("the AI assistant couldn't propose a way to build this app")
	}
	return &out, nil
}

// WriteDockerfile asks for the Dockerfile donkey's own builder builds.
func (o *openAI) WriteDockerfile(ctx context.Context, in *DockerfileInput) (string, error) {
	var b strings.Builder
	if in.Port > 0 {
		fmt.Fprintf(&b, "donkey will set PORT=%d.\n\n", in.Port)
	}
	// The failure goes first: on a retry it is the only thing that matters,
	// and burying it under a file tree is how a model misses it.
	if in.Failure != "" {
		fmt.Fprintf(&b, "Your previous Dockerfile did not work. What happened:\n%s\n\n", in.Failure)
		if in.Previous != "" {
			fmt.Fprintf(&b, "The Dockerfile that produced it:\n%s\n\n", in.Previous)
		}
	}
	if in.Summary != nil {
		b.WriteString(in.Summary.Render())
	}

	var out struct {
		Dockerfile string `json:"dockerfile"`
		Notes      string `json:"notes"`
	}
	if err := o.structured(ctx, dockerfileInstructions, b.String(), "dockerfile", dockerfileSchema, &out); err != nil {
		return "", err
	}
	// Models reach for a fence even when told not to; stripping it is cheaper
	// than a build that fails on line one.
	df := strings.TrimSpace(stripCodeFence(out.Dockerfile))
	if df == "" {
		return "", fmt.Errorf("the assistant returned no Dockerfile")
	}
	return df + "\n", nil
}

// stripCodeFence removes a ```dockerfile … ``` wrapper if one came back.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	if _, rest, ok := strings.Cut(t, "\n"); ok {
		t = rest
	}
	if i := strings.LastIndex(t, "```"); i >= 0 {
		t = t[:i]
	}
	return t
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

	if h := a.HostRestriction; h != nil {
		h.Setting = strings.TrimSpace(h.Setting)
		h.EnvVar = strings.TrimSpace(h.EnvVar)
		if h.Setting == "" {
			a.HostRestriction = nil
		}
	}
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
	// With the app's folder to hand, the model reads the code before it
	// answers instead of proposing a fix for source it has never seen.
	var out FixProposal
	if fc.Root != "" {
		history := o.investigate(ctx, fc.Root, diagnoseInstructions, b.String()+"\n"+exploreInstruction)
		history = append(history, responses.ResponseInputItemParamOfMessage(concludeInstruction, responses.EasyInputMessageRoleUser))
		if err := o.structuredWith(ctx, diagnoseInstructions, history, "fix_proposal", proposalSchema, &out); err == nil {
			return &out, nil
		}
		// Exploration or the answer call failed — fall through to the plain
		// one-shot diagnosis rather than leaving the user with nothing.
		out = FixProposal{}
	}
	if err := o.structured(ctx, diagnoseInstructions, b.String(), "fix_proposal", proposalSchema, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (o *openAI) structured(ctx context.Context, instructions, user, name string, schema map[string]any, out any) error {
	return o.structuredWith(ctx, instructions, []responses.ResponseInputItemUnionParam{
		responses.ResponseInputItemParamOfMessage(user, responses.EasyInputMessageRoleUser),
	}, name, schema, out)
}

// structuredWith is the same, over an exchange that already happened — the
// tool-using investigation, whose findings the answer is drawn from.
//
// The schema is enforced by the provider (strict), so what comes back is
// either valid against it or an error; donkey never parses prose into a
// struct and hopes.
func (o *openAI) structuredWith(ctx context.Context, instructions string, input []responses.ResponseInputItemUnionParam, name string, schema map[string]any, out any) error {
	resp, err := o.client.Responses.New(ctx, responses.ResponseNewParams{
		Model:        shared.ResponsesModel(o.model),
		Instructions: openai.String(instructions),
		Input:        responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		Text: responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfJSONSchema: &responses.ResponseFormatTextJSONSchemaConfigParam{
					Name:   name,
					Schema: schema,
					Strict: openai.Bool(true),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("asking OpenAI didn't work: %w", err)
	}
	text := resp.OutputText()
	if text == "" {
		return fmt.Errorf("OpenAI sent an empty answer")
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("the AI answer wasn't usable: %w", err)
	}
	return nil
}
