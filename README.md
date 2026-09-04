# donkey 🫏

Deploy your apps to Kubernetes without the hard parts.

```
$ donkey deploy
· Packing up shop (space acme) and sending it to the server…
✓ shop will be built with automatic detection (python)
✓ Image ready: registry.example.com/donkey/shop:4f2a9c1b03de-20260724143022
· Keeping /app/uploads on a disk — views.py saves uploaded files there
· Checking the app's health at /healthz
✓ shop is running.
  → https://shop.acme.apps.example.com
```

## The concept

Donkey is a small PaaS for teams that already run Kubernetes. It follows
five ideas:

1. **The cluster does the work.** A control plane (`donkey-server`) runs in
   your cluster and owns everything heavy: recognizing apps, building
   images, pushing to your registry, applying Kubernetes objects, watching
   rollouts. The `donkey` CLI is a thin client that signs in through your
   browser — no Docker, no kubectl, no build tools on laptops.
2. **Two readers, one answer.** Every look at an app combines a **scanner**
   that establishes what the files prove — which lockfile decides the
   package manager, which version files pin the toolchain, whether the
   front end builds to static files and where — with an **assistant that
   reads the source** for everything no scanner can see: the port the code
   really binds, a health route that exists, the folders holding data that
   must survive a restart. The scanner hands the assistant evidence, never
   conclusions, so every fact carries the file it came from. The assistant
   is a required part of the
   pipeline, not a nice-to-have: donkey without a model configured refuses
   to start rather than half-read your app. It only fills in what
   `donkey.yaml` leaves open, and reports every decision line by line.
   See [What donkey works out for you](#what-donkey-works-out-for-you).
3. **Spaces, not a flat list.** An app belongs to a space — by default the
   repository it lives in, overridable with `space:` in donkey.yaml or
   `--space`. A space is one namespace (siblings talk to each other as
   plain `http://backend`) and one level of the address:
   `<app>.<space>.<domain>`. Two teams can both have a `web`.
4. **Private by default.** Deployed apps sit behind an OIDC login run by
   donkey's own gateway (**donkey-gateway**, against your provider, e.g.
   Entra ID) — and `access:` in donkey.yaml (or the console) narrows an app
   further to named users, groups or roles. `public: true` opts an app out.
   No issuer configured — no login, and the install says so.
