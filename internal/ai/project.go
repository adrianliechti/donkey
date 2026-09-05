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
	Name    string       `json:"name"`
	Path    string       `json:"path"`
	Context string       `json:"context"`
	Reason  string       `json:"reason"`
	Env     []ProjectEnv `json:"env"`
}

type ProjectEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
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

var projectConnectionSchema = func() map[string]any {
	schema := stringObject("app", "target", "kind", "env", "key", "suffix", "file", "old", "new", "reason")
	schema["properties"].(map[string]any)["kind"] = map[string]any{
		"type": "string", "enum": []string{"browser", "server", "address", "origin", "service"},
	}
	return schema
}()

var projectSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"apps":        map[string]any{"type": "array", "items": projectAppSchema},
		"connections": map[string]any{"type": "array", "items": projectConnectionSchema},
	},
	"required": []string{"apps", "connections"}, "additionalProperties": false,
}

var projectAppSchema = func() map[string]any {
	schema := stringObject("name", "path", "context", "reason", "env")
	schema["properties"].(map[string]any)["env"] = map[string]any{
		"type": "array", "items": stringObject("name", "value"),
	}
	return schema
}()

const projectInstructions = `Read this uploaded repository to deploy ALL of its runnable applications as separate apps in one space.
Use the read-only tools to inspect manifests, entry points, workspace definitions, source that makes network calls, and configuration. Do not guess from folder names alone.

apps: every independently runnable frontend, API or worker that listens on a port (including a health or metrics port). Exclude headless test/load generators with no listening port, shared libraries, tests, fixtures, examples within an app, and workspace root manifests that only orchestrate child apps. When the repository declares a default deployment, inspect its manifests and feature flags: exclude explicitly opt-in extensions that the default deployment and enabled callers do not use. Do not exclude a required app merely because it needs configuration. A repository with one app returns one entry. name is the application's name from its manifest, deployment or project file; never use a generic code directory like src as its name. path is its directory relative to the upload ("." for the root). context is the directory needed for its Docker build, relative to the upload: normally the app directory, but the workspace root when it needs shared packages, root lockfiles or workspace installs. Respect an existing Dockerfile's COPY paths. Explain the evidence in reason.

Each app's env is a list of name/value pairs for non-secret runtime defaults explicitly supplied by its default deployment manifests, such as disabling optional cloud profilers or tracing agents. Copy the exact declared values. Exclude PORT (donkey sets it), credentials, secret references, addresses of included siblings and managed service settings (use connections for those). Retain manifest defaults for disabled optional components when startup code still requires those variables, even if the disabled feature never calls their placeholder address. Do not invent settings. An app without such defaults has an empty env list.

connections: inspect actual runtime callers, environment overrides AND hardcoded defaults. Include frontend/backend links and PostgreSQL, Redis, OpenAI-compatible inference and S3/RustFS configuration, even for a single app. Ignore docs/tests/dev-only servers. Each entry has:
- app: source app path, exactly as listed in apps.
- target: sibling app path, or "postgres", "redis", "openai", "rustfs" for a managed service.
- kind: "browser" for browser API calls (donkey gives a same-origin proxy URL); "server" for HTTP URLs from a container to a sibling; "address" for host:port settings such as gRPC targets (no scheme and no suffix); "origin" for a CORS/allowed-origin or callback setting that needs a sibling's public origin; "service" for managed service configuration. Never connect a browser directly to a managed service or put credentials in a browser build.
- env: the actual environment variable the caller reads, or "" when a source edit alone suffices. Browser variables go into the image build AND runtime. Server/origin variables go into runtime, and into the build when already declared in build.env; origins exposed through framework browser variables also go into the build. Internal server URLs must never use a browser variable such as VITE_* or NEXT_PUBLIC_*. Donkey declares build variables as ARG in every Dockerfile stage; no source edit is needed just to declare an ARG.
- key: for a service, the canonical binding key to inject under env. PostgreSQL provides DATABASE_URL, JDBC_DATABASE_URL, JDBC_DATABASE_USERNAME, JDBC_DATABASE_PASSWORD, PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE. Redis provides REDIS_URL, REDIS_ADDR (host:port only), REDIS_HOST, REDIS_PORT, REDIS_USERNAME, REDIS_PASSWORD, REDIS_DB, REDIS_TLS. OpenAI provides OPENAI_BASE_URL, OPENAI_API_KEY, OPENAI_MODEL. RustFS provides AWS_ENDPOINT_URL_S3, S3_URL, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION. Use separate entries for custom variable names. Other kinds use "".
- suffix: for sibling URLs only, the original URL's path (e.g. /api or /v1), otherwise "". Donkey preserves it after the proxy prefix. Do NOT invent or remove the API's own path prefix. If a browser already calls a same-origin path covered by the target's donkey.yaml routes, return that path as suffix with empty env/file/old/new; the existing route needs no code change.
- file, old, new: an optional minimal exact source replacement, all "" if env already controls the call. file is relative to the repository. Read the file first; old must occur EXACTLY ONCE and new must keep unrelated code intact. For sibling addresses, use {{url}} in new where donkey must substitute the resolved address; include the language's surrounding quotes yourself. When an env override already suffices, do not edit its fallback. For hardcoded service configuration, change code to read the matching runtime environment variable, never embed a service URL or credential. Supply necessary imports in the same minimal snippet or a separate connection edit. Do not invent environment interpolation in a language/config format that doesn't support it. Redis clients must honor credentials, database and TLS as well as the address; clients using REDIS_ADDR must separately read REDIS_PASSWORD (and username/TLS for external services), because an address alone cannot authenticate. OpenAI clients must honor base URL, API key AND model. S3 clients must honor endpoint, credentials, region and path-style addressing. Runtime credentials must never be read during a frontend build. Never write secrets, .env files, shell commands or unrelated refactors.
- reason: one sentence citing the caller/configuration and why the connection is needed.

Use only the supported kinds and binding keys. Empty strings for unused fields. No duplicate replacements. If the target cannot be established from source, omit the connection; never guess a different service.`
