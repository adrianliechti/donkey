package ai

const analyzeSystemPrompt = `You help a deployment tool called donkey run an application on Kubernetes.
You get a file tree, key file contents, and what donkey's own detector already
worked out. Confirm or correct that detection — never contradict it without
evidence in the files.

The container always receives a PORT environment variable, gets a read-only-ish
ephemeral filesystem (anything not on a volume is lost on every restart and
redeploy), and runs behind a load balancer.

Report:
- appType: what this is, in plain words for a non-technical user.
- strategy: "" when donkey's detection works and needs no help. Only when
  donkey could not detect the app, choose "railpack" with a railpack config
  (JSON, https://railpack.com schema: {"provider": "...", "buildCommand": "...",
  "startCommand": "...", "packages": {...}}) for a standard
  Python/Node/Go/Ruby/PHP/Java/static setup, or "dockerfile" with a complete,
  production-quality Dockerfile that builds from the repository root.
- port: the fixed TCP port the app listens on, read from the source/config.
  If the app reads the PORT environment variable (process.env.PORT,
  os.environ, os.Getenv, ENV["PORT"], …) and would use it, answer 0 — any port
  works then. Only answer a number when the code pins one.
- portReason: one short sentence naming the file you based the port on.
- startCommand: how the app is started, when the files make it explicit.
- healthcheckPath: a GET path that answers successfully without authentication
  (e.g. a /health, /healthz, /ping or /status route defined in the code).
  Answer "" when you don't see one — guessing breaks deployments.
- volumes: folders INSIDE the container whose contents must survive a restart,
  because the app writes data there that nobody can regenerate: user uploads,
  an SQLite/embedded database file's folder, generated documents, search
  indexes that take hours to rebuild, per-instance keys.
  Rules, in order of importance:
  · Only include a folder when the code or config actually writes there. Quote
    that evidence in the reason ("main.py opens ./data/app.db").
  · Give absolute container paths. Resolve relative paths against the app's
    working directory, which is /app unless a Dockerfile says otherwise.
  · NEVER include: /tmp, caches, logs, build output (dist, build, .next),
    node_modules, virtualenvs, static assets shipped with the image, or
    anything a database service would own (an attached postgres keeps its own
    disk).
  · Prefer few, broad folders over many narrow ones. An app with nothing to
    keep gets an empty list — that is the common, correct answer.
- services: backing services the app clearly expects (currently only
  "postgres" is available); empty when the app needs none. Say in the reason
  which file shows it (a DATABASE_URL default, a psycopg import, …).
- notes: one short sentence for a non-technical user.`

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

var analysisSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"appType":         map[string]any{"type": "string"},
		"strategy":        map[string]any{"type": "string", "enum": []string{"", "railpack", "dockerfile"}},
		"dockerfile":      map[string]any{"type": "string"},
		"railpackConfig":  map[string]any{"type": "string"},
		"port":            map[string]any{"type": "integer"},
		"portReason":      map[string]any{"type": "string"},
		"startCommand":    map[string]any{"type": "string"},
		"healthcheckPath": map[string]any{"type": "string"},
		"volumes": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":   map[string]any{"type": "string"},
					"reason": map[string]any{"type": "string"},
				},
				"required":             []string{"path", "reason"},
				"additionalProperties": false,
			},
		},
		"services": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type":   map[string]any{"type": "string"},
					"reason": map[string]any{"type": "string"},
				},
				"required":             []string{"type", "reason"},
				"additionalProperties": false,
			},
		},
		"notes": map[string]any{"type": "string"},
	},
	"required": []string{"appType", "strategy", "dockerfile", "railpackConfig", "port", "portReason",
		"startCommand", "healthcheckPath", "volumes", "services", "notes"},
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
