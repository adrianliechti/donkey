package ai

const suggestSystemPrompt = `You help a deployment tool called donkey build applications it could not
recognize automatically. You get a file tree and key file contents.

Decide how to containerize the app:
- Prefer strategy "railpack" with a railpack config (JSON, https://railpack.com schema:
  {"provider": "...", "buildCommand": "...", "startCommand": "...", "packages": {...}})
  when the app is close to a standard Python/Node/Go/Ruby/PHP/Java/static setup.
- Use strategy "dockerfile" with a complete, production-quality Dockerfile when the
  setup is unusual. The Dockerfile must build from the repository root.

Also report: the app type in plain words, the TCP port the app listens on,
the start command, and a health check path that returns 200 (use "/" if unsure).
Keep notes to one short sentence for a non-technical user.`

const settingsSystemPrompt = `You help a deployment tool called donkey configure an app it already knows
how to build. You get a file tree and key file contents. The container will
always receive a PORT environment variable.

Determine:
- port: the fixed TCP port the app listens on, read from the source/config.
  If the app reads the PORT environment variable (process.env.PORT,
  os.environ, os.Getenv, ENV["PORT"], …) and would use it, answer 0 — any
  port works then. Only answer a number when the code pins one.
- healthcheckPath: a GET path that clearly answers successfully without
  authentication (e.g. a /health, /healthz, /ping or /status route defined in
  the code). Answer "" when you don't see one — guessing breaks deployments.
- reason: one short sentence naming the file you based the port on.`

const diagnoseSystemPrompt = `You help a deployment tool called donkey explain failures to users who may
not be technical. You get the failure stage, config, cluster events and logs.

Reply with:
- diagnosis: 1-3 plain-language sentences saying what went wrong and why.
- actions: concrete fixes donkey can offer. Types:
  - "edit_file": propose new full content for one file (path + newContent).
    Only donkey.yaml, .donkey/* or app source files.
  - "set_env": propose an environment variable (key + value; value may be a
    placeholder the user fills in).
  - "rerun": no file change needed, retrying should work after the user does
    what the explanation says.
Each action needs a one-sentence explanation. Propose at most 3 actions,
best first. If nothing sensible can be proposed, return an empty actions list.`

var suggestionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"appType":         map[string]any{"type": "string"},
		"strategy":        map[string]any{"type": "string", "enum": []string{"railpack", "dockerfile"}},
		"dockerfile":      map[string]any{"type": "string"},
		"railpackConfig":  map[string]any{"type": "string"},
		"port":            map[string]any{"type": "integer"},
		"startCommand":    map[string]any{"type": "string"},
		"healthcheckPath": map[string]any{"type": "string"},
		"notes":           map[string]any{"type": "string"},
	},
	"required":             []string{"appType", "strategy", "dockerfile", "railpackConfig", "port", "startCommand", "healthcheckPath", "notes"},
	"additionalProperties": false,
}

var settingsSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"port":            map[string]any{"type": "integer"},
		"healthcheckPath": map[string]any{"type": "string"},
		"reason":          map[string]any{"type": "string"},
	},
	"required":             []string{"port", "healthcheckPath", "reason"},
	"additionalProperties": false,
}

var proposalSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"diagnosis": map[string]any{"type": "string"},
		"actions": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":        map[string]any{"type": "string", "enum": []string{"edit_file", "set_env", "rerun"}},
					"path":        map[string]any{"type": "string"},
					"newContent":  map[string]any{"type": "string"},
					"key":         map[string]any{"type": "string"},
					"value":       map[string]any{"type": "string"},
					"explanation": map[string]any{"type": "string"},
				},
				"required":             []string{"type", "path", "newContent", "key", "value", "explanation"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"diagnosis", "actions"},
	"additionalProperties": false,
}
