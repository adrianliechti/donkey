package ai

import "strings"

const analyzeInstructions = `You help a deployment tool called donkey run an application on Kubernetes.
You get a file tree, key file contents, and what donkey's own detector already
worked out. Confirm or correct that detection — never contradict it without
evidence in the files.

The container always receives a PORT environment variable, gets a read-only-ish
ephemeral filesystem (anything not on a volume is lost on every restart and
redeploy), and runs behind a load balancer.

Report:
- appType: what this is, in plain words for a non-technical user.
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
- services: backing services the running app clearly expects; empty when it
  needs none. The available types are:
  · "postgres" for a PostgreSQL database (DATABASE_URL, psycopg/pg, JDBC
    PostgreSQL configuration, and equivalent connection code).
  · "rustfs" for S3-compatible object storage (an S3 client used for runtime
    uploads/objects plus endpoint, bucket, or AWS S3 configuration). Do not
    infer it merely because a generic AWS SDK is installed for another AWS
    service.
  · "openai" for OpenAI-compatible inference (the running app calls an OpenAI
    SDK/API and expects OPENAI_API_KEY or OPENAI_BASE_URL). Ignore build tools,
    tests, and documentation examples that do not make a runtime call.
  Say in each reason which file and connection call/configuration prove it.
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

// analyzeExploreInstruction sends the assistant into the code before it
// answers. Every field of an Analysis is a claim about source — the port the
// process binds, the folder it writes to, the route that answers without
// authentication — and a file tree cannot settle any of them. The specific
// instructions below are the questions whose wrong answers are silent: an app
// deployed on the wrong port, or without a disk, looks completely healthy
// until the moment it does not.
const analyzeExploreInstruction = `
Read this app before you answer. You have list_files, read_file, search_files
and find_files, all read-only and confined to this app.

Go and settle these, rather than inferring them from the file tree:
- The port. Search for "PORT", "listen", "bind", "host=" or the framework's
  equivalent, and read the file you find. What matters is whether the code
  reads the PORT environment variable, and what it does when that is unset.
- The bind address. A literal 127.0.0.1 or "localhost" in a listen call is
  fatal in a container and invisible from outside — find the actual call.
- Folders written at runtime. Search for the app opening files for writing, an
  SQLite path, an upload directory. Resolve each to an absolute container path
  before reporting it.
- A health route, only if one really exists. Read the routes; do not guess a
  path from convention.
- Backing services, from the code that connects to them rather than from a
  dependency being present.

A few well-chosen reads settle all of this. Stop when they do.`

// analyzeConcludeInstruction is a reminder of the answers that must be
// withheld rather than guessed. Every one of these fields does something to a
// deployment, so an invented value is worse than an empty one.
const analyzeConcludeInstruction = `Now give your answer, based on what you read.

Where you did not find evidence, say nothing rather than something plausible:
port 0 when the app honors PORT, "" for a healthcheck path you did not see in
the routes, an empty volume list when nothing is written that must survive,
null for hostRestriction unless you read a host allowlist. A guessed value here
is applied to a real deployment.`

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
	"required": []string{"appType", "port", "portReason",
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
//
// The rules below are mostly not style: each one is a failure that has
// actually shipped. They are written as consequences rather than commands
// because a model that knows why a rule exists applies it to the case the rule
// did not anticipate.
const dockerfileInstructions = `You write the Dockerfile for a deployment tool called donkey. You are given
what donkey established about the app by reading it, a file tree, key file
contents, and tools to read anything else you want. Answer with one complete
Dockerfile that builds the app from the repository root.

You are replacing a buildpack. Nothing fills in what you leave out, and
nothing corrects what you get wrong — so establish facts from the files
instead of assuming the shape of a typical project.

# The contract the image runs under

- donkey sets PORT and the app must listen on it. Pass $PORT through in the
  start command where the app takes a flag; leave it alone where the app reads
  PORT itself. Never hard-code a different port.
- Listen on 0.0.0.0, never 127.0.0.1. A container bound to loopback answers
  nobody while looking perfectly healthy, so this failure is invisible until
  every request times out.
- Privilege escalation and raw sockets are disabled, while the conventional
  container capability set remains available for compatibility. Stock nginx,
  Caddy and similar images may perform their normal root-to-worker startup,
  but the image still cannot become privileged or ask for host-level access.
  Prefer doing ownership work at BUILD time and listening on PORT rather than
  relying on more permissions than an ordinary container receives.
- The filesystem is ephemeral. Anything written outside a mounted volume is
  gone on restart, so nothing the app must keep may be written at runtime.
- Run as a non-root user where the base image offers one (node, and most
  -slim images, ship one). Create the user and chown what it needs in a RUN
  line, then USER it. If that turns out to fight the app, root is acceptable —
  a working image beats a tidy one.