5. **Services belong to the space.** A database is the project's, not one
   container's: postgres, S3-style storage, and the platform's AI are
   provisioned once per space from a small catalog, and apps *connect* —
   each gets a canonical env contract (`DATABASE_URL`, `AWS_*`,
   `OPENAI_*`) injected, whether the service runs in the cluster or
   somewhere else. See [Services](#services).

## Components

```
 developer machine                     Kubernetes cluster
┌───────────────┐          ┌─────────────────────────────────────────┐
│  donkey CLI   │─sign-in─▶│ donkey-system                           │
│  (thin client)│   HTTP   │  ├─ donkey-server   REST API, detection,│
└───────────────┘          │  │      manifests, AI, rollout watching │
                           │  ├─ donkey-buildkitd  image builds      │──▶ registry
                           │  ├─ donkey-packages  build package proxy│─▶ package registries
                           │  ├─ donkey-ai      openai addon endpoint│──▶ model API
                           │  ├─ donkey-telemetry  OTLP + DuckDB     │
 browser ────80/443───────▶│  └─ donkey-gateway  TLS (ACME/CA/none)  │──OIDC──▶ Entra ID /
                           │       · routing by hostname and path    │        any IdP
     <app>.<space>.<domain>│       · OIDC login                      │
                           │       · access (users/groups/roles) ──▶ app
                           ├─────────────────────────────────────────┤
                           │ donkey-<space>                          │
                           │  app "web"      Deployment · Service    │
                           │  app "backend"  Deployment · Service    │
                           │  service "postgres"  Deployment · PVC   │
                           │  per app: a route entry (ConfigMap)     │
                           │  one PVC per app · env/binding secrets  │
                           └─────────────────────────────────────────┘
```

- **donkey CLI** — talks only to the donkey-server API. `donkey login`
  opens the installation's OIDC sign-in in the browser and keeps the sealed
  token it gets back in `~/.config/donkey/config.yaml`. The single
  exception: `donkey server install` uses your kubeconfig to bootstrap the
  control plane.
- **donkey-server** — stateless API pod; all state lives in Kubernetes
  objects. Deploys arrive as tar.gz uploads and stream progress back as
  NDJSON. Configured entirely via env from the install settings. It also
  serves the **admin console** (React/Tailwind, embedded in the binary) on
  the domain itself: open `https://<domain>` in a browser, sign in at your
  IdP (the gateway runs the login, like for any app — one sign-in covers
  console and apps), and you get the apps overview with health at a glance, live
  logs, selectable 48-hour/7-day/30-day traffic and latency, an env-var editor, restart, an AI "explain what's wrong" for
  unhealthy apps, remove-app, and the system checks. Spaces are managed
  here too — create one before any app exists, remove an empty one — and
  each space card carries its services: a catalog to add from, plus a
  browser per capability (tables, rows and SQL for a database; buckets and
  objects for storage; a streaming chat for the AI — see
  [Services](#services)). Deploys work straight from the browser: drop a
  project folder or a .zip on the Deploy page and donkey reads it first —
  the same [plan](#the-plan-read-first-then-deploy) `donkey init` prints —
  then shows what it found for review before anything is built. Nothing is
  asked before the upload: the port comes out of the code, the disks out of
  the folders the app writes to, the services out of its imports. A dropped
  folder is packed client-side with the CLI's exclusion rules.
  Views are linkable (`#/app/<space>/<name>`) and follow your light/dark
  preference.
- **donkey-ai** — the openai addon's endpoint (same image, `ai`
  entrypoint). It is the one donkey workload apps are *meant* to reach, so it
  runs apart from the admin API: it holds the platform's AI credential and
  read-only access to the binding Secrets, and nothing else. Apps call it with
  the per-space token their binding carries; it swaps that for the real
  credential, which never leaves this process. Removing the service revokes
  the space's access. The console's own AI chat is browser code, so it calls
  its own origin — `donkey-server` forwards `/ai/v1` here without inspecting
  it, because on that origin `/v1` is already the admin API.
- **donkey-buildkitd** — a plain BuildKit deployment; the server drives it
  over gRPC with a Dockerfile (the app's own, or one donkey's assistant
  wrote), and pushes straight to the registry. It is also
  where a freshly built image is run to check it starts. Registry mirrors for
  restricted networks are configured here (`build.registryMirrors`).
- **donkey-packages** — an always-on, read-only endpoint used only by
  Dockerfile build steps. It proxies npm, PyPI, Go module, Maven and NuGet v3
  requests to public registries by default or installation-wide enterprise upstreams.
  BuildKit is the only pod admitted by its NetworkPolicy, and upstream
  credentials stay in this process rather than becoming Docker build args.
- **donkey-telemetry** — a dedicated `telemetry` process role of the single
  `donkey-server` binary: a single-replica OTLP/HTTP metrics receiver with a
  private query API backed by an embedded DuckDB file on its own 2 Gi PVC.
  It keeps 30 days of hourly HTTP histograms, hourly view/visitor
  rollups, and the versioned build records the console's Deployment tab
  renders (a bounded history per app). Only this process opens the file; `Recreate` upgrades preserve
  DuckDB's single-writer ownership. The server can query it, while the gateway
  and apps can only ingest with scope-specific credentials.
- **donkey-gateway** — donkey's own edge (same binary as the server,
  started with `gateway`): traffic enters the cluster here, on ports 80 and
  443. By default it terminates TLS and issues the certificates itself — one per
  hostname via ACME (Let's Encrypt or an internal ACME CA) on demand, or
  minted by its own CA for internal installs (`tls.mode`) — routes by
  hostname and path (from the route ConfigMaps it watches), runs the OIDC
  code flow itself where a route demands a login, seals what it learned
  into an AES-GCM cookie (stateless — replicas just scale), checks the
  app's access rules (users, groups, roles from the ID token's claims) and
  forwards with `X-Auth-Request-*` identity headers. Those headers are
  information for the app, never authorization: donkey's own API is handed a
  short-lived sealed token instead (`X-Access-Token`) and verifies it itself,
  so nothing in the cluster can reach it by typing a header. TLS and auth live here
  because the load-balancer Service sends HTTP and HTTPS straight to it.
  Certificates can come from ACME, donkey's built-in CA, or cert-manager
  Secrets.
- **Per space** — one namespace (`donkey-<space>`) shared by the apps of
  that space, so they reach each other by plain service name — and that
  includes its services: a space's postgres is simply `postgres` to its
  siblings. The namespace is created with the first app (or ahead of time
  in the console). Removing an app never removes its space; removing the
  space is the explicit cascade that deletes its routes, apps, services,
  bindings, disks and retained telemetry. A namespace donkey didn't create
  is never taken over or deleted.
- **Per app** — Deployment (probes, `PORT`, `envFrom` secrets), Service, a
  route entry (`<app>.<space>.<domain>`, https only — the gateway's http
  listener redirects) — a ConfigMap carrying the app's backend, hostname,
  login and access rules as annotations — and Secrets for env vars,
  registry pull auth and service bindings.

Runtime metadata has one small identity vocabulary, spelled `donkey.local/*`
on Kubernetes and `local.donkey.*` (reverse-DNS) on Docker: the installation
is the ownership boundary, the space the lifecycle boundary, app and addon
the logical workload identities, and role its immutable internal purpose. On
Kubernetes every object additionally carries the applicable
[recommended common labels](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/)
(`app.kubernetes.io/name`, `instance`, `version`, `part-of`, `managed-by`) —
on the platform's own workloads and on every deployed app — so donkey's
grouping is legible to standard tooling. Architectural `component` is omitted
because donkey does not know it. Destructive sweeps require the installation
and exact logical scope; selectors and ownership use only `donkey.local/*`;
non-identifying configuration stays in annotations. The
complete reference, per runtime:
[README_KUBERNETES.md](README_KUBERNETES.md#labels-annotations-and-selectors)
and [README_DOCKER.md](README_DOCKER.md#labels).

## Traffic telemetry

Every app and space has a Traffic view in the console with **last 48 hours**,
**last 7 days**, and **last 30 days** ranges. The 48-hour series uses hourly
buckets; week and month series use daily buckets. The default
source is **Gateway**: donkey-gateway records one stable OpenTelemetry
`http.server.request.duration` histogram after an allowed request has been
forwarded. The histogram count supplies request volume; status attributes split
HTTP 200–399 successes, 4xx client errors and 5xx server errors; its standard
explicit buckets supply average, p50 and p95 latency. Logical identity is kept
as the bounded `donkey.space.name` and `donkey.app.name` attributes. This follows
the [stable OTel HTTP metric semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/).

A **view** is a successful top-level HTML document load at the gateway. It does
not pretend to see client-side SPA route transitions. Unique visitors are
distinct signed-in subjects: the gateway HMAC-pseudonymizes them separately per
app and per space before export. DuckDB never receives the subject, email, IP,
user agent, path or query string, and anonymous visitors receive no tracking
cookie. The report shows identification coverage alongside the unique count.

Apps also receive standard exporter defaults:

```text
OTEL_SERVICE_NAME=<app>
OTEL_RESOURCE_ATTRIBUTES=service.namespace=<space>
OTEL_EXPORTER_OTLP_ENDPOINT=http://donkey-telemetry…:4318
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_METRICS_EXPORTER=otlp
```

Explicit app environment values win over these defaults. The injected variables
configure an OpenTelemetry SDK; they do not magically instrument application
code. Use the language's HTTP auto-instrumentation or SDK and emit the stable
HTTP server duration histogram. The console exposes this as **In-app OTEL** and
keeps it separate from Gateway data, because adding both would count the same
request twice. Each app's HMAC token is valid only for its own
[`service.namespace` / `service.name` resource](https://opentelemetry.io/docs/specs/semconv/resource/service/),
so it cannot write another app's series or the trusted edge series.

No Collector is required: the purpose-built service already accepts standard
OTLP/HTTP and owns retention/query semantics. A Collector remains useful when an
installation needs external routing or processing, but it is a pipeline rather
than the datastore. The embedded service uses the official
[DuckDB Go client](https://duckdb.org/docs/current/clients/go) and one writer
process; reports are analytical SQL over compact rollups rather than scans of
gateway logs.
- **Storage** — `storage.paths` in donkey.yaml (or what the assistant
  found) gives an app **one** PersistentVolumeClaim, with every path mounted
  as its own folder on it (`subPath`, derived from the path so data never
  moves). The volume is group-owned via `fsGroup` so the app can write
  whichever user the image runs as; an app with storage runs as a single
  copy with a `Recreate` rollout, because a disk attaches to one machine at
  a time. The disk is the one part of an app a redeploy cannot rebuild, so
  `donkey files` (and the console's file browser) reach it directly: a
  short-lived helper pod mounts the volume, which works whatever the app's
  image contains — and still works when the app is crash-looping or stopped,
  which is when a full disk usually needs looking at.
- **Services** (postgres & co.) belong to the space, not to one app; apps
  hold *connections* — a copy of the service's binding Secret, wired into
  their `envFrom`. The whole model has [its own section](#services).

## What donkey works out for you

Everything below happens on `donkey init` and again on every `donkey deploy`.
Two readers run over the folder and their answers are combined:

| | **Scanner** (in-process, deterministic) | **Assistant** (your OpenAI-compatible model) |
|---|---|---|
| Question | what do the files prove? | what is true about this app? |
| Sees | manifests, lockfiles, version files, framework configs | everything the scanner found, plus the source — it reads the app with `list_files`, `read_file`, `search_files`, `find_files` |
| Answers | runtime, package manager, pinned versions, static output directory, start-command candidates, OS packages the dependencies need — each with the file it came from | app type, port, health route, data folders, backing services, and the app's Dockerfile |
| Fails when | the app is unusual | the endpoint is unreachable → donkey stops and says so |

The detector's verdict is handed to the assistant as evidence, so the model
confirms or corrects it instead of guessing from scratch.

### The plan: read first, then deploy

Both readers run **on the server**, as one step called a plan — the app is
uploaded, read, and checked against the cluster (is this name free, does the
space exist, what runs there now), and out comes everything below plus the
`donkey.yaml` it amounts to. Nothing is built and nothing changes.

That plan is the one thing `donkey init` and the console both show, which is
why neither ever asks for something it could work out:

- **`donkey init`** (and the first `donkey deploy` in a folder) prints it,
  asks the few questions worth asking, and writes the file the server
  rendered. The deploy that follows carries the plan's id, so the assistant
  reads the app once, not twice.
- **The console** shows it as a review screen: name, space, port and health
  route already filled in with where each answer came from, the data folders
  and services as ticked choices carrying the assistant's reason, the
  address the app will answer on, every warning, and anything that *blocks*
  the deploy before a minute of building finds it instead. Editing anything
  re-runs the checks on the spot. The rendered `donkey.yaml` is there to
  copy into the project, because a browser deploy has no folder to write it
  back to.

Plans expire after two hours; losing one costs a re-upload, never a build.

**What comes out**

| Finding | How it is used | Overridable by |
|---|---|---|
| **Build strategy** — the app's own `Dockerfile`, one donkey wrote earlier (`.donkey/Dockerfile`), or one the assistant writes during this build | drives the BuildKit build | `build.type`, `build.dockerfile` |
| **Port** — read from the source (`app.listen(4000)`, `uvicorn --port`, …); "0" means the app honors `PORT` and any port works | container port, Service target, probes | `port` (required in donkey.yaml, never overwritten — a mismatch is reported as a warning) |
| **Health route** — a real unauthenticated route such as `/healthz`; when none exists donkey probes the TCP port instead of guessing a path | readiness + liveness probes | `healthcheck.path` |
| **Data folders** — folders the code writes to and cannot regenerate: uploads, an SQLite file's folder, generated documents, search indexes. Caches, logs, `/tmp` and build output are refused by construction | one PVC per app, each folder mounted as its own `subPath` | `storage.paths` (an empty list means "no disk", and is respected) |
| **Backing services** — e.g. a `DATABASE_URL` default or a `psycopg` import | offered in the plan (ticked, with the reason) and provisioned by the deploy only once confirmed; a plain deploy just reports it | `donkey addon add postgres`, or untick it |
| **Addresses that only work on a laptop** — `http://localhost:3001/api` and friends. The assistant says where the code runs, because that decides whether it's a mistake: in browser code localhost is the *visitor's* machine (always wrong), in server code it's the app's own container (wrong only when it meant another app) | reported as a warning, with the fix | rewrite the call, or set the real address in `build.env` |
| **Failure diagnosis** — on a failed build or rollout, logs and cluster events go back to the assistant | plain-language explanation + up to 3 proposed fixes | proposals are printed, never applied |

**The rules it plays by**

- *What you wrote wins.* A value present in `donkey.yaml` is never
  overwritten — at most commented on ("donkey.yaml says port 8080, but the
  code listens on 5000").
- *Nothing silent.* Every filled-in value is printed as its own line during
  the deploy, with the reason the assistant gave.
- *Nothing written behind your back.* `donkey init` and the console's review
  screen ask before saving a proposed Dockerfile, a storage block or a
  service — and unattended (`--yes`, no terminal) nothing is provisioned at
  all; deploy-time findings are recorded on
  the app's Deployment (never in your files), so a later `--no-build` redeploy
  keeps the same disks and health checks instead of silently dropping them —
  committing them to `donkey.yaml` stays your choice.
- *Secrets never travel.* The summarizer has a hard denylist (`.env*`, keys,
  certificates, anything secret-shaped) and uploads exclude the same. Failure
  diagnosis works on logs, where a denylist has nothing to grip: those are
  redacted on the way out instead (see [Security notes](#security-notes-poc-status)).
- *No half-service.* No reachable model means a clear error at install,
  `donkey doctor` and deploy time — not a quietly worse deploy.

```
$ donkey deploy
· Packing up wiki (space acme) and sending it to the server…
Looking at your app…
✓ wiki will be built with automatic detection (node)
Building the image…
✓ Image ready: registry.example.com/donkey/wiki:4f2a9c1b03de-20260725143022
· Keeping /app/data on a disk — server.js opens data/wiki.db with better-sqlite3
· Keeping /app/uploads on a disk — routes/upload.js writes attachments there
· One 1Gi disk holds those folders. Put a `storage:` block in donkey.yaml to
  resize it, or an empty `paths: []` to do without.
· Checking the app's health at /healthz
Setting up on the cluster…
· One disk (1Gi) for /app/data, /app/uploads
✓ wiki is running.
  → https://wiki.acme.apps.example.com
```

## donkey.yaml

The whole file is optional — `donkey init` writes one, and everything in it
has a working default. It holds no secrets and belongs in the repository.

```yaml
name: shop                    # the app; becomes <name>.<space>.<domain>
space: acme                   # default: the repository's folder name
port: 8000                    # the port your code listens on

public: true                  # skip the login gateway (machine clients)
internal: true                # no address at all; siblings reach it by name
access:                       # narrow the login to named people (any match)
  users: [ada@example.com]
  groups: [staff]
  roles: [developer]          # Entra app roles / Keycloak realm roles
routes:                       # …or answer on another app's address
  - app: dashboard
    path: /api

command: ["node", "server.js"]        # override the image's start command

healthcheck:
  path: /healthz              # empty = check that the port answers

env:                          # non-secret values; `donkey env set` for the rest
  GREETING: hello

storage:                      # one disk, each path its own folder on it
  size: 5Gi
  class: fast-ssd             # default: the cluster's default StorageClass
  paths:
    - /app/data

addons:                       # space services this app uses; the deploy
  - type: postgres            # creates missing ones. postgres | rustfs (S3)
    name: db                  # | openai (the platform's AI).
                              # apps naming the same service share it

build:
  type: auto                  # auto | ai | dockerfile
                              # auto: your Dockerfile if there is one, then
                              # one donkey wrote earlier (.donkey/Dockerfile),
                              # else donkey writes one for this app
                              # ai: write a fresh one even if a Dockerfile
                              # is sitting in the folder
  dockerfile: Dockerfile
  platform: linux/arm64
  env:                        # build-time values (VITE_*, build args)
    VITE_API_URL: https://api.example.com

replicas: 2
host: shop.example.com        # where this app answers: a full hostname, a
                              # plain name to sit under the space (shop →
                              # shop.<space>.<domain>), or "@" for the
                              # space's own address. Omitted = <app>.<space>.<domain>
resources:
  requests: { cpu: 100m, memory: 128Mi }
  limits:   { memory: 512Mi }
```

**How an app is reached.** By default it gets `<app>.<space>.<domain>` behind
the login gateway. `host:` moves it: `"@"` puts it at the space's own address
(`<space>.<domain>` — one app per space can be that front door), a plain name
gives it a label of its own under the space (so the address outlives a rename),
and anything with a dot is taken as an exact hostname. `public: true` drops the login. `access:` narrows it — only
the named users, groups or roles (any match, from the ID token's claims) get
past the sign-in. `internal: true` drops the address — the app has no
hostname, no certificate and nothing on the internet, and its siblings call it
as `http://<name>`. `routes:` puts it on *another* app's address under a path
prefix. All of it can also be changed in the console's Settings tab without a
deploy — a console choice holds until donkey.yaml itself settles the matter,
which always wins.

Those combine, and the demo uses the combination: its backend is `internal`
(no address of its own), claims `/api` and `/mcp` on the dashboard's address,
and is `public` so those two paths stay open for machine clients while the
dashboard around them still asks for a login. Protection attaches per route,
not per hostname, which is what makes that possible. The gateway does the
forwarding, so there is no proxy to write.

**Schema setup belongs to the app.** donkey runs no command of its own
against your image: an app whose tables have to exist before the first
request creates them in its own start path — `Base.metadata.create_all`,
`db.create_all`, a `migrate()` call before the server listens — written so it
is safe to run again on every deploy and every restart. A container that
starts by migrating is one fewer moving part than a container plus a job.

**`env` vs `build.env`.** `env` reaches the running container. `build.env`
reaches the build, which is where a bundler inlines values — a browser bundle
cannot read a runtime variable, because it was built long before the container
started.

## Services

A space owns its services; apps connect to them. Connecting injects the
service's env contract — canonical variable names every ecosystem already
understands — and disconnecting removes them again without touching the
service or its data. Only removing the service *from the space* deletes
anything, and that is where the confirmation lives.

| Type | What it is | Connected apps see |
|---|---|---|
| `postgres` | the relational database (its own Deployment + disk) | `DATABASE_URL`, the `PG*` family, Heroku's `JDBC_*` convention for the Java world |
| `rustfs` | S3-compatible object storage | the AWS SDK convention (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL_S3`, `AWS_REGION`) + `S3_URL` |
| `openai` | the platform's AI — nothing runs in the space | `OPENAI_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_MODEL` |

Ways in and out, all equivalent:

- `donkey addon add postgres` creates the service if the space lacks it and
  connects the app; a second app naming the same service simply joins it.
  `addons:` in donkey.yaml means the deploy itself provisions what's
  missing — a cloned repository comes up with its services.
- `donkey addon attach <type> --url …` binds a service that already runs
  elsewhere (a managed database, a real S3, an OpenAI account). The binding
  is derived from the address, so **the app cannot tell managed from
  remote** — swapping a provisioned postgres for RDS is an attach, not a
  code change.
- The console does the same from the space card's catalog; a service
  connected there reaches the running app immediately and survives later
  CLI deploys.

**The `openai` service hides the real credential.** Its binding points apps
at `donkey-ai` with a generated per-space token; that endpoint checks the
token against the bindings donkey issued, swaps in the platform's actual key
upstream, and streams the answer back (SSE included). The real key never
leaves `donkey-system` — and never sits in the process that writes to the
cluster — and removing the service from the space *is* the revocation.

**Getting at a service yourself.** Services rightly have no public address,
so donkey provides the ways in: `donkey addon tunnel NAME` opens a private
line from your machine (localhost listener, carried over the authenticated
API, alive only while the command runs — the printed `DATABASE_URL` is
psql-ready), and the console opens a browser per capability — tables, rows
and a SQL view for anything with a `DATABASE_URL`; buckets, uploads and
downloads for anything speaking S3; a streaming markdown chat for the AI,
using the very token and proxy an app would.

## Requirements for a real deployment

donkey installs into a cluster you already run, and stays inside its own
namespaces. It **never modifies cluster-wide settings**: no CoreDNS edits, no
files on nodes, no admission webhooks, no StorageClasses, and no CRDs unless
the configured certificate or DNS source needs them — everything
it applies is in `donkey-system`, in a `donkey-<space>` namespace, or is one of
the few cluster-scoped RBAC objects it names after itself. A test asserts
exactly that (`TestInstallTouchesOnlyItsOwnObjects`). Configured with a
[workload namespace](README_KUBERNETES.md#namespaces-and-restricted-clusters) it applies nothing
cluster-scoped at all.

What has to be there first (`donkey server install` checks these before it
applies anything, and `donkey doctor` keeps checking them after):

| Requirement | Why | Notes |
|---|---|---|
| **Kubernetes cluster** with a default StorageClass | donkey-telemetry always claims its 2 Gi DuckDB volume; apps with `storage.paths` and provisioned services claim their own | any conformant cluster; nothing donkey-specific. Donkey brings its own edge, normally as a `LoadBalancer` Service; on clusters where nothing assigns those an address (kind), set `gateway.serviceType: NodePort` or `ClusterIP`. cert-manager is required only for `tls.mode: cert-manager`; external-dns with `--source=crd` only when `gateway.externalDNS` is set |
| **Container registry** the cluster can pull from | donkey pushes every build there | `registry.url` + credentials; `registry.caSecret` when its certificate comes from a private CA |
| **DNS** pointing public hostnames to the gateway address | apps are served at `<app>.<space>.<domain>`, the API and login callback at `<domain>` | configure a wildcard plus apex yourself, or let `gateway.externalDNS` publish exact records |
| **OIDC provider** (Pocket ID, Entra ID, Keycloak, …) | the one way in: console, CLI and protected apps all sign in against it | required — `auth.issuer` (+ client id/secret); `auth.admins` optionally narrows who may administer |
| **OpenAI-compatible endpoint** | donkey reads every app with it (ports, health routes, data folders) | required — the server refuses to start without one. Point it at a model you control: source summaries go there |

A complete copy-paste walkthrough that stands all of this up on a single
Debian server — TLS comes from the gateway itself via Let's Encrypt — exists
twice: [examples/INSTALL_KIND.md](examples/INSTALL_KIND.md) (kind cluster, a
registry container, an OIDC provider) and
[examples/INSTALL_DOCKER.md](examples/INSTALL_DOCKER.md) (no Kubernetes at
all — `runtime: docker` runs apps straight on the Docker engine).

RBAC: by default donkey-server binds `cluster-admin`, because it creates and
deletes namespaces and no Role can express that. Giving it a workload
namespace instead drops it to two namespaced Roles — see [README_KUBERNETES.md](README_KUBERNETES.md#namespaces-and-restricted-clusters).

### Installation modes (Kubernetes)

Four arrangements change the shape of a Kubernetes installation, all
documented in [README_KUBERNETES.md](README_KUBERNETES.md):

- **[Namespaces and restricted clusters](README_KUBERNETES.md#namespaces-and-restricted-clusters)**
  — a namespace per space (the default), or one workload namespace you were
  given: qualified object names, and no cluster-scoped grant at all.
- **[Isolating spaces from each other](README_KUBERNETES.md#isolating-spaces-from-each-other)**
  — the per-space ingress NetworkPolicy fence (`kubernetes.isolateSpaces`,
  on by default) and its opt-in egress half
  (`kubernetes.restrictEgress`), which confines a space's pods to their own
  space, the platform endpoints and DNS.
- **[Incoming traffic, certificates and DNS](README_KUBERNETES.md#incoming-traffic-certificates-and-dns)**
  — one load-balancer VIP into donkey-gateway, with ACME, built-in CA, or
  cert-manager as its certificate source and optional external-dns records.
- **[Several donkeys on one cluster](README_KUBERNETES.md#several-donkeys-on-one-cluster)**
  — what must differ per installation, and why the system namespace is the
  installation's identity.

The Docker runtime's shape — per-space networks as the isolation model, the
`local.donkey.*` labels, the Compose bundle — is described in
[README_DOCKER.md](README_DOCKER.md).

### The builder

An app that brings a Dockerfile is built with it, always. Every other app
gets one written for it, and that is the whole of the build system: there is
one path to the build engine, and what an app is built with is a file you can
read, edit and keep.

Writing it is an investigation, not a guess. donkey's scanner establishes what
the files prove — the lockfile that decides the package manager, the version
files that pin the toolchain, the framework config that names the build output
directory, the dependencies that need OS packages — and hands the assistant
that evidence with the file behind each fact. The assistant then reads the app
for itself, with read-only tools confined to the upload, before it writes
anything: which directory is really the app in a monorepo, the module path of
a Django project, whether the code honors `PORT`. A few well-chosen reads
settle what a file tree can only hint at.

Because the output is an ordinary Dockerfile, everything it names — `FROM
python:3.12-slim`, `apt-get`, `npm ci` — is an ordinary registry or package
pull that `registryMirrors` and the internal package proxy cover. There is
nothing else to reach.

**The Dockerfile is the deliverable, not the model.** donkey prints what it
wrote and tells you to save it as `.donkey/Dockerfile`. Once it is there,
detection prefers it, so every later deploy is a plain, repeatable Dockerfile
build with no model in the path at all. The assistant is how the file gets
written the first time — not a step in every build.

**Built images are run before they are published.** A generated Dockerfile
fails in ways a build log cannot show, because the build succeeded: an
interpreter that is not on `PATH`, a dependency that was never installed, an
entrypoint that exits at once. So donkey starts the image, waits for it to
listen on the port it will be deployed with, and only exports it once it
does. When it does not, the failure goes back to the assistant with the log —
and with every earlier attempt, so it cannot oscillate between two broken
variants — and it writes another one, `build.attempts` times, three by
default.

The check is deliberately narrow: it fails an image whose process *exits*,
and passes one that is still running but not yet listening. An app whose
first act is to open its database would otherwise fail in a sandbox that has
no database, and be "fixed" by a model with nothing to fix.

```yaml
build:
  verify: true          # default; false skips running the image
  verifySeconds: 45     # how long the app gets to come up
  attempts: 3           # the first Dockerfile plus two revisions
  provenance: min       # SLSA provenance attestation: min (default) | max | off
  sbom: true            # attach an SPDX SBOM to every image (off by default)
```

**Every image carries its paperwork.** BuildKit attaches a standard SLSA
provenance attestation to every push (and an SPDX SBOM when opted in), and
the image config carries the OCI `org.opencontainers.image.revision`/`created`
labels — so any registry tool can answer where an image came from without
knowing donkey. `provenance: max` additionally embeds the Dockerfile and
every build-arg **value** into the pushed attestation; never choose it while
`build.env` carries credentials, which is exactly why `min` is the default.
The SBOM stays opt-in because it pulls a scanner image (covered by
`registryMirrors`) and lengthens every build.

**Every deploy leaves a versioned record.** The full release view — plan,
findings, complete build log, the exact Dockerfile the image was built from,
and the decisions the deploy settled — is stored per build in
donkey-telemetry (bounded history per app), which is what the console's
Deployment tab renders and lets you browse. A `--no-build --tag` rollback
adopts the decisions recorded with that image, not whatever a newer analysis
found.

### Restricted networks

`build.registryMirrors` is written into the build engine's own configuration,
so it covers every image a build pulls — every `FROM` in a Dockerfile,
generated or not. Dockerfiles and BuildKit progress keep the canonical source
name (for example `docker.io/library/node`) even while the registry requests go
to the configured mirror; donkey prints the effective mapping immediately
before that progress. As in BuildKit itself, an unavailable mirror falls back
to the source registry; a restricted network may then show Docker Hub in the
final error even though the mirror was tried first.

Every installation also runs `donkey-packages`. donkey declares its internal
npm, pip, Go, Maven and NuGet settings after every `FROM` in an in-memory
Dockerfile overlay, so they reach every `RUN` in every stage without changing
the app's file. The public defaults are npmjs, PyPI, `proxy.golang.org`, Maven
Central and the NuGet v3 service index; only the proxy process sees configured
upstream URLs or credentials. The package-variable names are platform-owned,
so `build.env` cannot accidentally bypass the proxy or expose an upstream
credential as a build arg.

```yaml
build:
  instructions: |
    Use base images only from registry.example.com/dockerhub or /ghcr.
    Docker Hub official images are under registry.example.com/dockerhub/library/.
  registryMirrors:
    docker.io: https://registry.example.com/dockerhub
    ghcr.io: https://registry.example.com/ghcr
  packages:
    npm: https://artifactory.example.com/api/npm/npm
    pypi: https://artifactory.example.com/api/pypi/pypi/simple
    go: https://artifactory.example.com/api/go/go
    maven: https://artifactory.example.com/maven-remote
    nuget: https://artifactory.example.com/api/nuget/v3/nuget/index.json
    # resourceHosts: [artifacts.example.com]
    credentialHosts:
      - artifactory.example.com
    username: package-reader
    # password: … # or DONKEY_PACKAGES_PASSWORD
```

NuGet builds receive the internal v3 service index as `DONKEY_NUGET_SOURCE`.
Modern NuGet requires a `NuGet.Config` entry with
`allowInsecureConnections="true"` for the cluster-local HTTP hop; the
Dockerfile assistant creates it using `%DONKEY_NUGET_SOURCE%` and passes
`--configfile`. Maven 4 receives
`MAVEN_REPO_CENTRAL`, and Maven Wrapper bootstrap receives `MVNW_REPOURL`.
Maven 3 requires a mirror in `settings.xml`; the assistant creates one from
`DONKEY_MAVEN_MIRROR`. Existing hand-written Maven 3 or modern NuGet
Dockerfiles must consume these variables in the same way.

Package credentials use Basic authentication, which covers the usual
Artifactory/Nexus username-and-token setup. The same explicit read-only
credential is sent only to `credentialHosts`. When a PyPI or NuGet index names
metadata or artifacts on another HTTPS host, add that host to `resourceHosts`;
also add it to `credentialHosts` only when it needs authentication. An omitted
`packages` block needs no credentials and uses the public defaults.

Credentials can instead come from standard Docker config JSON. On Kubernetes,
name an existing Secret in the control-plane namespace:

```yaml
build:
  packages:
    npm: https://npm.mirror.example.com/npm
    pypi: https://pypi.mirror.example.com/simple
    go: https://go.mirror.example.com
    maven: https://maven.mirror.example.com/maven2
    nuget: https://nuget.mirror.example.com/v3/index.json
    credentialHosts:
      - npm.mirror.example.com
      - pypi.mirror.example.com
      - go.mirror.example.com
      - maven.mirror.example.com
      - nuget.mirror.example.com
    dockerConfigSecret: oci-mirror-example-com
    dockerConfigHost: oci.mirror.example.com
```

The Secret may be `kubernetes.io/dockerconfigjson` or a generic Secret holding
one or more Docker config files. On the Docker runtime use an absolute host
path as `dockerConfigDir` instead. `dockerConfigHost` selects the one `auths`
entry that holds the shared read-only credential; it need not be a package
hostname. `credentialHosts` remains the exact allowlist of HTTPS destinations
that may receive that credential. Naming a Docker config source does not by
itself authorize credentials to be sent there. The files are reread per
request, so projected Secret or file rotation needs no restart. A missing
source entry, an incomplete credential, or an upstream `401/403` makes
`donkey-packages` unready instead of silently sending anonymous build traffic.
Explicit passwords are preserved across reinstalls. `registry.caSecret`/
`registry.caFile` is also trusted by the package proxy for an in-house upstream.

`build.env` still reaches every build step for settings not owned by the
package proxy; donkey layers it under each app's own `build.env`, so app values
win for those names.

`build.instructions` extends the Dockerfile-writing assistant's built-in
instructions with installation-specific constraints or hints—the available
image prefixes, required base-image families, or internal build conventions.
It is sent to the configured model, so keep credentials out of it. Use it when
the saved generated Dockerfile itself must name an internal image prefix.
`registryMirrors` is the transparent transport mapping and also covers
hand-written Dockerfiles.

One image is **not** covered by those mirrors: `buildkitImage`, which the
cluster pulls itself. Mirror it by setting it directly. Managed PostgreSQL
and RustFS instances are runtime pulls too, so point those at
mirrored repositories explicitly (donkey appends the service version):

```yaml
buildkitImage: registry.example.com/dockerhub/moby/buildkit:v0.31.2
addonImages:
  postgres: registry.example.com/dockerhub/library/postgres
  rustfs: registry.example.com/dockerhub/rustfs/rustfs
```

When a direct image override uses the same host as `registry.url`, donkey
reuses that registry's username and password for the Kubernetes or Docker
pull. Credentials are scoped to that host and are not sent to public or
unrelated registries. On Kubernetes, an enterprise registry operator can
instead manage the credentials and their rotation:

```yaml
addonImagePullSecrets:
  - registry-example-com
```

These must be existing `kubernetes.io/dockerconfigjson` Secrets in every
workload namespace where a managed addon can run. Donkey references them
without reading or copying their contents; when the list is set, it replaces
the pull secret donkey would generate from `registry.url` credentials.

The same-host rule also authenticates BuildKit mirrors. A separately hosted
authenticated mirror can provide build credentials through an existing
Docker-config Secret (`registry.dockerConfigSecret`) on Kubernetes or an
absolute host directory (`registry.dockerConfigDir`) on Docker. Donkey mounts
the source into `donkey-server`, selects credentials by requested host and
rereads the files for every build, so rotation does not require a restart.
The push registry itself still uses `registry.username` plus its password; the
Docker config is for separately hosted mirrors. The registry URL remains an
image prefix: builds are pushed as
`<registry.url>/<app>:<tag>`, so a setting such as
`registry.example.com/platform/donkey` keeps every pushed app image below that
enterprise folder.

Mirror hosts inherit `registry.caSecret`, including the separate Bearer-token
request made by donkey-server on BuildKit's behalf, so an in-house Artifactory
behind a private CA needs nothing further. The referenced Secret must be in the
control-plane namespace and contain the trust bundle under exactly `ca.crt`;
the installer checks it before applying changes. There is no builder image for
`donkey doctor` to resolve — a generated Dockerfile names its own bases, and
whether those resolve is a question only the finished file can ask, which the
build engine asks through exactly these mirrors.

## Deployment model

**Day 0 — admin deploys the control plane.** Start from the short generated
settings file ([examples/server-install.yaml](examples/server-install.yaml));
flags override it and secrets come from env:

```sh
donkey server init --runtime kubernetes > server-install.yaml
# edit image, registry, domain, OIDC and model settings
donkey server install -f server-install.yaml
```

Prints where to sign in — there is no API token: the console and the CLI
authenticate through the same OIDC login the apps use, and `auth.admins`
(users/groups/roles, matched like an app's `access` rules) narrows who may
administer; empty means everyone the IdP admits. Re-running with changed
settings updates the installation idempotently (server-side apply; existing
secrets and sessions survive; config changes roll the affected pods).

For GitOps, review, or a machine where the CLI must not hold cluster access,
render the same settings instead:

```sh
donkey server render -f server-install.yaml --output deploy/
```

Kubernetes output contains `manifests.yaml`, an apply helper, and a
`secrets.env.example` containing names only — no Secret objects or credential
values are written. `runtime: docker` renders `compose.yaml`,
`buildkitd.toml`, and an equivalent `up.sh` bundle. The direct installer and
both renderers share the install-file validation; Kubernetes uses the same
canonical object generator in both paths.

**One callback, one sign-in.** Every login under the domain rides through a
single OAuth callback on the domain itself
(`https://<domain>/oauth2/callback`) — one redirect URI to register at any
OIDC provider, no wildcard support needed. The session cookie lives on the
domain, so signing in to one app signs you in to all of them. Only apps
with a `host:` outside the installation's domain keep a per-host
callback (`https://<their-domain>/oauth2/callback`) — register those
individually.

**Certificates and DNS.** Every request reaches donkey-gateway directly;
`tls.mode` changes its certificate source, not the traffic path. `acme`
requests exact-host certificates from Let's Encrypt or an internal ACME
directory, while `ca` mints them from donkey's own CA for private installs.
On Kubernetes, `cert-manager` creates one Certificate per public app and one
for the control-plane hosts; the gateway consumes and reloads the generated
TLS Secrets directly. This keeps both ACME and CA useful in Kubernetes without
requiring cert-manager. DNS can remain external, or `gateway.externalDNS` can
emit exact `externaldns.k8s.io/v1alpha1` DNSEndpoint records aimed at the
gateway's stable load-balancer VIP. See the
[Kubernetes traffic contract](README_KUBERNETES.md#incoming-traffic-certificates-and-dns).

**Day 1 — everyone deploys:**

```sh
donkey login --server https://apps.example.com   # once — opens your browser
cd your-project && donkey deploy
```

`donkey login` opens the installation's OIDC sign-in in the browser and
receives a sealed token on the loopback — 30 days of CLI access from one
sign-in, no credential ever typed into a terminal. (`--token` accepts an
existing token for automation.)

| Command | What it does |
|---|---|
| `donkey` | friendly overview and next step |
| `donkey doctor` | client + server health with plain-language fixes |
| `donkey init` | reads the app on the server (detector + assistant + cluster checks), writes the `donkey.yaml` that plan amounts to |
| `donkey apps` | everything deployed, grouped by space |
| `donkey deploy` | upload → detect → build → deploy → URL (`--dry-run`, `--build-only`, `--no-build --tag`, `--no-wait`, `--platform`) |
| `donkey status` / `logs -f` / `open` | day-2 basics (scoped to the folder's space) |
| `donkey env set/unset/list` | env vars as cluster Secrets, auto-restart |
| `donkey files ls/get/put/rm/mv` | what's on the app's disk — seed it, fetch something out, clear it |
| `donkey addon add/attach/remove/list` | the space's services (see [Services](#services)); `remove NAME` disconnects the app, `remove NAME --space` deletes the service itself |
| `donkey addon tunnel NAME` | reach a service from your machine — a private line while the command runs; services never get a public address |
| `donkey destroy` | remove everything donkey created for the app (`--keep-data` spares disks and secrets — a kept database keeps its credentials; deleting data always asks — `--force` for scripts) |

Image tags are `<git-sha>-<timestamp>` (the CLI reads the sha locally and
sends it along; uploads never include `.git`), so every deploy is traceable
and always rolls. Two deploys of one commit inside the same second reuse the
tag and overwrite it, which costs nothing on an ordinary registry — set
`registry.immutable: true` when yours refuses to overwrite tags (ECR tag
immutability, GHCR immutable tags, a Harbor rule) and donkey adds a random
suffix so no tag is ever written twice.

## Running it in production

What an operator has to know before a team depends on the installation. None
of this is a gap waiting to be filled — it follows from how donkey is built,
so plan around it.

**Sessions and revocation.** One secret underlies every login: the session
key in `donkey-system-config` seals the browser cookies (12 h) and the CLI
tokens (30 days), all derived from an OIDC sign-in. Individual logins expire
on their own and revive with a fresh sign-in; kicking one specific person out
happens at the IdP (or by removing them from `auth.admins` and the affected
apps' `access` rules — the gateway checks those on every request). The big
lever is rotating the session key itself: delete the `session-key` entry from
the Secret and re-run the install — a new key is generated, every cookie and
every CLI token everywhere is invalid the moment the pods roll, and everyone
signs in again through the browser. **There is no overlap window.**

Running app workloads are not rolled: their pods, disks and routes stay as
they are while donkey-server restarts. A re-install reconciles each app's
Certificate and optional DNS record from its existing route, so changing the
certificate source or DNS publication applies immediately; unchanged objects
are idempotent server-side applies. A re-install also keeps every secret it
isn't given again
(registry password, OIDC client secret, AI key), so the same settings file is
enough — rotating one thing must never blank the rest.

**Metadata v2 is a clean cutover.** It deliberately does not claim resources
from versions that used the old ownership labels or selectors. Kubernetes
Deployment selectors are immutable, so adopting only their top-level labels
would create a half-migrated installation that cannot reconcile. At this
early stage, remove the old app/control-plane resources and recreate them;
new Deployments, Pods, Services, routes, services and Docker resources are
then born with one consistent schema. Back up data first if an old PVC or
Docker volume is not disposable — deleting a space intentionally deletes it.

**What to back up.** Donkey state lives in Kubernetes objects and the telemetry
PVC, and nearly
all of it is reproducible: images rebuild from the repositories, manifests
re-render from `donkey.yaml`, namespaces and routes are re-created by the next
deploy. What is *not* reproducible has to live outside the cluster:

| Keep | Why |
|---|---|
| the install settings file (`server-install.yaml`) | every install, re-install, or re-render needs it; it is the written description of the installation |
| `DONKEY_REGISTRY_PASSWORD` | donkey stores it, but never invents it; without it the server can't push and apps can't pull |
| `DONKEY_OIDC_CLIENT_SECRET` (with its client id) | belongs to a registration in your IdP — a new one means updating the IdP as well |
| `OPENAI_API_KEY` | the assistant is required; no key, no deploys |
| the `session-key` entry of `donkey-system-config` | a fresh install **generates a new one** and thereby invalidates every session and CLI token. Restoring it makes the rebuilt installation the same installation — everyone stays signed in |
| the `telemetry-key` entry of `donkey-system-config` | existing app exporters carry credentials derived from it. Restoring it keeps those exporters valid and lets a restored telemetry database continue accepting them independently of login-session rotation |
| the registry CA secret (`registry.caSecret`) | donkey only *references* that Secret in the configured control-plane namespace — it never creates it. Whatever created it has to run again |
| DNS for `*.<domain>` and `<domain>` | they point at the gateway address, which a rebuilt cluster changes |

Two more things exist only in the cluster: env values set with `donkey env
set` (the ones in `donkey.yaml` come back with the app, the rest don't), and
the generated credential of a provisioned service — which matters only as
long as its data does.

The `donkey-telemetry-data` PVC is cluster-only too; its database file is
`/var/lib/donkey/telemetry.duckdb`. Losing it does not
affect apps, but it erases the current 30-day traffic history and the
retained build records; include it in volume snapshots when that history
matters.

**PVC data is the one thing that cannot be reconstructed.** Everything above
is a copy of something you have; a volume is not. donkey never snapshots,
exports or restores a PersistentVolumeClaim — neither an app's `storage.paths`
volume, a provisioned addon's data disk, nor the telemetry DuckDB disk — and it has no restore command to
add later in a hurry. `donkey destroy` without `--keep-data` deletes them
(interactively it asks first, `--force` skips the question), and that is
final: no donkey command brings that data back. Backing volumes up is your
cluster's job — VolumeSnapshots, Velero, the storage system's own snapshots, a
dump job inside the app — and donkey neither knows about it nor gets in its
way.

**The provisioned database is not a production database.** `donkey addon add
postgres` gets an app a real postgres in one command, and that is all it is
built for:

- one replica with a `Recreate` rollout — every restart, node drain and
  upgrade is downtime;
- no backups, no WAL archiving, no point-in-time recovery — its disk is
  exactly as unrecoverable as the paragraph above says;
- `sslmode=disable` in the binding: the connection stays inside the cluster
  network, unencrypted;
- a mutable image tag (`postgres:16`) — a pod re-created months later can come
  up on a different patch release.

The way out is already there and needs no new feature. Attaching a managed
database binds identically — same binding Secret, same keys (`DATABASE_URL`,
`PG*`), same injection — and the app cannot tell the difference:

```sh
donkey addon attach postgres --url postgres://user:password@db.example.com:5432/app
```

Provisioned is for development and small internal apps. Attached is the
production path.

**When the assistant is unreachable.** A building deploy needs the assistant
by design and fails with a clear message — that is the point of "no
half-service". Two properties make an outage survivable:

- *The analysis runs alongside the build, not before it.* By the time the
  model is missed, the image is built and pushed: the deploy has already
  printed `✓ Image ready: <ref>`, and the failure hands the same reference
  back. `donkey deploy --no-build --tag <ref>` finishes the deploy without
  rebuilding anything. (A deploy waits up to 5 minutes for an answer before
  it gives up.)
- *`--no-build` deploys never ask the assistant at all.* They adopt what the
  previous deploy decided — storage paths and health route, kept as an
  annotation on the Deployment — so rollbacks to an older tag and emergency
  redeploys keep working while the endpoint is down. So do `donkey env
  set`, `logs`, `status`, `addon` and `destroy`, and the console's restart;
  only reading an app (`init`, a building `deploy`) needs a model.

`donkey doctor` reports the endpoint like every other dependency, so an
outage is visible before someone hits it mid-deploy.

Both places that commit to a configuration prove the endpoint answers first.
`donkey server install` reaches it before changing anything and stops with the
installation untouched — an install is a one-shot command with a person
watching, which makes it the cheapest place to find out the endpoint is wrong.
`donkey-server` does the same at startup and refuses to come up: under
Kubernetes the pod then stays unready and out of rotation with the reason in
its logs, which beats a control plane that looks healthy and fails on its
first deploy.

## Embedding the CLI

The donkey command tree can be mounted as a subcommand of another
[urfave/cli](https://github.com/urfave/cli) application:

```go
import (
	donkeycli "github.com/adrianliechti/donkey/pkg/cli"
	urfavecli "github.com/urfave/cli/v3"
)

root := &urfavecli.Command{
	Name: "platform",
	Commands: []*urfavecli.Command{
		donkeycli.Command(donkeycli.RunOptions{
			DisableManagement: true,
			Server:            "https://donkey.example.com",
		}),
	},
}
```

`DisableManagement` omits `donkey server` and the server-only `--context`
flag. `Server` supplies the default server address: an explicit `donkey login
--server` overrides it, and the selected address is saved for later commands.
Client commands such as `deploy`, `status`, and `login` remain available. The
standalone entry point is also exposed as `cli.Run(ctx, args, options)`.

## Repository structure

```
cmd/donkey            CLI entry point          cmd/donkey-server  control plane entry point
web/                  admin console (Vite + React + TS + Tailwind, lucide icons; embedded via go:embed)
internal/
  cli/                one file per command; session/config wiring
  apiclient/          CLI↔server transport: tar upload, NDJSON stream, typed calls
  server/             REST handlers, deploy pipeline, env-based config
  detect/             build strategy decision (donkey.yaml > Dockerfile > the installation's builder > AI)
  build/              BuildKit gRPC builder (dockerfile.v0), startup verification, registry auth, tags
  k8s/                client-go wrapper: server-side apply, rollout wait, logs, env secrets
  k8s/manifests/      pure config→object rendering (app + control plane) — golden-tested
  addons/             service catalog: postgres, rustfs, openai (provisioned & external bindings)
  ai/                 Assistant interface (analysis + failure diagnosis), OpenAI impl, secret-safe summarizer, log redaction
  config/             donkey.yaml, client config, install settings file
  doctor/             client-side check engine behind `donkey doctor`
  ui/                 all terminal output: plain language, jargon behind -v
  gitutil/            pure-Go .git reader: HEAD sha (image tags), repo root (default space)
pkg/
  cli/                public facade for embedding the donkey command tree
  runtime/            runtime implementations shared by the server and management CLI
testdata/             deployable samples incl. a kitchen-sink monorepo; golden files
examples/             finished-project demos + the concise server-install.yaml
                      emitted by `donkey server init`
hack/                 dev/demo scaffolding only — never part of the product
                      (cluster.sh → devkube, demo-ca.sh, demo/fake-idp)
```

Conventions: implementation stays under `internal/`, with the supported
embedding and runtime surfaces under `pkg/`; `config` and `k8s/manifests`
are pure (no I/O) and unit-tested; nothing in the product shells out to
external CLIs (the one exception: `donkey open` launching your browser);
demo concerns are code-review-enforced out of the product
(`TestSystemObjectsSelfContainedEdge`, `TestInstallTouchesOnlyItsOwnObjects`).

## Trying it out

The local playground is [devkube](https://github.com/adrianliechti/devkube),
which brings exactly the prerequisites above: a cluster and an in-cluster
registry the nodes already trust. donkey is then installed with the same
`donkey server install` an admin runs (`tls.mode: ca` — the gateway mints
its own certificates locally) — the demo adds nothing but a deliberately
trivial auto-approving identity provider (`hack/demo/fake-idp/` — everyone
is instantly demo@example.com).

```sh
task demo            # kind cluster + control plane + the demo app (dashboard, MCP backend, three services)
task test            # unit tests (no cluster needed)
task e2e             # from nothing to verified: setup:kind + install:kind + the Go e2e suite
task demo:samples    # deploy every testdata sample; all of them must come up
task demo:broken     # deploy apps that MUST fail, and check each was explained and patched
```

The phases are tasks of their own — and the docker runtime has its own
install:

```sh
task setup:kind      # the local kind cluster and donkey's prerequisites
task install:kind    # the identity provider, the CA, and donkey itself
task install:docker  # donkey straight on the local Docker engine (apps.localhost, no cluster)
task demo:app        # log in and deploy the demo app (kind; needs devkube connect)
task verify          # the Go e2e suite against the running kind installation
task verify:docker   # the same suite against the running docker installation
```

### Scoring the builder

donkey's builder writes each app's Dockerfile, so the thing to measure is how
often it gets an app right. Three rungs, each costlier and more convincing
than the last:

```sh
task builder:detect  # 122 apps · no model · no builds · seconds
task builder:write   #  10 apps · a model  · no builds · minutes
task demo:samples    #  19 apps · a model  · real builds and deploys · ~20 min
```

**`builder:detect`** is the deterministic rung. The hard part of measuring a
builder is a corpus of real projects with known-correct answers, and Railpack
has one: 122 example apps, each built and HTTP-checked in its CI, each with
the build plan railpack commits for it (`task builder:corpus` fetches them).
This compares what donkey works out by reading files — runtime, package
manager, static site and its output directory, start command — with what
railpack concluded for the same app, and prints a percentage per question.
That is the number to watch when a detection rule changes; the gate fails
under 95%.

**`builder:write`** needs an OpenAI-compatible endpoint (`OPENAI_BASE_URL`,
`OPENAI_API_KEY`, and `DONKEY_MODEL` to choose the model). It has a real model
write Dockerfiles for the apps in `testdata/` and checks the failures that are
cheap to catch and expensive to ship: a missing CMD, a `:latest` base image, a
development server in production, a static site that did not end up on a web
server.

**`demo:samples`** is the one that actually proves it: every app in `testdata/`
built, run to check it starts, pushed and deployed against a running
installation. Slow, and the only rung that can tell you the image works.

The e2e suite (`./e2e`, Go, `-tags e2e`) tests a RUNNING installation end to
end through the same doors users take — the server API and the gateway's
https edge: deploy, status, logs, login redirect, token access, env, addon,
network flips, files, destroy. Because it never talks to Kubernetes or
Docker directly, one suite verifies both runtimes.

Both live E2E installs put an authenticated TLS mirror in front of npm, PyPI,
Go, Maven and NuGet. The package build therefore also verifies Docker-config
credential lookup, private-CA trust, readiness authentication, cross-host
artifact routing and the real clients for all five ecosystems.

Hostnames resolve through `sudo devkube connect`, which keeps donkey's
hostnames in `/etc/hosts` while it runs. That is why this repo no longer
patches CoreDNS, pins NodePorts or writes registry files on nodes — the demo
cluster is left as unmodified as a real one. (devkube/loop needs a version
that discovers donkey's hostnames from its route ConfigMaps.)

The demo's certificates come from the cluster's own CA, so the CLI is pointed
at it with `donkey login --ca-file .build/platform-ca.crt` — the same flag a
company with an internal PKI uses. Tear down with `devkube --cluster donkey delete`.

## Security notes (PoC status)

- CLI↔API auth is OIDC end to end: `donkey login` runs the browser sign-in
  through the gateway, which seals the identity into a 30-day token
  (AES-GCM, the installation's session key). Sealed tokens cannot be revoked
  individually — rotating the session key revokes all of them at once.
- The server trusts the `X-Auth-Request-*` identity headers on requests that
  reached it — sound from outside (the only route to it passes the gateway,
  which strips inbound copies), but an in-cluster caller who can reach the
  Service directly can forge them. That caller sits inside the same trust
  boundary as the cluster-admin ServiceAccount below.
- donkey-server currently binds cluster-admin; scoping it down is on the
  list.
- The demo IdP approves everyone by design; it never leaves `hack/demo/`.
- Space and app names from the API are validated server-side, and the space
  that would map onto `donkey-system` is reserved — no space name can reach
  the control plane's own namespace.
- The repo summarizer has a hard denylist (`.env*`, keys, certs, anything
  secret-looking), so no secret-shaped *file* is summarized for the model.
  That denylist covers the summarizer only — it says nothing about the
  second thing donkey sends: failure evidence.
  Uploads exclude key material (`.env*`, `.pem`/`.key`/`.p12`/`.pfx`, SSH
  keys) but deliberately stay narrower: a source file that merely has
  "token" in its name still has to reach the build. The CLI says what it
  left behind (the first few names, plus a count).
- Failure diagnosis sends pod logs, cluster events and a build-log tail to
  the model, and a failing app prints credentials: donkey injects
  `DATABASE_URL` (password included) into every app with a postgres addon,
  and Django, Rails, SQLAlchemy and node-postgres all echo that URL back
  when they cannot connect — which is exactly what starts a diagnosis. So
  the evidence is redacted inside `DiagnoseFailure` itself, where no caller
  can forget it: credentials in URLs become `scheme://***:***@host`,
  credential-shaped assignments (`PGPASSWORD=…`, `api_key: …`,
  `Authorization: Bearer …`) lose their value, and any value the caller
  hands over as a known secret (`FailureContext.KnownSecrets`) is blanked
  literally before the patterns run — that list is what catches a password
  no pattern recognizes, and the server fills it with everything donkey
  itself handed the app: its env values, its addon bindings, and the
  registry and OIDC credentials that surface in build logs. Nothing matches on
  "looks like a long random string" — git shas, image digests and pod ids
  are what makes a diagnosis readable, and blanking them would buy nothing.
  Your own logs stay unredacted; only the copy sent to the endpoint is
  scrubbed.
- The assistant is required, so its endpoint is part of donkey's trust
  boundary: source summaries of every deployed app go there, and redacted
  failure logs when something breaks. Point it at a model you control (the
  install takes any OpenAI-compatible base URL).
- The `openai` service never hands out the platform's real AI key: bindings
  carry a generated per-space stand-in, `donkey-ai` swaps it for the real
  one on the way upstream, and the swap only happens for tokens donkey
  itself issued — so removing the service revokes the space's access, and
  the upstream credential exists nowhere outside `donkey-system`.
