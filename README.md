# donkey 🫏

Deploy your apps to Kubernetes without the hard parts.

```
$ donkey deploy
· Packing up shop and sending it to the server…
✓ shop will be built with automatic detection (python)
✓ Image ready: registry.example.com/donkey/shop:4f2a9c1b03de-20260724143022
✓ shop is running.
  → https://shop.apps.example.com
```

## The concept

Donkey is a small PaaS for teams that already run Kubernetes. It follows
three ideas:

1. **The cluster does the work.** A control plane (`donkey-server`) runs in
   your cluster and owns everything heavy: recognizing apps, building
   images, pushing to your registry, applying Kubernetes objects, watching
   rollouts. The `donkey` CLI is a thin, token-authenticated client — no
   Docker, no kubectl, no build tools on laptops.
2. **Zero config that holds up.** `donkey deploy` in a plain source folder
   works: app type via embedded [Railpack](https://railpack.com) (Python,
   Node, Go, Ruby, PHP, Java, static — a `Dockerfile` always wins), a TCP
   probe when no health route is known, the `PORT` convention injected,
   the platform defaulted by the server. A committed `donkey.yaml` (written
   by `donkey init`) pins names, ports and addons. Optional AI (any
   OpenAI-compatible endpoint) reads the source to find real ports and
   health routes, containerizes apps nothing else recognizes, and explains
   failures in plain language — always propose-only.
3. **Private by default.** Deployed apps sit behind an OIDC login enforced
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
 browser ─────────────────▶│  └─ oauth2-proxy    login gateway ──OIDC──▶ Entra ID /
        │                  ├─────────────────────────────────────────┤    any IdP
        │   per app:       │ donkey-<app>                            │
        └─ app ingress ───▶│  Deployment · Service · Ingress         │
           (auth_request)  │  Secrets: env, addon bindings, pull auth│
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
- **Per app** — an own namespace (`donkey-<app>`) holding Deployment
  (probes, `PORT`, `envFrom` secrets), Service, HTTPRoute
  (`<app>.<baseDomain>`), SecurityPolicy + OIDC client secret (unless
  `public: true`), and Secrets for env vars, registry pull auth and addon
  bindings. Everything is labeled `donkey.dev/app` so status, logs and
  destroy know exactly what donkey owns.
- **Addons** follow the attached-resource pattern: a binding Secret with a
  canonical contract (`DATABASE_URL`, `PG*`). `addon add postgres`
  provisions in-cluster (Deployment + PVC); `addon attach postgres --url …`
  binds a remote database — the app cannot tell the difference.

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
(`https://<app>.<domain>/oauth2/callback`) — register each, or use a
wildcard-capable provider.

**Day 1 — everyone deploys:**

```sh
donkey login --server https://donkey.apps.example.com --token …   # once
cd your-project && donkey deploy
```

| Command | What it does |
|---|---|
| `donkey` | friendly overview and next step |
| `donkey doctor` | client + server health with plain-language fixes |
| `donkey init` | recognizes the app (AI-assisted), writes `donkey.yaml` |
| `donkey deploy` | upload → detect → build → deploy → URL (`--dry-run`, `--build-only`, `--no-build --tag`, `--no-wait`, `--platform`) |
| `donkey status` / `logs -f` / `open` | day-2 basics |
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
  ai/                 Assistant interface, OpenAI impl, secret-safe repo summarizer
  config/             donkey.yaml, client config, install settings file
  doctor/             client-side check engine behind `donkey doctor`
  ui/                 all terminal output: plain language, jargon behind -v
  gitutil/            pure-Go .git HEAD reader (for image tags)
testdata/             deployable samples incl. a kitchen-sink monorepo; golden files
hack/                 dev/demo scaffolding only — never part of the product
examples/             server-install.yaml reference
```

Conventions: everything under `internal/`; `config` and `k8s/manifests` are
pure (no I/O) and unit-tested; nothing in the product shells out to
external CLIs (the one exception: `donkey open` launching your browser);
demo concerns are code-review-enforced out of the product
(`TestPocketIDIsNotPartOfTheProduct`).

## Trying it out

`task demo` is the only thing that creates a cluster: kind + local registry
+ Envoy Gateway, a deliberately trivial auto-approving demo IdP
(`hack/demo/fake-idp/` — everyone is instantly demo@example.com, so the
login flow completes with zero setup), the nip.io DNS/NodePort glue
(`hack/demo-dns.sh`, `hack/demo-expose.sh`) — and then the exact same
`donkey server install` an admin would run, followed by a sample deploy.
Tear down: `kind delete cluster --name donkey && docker rm -f donkey-registry`.

```sh
task demo            # local playground, ready to click
task test            # unit tests (no cluster needed)
task e2e             # full smoke test, isolated from your real config
```

## Security notes (PoC status)

- CLI↔API auth is a bearer token (`donkey login`); OIDC device-flow is the
  intended follow-up.
- donkey-server currently binds cluster-admin; scoping it down is on the
  list.
- The demo IdP approves everyone by design; it never leaves `hack/demo/`.
- The AI repo summarizer has a hard denylist (`.env*`, keys, certs,
  anything secret-looking) and uploads exclude the same — nothing
  secret-shaped leaves the machine or reaches the AI endpoint.