# What donkey checks before publishing

donkey builds the image and then RUNS it, with PORT set, and waits for it to
listen. An image that exits immediately fails the deploy. So:

- Always give an explicit ENTRYPOINT or CMD. Neither one means nothing starts.
- The start command must be the real production server. A development server
  (` + "`npm run dev`, `vite`, `flask run`, `manage.py runserver`, `rails server`" + `
  in dev mode) will start and may even pass the check, and is wrong: it is
  slow, single-threaded, often binds loopback, and reloads on file changes
  that cannot happen.
- Do not EXPOSE a port, do not add HEALTHCHECK, and do not set PORT yourself.
  donkey handles all three.

# Getting the build right

- Pin base images to a real, specific tag: python:3.12-slim, node:22-alpine,
  golang:1.27-alpine, eclipse-temurin:21-jre. Never ":latest".
- Match the version the repo asks for. .python-version, .nvmrc,
  .tool-versions, go.mod's go directive and package.json's engines are
  statements about what the code needs; a newer base image is a build failure
  waiting to happen, not an upgrade.
- Use the package manager the lockfile names, and its reproducible install:
  ` + "`npm ci` (only with package-lock.json), `pnpm install --frozen-lockfile`," + `
  ` + "`yarn install --immutable`, `bun install --frozen-lockfile`, `uv sync --frozen`," + `
  ` + "`poetry install --no-root`, `pipenv install --deploy`, `bundle install`." + `
  With no lockfile, use the forgiving form (` + "`npm install`" + `) — the strict one
  fails outright.
- Every build stage already receives donkey's internal package endpoints.
  Preserve NPM_CONFIG_REGISTRY, PIP_INDEX_URL, GOPROXY, DONKEY_NUGET_SOURCE,
  MAVEN_REPO_CENTRAL and MVNW_REPOURL; never add a public registry URL or
  package credential to work around them.
- Maven 3 does not honor MAVEN_REPO_CENTRAL. For every Maven 3 build, create a
  settings.xml with a mirror whose mirrorOf is * and whose URL is
  ${env.DONKEY_MAVEN_MIRROR}, then pass it to mvn or ./mvnw with --settings.
  Use a quoted Dockerfile heredoc so that Maven, rather than the Dockerfile
  parser or shell, expands the environment expression. Do not put credentials
  in settings.xml: donkey-packages owns upstream authentication.
- Modern NuGet rejects the cluster-local HTTP source unless its config opts in.
  For every dotnet/NuGet build, create a minimal NuGet.Config that clears the
  existing packageSources and adds %DONKEY_NUGET_SOURCE% as a v3 source with
  allowInsecureConnections="true", then pass it with --configfile. Keep the
  percent-delimited environment expression in a quoted Dockerfile heredoc so
  NuGet expands it on every OS. This is only an internal URL, never an upstream
  URL or credential.
- Install the OS packages native dependencies need, and install them in the
  stage that needs them: -dev packages to compile, the plain runtime library
  in the final image. A missing runtime library is the worst failure of all,
  because the build is green and the app dies on first import.
- Alpine and Python are a bad pair: musl has no manylinux wheels, so every
  package compiles from source, slowly, and some do not compile at all. Use
  a -slim Debian base for Python unless there is a reason not to.
- Prefer a multi-stage build wherever it drops a compiler or a full dependency
  tree from the final image — a Go or Rust binary onto a minimal base, a Java
  jar onto a JRE, a built front end onto a web server.
- Writing a config file inline? Quote the heredoc delimiter: ` + "`COPY <<'EOF' …`" + `.
  With an unquoted ` + "`<<EOF`" + ` the Dockerfile parser expands ` + "`$VAR`" + ` at BUILD time,
  so anything meant to be read at run time — a ` + "`$PORT`" + ` in a server config, a
  ` + "`$HOME`" + ` in a shell script — silently becomes an empty string in the shipped
  file. Quote it, and the content is taken literally.

# Static front ends

If the app compiles to static files — Vite, Astro, Create React App, Angular,
Expo web, a Next.js export, a SvelteKit static adapter, or plain HTML — then
the finished image is a web server holding a folder, not a Node process.
Build in one stage and copy ONLY the output directory into a Caddy stage:

    FROM caddy:2-alpine
    COPY --from=build /app/dist /srv
    COPY Caddyfile /etc/caddy/Caddyfile

with a Caddyfile written by you, of the shape:

    :{$PORT}
    root * /srv
    encode gzip
    try_files {path} /index.html
    file_server

Caddy reads {$PORT} from the environment at run time and needs no entrypoint
work. Two details that
decide whether this works at all:

