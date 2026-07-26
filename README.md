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
2. **Two readers, one answer.** Every look at an app combines embedded
   [Railpack](https://railpack.com) — which knows how to *build* Python,
   Node, Go, Ruby, PHP, Java and static apps — with an **assistant that
   reads the source** for everything a detector cannot see: the port the
   code really binds, a health route that exists, the folders holding data
   that must survive a restart. The assistant is a required part of the
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
 browser ─────────────────▶│  ├─ Gateway (any Gateway API impl): TLS │
        │                  │  │      + hostnames, routes to ↓        │
        │  per app:        │  └─ donkey-gateway  routing · OIDC login│──OIDC──▶ Entra ID /
        └─ HTTPRoute ──────▶       · access (users/groups/roles) ──▶ app     any IdP
     <app>.<space>.<domain>├─────────────────────────────────────────┤
                           │ donkey-<space>                          │
                           │  app "web"      Deployment · Service    │
                           │  app "backend"  Deployment · Service    │
                           │  service "postgres"  Deployment · PVC   │
                           │  per app: HTTPRoute (→ donkey-gateway)  │
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
  logs, an env-var editor, restart, an AI "explain what's wrong" for
  unhealthy apps, remove-app, and the system checks. Spaces are managed
  here too — create one before any app exists, remove an empty one — and
  each space card carries its services: a catalog to add from, plus a
  browser per capability (tables, rows and SQL for a database; buckets and
  objects for storage; a streaming chat for the AI — see
  [Services](#services)). Deploys work straight from the browser: drop a
  project folder or a .zip on the Deploy page and the same pipeline runs,
  streaming its progress.
  A dropped folder is packed client-side with the CLI's exclusion rules;
  an upload without donkey.yaml just needs a name and port in the form.
  Views are linkable (`#/app/<space>/<name>`) and follow your light/dark
  preference.
- **donkey-buildkitd** — a plain BuildKit deployment; the server drives it
  over gRPC with Railpack-generated LLB (or the app's Dockerfile) and pushes
  straight to the registry.
- **The shared Gateway** — donkey installs a `GatewayClass` (bound to
  [Envoy Gateway](https://gateway.envoyproxy.io) by default) and one shared
  `Gateway`; every app and the API attach `HTTPRoute`s to it. No Ingress,
  no nginx. The shared Gateway only terminates TLS and matches hostnames.
- **donkey-gateway** — donkey's own edge proxy (same binary as the server,
  started with `gateway`): every app route's real backend. It routes by
  hostname and path (from annotations on the HTTPRoutes it watches), runs
  the OIDC code flow itself where a route demands a login, seals what it
  learned into an AES-GCM cookie (stateless — replicas just scale), checks
  the app's access rules (users, groups, roles from the ID token's claims)
  and forwards with `X-Auth-Request-*` identity headers. Auth lives here
  because the Gateway API has no standard for it — donkey stays portable
  across Gateway implementations and IdPs.
- **Per space** — one namespace (`donkey-<space>`) shared by the apps of
  that space, so they reach each other by plain service name — and that
  includes its services: a space's postgres is simply `postgres` to its
  siblings. The namespace is created with the first app (or ahead of time
  in the console), and goes away with the last app unless kept data or
  services still live there; a namespace donkey didn't create is never
  taken over.
- **Per app** — Deployment (probes, `PORT`, `envFrom` secrets), Service,
  HTTPRoute (`<app>.<space>.<domain>`, https only — one shared route
  redirects plain http) pointing at donkey-gateway with the app's backend,
  login and access rules as annotations, a `ReferenceGrant` for that
  cross-namespace hop, and Secrets for env vars, registry pull auth and
  service bindings. Everything is labeled `donkey.dev/app` and
  `donkey.dev/space`, so status, logs and destroy know exactly what donkey
  owns.
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

| | **Railpack detector** (in-process) | **Assistant** (your OpenAI-compatible model) |
|---|---|---|
| Question | how do I *build* this? | what is true about this app? |
| Sees | files, manifests, lockfiles | file tree + manifests, Dockerfile/compose, entry points, current donkey.yaml |
| Answers | provider, build & start commands | app type, port, health route, data folders, backing services |
| Fails when | the app is unusual | the endpoint is unreachable → donkey stops and says so |

The detector's verdict is handed to the assistant as evidence, so the model
confirms or corrects it instead of guessing from scratch.

**What comes out**

| Finding | How it is used | Overridable by |
|---|---|---|
| **Build strategy** — `Dockerfile`, Railpack provider, or an assistant-written `Dockerfile`/`railpack.json` when nothing recognizes the app (stored under `.donkey/`, shown before it is saved) | drives the BuildKit build | `build.type`, `build.dockerfile` |
| **Port** — read from the source (`app.listen(4000)`, `uvicorn --port`, …); "0" means the app honors `PORT` and any port works | container port, Service target, probes | `port` (required in donkey.yaml, never overwritten — a mismatch is reported as a warning) |
| **Health route** — a real unauthenticated route such as `/healthz`; when none exists donkey probes the TCP port instead of guessing a path | readiness + liveness probes | `healthcheck.path` |
| **Data folders** — folders the code writes to and cannot regenerate: uploads, an SQLite file's folder, generated documents, search indexes. Caches, logs, `/tmp` and build output are refused by construction | one PVC per app, each folder mounted as its own `subPath` | `storage.paths` (an empty list means "no disk", and is respected) |
| **Backing services** — e.g. a `DATABASE_URL` default or a `psycopg` import | reported as a suggestion; never provisioned behind your back | `donkey addon add postgres` |
| **Addresses that only work on a laptop** — `http://localhost:3001/api` and friends. The assistant says where the code runs, because that decides whether it's a mistake: in browser code localhost is the *visitor's* machine (always wrong), in server code it's the app's own container (wrong only when it meant another app) | reported as a warning, with the fix | rewrite the call, or set the real address in `build.env` |
| **Failure diagnosis** — on a failed build or rollout, logs and cluster events go back to the assistant | plain-language explanation + up to 3 proposed fixes | proposals are printed, never applied |

**The rules it plays by**

- *What you wrote wins.* A value present in `donkey.yaml` is never
  overwritten — at most commented on ("donkey.yaml says port 8080, but the
  code listens on 5000").
- *Nothing silent.* Every filled-in value is printed as its own line during
  the deploy, with the reason the assistant gave.
- *Nothing written behind your back.* `donkey init` asks before saving a
  proposed Dockerfile or a storage block; deploy-time findings are recorded on
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
preDeploy: ["python", "manage.py", "migrate"]

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
  - type: postgres            # creates missing ones. postgres | pocketbase
    name: db                  # | rustfs (S3) | openai (the platform's AI).
                              # apps naming the same service share it

build:
  type: auto                  # auto | railpack | dockerfile
  dockerfile: Dockerfile
  platform: linux/arm64
  env:                        # build-time values (VITE_*, build args)
    VITE_API_URL: https://api.example.com

replicas: 2
domain: shop.example.com      # a hostname of your own instead of the default
resources:
  requests: { cpu: 100m, memory: 128Mi }
  limits:   { memory: 512Mi }
```

**How an app is reached.** By default it gets `<app>.<space>.<domain>` behind
the login gateway. `public: true` drops the login. `access:` narrows it — only
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

**`preDeploy`** runs once against the new image before the new version starts
serving, with the app's env, addon bindings and disk. Migrations are the
reason it exists. If it fails, the deploy stops and the version already
running keeps serving — nothing is swapped for something that cannot start.

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
| `pocketbase` | accounts, file uploads and a records API in one small server | `POCKETBASE_URL` + admin credentials |
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
at a proxy inside donkey-server with a generated per-space token; the proxy
checks that token against the bindings it issued, swaps in the platform's
actual key upstream, and streams the answer back (SSE included). The real
key never leaves `donkey-system`, and removing the service from the space
*is* the revocation.

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
files on nodes, no admission webhooks, no StorageClasses — everything it
applies is in `donkey-system`, in a `donkey-<space>` namespace, or is one of
two cluster-scoped objects it names after itself (the `donkey` GatewayClass
and the `donkey-server` ClusterRoleBinding). A test asserts exactly that
(`TestInstallTouchesOnlyItsOwnObjects`).

What has to be there first (`donkey server install` checks these before it
applies anything, and `donkey doctor` keeps checking them after):

| Requirement | Why | Notes |
|---|---|---|
| **Kubernetes cluster** with a default StorageClass | apps with `storage.paths` and provisioned services claim volumes | any conformant cluster; nothing donkey-specific |
| **[Envoy Gateway](https://gateway.envoyproxy.io)** | donkey attaches one shared Gateway and per-app HTTPRoutes; the OIDC login runs in the proxy itself | donkey creates the `GatewayClass` and `Gateway`, not the controller. Traffic enters through a `LoadBalancer` service; on clusters where nothing assigns those an address (kind), set `gateway.serviceType: ClusterIP` |
| **[cert-manager](https://cert-manager.io) + an Issuer** | every app hostname gets its own certificate | `tls.issuer` (ClusterIssuer or Issuer). Any solver works: HTTP-01, DNS-01, an internal CA, Vault |
| **Container registry** the cluster can pull from | donkey pushes every build there | `registry.url` + credentials; `registry.caSecret` when its certificate comes from a private CA |
| **DNS** for `*.<domain>` and `<domain>` → the gateway address | apps are served at `<app>.<space>.<domain>`, the API and login callback at `<domain>` | one wildcard covers every depth; the apex needs its own record |
| **OIDC provider** (Pocket ID, Entra ID, Keycloak, …) | the one way in: console, CLI and protected apps all sign in against it | required — `auth.issuer` (+ client id/secret); `auth.admins` optionally narrows who may administer |
| **OpenAI-compatible endpoint** | donkey reads every app with it (ports, health routes, data folders) | required — the server refuses to start without one. Point it at a model you control: source summaries go there |

A complete copy-paste walkthrough that stands all of this up on a single
Debian server — kind, Envoy Gateway, cert-manager with Let's Encrypt, a
registry container, an OIDC provider — is in
[examples/INSTALL.md](examples/INSTALL.md).

RBAC: donkey-server currently binds `cluster-admin` (it creates namespaces and
everything inside them). Scoping that down is on the list — see
[Security notes](#security-notes-poc-status).

## Deployment model

**Day 0 — admin installs the control plane** into an existing cluster with
[Envoy Gateway](https://gateway.envoyproxy.io/docs/tasks/quickstart/)
installed. Settings live in a file
([examples/server-install.yaml](examples/server-install.yaml)); flags
override; secrets come from env:

```sh
donkey server install -f server-install.yaml
```

Prints where to sign in — there is no API token: the console and the CLI
authenticate through the same OIDC login the apps use, and `auth.admins`
(users/groups/roles, matched like an app's `access` rules) narrows who may
administer; empty means everyone the IdP admits. Re-running with changed
settings updates the installation idempotently (server-side apply; existing
secrets and sessions survive; config changes roll the affected pods).

**One callback, one sign-in.** Every login under the domain rides through a
single OAuth callback on the domain itself
(`https://<domain>/oauth2/callback`) — one redirect URI to register at any
OIDC provider, no wildcard support needed. The session cookie lives on the
domain, so signing in to one app signs you in to all of them. Only apps
with a custom `domain:` outside the installation's domain keep a per-host
callback (`https://<their-domain>/oauth2/callback`) — register those
individually.

**Certificates and DNS.** cert-manager issues one certificate per app
hostname — a TLS wildcard covers only one level, so no single certificate
could — which means any solver works, HTTP-01 included. DNS is simpler than
TLS here: one `*.<domain>` record covers every depth, because DNS wildcards
match multiple labels (as long as no explicit record sits at an intermediate
name). Only the apex is never covered by the wildcard — and the API, console
and login callback live there, so give `<domain>` a record of its own.

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
| `donkey init` | reads the app (detector + assistant), writes `donkey.yaml` incl. space and storage |
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

Running apps are not touched: their pods, disks, routes and certificates stay
as they are while donkey-server restarts. A re-install also keeps every secret
it isn't given again (registry password, OIDC client secret, AI key), so the
same settings file is enough — rotating one thing must never blank the rest.

**What to back up.** All donkey state lives in Kubernetes objects, and nearly
all of it is reproducible: images rebuild from the repositories, manifests
re-render from `donkey.yaml`, namespaces and routes are re-created by the next
deploy. What is *not* reproducible has to live outside the cluster:

| Keep | Why |
|---|---|
| the install settings file (`server-install.yaml`) | every install and re-install needs it; it is the only written description of the installation |
| `DONKEY_REGISTRY_PASSWORD` | donkey stores it, but never invents it; without it the server can't push and apps can't pull |
| `DONKEY_OIDC_CLIENT_SECRET` (with its client id) | belongs to a registration in your IdP — a new one means updating the IdP as well |
| `OPENAI_API_KEY` | the assistant is required; no key, no deploys |
| the `session-key` entry of `donkey-system-config` | a fresh install **generates a new one** and thereby invalidates every session and CLI token. Restoring it makes the rebuilt installation the same installation — everyone stays signed in |
| the registry CA secret (`registry.caSecret`) | donkey only *references* that Secret in `donkey-system` — it never creates it. Whatever created it has to run again |
| DNS for `*.<domain>` and `<domain>` | they point at the gateway address, which a rebuilt cluster changes |

Two more things exist only in the cluster: env values set with `donkey env
set` (the ones in `donkey.yaml` come back with the app, the rest don't), and
the generated credential of a provisioned service — which matters only as
long as its data does.

**PVC data is the one thing that cannot be reconstructed.** Everything above
is a copy of something you have; a volume is not. donkey never snapshots,
exports or restores a PersistentVolumeClaim — neither an app's `storage.paths`
volume nor a provisioned addon's data disk — and it has no restore command to
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

## Repository structure

```
cmd/donkey            CLI entry point          cmd/donkey-server  control plane entry point
web/                  admin console (Vite + React + TS + Tailwind, lucide icons; embedded via go:embed)
internal/
  cli/                one file per command; session/config wiring
  apiclient/          CLI↔server transport: tar upload, NDJSON stream, typed calls
  server/             REST handlers, deploy pipeline, env-based config
  detect/             build strategy decision (donkey.yaml > Dockerfile > railpack > AI)
  build/              BuildKit gRPC builder (railpack LLB / dockerfile.v0), registry auth, tags
  k8s/                client-go wrapper: server-side apply, rollout wait, logs, env secrets
  k8s/manifests/      pure config→object rendering (app + control plane) — golden-tested
  addons/             service catalog: postgres, pocketbase, rustfs, openai (provisioned & external bindings)
  ai/                 Assistant interface (analysis + failure diagnosis), OpenAI impl, secret-safe summarizer, log redaction
  config/             donkey.yaml, client config, install settings file
  doctor/             client-side check engine behind `donkey doctor`
  ui/                 all terminal output: plain language, jargon behind -v
  gitutil/            pure-Go .git reader: HEAD sha (image tags), repo root (default space)
testdata/             deployable samples incl. a kitchen-sink monorepo; golden files
examples/             what a finished project looks like: the demo (two apps, one
                      address, three services) + server-install.yaml reference
hack/                 dev/demo scaffolding only — never part of the product
                      (cluster.sh → devkube, demo-ca.sh, demo/fake-idp)
```

Conventions: everything under `internal/`; `config` and `k8s/manifests` are
pure (no I/O) and unit-tested; nothing in the product shells out to
external CLIs (the one exception: `donkey open` launching your browser);
demo concerns are code-review-enforced out of the product
(`TestSystemObjectsGatewayAPI`, `TestInstallTouchesOnlyItsOwnObjects`).

## Trying it out

The local playground is [devkube](https://github.com/adrianliechti/devkube),
which brings exactly the prerequisites above: a cluster, cert-manager with its
`platform` CA ClusterIssuer, an in-cluster registry the nodes already trust,
and Envoy Gateway. donkey is then installed with the same
`donkey server install` an admin runs — the demo adds nothing but a
deliberately trivial auto-approving identity provider
(`hack/demo/fake-idp/` — everyone is instantly demo@example.com).

```sh
task demo            # cluster + control plane + the demo app (dashboard, MCP backend, three services)
task test            # unit tests (no cluster needed)
task e2e             # full smoke test, isolated from your real config
task demo:samples    # deploy every testdata sample and report what donkey got right alone
task demo:broken     # deploy apps that MUST fail, and check each was explained and patched
```

`task demo` is three phases, each runnable on its own when you only want to
redo one of them:

```sh
task demo:cluster    # the local cluster and donkey's prerequisites
task demo:setup      # the identity provider, the CA, and donkey itself
task demo:app        # log in and deploy the demo app
```

Hostnames resolve through `sudo devkube connect`, which watches donkey's own
HTTPRoutes and keeps their hostnames in `/etc/hosts` while it runs. That is why
this repo no longer patches CoreDNS, pins NodePorts or writes registry files on
nodes — the demo cluster is left as unmodified as a real one.

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
  carry a generated per-space stand-in, donkey-server's proxy swaps it for
  the real one on the way upstream, and the swap only happens for tokens
  the server itself issued — so removing the service revokes the space's
  access, and the upstream credential exists nowhere outside
  `donkey-system`.
