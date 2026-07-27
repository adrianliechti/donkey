package ai

const analyzeInstructions = `You help a deployment tool called donkey run an application on Kubernetes.
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
- bindsLoopback: true when the code listens on 127.0.0.1 or localhost rather
  than 0.0.0.0 / every interface. In a container that is fatal but silent —
  the app starts, prints a URL, and nothing outside can reach it. Flask's
  bare app.run(), Streamlit without --server.address, and any
  listen(port, "127.0.0.1") are the usual cases. false when the app binds all
  interfaces, when a Procfile or start command already passes a host, or when
  you cannot tell.
- bindReason: one short sentence naming the file and the call, when
  bindsLoopback is true; otherwise "".
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
- localURLs: absolute addresses written into the code that point at localhost,
  127.0.0.1, 0.0.0.0, host.docker.internal or a development port — the ones
  that quietly stop meaning what the author intended once the app runs on a
  cluster. For each, give the url, the file, a one-sentence reason, and
  runsIn:
  · "browser" — the code is delivered to a browser (a React/Vue/Svelte
    component, anything under src/ that a bundler like vite or webpack builds
    into the page). There, localhost is the *visitor's own machine*, so such
    an address is always wrong.
  · "server" — the code executes inside the container (an API server, a
    worker, a build script). There, localhost is that container itself: right
    when it calls a process running beside it, wrong when it means another
    app, which is reached by its name instead (http://backend).
  Report only addresses the running app actually calls. Ignore dev-only
  config a build never ships: a vite/webpack devServer proxy, test fixtures,
  docker-compose files, comments, README examples, and any default that an
  environment variable overrides at runtime.
- hostRestriction: set this ONLY when the app itself rejects requests whose
  Host header is not on a list it keeps. Django's ALLOWED_HOSTS is the case
  that matters: the generator leaves it empty, and Django then answers 400 to
  every request — so the app looks healthy, passes its probes, and refuses
  every visitor. Give the setting name as the author would recognize it, the
  file, the environment variable that overrides it if the framework has one
  (Django itself has none by default; say "" then), and a one-sentence reason.
  Only report a list that is empty or that cannot match the deployed hostname.
  null when the app serves any Host, which is the usual case.
- notes: one short sentence for a non-technical user.`

const diagnoseInstructions = `You help a deployment tool called donkey explain failures to users who may
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

// exploreInstruction is appended when the assistant can read the app. The
// rules exist because the answer is turned into a patch the user is shown and
// may apply: an edit to a file nobody read, or one that rewrites more than it
// needs to, is worse than no proposal at all.
const exploreInstruction = `
You can read this app before answering, using the tools available to you:
list_files, read_file and search_files. They are read-only and limited to this
app's folder.

Work like someone debugging it for real:
- Follow the evidence. A stack trace names a file and a line — read that file.
- When the logs point into a library rather than the app, search for the
  setting or symbol involved and read whatever configures it.
- Read a file BEFORE proposing any change to it. Never propose an edit to a
  file you have not read in full: newContent replaces the whole file, so
  anything you did not see would be deleted.
- Stop as soon as you can explain the failure. A few well-chosen reads beat
  listing everything.`

// concludeInstruction closes the investigation. The insistence on minimal
// edits is what makes the resulting diff readable: a model that reformats or
// "improves" the file produces a patch nobody can review, and the whole point
// is that the user can see exactly what would change.
const concludeInstruction = `Now give your answer.

For every edit_file action, newContent must be the COMPLETE new content of
that file — donkey diffs it against what is on disk and shows the user the
difference.

Change as little as possible:
- Keep every line you are not deliberately fixing exactly as it is, including
  comments, blank lines, import order, quoting style and indentation.
- Do not reformat, rename, tidy, upgrade or "improve" anything the failure did
  not require.