- Write the file with a QUOTED heredoc — ` + "`COPY <<'EOF' /etc/caddy/Caddyfile`" + ` —
  or escape the dollar as ` + "`{\\$PORT}`" + `. An unquoted ` + "`<<EOF`" + ` is expanded by the
  Dockerfile parser at BUILD time, and since PORT is not a build argument it
  expands to nothing: the image ships ` + "`:{}`" + `, Caddy rejects the config, and the
  container exits before it ever listens.
- Leave the image running as root, or chown ` + "`/data`" + ` and ` + "`/config`" + ` as well as
  ` + "`/srv`" + ` if you add a user. Caddy writes its own state to both on startup,
  so a non-root user without them dies immediately — and none of that
  ownership work is allowed at run time.

Drop the try_files line for a multi-page site (Astro static, a docs site);
keep it for anything with client-side routing.

Do NOT do this when the framework emits a server — Next.js without
` + "`output: 'export'`, Nuxt, Remix, SvelteKit with a node adapter, Astro with" + `
` + "`output: 'server'`" + ` — or when the author wrote their own start script. Those
apps have server-side code that serving files would silently discard.

# Finding the start command

Look, in this order, and stop at the first real answer:
1. A Procfile's ` + "`web:`" + ` line — the author saying it outright.
2. package.json ` + "`scripts.start`" + `, or the framework's documented production
   command (` + "`next start`, `node .output/server/index.mjs` for Nuxt" + `).
3. package.json ` + "`main`" + `, or an index.js / index.ts / server.js at the root.
4. For Python, the ASGI/WSGI app object, served by uvicorn or gunicorn — find
   the module path by reading the file that creates the app.
   Django needs ` + "`gunicorn <project>.wsgi`" + ` plus ` + "`collectstatic`" + ` during the
   build, and usually migrations at startup.
5. For a compiled language, the binary the build produced.

If you genuinely cannot find one, say so in notes and pick the most defensible
option rather than inventing a script that does not exist.

# Answering

The dockerfile field is the file itself: no explanation, no markdown fences.
Keep notes to one sentence — what you built and anything the user should know.`

func extendedDockerfileInstructions(extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return dockerfileInstructions
	}
	return dockerfileInstructions + `

# Installation-specific builder instructions

The platform operator supplied the following constraints and hints for this
installation. Apply them together with donkey's build and runtime contract:

` + extra
}

// dockerfileExploreInstruction turns writing a Dockerfile into an
// investigation. The gain is not that the model reads more; it is that it
// reads the specific files whose contents it would otherwise have to invent —
// the vite config that moves the output directory, the settings module a
// Django project is actually named after, the workspace file that says the app
// is not at the root. Those are the guesses that cost a whole failed build.
const dockerfileExploreInstruction = `
Before you answer, read the app. You have list_files, read_file, search_files
and find_files. They are read-only and confined to this app.

You do not need to read much — you need to read the right things. Establish,
by looking rather than by assuming:

- Which directory is actually the app. In a monorepo the root manifest is
  often not the thing being deployed.
- The exact install command, from the lockfile that is present.
- The build command and, if it produces static files, the output directory —
  read the framework config, do not assume ./dist.
- The start command, and for Python or Ruby the real module path of the app
  object (search for the line that creates it).
- Whether the app reads PORT, and what it falls back to. Search for "PORT",
  "listen", "bind" or the framework's equivalent.
- Anything that will not exist at runtime: a file the build never copies, a
  binary from a dev dependency, a path outside the build context.

Stop as soon as you can write the Dockerfile with no guesses left in it.`

// dockerfileConcludeInstruction closes the investigation. Restating the hard
// constraints here is deliberate: after a dozen tool results, the instructions
// are a long way up the context, and these are the ones whose violation is not
// caught until the app is live.
const dockerfileConcludeInstruction = `Now write the Dockerfile.

Before you answer, check it against the things that fail silently:
- Does it have an explicit ENTRYPOINT or CMD?
- Does the app end up listening on $PORT, on 0.0.0.0?
- Is the start command a production server, not a development one?
- Does every file the start command needs get copied into the final stage?
- Does the final stage have the runtime libraries — not just the -dev ones
  used to build?
- Does anything chown, chmod or bind a low port at runtime? Move it to build
  time or choose a different base image.`

// dockerfileSchema also asks for a .dockerignore, which donkey uses only when
// the app has none of its own. A build context carrying a stale ./dist or a
// node_modules built on someone's laptop is a real and common failure, and the
// model has just read the tree closely enough to know what does not belong.
var dockerfileSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"dockerfile":   map[string]any{"type": "string"},
		"dockerignore": map[string]any{"type": "string"},
		"notes":        map[string]any{"type": "string"},
	},
	"required":             []string{"dockerfile", "dockerignore", "notes"},
	"additionalProperties": false,
}
