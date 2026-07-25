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
three ideas:

1. **The cluster does the work.** A control plane (`donkey-server`) runs in
   your cluster and owns everything heavy: recognizing apps, building
   images, pushing to your registry, applying Kubernetes objects, watching
   rollouts. The `donkey` CLI is a thin, token-authenticated client — no
   Docker, no kubectl, no build tools on laptops.
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
4. **Private by default.** Deployed apps sit behind an OIDC login enforced
   by the gateway itself (Envoy `SecurityPolicy` against your provider,
   e.g. Entra ID). `public: true` in donkey.yaml opts an app out. No issuer
   configured — no login, and the install says so.

## Components

```
 developer machine                     Kubernetes cluster
┌───────────────┐          ┌─────────────────────────────────────────┐
│  donkey CLI   │──token──▶│ donkey-system                           │
│  (thin client)│   HTTP   │  ├─ donkey-server   REST API, detection,│
└───────────────┘          │  │      manifests, AI, rollout watching │
                           │  ├─ donkey-buildkitd  image builds      │──▶ registry
 browser ─────────────────▶│  └─ Gateway (Envoy) login ──────OIDC──────▶ Entra ID /
        │                  ├─────────────────────────────────────────┤    any IdP
        │  per space:      │ donkey-<space>                          │
        └─ HTTPRoute ─────▶│  app "web"      Deployment · Service    │
     <app>.<space>.<domain>│  app "backend"  Deployment · Service    │
                           │  per app: HTTPRoute · SecurityPolicy    │
                           │  one PVC per app · env/addon/pull secrets│
                           └─────────────────────────────────────────┘
```

- **donkey CLI** — talks only to the donkey-server API (`donkey login`
  stores server + token in `~/.config/donkey/config.yaml`). The single
  exception: `donkey server install` uses your kubeconfig to bootstrap the
  control plane.
- **donkey-server** — stateless API pod; all state lives in Kubernetes
  objects. Deploys arrive as tar.gz uploads and stream progress back as
  NDJSON. Configured entirely via env from the install settings. It also
  serves the **admin console** (React/Tailwind, embedded in the binary) at
  its own URL: open `donkey.<baseDomain>` in a browser, sign in with the
  API token, and you get the apps overview with health, an env-var editor,
  logs, remove-app, and the system checks.
