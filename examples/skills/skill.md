---
name: donkey
description: >
  Deploy apps to the donkey platform (a small PaaS on Kubernetes). Use this
  skill whenever you build, deploy, test or operate an app with the `donkey`
  CLI — including creating backing services (postgres, redis, S3-compatible
  storage, the platform's AI), connecting apps to them, exposing APIs and
  MCP servers, seeding disks, reading logs and diagnosing failed deploys.
---

# Deploying on donkey

donkey is a PaaS running inside a Kubernetes cluster. You only ever talk to
it through the `donkey` CLI — a thin, token-authenticated client. The server
does everything heavy: it recognizes the app, builds the image (from your
Dockerfile, or one its assistant writes for you), pushes it to the registry,
applies the Kubernetes objects and watches the rollout. **Never use `docker`,
`kubectl` or `helm` for anything donkey manages** — the CLI is the entire
interface, and everything you need (logs, env vars, files on disks, database
tunnels) is a subcommand. (`donkey server …` exists too, but it installs the
control plane and is an administrator's job, not part of deploying an app.)

A deploy is: write code + a small `donkey.yaml`, run `donkey deploy` from
the app folder, read the streamed output, verify with `donkey status` and
`donkey logs`.

## Before you start

```sh
donkey doctor        # client + server health, with plain-language fixes
donkey config show   # which server this machine is connected to
```

If `doctor` reports no login, this machine has to be connected once. The
browser sign-in — `donkey login --server https://<domain>` — is not
something you can do yourself. But if you were handed a token, you can
connect unattended:

```sh
donkey login --server https://<domain> --token <token> [--ca-file ca.crt]
```

or skip the login entirely by exporting `DONKEY_TOKEN=<token>` — it wins over
the saved one for every command. The saved connection lives in
`~/.config/donkey/config.yaml` (server address, token, CA file). Without a
TTY, `donkey login` needs `--server`; it never prompts. Everything else in
this skill works unattended.

Practical CLI facts for scripted use:

- Most commands need the app's folder: they read `donkey.yaml` to know the
  app and space. Either `cd` there or pass `--app-dir <folder>` — it works
  from anywhere. Global flags (`--app-dir`, `--space`, `--yes`, `-v`) parse
  before or after the subcommand.
- Without a TTY every question takes its default. `--yes` does the same in
  a terminal. Neither covers deletions: `destroy`, `addon remove --space`
  and `files rm` default to "no" and, unattended, exit with an error asking
  for `--force` rather than removing anything.
- `donkey apps` lists every deployed app by space — use it to see what
  already exists before inventing names.
- `-v` shows the full build output instead of one line per build step.

## Concepts you must know

- **App** — one deployable unit. Its folder holds the code and a
  `donkey.yaml`. Deployed as `<app>.<space>.<domain>`.
- **Space** — a group of apps sharing one namespace. Defaults to the
  repository folder name (the git root's name, not the app folder's);
  override with `space:` in donkey.yaml or `--space`. Apps in a space reach
  each other by plain name: `http://backend` — no ports, no full hostnames.
- **Space, app and service names** become DNS labels: lowercase letters,
  digits and dashes, starting and ending alphanumeric, at most 40
  characters (`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`). `donkey`,
  `platform`, and `system` are reserved as space names. An app cannot take
  the name of a service that already runs in its space (an app called
  `postgres` next to a postgres service is refused). Pick valid names up
  front — the server rejects the rest before building.
- **Addons (services)** — `postgres`, `redis`, `rustfs` (S3), `openai`.
  They belong to the *space*, and apps *connect* to them. Two apps naming
  the same service share it (a monorepo's database is the project's
  database). Connecting injects a canonical set of env vars; the app never
  needs a config file or a secret of its own.
- **Private by default** — every app sits behind an OIDC browser login.
  `public: true` opts out; `access:` narrows the login to named
  users/groups/roles; `internal: true` removes the public address entirely
  (siblings still reach it by name); `mode: api` turns the login into OAuth
  for machine clients (MCP servers, APIs called with bearer tokens). The
  exact combination rules are in the reference below.
- **Console** — the same server has a web console. Network settings
  changed there (mode, exposure, host, access, CORS) are kept on later
  CLI deploys as long as `donkey.yaml` is silent on them; the deploy prints
  `Keeping the console's setting: …` when that happens. Whatever
  `donkey.yaml` states always wins.

## The workflow

Follow this order. Declaring addons in `donkey.yaml` *before* the first
deploy is the key move: one `donkey deploy` then provisions the services,
injects their bindings and starts the app — a cloned repo comes up whole.

### 1. Write `donkey.yaml`

Minimal, in the app folder (all other fields have working defaults):

```yaml
name: shop          # becomes <name>.<space>.<domain>
port: 8000          # the port your code actually listens on — required
healthcheck:
  path: /healthz    # a real route needing no auth; omit for a TCP check
addons:
  - type: postgres  # provisioned on first deploy; bindings injected as env
  - type: openai
```

`donkey init` can write this file for you (the server reads the source with
the platform's assistant and prints what it found), but when you wrote the
app yourself you already know the answers — writing the file directly is
faster and exact. It also matters unattended: a deploy in a folder with no
`donkey.yaml` sets one up (name from the folder, port and health route from
the code, data folders on a disk) but connects **no services**, because
creating a database is not a question a default answers for you. Declare
`addons:` yourself and the deploy provisions them. `donkey init` refuses to
touch an existing `donkey.yaml`, valid or broken.

Full reference of the fields you'll actually use:

```yaml
name: shop
space: acme                   # default: repository folder name
port: 8000                    # required; the port the code binds

public: true                  # skip the login (machine clients, public APIs)
internal: true                # no public address; siblings use http://shop
mode: api                     # web (default) | api — OAuth instead of the
                              # browser login; use for MCP servers and APIs
cors: allowAll                # gateway answers preflights, sends ACAO: *
host: shop.example.com        # custom address: a full hostname, a plain
                              # label (→ label.<space>.<domain>), or @ for
                              # the space's apex <space>.<domain>
access:                       # narrow the login (any match wins)
  users: [ada@example.com]    # the ID token's email claim
  groups: [staff]             # groups claim
  roles: [developer]          # roles claim (Entra app roles, Keycloak realm roles)
routes:                       # answer on ANOTHER app's address under a path
  - app: dashboard            # same space; not this app itself
    path: /api                # below the root, e.g. /api — never /
    stripPrefix: true         # optional: the app sees / instead of /api

command: ["node", "server.js"]          # override the image's start command

healthcheck:
  path: /healthz              # empty = check that the port answers (TCP)

env:                          # NON-secret runtime values only
  GREETING: hello
  DB_URL: ${db.DATABASE_URL}  # a binding of a declared addon, by name and
                              # key — resolved at runtime, never copied

storage:                      # one disk; each path a folder on it
  size: 5Gi                   # default 1Gi
  class: fast-ssd             # default: the cluster's default StorageClass
  paths: [/app/data]          # paths: [] means "no disk" and is respected

addons:
  - type: postgres            # postgres | redis | rustfs | openai
    name: db                  # optional; defaults to the type. Apps naming
                              # the same service share it.
    version: "16"             # optional; only used when the service is created
    storage: 5Gi              # optional; same

build:
  type: auto                  # auto | dockerfile | ai
  dockerfile: Dockerfile      # path to use when type is dockerfile
  context: .                  # build context relative to the app folder
  platform: linux/arm64       # default: the server's default platform
  env:                        # BUILD-time values — where bundlers inline
    VITE_API_URL: /api        # (a browser bundle can't read runtime env)

replicas: 2                   # must stay 1 when the app has storage.paths
resources:
  requests: { cpu: 100m, memory: 128Mi }
  limits:   { memory: 512Mi }
```

Rules the platform enforces:

- What you write in `donkey.yaml` **wins** — the assistant that reads the
  source never overwrites it, at most warns (e.g. "yaml says 8080, code
  listens on 5000"). So keep the file truthful. Unknown keys are an error:
  the file is parsed strictly.
- The container always gets a `PORT` env var set to `port:` — reading
  `PORT` in the code (with your intended port as fallback) is the safest
  pattern and can never disagree with the yaml. `donkey env set PORT=…` is
  refused for the same reason.
- `env` is for non-secret values. Secrets go through
  `donkey env set KEY=VALUE` (stored as a cluster Secret, app restarts
  automatically). Never write a secret into `donkey.yaml` — it belongs in
  the repository.
- `env` vs `build.env`: runtime container vs build. Frontend bundlers
  (`VITE_*`, `NEXT_PUBLIC_*`, `PUBLIC_*`, `REACT_APP_*`, `NUXT_PUBLIC_*`)
  need `build.env`. A `${service.KEY}` reference may only appear in `env`,
  only for an addon declared in the same file, and never under one of
  those browser-exposed prefixes.
- Exposure combinations (validation rejects the rest):
  `public` + `access` never; `internal` + `host` never; `internal` +
  `public` or `internal` + `access` only when the app has `routes:`
  (the routes are what the setting applies to); `access` + `internal` +
  `mode: api` never. `access` needs a login-protected route to attach to.
- `mode: api` with the default (protected) exposure means OAuth: the
  gateway publishes `/.well-known/oauth-protected-resource`, runs PKCE
  against the installation's identity provider and answers requests
  without a token with `401` and `WWW-Authenticate: Bearer
  resource_metadata="…"`. MCP clients handle that flow on their own; an
  `access:` block still decides who is let in. `public: true` on an API
  drops the login; `internal: true` drops the address.
- Every build ends at a Dockerfile. `type: auto` uses your `Dockerfile` if
  the folder has one, else a `.donkey/Dockerfile` you committed earlier,
  else donkey's assistant writes one for this build (`type: ai` forces a
  fresh one; `type: dockerfile` pins a path). An assistant-written
  Dockerfile lives in the server-side build only — **the CLI does not print
  or save it** (the console's build record shows it). To make builds
  repeatable and exact, write the Dockerfile yourself.
- When the app has no addons and no `${…}` env references, a freshly built
  image is started once on the server to prove it comes up before it is
  deployed; a failure there is diagnosed like any other.
- A `storage.paths` folder is a fresh, empty volume mount: anything the
  image itself had at that path is shadowed and invisible. Seed data at
  startup or with `donkey files put` — don't bake it into the image at a
  stored path. An app with storage runs as a single replica.
- The upload never contains `.git`, `node_modules`, `.venv`/`venv`,
  `__pycache__`, `.idea`, `.vscode`, symlinks, `.env*` files, `.DS_Store`
  or key material (`*.pem`, `*.key`, `*.p12`, `*.pfx`, `id_rsa` & co.).
  The deploy says what it left behind. A build that needs one of those
  files will fail — generate it in the Dockerfile instead.

### 2. Write the code against the binding contracts

Addon bindings arrive as environment variables. Read them from the
environment — **never hardcode hosts, credentials or model names**, and
never invent a localhost fallback in code that ships (browser code calling
`http://localhost:3001` is always a bug in production; the deploy will warn
about it).

**postgres** — one per space by default; dev-grade (single replica, no
backups). Injected:

| Variable | Content |
|---|---|
| `DATABASE_URL` | `postgres://user:pass@host:5432/db?sslmode=disable` |
| `PGHOST` `PGPORT` `PGUSER` `PGPASSWORD` `PGDATABASE` | the same, split (libpq convention — psql and most drivers pick these up bare) |
| `JDBC_DATABASE_URL` `JDBC_DATABASE_USERNAME` `JDBC_DATABASE_PASSWORD` | Heroku's JDBC convention for the Java world |

Connect with `psycopg.connect(os.environ["DATABASE_URL"])`,
`new pg.Pool()` (reads `PG*` on its own), Spring's
`JDBC_DATABASE_URL`, etc. **Create the schema in the app's own start path**
(`CREATE TABLE IF NOT EXISTS`, `Base.metadata.create_all`, `db.create_all`, a
migrate call before the server starts) and make it safe to run again: it runs
on every deploy and every restart. donkey runs no job or command of its own
against your image.

**redis** — password-protected, persistent (append-only file). Injected:

| Variable | Content |
|---|---|
| `REDIS_URL` | `redis://default:pass@host:6379/0` (`rediss://` when attached over TLS) |
| `REDIS_ADDR` | `host:6379` — what Go's go-redis and similar clients take |
| `REDIS_HOST` `REDIS_PORT` `REDIS_USERNAME` `REDIS_PASSWORD` `REDIS_DB` | the same, split |
| `REDIS_TLS` | `true`/`false` |

`redis.from_url(os.environ["REDIS_URL"])`, `createClient({ url:
process.env.REDIS_URL })`, `redis.ParseURL(os.Getenv("REDIS_URL"))` — the
URL carries the password, so never read only `REDIS_HOST`.

**rustfs** (S3-compatible object storage) — injected in the AWS SDK's own
convention, so every S3 client works with zero configuration:

| Variable | Content |
|---|---|
| `AWS_ENDPOINT_URL_S3` | `http://rustfs.<namespace>.svc:9000` |
| `AWS_ACCESS_KEY_ID` `AWS_SECRET_ACCESS_KEY` `AWS_REGION` | credentials + region |
| `S3_URL` | the endpoint once more, for apps reading a single var |

There is **no pre-made bucket**. The service is shared by the space, so
create your own app-specific bucket at startup and treat
"already exists / already owned by you" as success:

```python
client = boto3.client("s3", endpoint_url=os.environ["AWS_ENDPOINT_URL_S3"])
try:
    client.create_bucket(Bucket="shop-uploads")
except client.exceptions.ClientError as exc:
    if exc.response["Error"]["Code"] not in ("BucketAlreadyOwnedByYou", "BucketAlreadyExists"):
        raise
```

The endpoint is a bare in-cluster hostname, so the client must use
path-style addressing (boto3 does so by itself for a custom endpoint — the
demo passes only `endpoint_url`; the JS SDK needs `forcePathStyle: true`).
A client that resolves `<bucket>.rustfs…` hostnames is misconfigured.

**openai** — the platform's AI. Nothing runs in the space: apps go through
donkey's in-cluster proxy with a per-space key; the real upstream credential
never reaches the app. Injected:

| Variable | Content |
|---|---|
| `OPENAI_BASE_URL` | the in-cluster endpoint's address, ending in `/v1` (OpenAI-compatible, plain `http://`) |
| `OPENAI_API_KEY` | the per-space stand-in key — send it as the bearer token exactly like a real key |
| `OPENAI_MODEL` | the model the platform actually serves — **use this**, never hardcode `gpt-4o` or similar; the upstream may not have it |

The official OpenAI SDKs (Python and Node) read `OPENAI_BASE_URL` and
`OPENAI_API_KEY` from the environment on their own, so
`client = OpenAI()` plus `model=os.environ["OPENAI_MODEL"]` is the whole
integration. Any plain HTTP client works the same way — POST to
`$OPENAI_BASE_URL/chat/completions` with the key as `Authorization: Bearer`.
Streaming (SSE) passes through.

**Persistent files** — anything the app writes and cannot regenerate
(uploads, an SQLite file, indexes) needs `storage.paths`; each listed path
becomes a folder on the app's one disk and survives redeploys. Without it,
the filesystem is ephemeral.

### 3. Add or attach services (when not declared in donkey.yaml)

`addons:` in donkey.yaml is the default path — the deploy provisions them.
The imperative commands exist for changes to a running app. Know their side
effects: they **rewrite `donkey.yaml`** (normalized — comments in the file
don't survive) and **redeploy the app** without a rebuild so it sees the
change (an app that was never deployed is just told to run `donkey deploy`).

```sh
donkey addon add postgres                 # create in space if missing + connect app
donkey addon add postgres --name analytics --version 16 --storage 5Gi
donkey addon add redis
donkey addon add openai                   # connect the platform's AI
donkey addon list                         # the space's services and who uses them
```

Attaching an existing external service binds *identically* — the app cannot
tell managed from remote. This is also the production path for databases
(the provisioned postgres is explicitly not a production database):

```sh
donkey addon attach postgres --url postgres://user:pass@db.example.com:5432/app
donkey addon attach redis --url rediss://default:pass@cache.example.com:6380/0
donkey addon attach rustfs --url https://s3.example.com   # endpoint only —
donkey env set AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…   # keys are app env
donkey addon attach openai --url https://api.openai.com/v1   # base URL only —
donkey env set OPENAI_API_KEY=sk-…                           # the key is app env
```

Disconnect vs delete — be careful with the second one:

```sh
donkey addon remove db                    # disconnect this app; service + data stay
donkey addon remove db --space --force    # delete the service AND its data
donkey addon remove db --space --keep-data --force   # delete service, keep data
```

### 4. Deploy

```sh
donkey deploy
```

Run it from the app folder (or `--app-dir path`). The output streams:
what stayed out of the upload, detection, the assistant's reading of the
app (port confirmation, health route, data folders, warnings about
localhost addresses or missing services), settings kept from the console,
service provisioning (the first time a space gets a service takes a minute
— image download), one line per build step, `✓ Image ready: <ref>`, then
the rollout. **Read those lines**; they are the review of your donkey.yaml.

The last line of a successful deploy is the app's address
(`→ https://<app>.<space>.<domain>`); `donkey status` repeats it later as
`Address:`. An `internal` app has no address and prints none.

Useful variants:

```sh
donkey deploy --dry-run             # print the k8s objects to stdout, change
                                    #   nothing (progress goes to stderr)
donkey deploy --build-only          # build + push, print the image ref, don't deploy
donkey deploy --no-build --tag REF  # deploy an image donkey built earlier
                                    #   (rollback, or finishing a deploy whose
                                    #   build already printed "Image ready")
donkey deploy --no-wait             # don't wait for health
donkey deploy --platform linux/arm64
```

`--tag` must name an image in donkey's own registry (the refs the deploy
prints); a no-build deploy reuses the decisions (storage, health route)
that image was built with. If the stream drops mid-deploy, the server keeps
going and the CLI polls `status` for up to 10 minutes before giving up.

In a monorepo, deploy each app from its own folder, or run `donkey deploy`
from the repo root: a folder with no `donkey.yaml` of its own is searched
up to three levels down, the shallowest level that holds `donkey.yaml`
files wins, and those apps deploy one after another in path order. A
failure stops the run; the apps before it stay deployed. Give every
`donkey.yaml` the same `space:` so the apps land together and see each
other by name. The upload is always the app's own folder, so
`build.context` can't reach above it from the CLI — a Dockerfile that
needs files from sibling packages has to find them inside the app folder.

### 5. Verify

```sh
donkey status                # healthy or not ("n of m running"), Address, image, pods
donkey logs --tail 100       # what the app writes (default 200 lines;
                             #   -f to follow, --since 30m; --tail 0 -f = only new lines)
donkey logs postgres         # a managed service's logs, by its addon name
donkey apps                  # everything deployed, by space
donkey open                  # open the app's address in a browser
```

A successful deploy already waited for the app to become healthy and
printed its URL — `donkey status` confirming healthy plus clean logs is
normally proof enough. `donkey logs` only knows addons listed in this
app's `donkey.yaml`, and an attached (external) service has no logs here.

Probing over HTTPS yourself has three traps:

- **Login-protected web apps (the default) answer machines with `401`** and
  `WWW-Authenticate: Bearer realm="donkey"` — browsers (an `Accept:
  text/html` request) get redirected to the identity provider instead. A
  401 from the app's address means the login gateway is doing its job, not
  that the app is broken. Health probes don't go through it — they hit the
  pod directly, inside the cluster. To reach the app as a machine you have
  two options: send the CLI's own login token as a bearer —
  `curl -H "Authorization: Bearer $DONKEY_TOKEN" https://…` (the token from
  `donkey login --token`, `$DONKEY_TOKEN`, or the `token:` line of
  `~/.config/donkey/config.yaml`; the gateway accepts it on any
  login-protected web route and applies `access:` to its identity) — or
  mark the paths `public: true`, e.g. the demo's pattern: an `internal` +
  `public` backend claiming `/api` on the frontend's address via `routes:`,
  so exactly those paths skip the login while the UI around them keeps it.
- **`mode: api` apps want an OAuth access token**, not the CLI token: an
  unauthenticated call gets `401` with `WWW-Authenticate: Bearer
  resource_metadata="https://<host>/.well-known/oauth-protected-resource"`.
  An MCP client completes that flow itself; for a curl smoke test use
  `public: true` or trust `donkey status` + `donkey logs`.
- **Private CA**: if the installation's certificates come from an internal
  CA (demo setups do), curl needs `--cacert <ca-file>` — the same file
  `donkey login --ca-file` was given. A brand-new hostname can also be
  unknown to the gateway for a few seconds after the deploy — retry before
  concluding anything.

To test service-backed code from your machine before deploying, tunnel the
space's service to localhost — it prints every binding variable rewritten
to point at the local end, ready to export:

```sh
donkey addon tunnel postgres --port 15432
#   DATABASE_URL=postgres://donkey:…@localhost:15432/app?sslmode=disable
#   PGHOST=localhost  PGPORT=15432  …
donkey addon tunnel redis           # local port defaults to the service's own
```

The line lives only while the command runs. The openai addon has no
tunnel (binding-only, nothing runs in the space), nor does an attached
external service — reach those at their own address.

Seed or inspect the app's disk directly — this works even while the app is
crash-looping or stopped. Paths are **relative to the disk**, whose
top-level folders are one per `storage.paths` entry, named after the path
(`/app/data` → `app-data`, `/var/lib/app` → `var-lib-app`); `donkey files
ls` shows which is which. A leading slash is rejected.

```sh
donkey files ls                         # app-data/ → /app/data in the app
donkey files ls app-data
donkey files put seed.db app-data/seed.db   # the app sees it immediately
donkey files put seed.db app-data/          # keeps the local name
donkey files get app-data/app.log -o app.log
donkey files mv app-data/a.db app-data/b.db
donkey files rm app-data/cache --force
```

### 6. When a deploy fails

A failed build or rollout automatically ends with a plain-language
diagnosis (`What happened: …`) and up to three proposals: an env var to set
(shown as the exact `donkey env set` line), a file edit (shown as a unified
diff), or "retry after you do X". In a terminal without `--yes` the CLI
offers to apply each file edit; **unattended or with `--yes` nothing is
applied** — apply the diff yourself, then redeploy. Then:

1. `donkey logs --tail 200` for the app's own view.
2. Fix the code or `donkey.yaml`, redeploy.
3. If the failure came *after* `✓ Image ready: <ref>` (e.g. the assistant
   endpoint timed out), finish without rebuilding:
   `donkey deploy --no-build --tag <ref>`.
4. `donkey doctor` when the platform itself seems off.

Common causes, in order of likelihood: health check path doesn't exist or
requires auth (probes need a route that answers without credentials); the
code binds a different port than `port:` says (read the injected `PORT`
env var); schema setup at startup crashed the app; the app writes to a path
not listed in `storage.paths` and loses it on restart; the build needed a
file the upload excludes (`.env`, a key file).

## Cleaning up

```sh
donkey destroy --force               # remove everything donkey created for the app
donkey destroy --keep-data --force   # …but keep disks and secrets
```

`destroy` never touches the local folder — `donkey.yaml` stays, and a later
`donkey deploy` brings the app back. The space's services (and their data)
are never touched by `destroy`; `donkey addon remove NAME --space` is the
only command that deletes a service.

Deleting data is final — donkey has no snapshots and no restore. Never pass
plain `--force` on `destroy` or `addon remove --space` unless the user
asked for the data to go too; prefer `--keep-data` when in doubt.

## Command reference

| Command | Purpose |
|---|---|
| `donkey doctor` | client + server health with fixes |
| `donkey login --server URL [--token T] [--ca-file F]` | connect this machine (browser, or `--token` for automation; `$DONKEY_TOKEN` also works) |
| `donkey config show` | which server this machine talks to |
| `donkey init [--name N]` | read the app, write donkey.yaml (never overwrites one) |
| `donkey deploy [--dry-run\|--build-only\|--no-build --tag REF\|--no-wait\|--platform P]` | upload → detect → build → deploy → URL; from a repo root: every app beneath |
| `donkey status` / `donkey apps` | one app / everything, by space |
| `donkey open` | open the app's address in a browser |
| `donkey logs [addon-name] [-f] [--tail N] [--since 30m]` | app or managed-service logs (default 200 lines) |
| `donkey env set K=V… [--no-restart]` / `env unset K…` / `env list [--show-values]` | env vars as cluster Secrets (list masks values without `--show-values`; `PORT` is refused) |
| `donkey addon add TYPE [--name N] [--version V] [--storage SIZE]` | create-if-missing + connect (postgres, redis, rustfs, openai) |
| `donkey addon attach TYPE --url URL [--name N]` | bind an external service identically |
| `donkey addon remove NAME [--space] [--keep-data] [--force]` | disconnect / delete |
| `donkey addon list` / `donkey addon tunnel NAME [--port P]` | inspect / reach a managed service from this machine |
| `donkey files ls\|get\|put\|rm\|mv` | the app's persistent disk (disk-relative paths, `app-data/…`) |
| `donkey destroy [--keep-data] [--force]` | remove the app |
| Global (either side of the subcommand): `--space S`, `--app-dir DIR`, `--yes`, `-v` | scripting and monorepos |

## Rules for agents — recap

1. Declare `addons:` in `donkey.yaml` before the first deploy; one deploy
   brings up services and app together.
2. Read every binding from env (`DATABASE_URL`, `REDIS_URL`,
   `AWS_ENDPOINT_URL_S3`, `OPENAI_BASE_URL`/`OPENAI_MODEL`, …). No
   hardcoded hosts, keys or model names anywhere.
3. Names are DNS labels: lowercase `a-z0-9-`, max 40 chars; `shared` and
   the space's service names are taken.
4. Create your own S3 bucket at startup; tolerate "already exists".
5. Secrets via `donkey env set`, never in `donkey.yaml`.
6. Create the schema in the app's start path, safe to run again on every
   deploy. Irreplaceable files go in `storage.paths`, and a stored path
   starts empty, whatever the image had there.
7. Give the app a `healthcheck.path` that answers without credentials, and
   read the injected `PORT` env var.
8. A `401` from a private app's URL is the login gateway working as
   designed — smoke-test with the CLI token as bearer, through
   `public: true` (or the `routes:` pattern), or trust `donkey status` +
   `donkey logs`; `mode: api` apps need an OAuth token instead; with a
   private CA, curl needs `--cacert`.
9. MCP servers and token-authenticated APIs get `mode: api` (plus
   `cors: allowAll` when browsers call them cross-origin).
10. Non-interactive runs take every default; `--force` only on deletions
    the user explicitly wants. Proposed fixes are never applied for you.
11. The deploy output tells you what the platform decided and why — read
    it, and fix what it warns about. Files paths are disk-relative
    (`app-data/…`), never `/app/data/…`.