- One concern per action. If two unrelated things are wrong, propose two.
Say in the explanation which line you changed and why that fixes the failure.`

var analysisSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"appType":         map[string]any{"type": "string"},
		"strategy":        map[string]any{"type": "string", "enum": []string{"", "railpack", "dockerfile"}},
		"dockerfile":      map[string]any{"type": "string"},
		"railpackConfig":  map[string]any{"type": "string"},
		"port":            map[string]any{"type": "integer"},
		"portReason":      map[string]any{"type": "string"},
		"bindsLoopback":   map[string]any{"type": "boolean"},
		"bindReason":      map[string]any{"type": "string"},
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
		"localURLs": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":    map[string]any{"type": "string"},
					"file":   map[string]any{"type": "string"},
					"runsIn": map[string]any{"type": "string", "enum": []string{"browser", "server"}},
					"reason": map[string]any{"type": "string"},
				},
				"required":             []string{"url", "file", "runsIn", "reason"},
				"additionalProperties": false,
			},
		},
		"hostRestriction": map[string]any{
			"type": []string{"object", "null"},
			"properties": map[string]any{
				"setting": map[string]any{"type": "string"},
				"file":    map[string]any{"type": "string"},
				"envVar":  map[string]any{"type": "string"},
				"reason":  map[string]any{"type": "string"},
			},
			"required":             []string{"setting", "file", "envVar", "reason"},
			"additionalProperties": false,
		},
		"notes": map[string]any{"type": "string"},
	},
	"required": []string{"appType", "strategy", "dockerfile", "railpackConfig", "port", "portReason",
		"bindsLoopback", "bindReason",
		"healthcheckPath", "volumes", "services", "localURLs",
		"hostRestriction", "notes"},
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

// dockerfileInstructions drive donkey's own builder. The Dockerfile it
// produces is the whole build — there is no buildpack underneath to fill
// anything in — and donkey runs the result before publishing it, so a
// plausible-looking image that cannot start is caught rather than shipped.
const dockerfileInstructions = `You write the Dockerfile for a deployment tool called donkey.
You get a file tree and key file contents. Answer with one complete Dockerfile
that builds the application from the repository root.

Requirements, all of which donkey checks:
- It must build with no arguments, from the repository root as context.
- The image must START. donkey runs it before publishing it: it launches your
  ENTRYPOINT/CMD and waits for the app to listen. An image that exits
  immediately fails the deploy.
- Always give an explicit ENTRYPOINT or CMD. An image with neither cannot be
  started at all.
- The app is given a PORT environment variable and must listen on it. Prefer a
  start command that passes $PORT through; where the app reads PORT itself,
  leave it alone. Never hard-code a different port.
- Listen on 0.0.0.0, never 127.0.0.1: a container that binds loopback answers
  nobody, and it looks healthy while doing it.
- Pin base images to a real, specific tag (python:3.12-slim, node:22-alpine,
  golang:1.23-alpine, eclipse-temurin:21-jre). Never ":latest".
- Install exactly the dependencies the manifest files name. Do not invent
  packages, and do not skip system packages a library needs to build.
- Prefer a multi-stage build where it removes a compiler from the final image.
- Do not EXPOSE a port, do not add HEALTHCHECK, and do not set PORT yourself —
  donkey handles all three.
- The container runs with EVERY Linux capability dropped and no privilege
  escalation. Anything needing chown, chmod of another user's files, setuid,
  or a port below 1024 fails at runtime even as root. Stock nginx is the
  classic trap: its entrypoint chowns its cache directory and dies. Reach for
  an image built to run unprivileged (nginxinc/nginx-unprivileged) or do the
  ownership work at BUILD time, in a RUN line, where it is still allowed.
- Everything outside a mounted volume is lost on restart, so write nothing at
  runtime that has to survive.
- Write no explanation, no markdown fences: the dockerfile field is the file.

When you are given a previous Dockerfile and the failure it produced, fix THAT
failure. Read the error before rewriting: a missing shared library, an
interpreter that is not on PATH, a start command that exits, a file the build
never copied. Change what is wrong and keep what worked.`

// dockerfileSchema is deliberately two fields: the file, and one line about
// what changed — which is what donkey shows when a retry happened.
var dockerfileSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"dockerfile": map[string]any{"type": "string"},
		"notes":      map[string]any{"type": "string"},
	},
	"required":             []string{"dockerfile", "notes"},
	"additionalProperties": false,
}
