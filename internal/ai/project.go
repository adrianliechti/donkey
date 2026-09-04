package ai

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3/responses"
)

// ProjectReader reads the repository as a whole before individual apps are
// analyzed. Libraries and workspace coordinators are not deployable apps.
type ProjectReader interface {
	ReadProject(context.Context, string) (*Project, error)
}

type Project struct {
	Apps        []ProjectApp        `json:"apps"`
	Connections []ProjectConnection `json:"connections"`
}

type ProjectApp struct {
	Path    string `json:"path"`
	Context string `json:"context"`
	Reason  string `json:"reason"`
}

// File/Old/New describe a small, exact source replacement in the uploaded
// copy. {{url}} is resolved by the deployment planner, never by the model.
// Service credentials are only referenced through runtime environment vars.
type ProjectConnection struct {
	App    string `json:"app"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Env    string `json:"env"`
	Key    string `json:"key"`
	Suffix string `json:"suffix"`
	File   string `json:"file"`
	Old    string `json:"old"`
	New    string `json:"new"`
	Reason string `json:"reason"`
}

func (o *openAI) ReadProject(ctx context.Context, root string) (*Project, error) {
	summary, err := Summarize(root)
	if err != nil {
		return nil, err
	}
	history := o.investigate(ctx, root, projectInstructions, summary.Render(), maxBuildExploreTurns)
	history = append(history, responses.ResponseInputItemParamOfMessage(
		"Return every independently runnable app and the connections proven by the code you read. Use paths relative to this repository, including for source edits. Do not omit an app just because another has donkey.yaml.",
		responses.EasyInputMessageRoleUser))
	var out Project
	if err := o.structuredWith(ctx, projectInstructions, history, "project", projectSchema, &out); err != nil {
		return nil, fmt.Errorf("couldn't read the project's apps and connections: %w", err)
	}
	return &out, nil
}

func stringObject(fields ...string) map[string]any {
	props := map[string]any{}
	for _, field := range fields {
		props[field] = map[string]any{"type": "string"}
	}
	return map[string]any{"type": "object", "properties": props, "required": fields, "additionalProperties": false}
}

var projectSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"apps":        map[string]any{"type": "array", "items": stringObject("path", "context", "reason")},
		"connections": map[string]any{"type": "array", "items": stringObject("app", "target", "kind", "env", "key", "suffix", "file", "old", "new", "reason")},
	},
	"required": []string{"apps", "connections"}, "additionalProperties": false,
}

const projectInstructions = `Read this uploaded repository to deploy ALL of its runnable applications as separate apps in one space.
Use the read-only tools to inspect manifests, entry points, workspace definitions, source that makes network calls, and configuration. Do not guess from folder names alone.

apps: every independently runnable frontend, API or worker. Exclude shared libraries, tests, fixtures, examples within an app, and workspace root manifests that only orchestrate child apps. A repository with one app returns one entry. path is its directory relative to the upload ("." for the root). context is the directory needed for its Docker build, relative to the upload: normally the app directory, but the workspace root when it needs shared packages, root lockfiles or workspace installs. Respect an existing Dockerfile's COPY paths. Explain the evidence in reason.

connections: inspect actual runtime callers, environment overrides AND hardcoded defaults. Include frontend/backend links and PostgreSQL, OpenAI-compatible inference and S3/RustFS configuration, even for a single app. Ignore docs/tests/dev-only servers. Each entry has:
- app: source app path, exactly as listed in apps.
- target: sibling app path, or "postgres", "openai", "rustfs" for a managed service.
- kind: "browser" for browser API calls (donkey gives a same-origin proxy URL); "server" for calls from a container to a sibling; "origin" for a CORS/allowed-origin or callback setting that needs a sibling's public origin; "service" for managed service configuration. Never connect a browser directly to a managed service or put credentials in a browser build.
- env: the actual environment variable the caller reads, or "" when a source edit alone suffices. Browser variables go into the image build AND runtime; server/origin variables go into runtime only. Donkey declares browser build variables as ARG in every Dockerfile stage; no source edit is needed just to declare an ARG.
- key: for a service, the canonical binding key to inject under env. PostgreSQL provides DATABASE_URL, JDBC_DATABASE_URL, JDBC_DATABASE_USERNAME, JDBC_DATABASE_PASSWORD, PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE. OpenAI provides OPENAI_BASE_URL, OPENAI_API_KEY, OPENAI_MODEL. RustFS provides AWS_ENDPOINT_URL_S3, S3_URL, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION. Use separate entries for custom variable names. Other kinds use "".
- suffix: for sibling URLs only, the original URL's path (e.g. /api or /v1), otherwise "". Donkey preserves it after the proxy prefix. Do NOT invent or remove the API's own path prefix.
- file, old, new: an optional minimal exact source replacement, all "" if env already controls the call. file is relative to the repository. Read the file first; old must occur EXACTLY ONCE and new must keep unrelated code intact. For sibling addresses, use {{url}} in new where donkey must substitute the resolved address; include the language's surrounding quotes yourself. When an env override already suffices, do not edit its fallback. For hardcoded service configuration, change code to read the matching runtime environment variable, never embed a service URL or credential. Supply necessary imports in the same minimal snippet or a separate connection edit. Do not invent environment interpolation in a language/config format that doesn't support it. OpenAI clients must honor base URL, API key AND model. S3 clients must honor endpoint, credentials, region and path-style addressing. Runtime credentials must never be read during a frontend build. Never write secrets, .env files, shell commands or unrelated refactors.
- reason: one sentence citing the caller/configuration and why the connection is needed.

Use only the supported kinds and binding keys. Empty strings for unused fields. No duplicate replacements. If the target cannot be established from source, omit the connection; never guess a different service.`