- **donkey-buildkitd** — a plain BuildKit deployment; the server drives it
  over gRPC with Railpack-generated LLB (or the app's Dockerfile) and pushes
  straight to the registry.
- **The Gateway** — donkey installs a `GatewayClass` (bound to
  [Envoy Gateway](https://gateway.envoyproxy.io), a cluster prerequisite)
  and one shared `Gateway`; every app and the API attach `HTTPRoute`s to
  it. No Ingress, no nginx. Login protection is a per-app Envoy
  `SecurityPolicy` doing the OIDC flow in the proxy itself — no extra auth
  component runs.
- **Per space** — one namespace (`donkey-<space>`) shared by the apps of
  that space, so they reach each other by plain service name. It is created
  with the first app and removed with the last; a namespace donkey didn't
  create is never taken over.
- **Per app** — Deployment (probes, `PORT`, `envFrom` secrets), Service,
  HTTPRoute (`<app>.<space>.<baseDomain>`), SecurityPolicy + OIDC client
  secret (unless `public: true`), and Secrets for env vars, registry pull
  auth and addon bindings. Everything is labeled `donkey.dev/app` and
  `donkey.dev/space`, so status, logs and destroy know exactly what donkey
  owns.
- **Storage** — `storage.paths` in donkey.yaml (or what the assistant
  found) gives an app **one** PersistentVolumeClaim, with every path mounted
  as its own folder on it (`subPath`, derived from the path so data never
  moves). The volume is group-owned via `fsGroup` so the app can write
  whichever user the image runs as; an app with storage runs as a single
  copy with a `Recreate` rollout, because a disk attaches to one machine at
  a time.
- **Addons** follow the attached-resource pattern: a binding Secret with a
  canonical contract (`DATABASE_URL`, `PG*`). `addon add postgres`
  provisions in-cluster (Deployment + PVC); `addon attach postgres --url …`
  binds a remote database — the app cannot tell the difference.

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
| **Failure diagnosis** — on a failed build or rollout, logs and cluster events go back to the assistant | plain-language explanation + up to 3 proposed fixes | proposals are printed, never applied |

**The rules it plays by**

- *What you wrote wins.* A value present in `donkey.yaml` is never
  overwritten — at most commented on ("donkey.yaml says port 8080, but the
  code listens on 5000").
- *Nothing silent.* Every filled-in value is printed as its own line during
  the deploy, with the reason the assistant gave.
- *Nothing written behind your back.* `donkey init` asks before saving a
  proposed Dockerfile or a storage block; deploy-time findings apply to that
  deploy only, so committing them to `donkey.yaml` stays your choice.
- *Secrets never travel.* The summarizer has a hard denylist (`.env*`, keys,
  certificates, anything secret-shaped) and uploads exclude the same.
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

## Requirements for a real deployment

donkey installs into a cluster you already run, and stays inside its own
namespaces. It **never modifies cluster-wide settings**: no CoreDNS edits, no
files on nodes, no admission webhooks, no StorageClasses — everything it
applies is in `donkey-system`, in a `donkey-<space>` namespace, or is one of
two cluster-scoped objects it names after itself (the `donkey` GatewayClass
and the `donkey-server` ClusterRoleBinding). A test asserts exactly that
(`TestInstallTouchesOnlyItsOwnObjects`).

What has to be there first:

| Requirement | Why | Notes |
|---|---|---|
| **Kubernetes cluster** with a default StorageClass | apps with `storage.paths` and provisioned addons claim volumes | any conformant cluster; nothing donkey-specific |
| **[Envoy Gateway](https://gateway.envoyproxy.io)** | donkey attaches one shared Gateway and per-app HTTPRoutes; the OIDC login runs in the proxy itself | donkey creates the `GatewayClass` and `Gateway`, not the controller |
| **[cert-manager](https://cert-manager.io) + an Issuer** | every app hostname gets its own certificate | `tls.issuer` (ClusterIssuer or Issuer). Any solver works: HTTP-01, DNS-01, an internal CA, Vault |
| **Container registry** the cluster can pull from | donkey pushes every build there | `registry.url` + credentials; `registry.caSecret` when its certificate comes from a private CA |
| **DNS** for `*.<space>.<baseDomain>` → the gateway address | apps are served at `<app>.<space>.<baseDomain>` | one wildcard per space, or a record per app |
| **OIDC provider** (e.g. Entra ID) | the login gateway in front of every app | optional: without `auth.issuer`, apps are public and the install says so |
| **OpenAI-compatible endpoint** | donkey reads every app with it (ports, health routes, data folders) | required — the server refuses to start without one. Point it at a model you control: source summaries go there |

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

Prints the API URL and a one-time token. Re-running with changed settings
updates the installation idempotently (server-side apply; existing secrets
and tokens survive; config changes roll the affected pods). Note for
Entra ID: with the OIDC flow in the gateway, callback URLs are per app
(`https://<app>.<space>.<domain>/oauth2/callback`) — register each, or use
a wildcard-capable provider.

**Certificates and spaces.** Apps sit one DNS level below the base domain,
and a wildcard covers only one level: `*.apps.example.com` does *not* match
`web.acme.apps.example.com`. So the gateway certificate needs a name per
space (`*.<space>.apps.example.com`) — donkey warns on the first deploy
into a space it cannot serve. For demos and evaluations, `tls.selfSigned`
(`--tls-self-signed`) lets donkey issue the certificate and extend it with
a new name each time a space appears.

**Day 1 — everyone deploys:**

```sh
donkey login --server https://donkey.apps.example.com --token …   # once
cd your-project && donkey deploy
```

| Command | What it does |
|---|---|
| `donkey` | friendly overview and next step |
| `donkey doctor` | client + server health with plain-language fixes |
| `donkey init` | reads the app (detector + assistant), writes `donkey.yaml` incl. space and storage |
| `donkey apps` | everything deployed, grouped by space |
| `donkey deploy` | upload → detect → build → deploy → URL (`--dry-run`, `--build-only`, `--no-build --tag`, `--no-wait`, `--platform`) |
| `donkey status` / `logs -f` / `open` | day-2 basics (scoped to the folder's space) |
| `donkey env set/unset/list` | env vars as cluster Secrets, auto-restart |
| `donkey addon add/attach/remove/list` | databases & co. with injected bindings |
| `donkey destroy` | remove everything donkey created for the app |

Image tags are `<git-sha>-<timestamp>` (the CLI reads the sha locally and
sends it along; uploads never include `.git`), so every deploy is traceable
and always rolls.

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
  addons/             addon registry + postgres (provisioned & external bindings)
  ai/                 Assistant interface (analysis + failure diagnosis), OpenAI impl, secret-safe summarizer
  config/             donkey.yaml, client config, install settings file
  doctor/             client-side check engine behind `donkey doctor`
  ui/                 all terminal output: plain language, jargon behind -v
  gitutil/            pure-Go .git reader: HEAD sha (image tags), repo root (default space)
testdata/             deployable samples incl. a kitchen-sink monorepo; golden files
hack/                 dev/demo scaffolding only — never part of the product
                      (cluster.sh → devkube, demo-ca.sh, demo/fake-idp)
examples/             server-install.yaml reference
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
task demo            # cluster + control plane + sample app
task test            # unit tests (no cluster needed)
task e2e             # full smoke test, isolated from your real config
```

Hostnames resolve through `sudo devkube connect`, which watches donkey's own
HTTPRoutes and keeps their hostnames in `/etc/hosts` while it runs. That is why
this repo no longer patches CoreDNS, pins NodePorts or writes registry files on
nodes — the demo cluster is left as unmodified as a real one.

The demo's certificates come from the cluster's own CA, so the CLI is pointed
at it with `donkey login --ca-file .build/platform-ca.crt` — the same flag a
company with an internal PKI uses. Tear down with `devkube --cluster donkey delete`.

## Security notes (PoC status)

- CLI↔API auth is a bearer token (`donkey login`); OIDC device-flow is the
  intended follow-up.
- donkey-server currently binds cluster-admin; scoping it down is on the
  list.
- The demo IdP approves everyone by design; it never leaves `hack/demo/`.
- The repo summarizer has a hard denylist (`.env*`, keys, certs, anything
  secret-looking) and uploads exclude the same — nothing secret-shaped
  leaves the machine or reaches the model endpoint.
- The assistant is required, so its endpoint is part of donkey's trust
  boundary: source summaries of every deployed app go there. Point it at a
  model you control (the install takes any OpenAI-compatible base URL).
