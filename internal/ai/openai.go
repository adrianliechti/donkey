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
// caller owns cancellation: model reasoning may legitimately take longer
// than an HTTP request timeout.
func newOpenAI(key, model string) *openAI {
	return &openAI{
		client: openai.NewClient(
			option.WithAPIKey(key),
			option.WithMaxRetries(1),
			option.WithRequestTimeout(0),
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
		fmt.Fprintf(&b, "donkey's detector says: %s\n\n", in.Detected)
	}
	if in.CurrentConfig != "" {
		fmt.Fprintf(&b, "Existing donkey.yaml:\n%s\n\n", in.CurrentConfig)
	}
	if in.Summary != nil {
		b.WriteString(in.Summary.Render())
	}

	var out Analysis
	// With the app's folder to hand, the port, the volumes and the health
	// route come from reading the code rather than from recognizing the shape
	// of the project.
	if in.Root != "" {
		history := o.investigate(ctx, in.Root, analyzeInstructions,
			b.String()+"\n"+analyzeExploreInstruction, maxExploreTurns)
		history = append(history, responses.ResponseInputItemParamOfMessage(
			analyzeConcludeInstruction, responses.EasyInputMessageRoleUser))
		if err := o.structuredWith(ctx, analyzeInstructions, history, "app_analysis", analysisSchema, &out); err != nil {
			// Exploration or the answer call failed — the one-shot read below
			// is weaker but still an answer.
			out = Analysis{}
		}
	}
	if out.AppType == "" {
		if err := o.structured(ctx, analyzeInstructions, b.String(), "app_analysis", analysisSchema, &out); err != nil {
			return nil, err
		}
	}
	out.sanitize()
	return &out, nil
}

// WriteDockerfile explores before answering: a Dockerfile is a claim about a
// dozen specific facts — install command, output directory, the app object's
// module path — and each is written down somewhere in the repository.
//
// On a retry that exploration is where the work happens. "No module named X"
// is not an abstract problem; it is an instruction to go and look at what
// imports X.
func (o *openAI) WriteDockerfile(ctx context.Context, in *DockerfileInput) (*DockerfileResult, error) {
	instructions := extendedDockerfileInstructions(in.Instructions)
	var b strings.Builder
	if in.Port > 0 {
		fmt.Fprintf(&b, "donkey will set PORT=%d.\n\n", in.Port)
	}
	if in.Runtime != "" {
		fmt.Fprintf(&b, "Reviewed runtime contract:\n%s\n\n", in.Runtime)
	}
	if in.AllowEdits {
		b.WriteString("You may use edit to repair deployment compatibility in this disposable workspace. Read before editing; preserve the application's behavior. Every edit is recorded.\n\n")
	}
	// The failures go first: on a retry they are the only thing that matters,
	// and burying them under a file tree is how a model misses them.
	if n := len(in.Attempts); n > 0 {
		fmt.Fprintf(&b, "%d Dockerfile(s) have already been tried for this app and none produced a working image.\n"+
			"Read every failure before you write anything: they are the app telling you what it needs.\n"+
			"Do not repeat a fix that has already been tried, and do not undo one that worked.\n\n", n)
		for i, at := range in.Attempts {
			fmt.Fprintf(&b, "── Attempt %d of %d ──\n", i+1, n)
			if at.Dockerfile != "" {
				fmt.Fprintf(&b, "Dockerfile:\n%s\n", at.Dockerfile)
			}
			if at.Dockerignore != "" {
				fmt.Fprintf(&b, "Dockerignore:\n%s\n", at.Dockerignore)
			}
			if at.Stage != "" {
				fmt.Fprintf(&b, "Failure stage: %s\n", at.Stage)
			}
			fmt.Fprintf(&b, "What happened:\n%s\n\n", at.Failure)
		}
	}
	if in.Summary != nil {
		b.WriteString(in.Summary.Render())
	}

	var out DockerfileResult
	var filesRead []string
	var changes []SourceChange
	task := RedactEvidence(b.String(), in.KnownSecrets...)
	if in.Root != "" {
		history := o.investigate(ctx, in.Root, instructions,
			task+"\n"+dockerfileExploreInstruction, maxBuildExploreTurns,
			exploreOptions{Writable: in.AllowEdits, Progress: in.Progress, Changes: &changes, KnownSecrets: in.KnownSecrets})
		filesRead = exploredFiles(history)
		history = append(history, responses.ResponseInputItemParamOfMessage(
			dockerfileConcludeInstruction, responses.EasyInputMessageRoleUser))
		if err := o.structuredWith(ctx, instructions, history, "dockerfile", dockerfileSchema, &out); err != nil {
			// Keep the investigation and edit results on a transient failure.
			// Falling back to the original summary would describe stale source.
			out = DockerfileResult{}
			if err := o.structuredWith(ctx, instructions, history, "dockerfile", dockerfileSchema, &out); err != nil {
				return nil, err
			}
		}
	}
	if in.Root == "" {
		if err := o.structured(ctx, instructions, task, "dockerfile", dockerfileSchema, &out); err != nil {
			return nil, err
		}
	}
	// Models reach for a fence even when told not to; stripping it is cheaper
	// than a build that fails on line one.
	df := strings.TrimSpace(stripCodeFence(out.Dockerfile))
	if df == "" {
		return nil, fmt.Errorf("the assistant returned no Dockerfile")
	}
	out.Dockerfile = df + "\n"
	if di := strings.TrimSpace(stripCodeFence(out.Dockerignore)); di != "" {
		out.Dockerignore = di + "\n"
	} else {
		out.Dockerignore = ""
	}
	out.FilesRead = filesRead
	out.Changes = changes
	return &out, nil
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
		history := o.investigate(ctx, fc.Root, diagnoseInstructions, b.String()+"\n"+exploreInstruction, maxExploreTurns)
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

// structuredWith answers over an exchange that already happened. The schema
// is provider-enforced. Read the final message: OutputText also includes
// commentary, which may precede the structured answer.
func (o *openAI) structuredWith(ctx context.Context, instructions string, input []responses.ResponseInputItemUnionParam, name string, schema map[string]any, out any) error {
	params := responses.ResponseNewParams{
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
	}
	resp, err := o.client.Responses.New(ctx, params)
	if err != nil {
		return fmt.Errorf("asking OpenAI didn't work: %w", err)
	}
	text, err := answerText(resp)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("the AI answer wasn't usable: %w", err)
	}
	return nil
}

func answerText(resp *responses.Response) (string, error) {
	if resp.Status != "" && resp.Status != "completed" {
		return "", fmt.Errorf("OpenAI didn't finish its answer (status %s)", resp.Status)
	}
	final := false
	for _, item := range resp.Output {
		final = final || item.Type == "message" && item.Phase == "final_answer"
	}
	var text string
	for _, item := range resp.Output {
		if item.Type != "message" || item.Phase == "commentary" || final && item.Phase != "final_answer" {
			continue
		}
		for _, content := range item.Content {
			if content.Type == "refusal" {
				return "", fmt.Errorf("OpenAI declined to produce this answer")
			}
			if content.Type == "output_text" {
				if text != "" {
					return "", fmt.Errorf("OpenAI sent more than one answer block")
				}
				text = content.Text
			}
		}
	}
	if text == "" {
		return "", fmt.Errorf("OpenAI sent an empty answer")
	}
	return text, nil
}
