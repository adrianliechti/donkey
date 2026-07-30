---
name: donkey
description: >
  Deploy apps to the donkey platform (a small PaaS on Kubernetes). Use this
  skill whenever you build, deploy, test or operate an app with the `donkey`
  CLI — including creating backing services (postgres, S3-compatible storage,
  the platform's AI, pocketbase), connecting apps to them, seeding disks,
  reading logs and diagnosing failed deploys.
---

# Deploying on donkey

donkey is a PaaS running inside a Kubernetes cluster. You only ever talk to
it through the `donkey` CLI — a thin, token-authenticated client. The server
does everything heavy: it recognizes the app, builds the image (Railpack or
Dockerfile), pushes it to the registry, applies the Kubernetes objects and
watches the rollout. **Never use `docker`, `kubectl` or `helm` for anything
donkey manages** — the CLI is the entire interface, and everything you need
(logs, env vars, files on disks, database tunnels) is a subcommand.

A deploy is: write code + a small `donkey.yaml`, run `donkey deploy` from
the app folder, read the streamed output, verify with `donkey status` and
`donkey logs`.

## Before you start

```sh
donkey doctor        # client + server health, with plain-language fixes
```

If `doctor` reports no login, a human must run
`donkey login --server https://<domain>` once and sign in through the
browser — that is not something you can do yourself. Everything else in
this skill works unattended.

Practical CLI facts for scripted use:

- Most commands need the app's folder: they read `donkey.yaml` to know the
  app and space. Either `cd` there or pass `--app-dir <folder>` — it works
  from anywhere, and flags parse before or after the subcommand.
- Add `--yes` to accept safe defaults; destructive commands additionally
  need `--force` to skip their confirmation prompt. An unanswered prompt
  blocks the run.
- `donkey apps` lists every deployed app by space — use it to see what
  already exists before inventing names.

## Concepts you must know

- **App** — one deployable unit. Its folder holds the code and a
  `donkey.yaml`. Deployed as `<app>.<space>.<domain>`.
- **Space** — a group of apps sharing one namespace. Defaults to the
  repository folder name; override with `space:` in donkey.yaml or
  `--space`. Apps in a space reach each other by plain name:
  `http://backend` — no ports, no full hostnames.
- **Names** (apps, spaces, services) become DNS labels: lowercase letters,
  digits and dashes, starting and ending alphanumeric, at most 40
  characters (`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`). The app name
  `shared` is reserved for a space's services. Pick valid names up front —
  the server rejects the rest.
- **Addons (services)** — postgres, rustfs (S3), openai, pocketbase.
  They belong to the *space*, and apps *connect* to them. Two apps naming
  the same service share it (a monorepo's database is the project's
  database). Connecting injects a canonical set of env vars; the app never
  needs a config file or a secret of its own.
- **Private by default** — every app sits behind an OIDC browser login.
  `public: true` opts out; `access:` narrows to named users/groups/roles;
  `internal: true` removes the public address entirely (siblings still
  reach it by name).

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
the platform's assistant), but when you wrote the app yourself you already
know the answers — writing the file directly is faster and exact. It also
matters unattended: a deploy in a folder with no `donkey.yaml` sets one up
but connects **no services**, because creating a database is not a question
`--yes` answers for you. Declare `addons:` yourself and the deploy provisions
them.

Full reference of the fields you'll actually use:

```yaml
name: shop
space: acme                   # default: repository folder name
port: 8000                    # required; the port the code binds

public: true                  # skip the OIDC login (machine clients, APIs)
internal: true                # no public address; siblings use http://shop
access:                       # narrow the login (any match)
  users: [ada@example.com]
  groups: [staff]
  roles: [developer]
routes:                       # answer on ANOTHER app's address under a path
  - app: dashboard
    path: /api

command: ["node", "server.js"]          # override the image's start command

healthcheck:
  path: /healthz

env:                          # NON-secret runtime values only
  GREETING: hello

storage:                      # one disk; each path a folder on it
  size: 5Gi
  paths: [/app/data]          # paths: [] means "no disk" and is respected

addons:
  - type: postgres            # postgres | pocketbase | rustfs | openai
    name: db                  # optional; defaults to the type. Apps naming
                              # the same service share it.

build:
  type: auto                  # auto | railpack | dockerfile
  env:                        # BUILD-time values — where bundlers inline
    VITE_API_URL: /api        # (a browser bundle can't read runtime env)

replicas: 2
resources:
  requests: { cpu: 100m, memory: 128Mi }
  limits:   { memory: 512Mi }
```

Rules the platform enforces:

- What you write in `donkey.yaml` **wins** — the assistant that reads the
  source never overwrites it, at most warns (e.g. "yaml says 8080, code
  listens on 5000"). So keep the file truthful.
- The container always gets a `PORT` env var set to `port:` — reading
  `PORT` in the code (with your intended port as fallback) is the safest
  pattern and can never disagree with the yaml.
- `env` is for non-secret values. Secrets go through
  `donkey env set KEY=VALUE` (stored as a cluster Secret, app restarts
  automatically). Never write a secret into `donkey.yaml` — it belongs in
  the repository.
- `env` vs `build.env`: runtime container vs build. Frontend bundlers
  (`VITE_*`, `NEXT_PUBLIC_*`) need `build.env`.
- A `storage.paths` folder is a fresh, empty volume mount: anything the
  image itself had at that path is shadowed and invisible. Seed data at
  startup or with `donkey files put` — don't bake it into the image at a
  stored path.

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
on every deploy and every restart.

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

**openai** — the platform's AI. Nothing runs in the space: apps go through
donkey's in-cluster proxy with a per-space key; the real upstream credential
never reaches the app. Injected:

| Variable | Content |
|---|---|
| `OPENAI_BASE_URL` | the in-cluster endpoint's address, `…/v1` (OpenAI-compatible) |
| `OPENAI_API_KEY` | the per-space stand-in key — send it as the bearer token exactly like a real key |
| `OPENAI_MODEL` | the model the platform actually serves — **use this**, never hardcode `gpt-4o` or similar; the upstream may not have it |

The official OpenAI SDKs (Python and Node) read `OPENAI_BASE_URL` and
`OPENAI_API_KEY` from the environment on their own, so
`client = OpenAI()` plus `model=os.environ["OPENAI_MODEL"]` is the whole
integration. Any plain HTTP client works the same way — POST to
`$OPENAI_BASE_URL/chat/completions` with the key as `Authorization: Bearer`.

**pocketbase** — injected: `POCKETBASE_URL`, `POCKETBASE_ADMIN_EMAIL`,
`POCKETBASE_ADMIN_PASSWORD`.

**Persistent files** — anything the app writes and cannot regenerate
(uploads, an SQLite file, indexes) needs `storage.paths`; each listed path
becomes a folder on the app's one disk and survives redeploys. Without it,
the filesystem is ephemeral. An app with storage runs as a single replica.

### 3. Add or attach services (when not declared in donkey.yaml)

`addons:` in donkey.yaml is the default path — the deploy provisions them.
The imperative commands exist for changes to a running app. Know their side
effects: they **rewrite `donkey.yaml`** (normalized — comments in the file
don't survive) and **redeploy the app** so it sees the change.

```sh
donkey addon add postgres                 # create in space if missing + connect app
donkey addon add postgres --name analytics --version 16 --storage 5Gi
donkey addon add openai                   # connect the platform's AI
donkey addon list                         # the space's services and who uses them
```

Attaching an existing external service binds *identically* — the app cannot
tell managed from remote. This is also the production path for databases
(the provisioned postgres is explicitly not a production database):

```sh
donkey addon attach postgres --url postgres://user:pass@db.example.com:5432/app
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
detection, build, service provisioning (the first time a space gets a
service takes a minute — image download), and — line by line — every
decision the platform's assistant filled in (port confirmation, health
route, data folders, warnings about localhost addresses or missing
services). **Read those lines**; they are the review of your donkey.yaml.

The last line of a successful deploy is the app's address
(`→ https://<app>.<space>.<domain>`); `donkey status` repeats it later as
`Address:`.

Useful variants:

```sh
donkey deploy --dry-run             # print the k8s objects, change nothing
donkey deploy --build-only          # build + push, don't deploy
donkey deploy --no-build --tag REF  # deploy an existing image (rollback,
                                    #   or finishing a build that already printed "Image ready")
donkey deploy --no-wait             # don't wait for health
```

In a monorepo, deploy each app from its own folder; give every
`donkey.yaml` the same `space:` so the apps land together and see each
other by name.

### 5. Verify

```sh
donkey status                # healthy or not, Address, image version, pods
donkey logs --tail 100       # what the app writes (-f to follow, --since 30m)
donkey logs postgres         # a managed service's logs, by its addon name
donkey apps                  # everything deployed, by space
```

A successful deploy already waited for the app to become healthy and
printed its URL — `donkey status` confirming healthy plus clean logs is
normally proof enough.

Probing over HTTPS yourself has two traps:

- **Login-protected apps (the default) answer machines with `401`** and
  `WWW-Authenticate: Bearer` — browsers get redirected to the identity
  provider instead. A 401 from the app's address means the login gateway is
  doing its job, not that the app is broken. There is no unauthenticated
  path through the public address (health probes don't need one — they hit
  the pod directly, inside the cluster). To smoke-test endpoints as a
  machine, mark the app `public: true` — or use the demo's pattern: an
  `internal` + `public` backend claiming `/api` on the frontend's address
  via `routes:`, so exactly those paths skip the login while the UI around
  them keeps it.
- **Private CA**: if the installation's certificates come from an internal
  CA (demo setups do), curl needs `--cacert <ca-file>` — the same file
  `donkey login --ca-file` was given.

To test service-backed code from your machine before deploying, tunnel the
space's service to localhost — it prints every binding variable rewritten
to point at the local end, ready to export:

```sh
donkey addon tunnel postgres --port 15432
#   DATABASE_URL=postgres://donkey:…@localhost:15432/app?sslmode=disable
#   PGHOST=localhost  PGPORT=15432  …
```

The line lives only while the command runs. (The openai addon has no
tunnel — it is binding-only, nothing runs in the space.)

Seed or inspect the app's disk directly — this works even while the app is
crash-looping or stopped:

```sh
donkey files ls /
donkey files put seed.db /data/seed.db
donkey files get /data/app.log -o app.log
donkey files rm /data/cache --force
```

### 6. When a deploy fails

A failed build or rollout automatically ends with a plain-language
diagnosis and up to three proposed fixes (proposals are printed, never
applied — applying them is your job). Then:

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
not listed in `storage.paths` and loses it on restart.

## Cleaning up

```sh
donkey destroy --force               # remove everything donkey created for the app
donkey destroy --keep-data --force   # …but keep disks and secrets
```

`destroy` never touches the local folder — `donkey.yaml` stays, and a later
`donkey deploy` brings the app back. The space's namespace (and with it the
shared services) goes away with its last app.

Deleting data is final — donkey has no snapshots and no restore. Never pass
plain `--force` on `destroy` or `addon remove --space` unless the user
asked for the data to go too; prefer `--keep-data` when in doubt.

## Command reference

| Command | Purpose |
|---|---|
| `donkey doctor` | client + server health with fixes |
| `donkey init [--name N]` | read the app, write donkey.yaml |
| `donkey deploy [--dry-run\|--build-only\|--no-build --tag REF\|--no-wait\|--platform P]` | upload → detect → build → deploy → URL |
| `donkey status` / `donkey apps` | one app / everything, by space |
| `donkey logs [addon-name] [-f] [--tail N] [--since 30m]` | app or managed-service logs |
| `donkey env set K=V… [--no-restart]` / `env unset K…` / `env list [--show-values]` | env vars as cluster Secrets (list masks values without `--show-values`) |
| `donkey addon add TYPE [--name N] [--version V] [--storage SIZE]` | create-if-missing + connect |
| `donkey addon attach TYPE --url URL [--name N]` | bind an external service identically |
| `donkey addon remove NAME [--space] [--keep-data] [--force]` | disconnect / delete |
| `donkey addon list` / `donkey addon tunnel NAME [--port P]` | inspect / reach from this machine |
| `donkey files ls\|get\|put\|rm\|mv` | the app's persistent disk |
| `donkey destroy [--keep-data] [--force]` | remove the app |
| Global (either side of the subcommand): `--space S`, `--app-dir DIR`, `--yes`, `-v` | scripting and monorepos |

## Rules for agents — recap

1. Declare `addons:` in `donkey.yaml` before the first deploy; one deploy
   brings up services and app together.
2. Read every binding from env (`DATABASE_URL`, `AWS_ENDPOINT_URL_S3`,
   `OPENAI_BASE_URL`/`OPENAI_MODEL`, …). No hardcoded hosts, keys or model
   names anywhere.
3. Names are DNS labels: lowercase `a-z0-9-`, max 40 chars; `shared` is
   reserved.
4. Create your own S3 bucket at startup; tolerate "already exists".
5. Secrets via `donkey env set`, never in `donkey.yaml`.
6. Create the schema in the app's start path, safe to run again on every
   deploy. Irreplaceable files go in `storage.paths`, and a stored path
   starts empty, whatever the image had there.
7. Give the app a `healthcheck.path` that answers without credentials, and
   read the injected `PORT` env var.
8. A `401` from a private app's URL is the login gateway working as
   designed — smoke-test through `public: true` (or the `routes:` pattern)
   or trust `donkey status` + `donkey logs`; with a private CA, curl needs
   `--cacert`.
9. Non-interactive runs: `--yes` globally, `--force` only on deletions the
   user explicitly wants.
10. The deploy output tells you what the platform decided and why — read
    it, and fix what it warns about.
