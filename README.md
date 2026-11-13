# donkey 🫏

Deploy your apps to Kubernetes without the hard parts.

```
$ donkey deploy
· Packing up shop (space acme) and sending it to the server…
✓ shop will be built with automatic detection (python)
✓ Image ready: registry.example.com/donkey/acme/shop:4f2a9c1b03de-20260724143022
· Keeping /app/uploads on a disk — views.py saves uploaded files there
· Checking the app's health at /healthz
✓ shop is running.
  → https://shop.acme.apps.example.com
```

## The concept

Donkey is a small PaaS for teams that already run Kubernetes. It follows
five ideas:

1. **The cluster does the work.** A control plane (`donkey-server`, with
   `donkey-orchestrator` behind it) runs in your cluster and owns everything
   heavy: recognizing apps, building images, pushing to your registry,
   applying Kubernetes objects, watching rollouts. The `donkey` CLI is a thin
   client that signs in through your browser — no Docker, no kubectl, no build
   tools on laptops.
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
   Entra ID). Space membership controls who can open its apps. `public: true`
   opts an app out only when a space admin enables **Allow public apps**.
   No issuer configured — no login, and the install says so.
5. **Services belong to the space.** A database is the project's, not one
   container's: postgres, S3-style storage, and the platform's AI are
   provisioned once per space from a small catalog, and apps *connect* —
   each gets a canonical env contract (`DATABASE_URL`, `AWS_*`,
   `OPENAI_*`, `EXA_*`) injected, whether the service runs in the cluster or
   somewhere else. See [Services](#services).

## Components

```
 developer machine                     Kubernetes cluster
┌───────────────┐          ┌─────────────────────────────────────────┐
│  donkey CLI   │─sign-in─▶│ donkey-system                           │
│  (thin client)│   HTTP   │  ├─ donkey-server   REST API, console,  │
└───────────────┘          │  │      sign-in checks (stateless)      │
                           │  ├─ donkey-orchestrator  plans, builds, │
                           │  │      deploys, rollout watching       │
                           │  │      data volume: source history     │
                           │  ├─ donkey-buildkitd  image builds      │──▶ registry
                           │  ├─ donkey-packages  package proxy      │─▶ package registries
                           │  ├─ donkey-ai      openai addon endpoint│──▶ model API
                           │  ├─ donkey-proxy   HTTP service proxy   │──▶ configured APIs
                           │  ├─ donkey-telemetry  OTLP + DuckDB     │
 browser ────80/443───────▶│  └─ donkey-gateway  TLS termination     │──OIDC──▶ Entra ID /
                           │       · ACME / CA / cert-manager        │        any IdP
                           │       · routing by hostname and path    │
     <app>.<space>.<domain>│       · OIDC login                      │
                           │       · space users/groups ──▶ app
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
  objects. Deploys arrive here as tar.gz uploads; the server checks the
  sign-in, hands the upload to donkey-orchestrator, and relays its NDJSON
  progress stream back unchanged. Nothing long-running happens in this pod, so
  it rolls like any stateless service: the new pod is ready before the old one
  stops, and stopping takes seconds. Configured entirely via env from the
  install settings. It also
  serves the **admin console** (React/Tailwind, embedded in the binary) on
  the domain itself: open `https://<domain>` in a browser, sign in at your
  IdP (the gateway runs the login, like for any app — one sign-in covers
  console and apps), and you get the apps overview with health at a glance,
  Kubernetes certificate issuance and DNS propagation state, live logs,
  selectable 48-hour/7-day/30-day traffic and latency, an env-var editor,
  restart, an AI "explain what's wrong" for unhealthy apps, remove-app, and
  the system checks. Spaces are managed here too — create one before any app
  exists, remove an empty one — and
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
  Monorepos are reviewed as a project: every runnable app gets its own
  deployment in the same space, while workspace packages and lockfiles stay
  available to builds. Browser-to-backend calls use a shared-origin route;
  server calls use the runtime's internal address. Connection variables and
  source fixes are shown before deployment, and each app reports its own
  progress and result.
  Views are linkable (`#/app/<space>/<name>`) and follow your light/dark
  preference.
- **donkey-orchestrator** — the same binary started as `orchestrator`, and
  where the heavy work runs: plans (the assistant's reading of an upload, kept
  in its memory between review and deploy), builds and their repair loop
  against BuildKit, pushes, applies, rollout waits, and the release records it
  writes to donkey-data. Changes that must not interleave with a deploy
  run here too, because they share its per-app and per-service locks: an
  app's network settings, and adding, removing, connecting and
  disconnecting services. It owns the control plane's data volume, which
  holds deployment records, scan inventories and every app's
  [source history](#source-history): deploys write it,
  and the Source tab's reads and app and space removal are served here too,
  so one process owns the repositories. It runs as one replica with a
  `Recreate` rollout, since those locks and plans are process memory. Stopping it drains: new
  deploys are refused and the ones under way get up to 10 minutes to finish,
  with the pod's termination grace period sized to match. It takes requests
  from donkey-server alone — its NetworkPolicy admits only that pod, every
  call carries a credential derived from the session key, and it verifies the
  caller's sealed token again and authorizes the space exactly as the server
  would. A deploy still survives a dropped connection: it runs on in the
  orchestrator, and the CLI and console follow the app's status instead.
- **donkey-ai** — the openai addon's endpoint (same image, `ai`
  entrypoint). Apps reach it through their service bindings, so it
  runs apart from the admin API: it holds the platform's AI credential and
  read-only access to the binding Secrets, and nothing else. Apps call it with
  the per-space token their binding carries; it swaps that for the real
  credential, which never leaves this process. Removing the service revokes
  the space's access. The console's own AI chat is browser code, so it calls
  its own origin — `donkey-server` forwards `/ai/v1` here without inspecting
  it, because on that origin `/v1` is already the admin API.
- **donkey-proxy** — one shared HTTP service endpoint (same image, `proxy`
  entrypoint). It reads configured targets and space bindings,
  forwards HTTP/HTTPS through an optional outbound proxy, and uses its system
  CA store. Apps connect over internal HTTP; they need no enterprise CA bundle.
- **donkey-buildkitd** — a plain BuildKit deployment; the orchestrator
  drives it over gRPC with a Dockerfile (the app's own, or one donkey's
  assistant wrote), and pushes straight to the registry. It is also
  where a freshly built image is run to check it starts. Registry mirrors for
  restricted networks are configured here (`build.registryMirrors`).
- **donkey-packages** — an always-on, read-only endpoint for package
  managers: Dockerfile build steps, and developer machines through
  `donkey packages` (see
  [Packages on a developer's machine](#packages-on-a-developers-machine)). It
  proxies npm, PyPI, Go module, Maven and NuGet v3 requests to public
  registries by default or installation-wide enterprise upstreams. Its
  NetworkPolicy admits BuildKit, donkey-server and — where the installation
  has them — [machines](#machines); the server forwards a developer's
  requests after verifying their sign-in. Upstream credentials stay in this
  process rather than becoming Docker build args or reaching a laptop or a
  machine.
- **donkey-telemetry** — a dedicated `telemetry` process role of the single
  `donkey-server` binary: a single-replica OTLP/HTTP metrics receiver with a
  private query API backed by an embedded DuckDB file on its own 2 Gi PVC.
  It keeps 30 days of hourly HTTP histograms, hourly view/visitor
  rollups and [web analytics](#web-analytics) events.
  Only this process opens the file; `Recreate` upgrades preserve
  DuckDB's single-writer ownership. The server and the orchestrator can query
  it, while the gateway and apps can only ingest with scope-specific
  credentials.
- **donkey-gateway** — donkey's own edge (same binary as the server,
  started with `gateway`): traffic enters the cluster here, on ports 80 and
  443. By default it terminates TLS and issues the certificates itself — one per
  hostname via ACME (Let's Encrypt or an internal ACME CA) on demand, or
  minted by its own CA for internal installs (`tls.mode`) — routes by
  hostname and path (from the route ConfigMaps it watches), runs the OIDC
  code flow itself where a route demands a login, seals what it learned
  into an AES-GCM cookie (stateless — replicas just scale), checks the
  space's membership (users and groups from the sign-in) and
  forwards identity headers under oauth2-proxy's proxy-mode names —
  `X-Forwarded-User`, `-Email`, `-Preferred-Username`, `-Name` and `-Groups`,
  each also as `X-Auth-Request-*` for apps written against
  those — only to routes behind a login. `X-Forwarded-User` is Entra's
  tenant-wide object ID (`oid`) where the ID token carries one, else the
  `sub`; `X-Auth-Request-User` stays the `sub`. Public routes never learn
  who the visitor is, and inbound identity headers of both families are
  dropped everywhere, including names donkey never sets but an app may
  trust. Those headers are
  information for the app, never authorization: donkey's own API is handed a
  short-lived sealed token instead (`X-Access-Token`) and verifies it itself,
  so nothing in the cluster can reach it by typing a header. With
  `auth.scope` set, apps behind a login also get the visitor's own IdP access
  token (`X-Forwarded-Access-Token`) — see below. TLS and auth live here
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

- **Per user** — optionally, a [machine](#machines): a development container
  with a disk of its own, the donkey CLI and a coding agent, reached only by
  its owner. It belongs to a person rather than a space, and lives apart
  from every space.

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

Every app's **Analytics** tab shows its traffic below the
[audience](#web-analytics), and every space card opens the same traffic for
the whole space, with **last 48 hours**, **last 7 days**, and **last 30 days**
ranges. The 48-hour series uses hourly
buckets; week and month series use daily buckets. The console's
traffic source is the **gateway**: donkey-gateway records one stable OpenTelemetry
`http.server.request.duration` histogram for every matched app request,
including authentication failures, access denials, login redirects and CORS
preflights. Unmatched hosts/paths and the gateway's own OAuth and browser
analytics endpoints are excluded. The histogram count supplies request volume; status attributes split
HTTP 200–399 successes, 4xx client errors and 5xx server errors; its standard
explicit buckets supply average, p50 and p95 latency. Logical identity is kept
as the bounded `donkey.space.name` and `donkey.app.name` attributes. This follows
the [stable OTel HTTP metric semantic conventions](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/).

The traffic API also reports gateway **views**: successful top-level HTML
document loads, which don't include client-side SPA route transitions, with
unique signed-in visitors HMAC-pseudonymized per app and per space. The
console shows the [web analytics](#web-analytics) audience instead, which does
see route transitions and anonymous visitors.

Gateway analytics use built-in protocol plugins with shared request/response hooks:

- **HTTP** measures all routed websites and APIs, including authentication
  failures and CORS preflights, and reports verified callers.
- **Web** injects the browser analytics script into eligible HTML responses.
  Its page views and SPA navigation remain distinct from HTTP requests.
- **MCP** observes Streamable HTTP JSON-RPC requests and JSON/SSE responses,
  extracting operation metadata without application instrumentation.

All plugins use the gateway's authenticated OpenTelemetry exporter. Apps need
no SDK, exporter, analytics cookie or telemetry credentials. New deployments no
longer receive automatic `OTEL_*` environment variables. Explicit user-supplied
OTel configuration still works with the user's own observability backend.

The console shows gateway data only. For compatibility, the query API retains
`?source=app` and the receiver still accepts previously issued app credentials.
Legacy application series remain separate and are never added to gateway HTTP
or MCP totals. Existing app exporters can be removed when their apps redeploy.

**HTTP routes.** The traffic report includes up to 25 configured gateway route
prefixes ranked by request count, with method, app, 4xx/5xx counts and latency.
The gateway cannot infer an application's matched route templates; raw URLs,
query strings and resource URIs are not used as metric labels. Historical HTTP
totals survive upgrades; route breakdowns start with their first observed export.

**Authenticated callers.** The gateway separately reports distinct verified
identities across all matched HTTP requests, including API calls and verified
identities denied access. It exports app-scoped `user.hash` HMACs and a separate
space-scoped hash over the verified subject, using the telemetry key. The store
keeps one observation per caller, app and hour; retries are idempotent, and
space totals deduplicate a caller across apps. These identifiers are never
metric labels. Anonymous callers are excluded, no analytics cookie is issued,
and an application's own API key is not interpreted as a gateway identity.
Counts use the same time range as traffic and appear in the gateway report's
`totals.authenticatedCallers` and each series point.

**MCP operations.** The gateway MCP plugin emits the
[`mcp.server.operation.duration` histogram](https://github.com/open-telemetry/semantic-conventions-genai/blob/9ffc7e00c639be7712e544e3e54331e885dccf84/docs/gen-ai/mcp.md#metric-mcpserveroperationduration)
using that Development convention snapshot. It observes inbound requests and
matched results, including JSON-RPC errors and tool results with `isError: true`.
Notifications, progress updates and server-initiated requests are not counted as
completed client operations. Unknown request methods are grouped as `_OTHER`.
Latency is measured at the gateway, including the upstream hop; it is not an
application's internal execution time. HTTP requests and MCP operations are
separate counts: an HTTP 200 can carry a failed tool result.

The histogram uses seconds with boundaries
`[0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300]`.
Reports include method, tool/prompt name, operation and caller error counts,
and average/p50/p95 latency. JSON-RPC codes `-32700`, `-32600`, `-32601`,
`-32602` and `-32002` are caller errors. Other JSON-RPC errors, tool errors and
observed HTTP transport failures are operation errors.

Observation never reads ahead or delays a stream to finish parsing. Each JSON
body or SSE event has a 1 MiB observation limit; oversized, encoded or malformed
messages continue through unchanged but may have no MCP measurement. Only
metadata is exported: arguments, results, resource URIs and request/session IDs
are never stored in telemetry or used as metric labels. Hashed request/session
and SSE event IDs support in-memory correlation and replay deduplication, capped
at 4096 request entries and 4096 event entries for 10 minutes per gateway.
Resumption on another gateway or after state expires cannot recover the missing
observation. A disconnected stream without a matched result is not guessed to
be successful or failed. This plugin covers Streamable HTTP, not stdio or the
legacy HTTP+SSE transport. HTTP traffic remains visible in every case.

The [FastMCP sample](testdata/fastmcp-sample) exercises successful calls and tool
errors with no telemetry code or SDK. The analytics console shows MCP operations
as soon as the gateway observes them. Add further protocol plugins at the
internal `telemetryPlugin` interface; the shared hooks own proxy integration,
while plugins own protocol interpretation. All data follows normal telemetry
retention and space deletion.

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

## Web analytics

Every web app gets an **Analytics** tab in the console — visitors, visits,
page views, bounce rate and visit duration against the previous period, a
time series, and breakdowns by page, entry and exit page, channel, referrer,
UTM campaign, country, language, browser, OS and device, plus custom events
and their properties. Every row is a filter, and a live count shows who is on
the app right now. Ranges are the same as Traffic: 48 hours, 7 and 30 days.

Nothing has to be built in. donkey-gateway adds one tag to the `<head>` of an
app's HTML pages on their way out:

```html
<script defer src="/_donkey/s.js"></script>
```

The script is served by the gateway on the app's own address, and so are its
events (`POST /_donkey/e.json`): same origin, no third party, no CORS. It counts
the page load, client-side route changes (History API and `#/` hash routers),
how long each page stayed visible, and clicks on links to other sites. Apps add
their own events in code or in markup:

```js
window.donkey?.track('signup', { plan: 'pro' })
```

```html
<button data-donkey-event="signup" data-donkey-event-plan="pro">Sign up</button>
```

The gateway adds the script only where it is safe to: a `GET` navigation
(`Sec-Fetch-Dest: document`) answered `200 text/html`, on a web-mode route of
an app that has not opted out, when the response's Content-Security-Policy
allows same-origin scripts and requests, and no `Cache-Control: no-transform`
says otherwise. For those requests it asks the app for gzip only, inserts the
tag right after `<head>` (else before `<body>`), and compresses the page again
while it streams, so pages that flush early still do. The ETag becomes weak,
because the bytes changed. A page that already loads `/_donkey/s.js` is left
alone, and a page whose CSP keeps the script out (nonces, `'strict-dynamic'`)
can add the tag itself. API-mode and internal apps never get it. To switch it
off, set `analytics: false` in donkey.yaml or use the toggle in the app's
Settings tab, which holds until donkey.yaml says otherwise.

**What is stored.** The gateway turns each event into report dimensions before
anything leaves it. The browser, OS and device come from the user agent and
the country from the browser's time zone (no GeoIP database). The referrer
keeps only its host, dropping its path, and the URL keeps its path, dropping
the query string except the `utm_*` and `ref` values. Requests from bots,
headless browsers and other sites are dropped. Nothing is written to the
visitor's browser: no cookie and no storage. A **visitor** is an HMAC made
with the telemetry key. For anonymous visitors it is a per-app hash of IP
address and user agent that changes every UTC day, so one person returning on
another day counts again and cannot be followed across days or apps. For
signed-in visitors it is a per-app hash of the subject. A **visit** ends after
30 minutes without activity, and a **bounce** is a visit with one page view
and no custom event. DuckDB keeps the raw events for the telemetry retention
period (30 days). Visits are worked out when the report is read, so any filter
can select them. Removing the space removes its analytics. To exclude your own
browser, run `localStorage.setItem('donkey:ignore', '1')`.

## App insights

The console labels apps with **insights**: what donkey has noticed about them,
good or bad. Nobody sets an insight. Donkey works them out, so the next deploy
or a busier week changes them.

- **Unmaintained**: the running build is older than the platform allows: 90
  days unless a platform admin picks another limit in **Account → Platform
  settings**. The build's age comes from its image tag, so a rollback to an
  old image counts as old.
- **Popular**: one of the platform's most used apps. Apps are ranked by
  people on an average day over the last week. The busiest tenth are popular
  (at least the busiest one), as long as five or more people use each on an
  average day. A person is a signed-in caller where the app has any, since
  crawlers never sign in. Otherwise it is a browser visitor from
  [web analytics](#web-analytics), which drops bots. So public APIs, and
  public apps with analytics turned off, are never popular.

An app's insights show on its tile and are explained on its overview.
Unmaintained apps are also counted on each space's row, and the overview has a
filter for them. The API carries insights as a list of `{ kind, … }` with the
facts behind each (`since` for unmaintained, `people` for popular) on
`GET /v1/apps` and `/v1/fleet`. The console leaves out kinds it does not know.

## What donkey works out for you

Everything below happens on `donkey init` and again on every `donkey deploy`.
Two readers run over the folder and their answers are combined:

| | **Scanner** (in-process, deterministic) | **Assistant** (your OpenAI-compatible model) |
|---|---|---|
| Question | what do the files prove? | what is true about this app? |
| Sees | manifests, lockfiles, version files, framework configs | everything the scanner found, plus the source — it reads the app with `read`, `glob`, and `grep` |
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
✓ Image ready: registry.example.com/donkey/acme/wiki:4f2a9c1b03de-20260725143022
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

public: true                  # requires allowPublic: true in space settings
mode: api                     # web (default) | api with OAuth
cors: allowAll                # allow browser calls from every origin
analytics: false              # no web analytics script (default: on for web apps)
internal: true                # no address at all; siblings reach it by name
routes:                       # …or answer on another app's address
  - app: dashboard
    path: /api

command: ["node", "server.js"]        # override the image's start command

healthcheck:
  path: /healthz              # empty = check that the port answers

env:                          # non-secret values; `donkey env set` for the rest
  GREETING: hello
  DB_URL: ${db.DATABASE_URL}   # resolve a service binding when the app starts

storage:                      # one disk, each path its own folder on it
  size: 5Gi
  class: fast-ssd             # default: the cluster's default StorageClass
  paths:
    - /app/data

addons:                       # space services this app uses; the deploy
  - type: postgres            # creates missing ones. postgres | redis | s3 (S3)
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
  context: .                  # relative to the app; .. for an uploaded workspace
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
```

Default resource requests reserve scheduling capacity; Donkey imposes no
default CPU, memory, or ephemeral-storage limits on workloads or platform
services. Build-context, file, and object uploads have no application-defined
size ceiling. Model calls and database/storage requests follow caller
cancellation rather than a fixed operation deadline. Release records retain
complete logs and repair patches. Connection establishment and health probes
still have timeouts so unreachable services can be detected and repaired.

Environment values such as `${db.DATABASE_URL}` reference an addon by name
and binding key. They are resolved at runtime, so the config contains no
copied credential. Selecting a managed PostgreSQL, Redis, OpenAI or S3 service
also replaces stale values under its standard variable names with binding
references. Custom variable names can reference those same keys. Runtime
service references cannot be used in `build.env`; browser URL variables are
passed as build arguments and declared in each Dockerfile stage.

**How an app is reached.** By default it gets `<app>.<space>.<domain>` behind
the login gateway. `host:` moves it: `"@"` puts it at the space's own address
(`<space>.<domain>` — one app per space can be that front door), a plain name
gives it a label of its own under the space (so the address outlives a rename),
and anything with a dot is taken as an exact hostname. `mode: api` marks MCP
servers and ordinary APIs. With the default protected exposure, the gateway
provides OAuth discovery, PKCE and resource-bound access tokens. Clients open
the existing identity-provider login and return with a token the gateway
consumes before proxying. Compatibility client registration is automatic and
stateless, so an administrator does not have to create client IDs. API mode
does not invent an OAuth scope; space membership controls authorization.
`public: true` exposes either a web app or API without login only when its space
has `allowPublic: true`. Public access is off by default. `internal: true` gives
either mode no gateway exposure and cannot be combined with `routes:`, `host:`
or `public: true`. Shared-address routes require a non-internal app and still
require login unless the app explicitly sets `public: true`.
`cors: allowAll` makes the
gateway answer browser preflights and adds `Access-Control-Allow-Origin: *` to
responses. `analytics: false` keeps the gateway's
[web analytics](#web-analytics) script out of the app's pages. Space admins, developers and viewers can pass the sign-in; platform
admins can open every space's apps. `internal: true` drops the address — the app has no
hostname, no certificate and nothing on the internet, and its siblings call it
as `http://<name>`. `routes:` puts it on *another* app's address under a path
prefix. All of it can also be changed in the console's Settings tab without a
deploy — a console choice holds until donkey.yaml itself settles the matter,
which always wins.

The demo backend claims `/api` and `/mcp` on the dashboard's address and is
`public`, so those paths and its own hostname stay open for machine clients
while the dashboard still asks for a login. It is externally reachable and
therefore cannot be `internal`. Protection attaches per route,
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
| `s3` | S3-compatible object storage | the AWS SDK convention (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL_S3`, `AWS_REGION`) + `S3_URL` |
| `redis` | Redis with persistent storage and password authentication | `REDIS_URL`, `REDIS_ADDR`, `REDIS_HOST`, `REDIS_PORT`, `REDIS_USERNAME`, `REDIS_PASSWORD`, `REDIS_DB`, `REDIS_TLS` |
| `openai` | the platform's AI — nothing runs in the space | `OPENAI_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_MODEL` |
| `exa` | web search and page contents — nothing runs in the space | `EXA_BASE_URL`, `EXA_API_KEY` |

The `s3` addon provisions RustFS and can also attach any compatible S3
endpoint. `rustfs` remains accepted as a legacy type alias. Existing services
keep their names, credentials, endpoints and data volumes; changing the type
does not rename the instance. To update an existing declaration explicitly:

```yaml
addons:
  - type: s3
    name: rustfs   # preserves the existing service and its data
```

New `type: s3` entries default to the service name `s3`.
The managed server image remains `rustfs/rustfs`, configured through
`addonImages.rustfs` (`DONKEY_RUSTFS_IMAGE` on the server).

PostgreSQL and MySQL console scripts run on one connection per Run. Put the
whole transaction, including `BEGIN` and `COMMIT` or `ROLLBACK`, in that run;
connections are not retained between runs. Execution stops at the first error
and closes the connection, rolling back any unfinished transaction. Statements
outside an explicit transaction retain the database's normal commit behavior.

Services of protocol `trino` come from an add-on plugin (`TRINO_URL`,
optional `TRINO_CATALOG` and `TRINO_SCHEMA`) and open in the console's Trino
viewer. It browses catalogs, schemas and tables, previews up to 200 rows,
and runs one SQL statement at a time with up to 500 result rows. It does not
count entire tables.
SQL runs with the user's permissions; table previews are read-only.
Trino uses the same database API as PostgreSQL and MySQL: catalogs appear
under `databases`, with schema selection on tables and queries. Unknown
sizes and row totals are `null`; row pages report `hasMore` without counting
the entire table.

The viewer authenticates as the user, not as donkey: it sends the user's IdP
token for donkey's API (`auth.scope`) as `Authorization: Bearer <token>`
to the add-on service at `TRINO_URL`, over HTTP or HTTPS, on statement,
result, retry and cancellation requests. The add-on exchanges the token
on-behalf-of for Trino's own. Without `auth.scope`, the viewer says so instead of
connecting. donkey's own sealed token never leaves donkey, and no user token
is stored in the service binding.

For local development, `donkey addon tunnel <service>` serves Trino's client
protocol on `http://127.0.0.1:8080` (`--port` to change) and prints
`TRINO_URL`, `TRINO_CATALOG` and `TRINO_SCHEMA` for it. Point the app or any
Trino client at it without credentials: each request goes through the
gateway, which mints the signed-in user's (`donkey login`) IdP token, to
donkey-server (`/v1/spaces/<space>/services/<service>/tunnel/v1/statement…`),
which forwards it to `TRINO_URL` as `Authorization: Bearer`, exactly like the
console. Only the statement API passes, a client's own `X-Trino-User` is
dropped (the token decides who runs the query), results stay inline (a client's
`X-Trino-Query-Data-Encoding` is dropped, so no spooled segment is fetched
around the tunnel), and continuation links (`nextUri`, `partialCancelUri`)
are rewritten to point back at the tunnel. The tunnel needs the same
`auth.scope` as the console, and a `donkey login` made after it was
configured.

Services of protocol `webdav` come from an add-on plugin too (`WEBDAV_URL`,
an HTTP or HTTPS base URL) and open in the console's file browser — the same
browser the `s3` service has, with one difference: a WebDAV service is one
tree, so there is no bucket pane; folders are real collections, made and
removed as such. The browser authenticates as the user exactly like the
Trino viewer: every request carries the user's IdP token for `auth.scope` as
`Authorization: Bearer <token>`, and the add-on decides what that user may
see — for OneDrive or SharePoint, by exchanging it on-behalf-of for a Graph
token. Without `auth.scope`, the browser says so. No credential is stored in
the binding, and the plugin should expect none: apps reach the service with
the user's token a login-protected request hands them as
`X-Forwarded-Access-Token`. A plugin only needs WebDAV class 1 (no LOCK);
the browser uses PROPFIND, GET, PUT, DELETE and MKCOL.

`donkey addon tunnel <service>` serves a WebDAV service on
`http://127.0.0.1:8083` the same way, adding the signed-in user's token to
every request, so Finder, Explorer, davfs2 or `curl -X PROPFIND` reach it
without credentials. The whole protocol passes: MOVE and COPY name their
destination by the tunnel's address and donkey-server points it at the
service, file names keep their escaping, and what the service says about
itself — the hrefs of a PROPFIND answer, the `Location` of something just
made — comes back pointed at the tunnel, even when `WEBDAV_URL` has a path.
Finder mounts a service read-write only if it supports LOCK (class 2); the
console needs no locking.

Every type describes what can be chosen for it (a version, a disk size…) as
a JSON Schema, which the console renders as a form and the CLI takes as
`donkey addon add postgres --param storage=5Gi` (or `--version` and
`--storage` for those two). What was chosen is recorded with the service and
kept when it is stopped and re-added.

Out-of-tree services join this catalog through the versioned
[gRPC addon provider API](pkg/addons/v1/README.md): a provider lists any
number of offerings (databases, caches, APIs…), each describing its own
parameters the same way, streams provisioning progress, and returns app
environment variables and explicit network destinations. Providers are only
called when something needs them, never at startup, and every instance
records the offering it was created with, so a provider that is down slows
nothing but its own provisioning. Configure `addonPlugins` in the server
installation settings; the
[MariaDB example](examples/addon-mariadb/README.md) provisions in a separate
namespace and works with restricted Kubernetes egress.

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

**Configured HTTP services.** Define proxy offerings in the server installation
settings. Each gets a title and description in the catalog, while target URLs
and outbound proxy credentials stay in the control plane:

```yaml
# With an empty commonOfferings list, every offering is available everywhere.
# A nonempty list lets space admins enable additional offerings explicitly.
commonOfferings: [postgres, redis, s3, openai, exa]
proxyServices:
  - name: enterprise-api
    title: Enterprise API  # optional; defaults to name
    description: Company API
    targetUrl: https://api.corp.example/v1
    proxyUrl: http://proxy.corp.example:3128  # optional; HTTP or HTTPS
```

In the space's settings, add `enterprise-api` to its offerings (or set
`offerings: {enterprise-api: {}}` in the space config). Apps then connect it
like any other service:

```yaml
addons:
  - type: enterprise-api
    name: corporate
env:
  CORP_BASE_URL: ${corporate.ENTERPRISE_API_URL}
```

The generated `ENTERPRISE_API_URL` points at `donkey-proxy`. Appending `customers?limit=10` to
that URL forwards to `https://api.corp.example/v1/customers?limit=10`.
For example, a Kubernetes binding might be:

```text
http://donkey-proxy.donkey-system.svc/services/enterprise-api/
```

No token or special request header is needed. The proxy resolves the socket's
caller IP to a space and checks that space's provisioned service binding.
Kubernetes uses current pod identity labels; Docker uses the space's owned
network subnet. All apps in the granted space may use the endpoint. Attaching
the service injects its URL into the app. Unknown callers, ambiguous identities
and callers without a space grant are denied; forwarded IP headers are ignored.

Methods, bodies, escaped paths, query strings and app Authorization headers
are preserved. Target URL credentials, if configured, supply upstream Basic
authentication instead. Redirects within the target base path and cookie
paths are rewritten; response bodies and links inside them are unchanged.
Use this for HTTP APIs whose clients accept a configurable base URL. Clients
that require original URLs or sign the URL itself need a different integration.

One shared Go reverse proxy serves all offerings and spaces. Configuration
reloads within 30 seconds, plus Kubernetes Secret projection delay. Removing
the service from a space revokes its grant;
removing a global proxy entry disables its route. Changing space offerings
controls new connections and keeps existing services available for their
lifecycle. Apps use internal HTTP, while targets and outbound proxies may use
HTTPS with normal certificate verification. Your enterprise system CA bundle
must be present in the **proxy pod**; it is not needed in app pods. There is
no separate per-offering CA setting.

This endpoint is internal and needs the caller's original workload IP.
Kubernetes authorization reads current pod and binding records; Docker's
server refreshes a read-only network mapping every 10 seconds, which the
proxy refuses after 30 seconds without a refresh. The proxy has no Docker
socket. Traffic through an intermediary that replaces the source IP is denied.

In rendered deployments, `proxy-services.json` is private (mode `0600`):
Kubernetes loads it into the `donkey-proxy` Secret; Compose mounts it read-only.
Updating target configuration does not restart the shared proxy. New offering
names also require the admin server to reload its catalog configuration.

In the web console, open the HTTP service to test a request. Choose a method,
enter a path relative to its base URL (or its full binding URL), and add headers
and an optional body. The result shows status, timing, response headers, and up
to 1 MiB of response text. Requests have a 30-second timeout; redirects are shown
without following them. The console uses your space management permission and
a short-lived, request-specific control-plane grant through the shared proxy.
Apps continue to use their workload IP without a credential.

**Configuring OpenAI and Exa.** The installation file selects the upstreams:

```yaml
ai:
  baseUrl: https://api.openai.com/v1
  model: gpt-6.1-sol
exa:
  baseUrl: https://api.exa.ai
```

Supply `OPENAI_API_KEY` and the optional `EXA_API_KEY` as environment variables
when running `donkey server install -f server-install.yaml`. The installation
also accepts `ai.apiKey` and `exa.apiKey`, or `--openai-api-key` and
`--exa-api-key`; environment variables keep credentials out of the settings
file. `--openai-base-url` and `--exa-base-url` override the URLs. Exa also reads
`EXA_BASE_URL` when `exa.baseUrl` is omitted; its default is `https://api.exa.ai`.
Rendered Kubernetes and Compose bundles take the keys from `secrets.env` and
preserve existing keys on upgrades. Exa is optional: without its key, its proxy
returns a configuration error while OpenAI continues working.

For a gateway running on your laptop at port 4242, Docker Desktop containers
can use `http://host.docker.internal:4242/v1` for `ai.baseUrl`. Set `exa.baseUrl`
to the gateway's Exa-compatible base, including any route prefix it needs.
For Wingman, `http://host.docker.internal:4242/v1/exa` forwards search to
`http://host.docker.internal:4242/v1/exa/search`. Use an address reachable from the
cluster for Kubernetes; `localhost` inside a pod or container names that pod
or container itself.

Connect an app with `donkey addon add exa`, or declare it in `donkey.yaml`:

```yaml
addons:
  - type: exa
```

The app reads the injected `EXA_BASE_URL` and sends `EXA_API_KEY` in the
`x-api-key` header (bearer authentication also works). Configure your SDK to
use that base URL explicitly. The platform proxy serves only `POST /search`
and `POST /contents`, passing request and response bodies through unchanged,
including contents requested inside a search. As with OpenAI, bindings carry
per-space stand-in keys, the upstream key stays in `donkey-ai`, and removing
the service revokes its token within the proxy's 30-second cache lifetime.
The space's service chip opens a tester with **Search** and **Page contents**
tabs; console calls use the saved binding and never forward your login session
to the provider.

To attach an independent endpoint, use
`donkey addon attach exa --url https://search.example.com/exa`. This supplies
`EXA_BASE_URL`; put the endpoint's own `EXA_API_KEY` in the app environment.

**Getting at a service yourself.** Services rightly have no public address,
so donkey provides the ways in: `donkey addon tunnel NAME` opens a private
line from your machine (localhost listener, carried over the authenticated
API, alive only while the command runs), and the console opens a browser per
capability. The tunnel follows the service's protocol (`internal/addons`'
tunnel table): it listens on the protocol's usual port, prints the binding
rewritten for localhost plus a ready-to-run line (`psql "postgres://…"`),
and either carries bytes to the service or carries requests through
donkey-server, which adds what the service needs to know who is asking:

| Protocol | Tunnel | Local port | donkey adds |
| --- | --- | --- | --- |
| postgres, mysql, redis, s3 | bytes | 5432, 3306, 6379, 9000 | — |
| trino | requests, `/v1/statement` only | 8080 | the user's IdP token |
| openai, exa | requests | 8081, 8082 | the space's key from the binding |
| configured proxies (`http`) | requests | any free one | the console's grant; the client's own `Authorization` passes |

A protocol without an entry gets a byte tunnel at the service's own port.
Request tunnels go through `…/services/<service>/tunnel/<path>` and never hand
on donkey's credentials. Endpoints attached from elsewhere (`addon attach`)
get no tunnel: they are reached at their own address. The console's browsers
cover tables, rows and a SQL view for anything with a `DATABASE_URL`;
buckets, uploads and downloads for anything speaking S3; a streaming
markdown chat for the AI, using the very token and proxy an app would.

## Machines

A machine is a development environment of your own inside the installation:
a container with Python, Node and Go, the `donkey` CLI already signed in as
you, and the [Wingman](https://github.com/adrianliechti/wingman-agent) coding
agent. It opens in the console — **Machine**, beside *Deploy* on the front
page, or *Your machine* in the ⌘K palette from anywhere — with the agent's
editor, chat and terminals shown right there. Nothing to install on a
laptop, and nothing a laptop is allowed to reach has to be reachable from one.

```
 browser ── https://machine.<domain> ───────▶ donkey-gateway
   (the console shows it in a frame)            · signs you in (one sign-in with the console)
                                                · leads you to your machine, and to no other
                                                · presents the machine's credential
                                                        │
                                             ┌──────────▼───────────────────────┐
                                             │ your machine                     │
                                             │  front door :8080  (checks it)   │
                                             │  agent, in the folder you opened │──▶ donkey-ai (model)
                                             │  donkey CLI, signed in as you    │──▶ donkey-server (API)
                                             │  /data  your disk                │
                                             └──────────────────────────────────┘
```

**One per person, not per space.** A machine belongs to whoever is signed in.
It is not an app and has no space: it appears in no app list, and nothing a
space's members can do reaches it. Platform administrators and everyone who
is an admin or developer of at least one space may have one; a sign-in alone,
or a viewer's role, does not — a machine costs compute, disk and model calls.

**It follows the folder.** There is no machine to create and none to start.
One dialog asks one thing — which folder to open — and the machine follows
from the answer: it is made, disk and all, for the first folder, and started
whenever a folder is opened and it is not running. The dialog comes up over
whatever page you are on — from the Machine button, from the palette, from
an app's Source tab, from the folder named in the header while the agent is
open — so closing it leaves you where you were, and nothing is made or
started before a folder is named. With a folder already open, the Machine
button goes straight to the agent.

What can be done to the machine itself, for the day it should not look after
itself, is the dialog's last section and a menu beside the agent: *Stop*
ends what is running now and keeps the disk; *Delete* gives the disk back,
and leads back to the overview — there is no page for not having a machine;
*Restart* (in the menu) brings it back on the installation's current image
with a fresh sign-in, in the folder it had open.

**It stops when nobody uses it.** A machine is a desk, not a service: it is
worth its CPU and memory while you are at it or its agent is working, and a
start brings everything back, because the disk stays. So a machine that has
gone unused for an hour (`machines.idle`, below) is stopped — the same stop
as the one in the console, asked for by the machine itself with your
sign-in. "In use" is what its front door can see, and it is deliberately
generous:

- *a browser is talking to it* — any request through the gateway, and every
  byte on a connection that stays open, which is all a terminal ever sends;
- *something in it is talking to its model.* The machine reaches `donkey-ai`
  through its own front door (inside a machine, `OPENAI_BASE_URL` is a
  loopback address), so an agent left to finish a long task keeps its
  machine with no browser anywhere.

A tab left open on a computer that is awake counts as use; a closed laptop
does not. The console asking a machine for its folders never counts.

**Folders.** Your work is in folders under `/data/workspace`, and the agent
has one of them open at a time: its files, its sessions and its terminals
start there. The dialog lists your folders, the one used last first, and
its last row makes a new, empty one. The folder that is open is named in the
header, and choosing another moves the agent there: what was running in the
first one ends, its files stay. The machine itself does not restart for
that, so it takes seconds.

A machine that stops by itself is, most days, found stopped — so it can say
what is on its disk without being started. While it runs, `donkey-server`
keeps the list of its folders with the disk (an annotation on the
PersistentVolumeClaim; a note in the state directory on Docker), and the
dialog offers that list for a stopped machine: picking a folder starts the
machine and opens it. The note is what the console last saw, not the disk
itself — a folder made in a terminal while no console was looking is not on
it — so the dialog also offers to start the machine just to look.

**An app's source, in a folder.** The dialog's *From an app* section lists
the apps you can see; pick one and a version (the newest, unless you choose
another), and it is copied into a new folder as the first commit of a git
repository there — so everything you and the agent change from then on
reads as a change, and `donkey deploy` from the folder ships it. Every
version in an app's [source history](#source-history) also has an *Open in
your machine* button, on the app's Source tab, which opens the same dialog
on that version. The machine fetches the source itself, with your sign-in:
you get what the Source tab would let you download, and nothing else. A
folder is only ever made, never filled: where a folder of the app's name is
already there, the dialog offers to open it or to put the version beside it
under another name.

**What is yours and what is the image's.** The disk is mounted at `/data`:
your home is `/data/home`, your folders are in `/data/workspace`. Everything
outside `/data` comes from the image and is the same after every restart. A
machine runs as an unprivileged user and cannot become root, so there is no
`apt-get install` — what you install for yourself goes to your home, where
the image already points the tools: `npm install -g`, `go install`,
`uv tool install`, `uv python install`. A toolchain everybody needs belongs in
the image (below).

**What a machine is handed** when it starts, and nothing else:

- *Your donkey sign-in*, so `donkey deploy` works without a login. It is a
  token of the machine's own, and it ends when a `donkey login` made at the
  moment you signed in to the console would: 30 days after that sign-in,
  however often the machine is restarted. The console says so a few days
  ahead; restarting from the console renews it. No IdP token goes in.
- *A model token* for `donkey-ai`, as `OPENAI_BASE_URL` / `OPENAI_API_KEY` —
  the same stand-in scheme a space's `openai` service uses. It is generated
  per start and stops working when the machine stops. The agent starts on
  the installation's `ai.model`.
- *Your name and email* for `git config`, unless you already set your own.
- *The installation's package mirror*, the way a build gets it: npm, pnpm,
  yarn, bun, pip, uv and Go are pointed at `donkey-packages` by the names
  they read themselves (`NPM_CONFIG_REGISTRY`, `PIP_INDEX_URL`,
  `UV_INDEX_URL`, `GOPROXY`) — and Corepack, which fetches pnpm or yarn
  itself for a project that names one, by `COREPACK_NPM_REGISTRY`. `npm install` in a terminal or by the agent
  installs from the same registries as a deploy does, private packages
  included, and no upstream credential comes near the machine. There is
  nothing to prefix and nothing for the agent to remember. Lockfiles written
  in a machine are a developer's lockfiles (see
  [Packages on a developer's machine](#packages-on-a-developers-machine)):
  npm leaves the address out, `uv.lock` names the mirror's, and a deploy
  reads either.
- *The donkey skill* for its agent
  ([examples/skills/skill.md](examples/skills/skill.md)), installed in
  `~/.agents/skills/donkey` on every start: how to deploy and operate apps
  with the CLI, and what is different about working in a machine.

**How it is reached, and by whom.** The agent was written to listen on a
developer's loopback and trusts whoever reaches it. Three things stand in
front of that trust:

1. *One address, apart from the console's.* Every machine answers on
   `machine.<domain>`. Nothing of a machine or of its owner is in that name,
   so there is nothing to make when one starts: the installation sets the
   name up once — its certificate and, where donkey publishes them, its DNS
   record — and a machine answers the moment its container does, the first
   time and after every stop. No name that stands for a person reaches a
   certificate, or the public logs certificates end up in. An app's address
   has two labels under the domain, never one, and the gateway serves
   nothing but machines on this one even if a space that may use any
   hostname names it.
2. *The gateway.* Which machine you reach is decided from your sign-in and
   from nothing a request says — there is no way to name somebody else's.
   A machine's route admits one subject: the owner. Not the members of any
   space, and not a platform administrator, who may manage everything else
   but has no business in someone's home directory and sign-in. Signed in
   with no machine running, you are told so.
3. *The front door.* Inside the container, `donkey machine serve` listens on
   the one reachable port and refuses every request that does not carry a
   credential only the gateway can compute (derived per machine from the
   session key). So reaching the container is not enough — not for a
   neighbour on the same network, and not on a cluster whose network plugin
   ignores policies. It also refuses requests made by another site's page.

`donkey-server` reaches the same door for the two things it asks a machine
on your behalf — which folders it has, and to open one. It presents a
credential of its own, which the gateway's does not open and the gateway
never forwards: nothing a browser sends, yours included, is taken for the
control plane, and the control plane's credential is no way to the agent.

The console shows the machine in a frame, from that address. Being a
different origin is the point — and the reason machines are not served
under a path of the console itself, which would have needed no name at all:
nothing running in a machine can read the console's page or act with its
session, and the gateway lets only the console — and the machine itself —
frame it.

Sharing one address among machines means a browser files what any of them
leaves behind — unsent drafts, cached scripts — under the same origin. The
gateway keeps that from carrying over between people who sign in on the
same browser: it notes, in a cookie no machine can read or write, whose
machine a browser showed last, and has the browser drop what is stored for
the address before it shows another one's; responses are cached per
sign-in, so one person's never answer for another; and a machine's address
takes no service workers, which would answer for it without asking the
gateway.

A machine reads *running* the moment its container says it is up, and the
console opens your folder and shows the agent right then — which can be a
moment before the gateway and `donkey-server` can reach it (on Kubernetes,
before every node has heard of the new pod). Both wait for it, up to twenty
seconds, instead of answering with an error; and if a machine still is not
there after that, the page the gateway shows tries again by itself.

**Previews, and other ports.** The agent has one port, but a machine serves
more than that: the agent's HTML preview comes from a second loopback port,
and a server you start listens on a port of its own. Each of them answers on
the machine's address under a path, `/.donkey/port/<port>/`, and a redirect
that software in the machine writes to `http://127.0.0.1:<port>/…` — which is
how the agent opens a preview — is moved there, so previewing an HTML file
in the agent just works. Two things follow from sharing one address:

- *Previewing is running.* A page served from the machine's address can do
  in the agent what you can — open a terminal, change files — so a preview
  is trusted like code you build. That is what a machine is for; just do
  not preview a page you would not run.
- *A path is not a root.* A page that asks for `/assets/app.js` asks the
  agent, not the port. Pages with relative links work as they are; a
  development server has to be told its base path (`vite --base
  /.donkey/port/5173/`), and one that cannot be has no address yet.

**What can reach it, and what it can reach.** Both are limited, on both
runtimes, and neither depends on how the installation treats its spaces.

- *In:* the gateway and `donkey-server`, on the machine's front door, each
  with its own credential, and nothing else — no app, no other machine, no
  other workload of the cluster.
- *Out:* the control plane and nothing beyond it — DNS, the API its CLI
  calls, the AI endpoint its agent asks and the package mirror its package
  managers install from. There is no way out to the internet.

That is a decision about machines, not about workloads in general. A machine
runs an agent that acts on what it reads, with its owner's sign-in in reach;
left with the mirror, its model and the API it can still build, test and
deploy, and has nowhere to send anything else. It also means a machine
cannot clone from a Git host, fetch a URL or download a toolchain the mirror
does not carry (`uv python install`, the agent's own language servers). An
installation whose machines have to can open them:

```yaml
machines:
  egress: open       # default: restricted
```

Opened, a machine reaches the network at large — except, on Kubernetes, the
link-local range where cloud metadata services hand out node credentials.

On Kubernetes, machines live in a namespace of their own (`<prefix>platform`,
Pod Security `restricted`) or, where the installation was given one
namespace, beside the apps; one NetworkPolicy fences all of them, and a
machine's pod holds no Kubernetes credential. It takes a network plugin that
enforces NetworkPolicy — the front door's credential check holds either way,
the limit on what a machine can reach does not. On Docker each machine has a
network of its own, shared only with the gateway, the API, the AI endpoint
and the package mirror, and made internal — no route out of the engine —
unless machines are opened.

The agent's own per-command sandbox is switched off inside a machine: it is
built from user namespaces, which a container without privileges cannot
create. The machine is the sandbox — and it holds your sign-in, so what the
agent can do with `donkey` is what you can do. Its reviewer still asks before
destructive commands.

**Turning machines on.** Name the image in the install settings:

```yaml
machines:
  image: ghcr.io/example/donkey-machine:1.0.0
  storage: 10Gi        # each machine's disk (Kubernetes; a Docker volume has no size)
  storageClass: ""     # default: the cluster's
  cpu: "2"             # the most one machine may use
  memory: 4Gi
  egress: restricted   # or open — see above
  idle: 60m            # how long a machine may go unused before it stops; never to keep them
```

or pass `--machine-image` to `donkey server install`. Without an image the
feature is off: the console shows no way to it and the API answers 404.
`task machine-image` builds the image from
[hack/Dockerfile.machine](hack/Dockerfile.machine) (`task publish:machine`
pushes it for both architectures).

**An image of your own.** Add toolchains by building on that Dockerfile. The
contract is small: the entry point is `donkey machine serve`, which listens on
port 8080; the container runs as user 1000 with `/data` as its only writable
home; and `donkey machine serve -- <command>` runs another agent or editor
instead of Wingman, as long as it serves on the loopback
(`--upstream http://127.0.0.1:<port>`). The command is started in the folder
that is opened, and ended and started again in the next one.

**Not there yet.** One machine per person, and one folder open at a time. A
restricted machine cannot clone from a Git host, so its folders start empty
or from an app that is deployed here. Other ports share the machine's address
under a path rather than having one of their own, so a server that needs its
own root is not reachable. Machines are managed in the console only; the CLI
has no command for them.

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
| **Kubernetes cluster** with a default StorageClass | donkey-telemetry always claims its 2 Gi DuckDB volume and donkey-orchestrator its 5 Gi `donkey-data` volume for the [source history](#source-history) (unless `data` names a claim of your own); apps with `storage.paths` and provisioned services claim their own | any conformant cluster; nothing donkey-specific. Donkey brings its own edge, normally as a `LoadBalancer` Service; on clusters where nothing assigns those an address (kind), set `gateway.serviceType: NodePort` or `ClusterIP`. cert-manager is required only for `tls.mode: cert-manager`; external-dns with `--source=crd` only when `gateway.externalDNS` is set |
| **Container registry** the cluster can pull from | donkey pushes every build there | `registry.url` + credentials; `registry.caSecret` when its certificate comes from a private CA |
| **DNS** pointing public hostnames to the gateway address | apps are served at `<app>.<space>.<domain>`, the API and login callback at `<domain>`, [machines](#machines) at `machine.<domain>` | configure a wildcard plus apex yourself, or let `gateway.externalDNS` publish exact records |
| **OIDC provider** (Pocket ID, Entra ID, Keycloak, …) | the one way in: console, CLI and protected apps all sign in against it | required — `auth.issuer` (+ client id/secret); `platform.admins` optionally seeds the platform admins |
| **OpenAI-compatible endpoint** | donkey reads every app with it (ports, health routes, data folders) | required — the server refuses to start without one. Point it at a model you control: source summaries go there |

A complete copy-paste walkthrough that stands all of this up on a single
Debian server — TLS comes from the gateway itself via Let's Encrypt — exists
twice: [examples/INSTALL_KIND.md](examples/INSTALL_KIND.md) (kind cluster, a
registry container, an OIDC provider) and
[examples/INSTALL_DOCKER.md](examples/INSTALL_DOCKER.md) (no Kubernetes at
all — `runtime: docker` runs apps straight on the Docker engine).

RBAC: by default donkey-server and donkey-orchestrator each bind
`cluster-admin` under a ServiceAccount of their own, because they create and
delete namespaces and no Role can express that. Giving them a workload
namespace instead drops each to two namespaced Roles — see [README_KUBERNETES.md](README_KUBERNETES.md#namespaces-and-restricted-clusters).

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

An app that brings a Dockerfile is built with it first. Every other app
gets one written for it, and that is the whole of the build system: there is
one path to the build engine, and what an app is built with is a file you can
read, edit and keep.

Writing it is an investigation, not a guess. donkey's scanner establishes what
the files prove — the lockfile that decides the package manager, the version
files that pin the toolchain, the framework config that names the build output
directory, the dependencies that need OS packages — and hands the assistant
that evidence with the file behind each fact. The assistant then reads the app
for itself using `read` (1-based line offsets), `glob`, and `grep` (regex,
context, and result pagination). During a build, `edit` can create files and
apply batches of exact replacements in an isolated copy of the build context.
Existing files must be read before editing; stale reads and ambiguous matches
are rejected. Planning remains read-only. The investigation establishes: which directory is really the app in a monorepo, the module path of
a Django project, whether the code honors `PORT`. A few well-chosen reads
settle what a file tree can only hint at.

With vulnerability scanning enabled, the assistant can use `check_image`
before selecting a runtime base. This resolves the candidate through the
build engine's configured mirrors for the target platform, then scans the
exact resolved digest using the current vulnerability database. The answer
includes declared runtime versions, OS end-of-life status, database age, and
blocking packages with installed and fixed versions. The assistant can compare
compatible candidates and pin the inspected digest, instead of choosing a
version solely from its training data. There are at most eight checks per
deployment, each bounded to two minutes; repeated candidates reuse the same
evidence within that deployment. This is selection guidance: the completed
app image still undergoes its own scan. Builds resolve tagged bases from the
registry so local image caches do not hide published security updates.

Because the output is an ordinary Dockerfile, everything it names — `FROM
python:3.12-slim`, `apt-get`, `npm ci` — is an ordinary registry or package
pull that `registryMirrors` and the internal package proxy cover. There is
nothing else to reach.

**Keep the build recipe and its source repairs.** The release record includes
its Dockerfile and source diffs, and the console shows automatic repairs as
they happen. Saving the recipe as `.donkey/Dockerfile` makes it the first
choice on later deploys. Source repairs affect only the isolated build copy;
keep the corresponding patches in your project to reproduce that image.

**Build, scan, and rollout failures feed the same repair loop.** The assistant gets
the failed recipe, exclusions, runtime contract, and redacted failure evidence
from every attempt. It can correct the recipe or source, including when an
uploaded Dockerfile fails. There are six attempts by default; repeated failures
with unchanged inputs stop earlier. Generated exclusions are replaced on each
attempt in a Dockerfile-specific ignore file; author exclusions are preserved.
Each repaired candidate gets a distinct image tag so the runtime starts it.

**Readiness requires a reachable app.** BuildKit checks the reviewed command,
non-secret environment and storage layout before exporting. An app that exits,
only binds loopback, or never listens fails. A configured health route must
also respond. Apps needing live services or stored secrets are checked in the
runtime, where those dependencies exist. Docker checks from outside the app
and restores the previous container and app configuration when a replacement
fails. Persistent application data is not rolled back. With `--no-wait`, or
cyclic projects that defer readiness checks, later rollout failures are not
part of the automatic repair loop.

```yaml
build:
  verify: true          # default; false skips running the image
  verifySeconds: 45     # how long the app gets to come up
  attempts: 6           # build and rollout attempts, including the first candidate
  provenance: min       # SLSA provenance attestation: min (default) | max | off
  sbom: true            # attach an SPDX SBOM to every image (off by default)
  scan:                 # vulnerability scan before every push — see below
    rescan: 4h          # re-check deployed SBOMs; 0 disables periodic checks
    block: critical     # refuse findings with a fix at or above: none | critical | high
    ignoreUnfixed: true # findings without a fixed version inform, never refuse
    secrets: block      # a credential baked into the image: block | warn | off
    triage: assistant   # the assistant may waive a refusal it reads as not affecting the app: assistant | off
    # enabled: false    # publish images without scanning them
    # image: aquasec/trivy:<version>@sha256:…                        # mirrored like buildkitImage
    # dbRepository: registry.example.com/ghcr/aquasecurity/trivy-db   # no internet: see Restricted networks
    # javaDbRepository: registry.example.com/ghcr/aquasecurity/trivy-java-db
```

### Vulnerability scanning

**Every image is scanned before it is pushed.** The build engine runs
[Trivy](https://trivy.dev) against the freshly built image the same way it
runs the startup check — the scanner mounted beside the unexported result —
so an image the policy refuses is never pushed under the app's name. The
scanner reads the operating-system packages of the base image and the
language packages installed into it (npm, pip, Go binaries, .NET), plus
credential-shaped strings in files. JAR files are not inventoried yet — see
[the scanner without internet](#the-vulnerability-scanner-without-internet).
The policy is deliberately narrow so that a refusal means something: a
finding refuses the deploy only when it is at or
above `block` **and** the vendor has published a fixed version. Everything
else is said, not enforced — fixable findings below the threshold as one
warning, unfixed ones as one line for the record, an end-of-life base OS as
its own warning. When a finding would refuse the image, the assistant reads
the app first: is the package imported, executed, on a request path, or a
build-time leftover nothing runs? A high-confidence "not affected" verdict
turns the refusal into a warning and the deploy continues, with the reason
and the files it rests on in the deploy output and the release record;
`triage: off` makes the gate purely mechanical. Severity always comes from
the scanner; the assistant's verdict is a source-based assessment, not proof
that a vulnerable function is unreachable. A space admin can also record
a **waiver** in the space settings (`securityWaivers`: app, finding id,
reason, expiry) — deliberately not a `donkey.yaml` key, because a developer
who can lift the gate from the app's own config has no gate. A refused
generated Dockerfile, including a saved `.donkey/Dockerfile` selected by the
automatic builder, goes back through the repair loop. The assistant receives
the scan's OS, database timestamp, affected packages and fixed versions, and
can update a compatible base or dependency before rebuilding and rescanning.
An app's own or explicitly selected Dockerfile is never rewritten over a policy
verdict — the developer gets the findings and the assistant's proposals
instead. Scanner or policy-service failures stop the deploy without rewriting
the app; retry the deploy after the service recovers. A refused image never
replaces the running app. `--no-build --tag` deploys and rollbacks re-read the image's recorded
scan against today's policy: deploying is deploying. The scan's findings,
summary, verdicts and the policy they were judged against are stored with
the release record, together with the image's **inventory**. The work is
split in two: the step inside the build engine only writes an SPDX SBOM of
the unexported image and reads credential-shaped strings off its files — it
needs no network and keeps no state — and donkey-orchestrator matches that
inventory against the vulnerability database with its own copy of the
scanner. The database lives under `scanner/` on the orchestrator's data
volume, beside the source history, fetched once and refreshed on the
scanner's own schedule, so a scan adds seconds to a build. The inventory is
stored with the release (compressed), so re-reading an app against a newer
database later is a lookup against the stored document, never an image pull
and never a build. `donkey doctor` reports the scanner's version, that its
image resolves through the build engine, the database's age on the volume,
and that the database repository answers. The scan's outcome also travels
with the running workload the way the stack does, so the fleet, a space's
tiles and the app's page carry it as a badge — clean, fixable, warning, waived or
needs action — and the Deployment tab shows the numbers by severity, the
policy's findings, and every finding the scanner reported.

The orchestrator rechecks deployed SBOMs **every four hours** by default
(`build.scan.rescan`; `0` disables this). It reads the compressed inventory
from `donkey-data`, matches it against the shared, cached vulnerability
database and updates the badge and **Latest findings** on the app's Overview.
It does not pull application images, rebuild, restart apps or make assistant
calls. Database updates are the main download cost; matching uses CPU and
reads the cached database. If `donkey-data` is on NFS, those reads also use
the storage network. Secret findings from the original image scan and
existing assistant verdicts are preserved; new findings get no automatic
assistant waiver.
New vulnerabilities and expired waivers can mark an app **needs action**,
while the app continues running. Failed checks retain the last successful
findings, show a refresh error and retry after 15 minutes. Checks run one at
a time, skip active deployments, and resume overdue work after a restart.
Images without a stored inventory become eligible after their next build.
If an already scanned app loses its inventory, checks report a refresh error;
rebuilding the app restores the inventory. Incomplete stored scans also stop
rollbacks instead of being treated as clean results.
The interval accepts durations of at least one minute. `enabled: false`
disables both build scans and periodic checks; `rescan: 0` disables only
periodic checks. The latest report is also available at
`GET /v1/spaces/{space}/apps/{app}/security`; it omits the compressed SBOM.

**Every image carries its paperwork.** BuildKit attaches a standard SLSA
provenance attestation to every push (and an SPDX SBOM when opted in), and
the image config carries the OCI `org.opencontainers.image.revision`/`created`
labels — so any registry tool can answer where an image came from without
knowing donkey. `provenance: max` additionally embeds the Dockerfile and
every build-arg **value** into the pushed attestation; never choose it while
`build.env` carries credentials, which is exactly why `min` is the default.
This registry SBOM attestation stays opt-in because it adds a separate scan
(covered by `registryMirrors`) and lengthens every build. The vulnerability
scanner always creates its own stored inventory when enabled, even with
`build.sbom: false`.

**Every deploy leaves a versioned record.** The full release view — plan,
findings, complete build log, the exact Dockerfile the image was built from,
and the decisions the deploy settled — is stored per build in
`donkey-data/deployments/` (the latest 20 releases per app), which is what the console's
Deployment tab renders and lets you browse. A `--no-build --tag` rollback
adopts the decisions recorded with that image, not whatever a newer analysis
found.

SBOMs and the latest scan report for each image live under
`deployments/<space>/<app>/scans/`, beside the `releases/` directory, and
remain available when older releases expire. Source Git history
stays under `source/`; scanner databases stay under `scanner/`. Deleting an
app or space removes its deployment artifacts too; `keepData` preserves an
app's history and inventories for a later redeploy.

On the first orchestrator startup after upgrading, a one-off migration copies
existing release records from telemetry onto `donkey-data`, preserving build
numbers, timestamps and Git links. Each copy is verified before its telemetry
record is deleted. Interrupted migrations resume on startup; a completion
marker skips later runs. This migration and telemetry's legacy build API can
be removed in the next release, once installations have passed through this
upgrade. Telemetry keeps analytics, metrics and other OTel data.

### Source history

Every deploy's files are kept as git — one bare repository per app, in the
`source/` folder of the control plane's data volume — and the console's
**Source** tab reads it: who deployed what, line by line, and what donkey's
build changed on top.

- **Your uploads are `main`.** A deploy whose files differ from the last
  one becomes a commit by the person who deployed (name and email from the
  sign-in), compared with the previous upload. The CLI adds the git
  revision it deployed from. Redeploying unchanged files adds nothing.
- **The platform's changes are `platform`.** When the build changed its
  copy — wrote `.donkey/Dockerfile`, repaired a file — that becomes a commit
  by *Donkey Platform* merging the upload it built, compared with that
  upload: its diff is exactly the platform's change, and your next upload is
  still compared with your own previous one.
- **Builds are tags.** `build-<n>` names the commit build *n* used. The
  console marks the commit the app runs as *live*, and failed builds in red.
- Any version can be browsed file by file — syntax highlighted, with the
  changes that version made to each file — downloaded as a `.zip`, and,
  where the installation has [machines](#machines), opened in yours as a
  folder to keep working in.

The repositories are written with go-git (no `git` binary in the image), one
pack per commit, and are plain git: a copy of the volume opens in any client
(`git log --graph --all` on `<space>/<app>.git`). Files over 8 MiB, and
anything beyond 128 MiB per upload, are left out and named in the commit
message. Removing an app deletes its source history and build records with
it, so a later app by the same name starts with its own; `donkey destroy
--keep-data` (the console's *Keep its data*) keeps them, like the disk, for
the same app's redeploy. Deleting the space deletes all of it.

The data volume holds source history, deployment records and scanner data.
Every installation has one — the `donkey-data` claim (5 Gi) on Kubernetes, a
folder of the `donkey-data` state volume on Docker:

```yaml
data:
  existingClaim: donkey-data-nfs  # a claim you manage — or storageClass/size for donkey's
```

donkey-orchestrator alone mounts it: deploys write the history there, and
donkey-server forwards the Source tab's reads and app and space removal to
it, so one process owns the repositories. As one replica replaced (never
surged) on upgrades, the orchestrator needs the volume on one node at a
time, so any storage class works; while it drains before a restart, the
Source tab answers "try again in a moment" like a deploy would. A claim's
class is fixed once created; to move later, create the new claim yourself
and name it in `data.existingClaim`. The API behind the tab is
`GET /v1/spaces/{space}/apps/{app}/commits`, plus `/commits/{id}` and its
`/diff?path=`, `/tree`, `/file?path=` and `/archive`.

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
npm (and Corepack), pip, Go, Maven and NuGet settings after every `FROM` in an in-memory
Dockerfile overlay, so they reach every `RUN` in every stage without changing
the app's file. The public defaults are npmjs, PyPI, `proxy.golang.org`, Maven
Central and the NuGet v3 service index; only the proxy process sees configured
upstream URLs or credentials. The package-variable names are platform-owned,
so `build.env` cannot accidentally bypass the proxy or expose an upstream
credential as a build arg.

#### Lockfiles in a build

A lockfile pins versions and checksums, and many also record where each
package was downloaded from: the public registry, an enterprise one, or
`donkey packages` on a developer's machine. A frozen install follows those
addresses rather than the registry the build configures, so a lockfile
written elsewhere sends a build to hosts it may not reach, or to your own
registry without the credential only donkey-packages holds.

donkey therefore hands the build its lockfiles with the host of every
registry address disregarded: the package's name and file stay, and the
mirror is asked for them. It happens in memory, like the Dockerfile overlay —
the app's files and its source history stay as uploaded — and versions and
checksums are not touched, so the build installs exactly what the lockfile
pins: a file the mirror has under that name either matches the lockfile's
checksum or fails the install.

| Lockfile | What is read as the mirror |
|---|---|
| `package-lock.json`, `npm-shrinkwrap.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lock` | every recorded download in the shape npm registries use, `…/<name>/-/<file>.tgz`, on any host |
| `uv.lock` | every index a package or a dependency entry names, and the files of those packages, on any host |

Builds also receive `UV_INDEX_URL`, so `uv sync --frozen`, `uv sync --locked`
and `uv pip install` all go through the mirror. For a `uv.lock`, files at
`files.pythonhosted.org` keep their path on the mirror; files named anywhere
else are found by their name in the upstream's index page for the project.

What stays as written:

- **Registries the app assigns to particular packages** — a registry for an
  npm scope in `.npmrc`, `.yarnrc` or `bunfig.toml` (`@acme:registry=…`), an
  index in `pyproject.toml` or `uv.toml` other than the default one. The
  build's registry setting does not replace those either, so those packages
  still come from where the app says, and the build has to reach that host
  itself.
- **Everything that is not a registry download**: a dependency from git, a
  plain URL or a path — including its name where `package.json` spells a
  dependency as a URL — and registries whose downloads have another shape
  (GitHub Packages, GitLab).

The consequence to know: a package that exists only on a registry the mirror
does not proxy, and that the app does not declare as above, is no longer
fetched from the address in the lockfile. Declare the registry in the app, or
add it to the mirror's upstream.

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

Debian and Alpine OS package mirrors are optional. Set archive roots under
`build.packages`; the Dockerfile assistant configures APT/APK using the base
image's Debian codename or existing Alpine branch and repository selections:

```yaml
build:
  packages:
    debian: https://debian.mirror.registry.example.com/debian
    alpine: https://alpine.mirror.registry.example.com/alpinelinux
    credentialHosts:
      - debian.mirror.registry.example.com
      - alpine.mirror.registry.example.com
    username: package-reader@example.com
    # Supply the token through DONKEY_PACKAGES_PASSWORD or Docker config.
  instructions: |
    For Debian packages from our mirror, allow unsigned repositories:
    use apt-get update -o Acquire::AllowInsecureRepositories=true
    and apt-get install -y --allow-unauthenticated.
```

The unsigned-repository instructions are optional; omit them for signed
archives. The update flag permits untrusted indexes, while the install flag
permits installation of unauthenticated packages. Neither changes TLS checks;
upstream TLS uses the package proxy's existing CA configuration.

#### The vulnerability scanner without internet

The scan needs two things from outside: the scanner image the build engine
inventories with, and the vulnerability database the orchestrator's scanner
fetches by itself. The image is pulled by the build engine like any `FROM`,
so `build.registryMirrors` covers it (it lives on Docker Hub as
`aquasec/trivy`; set `build.scan.image` to the mirrored reference when the
mirror cannot be expressed as a Docker Hub mirror). The database is
different: Trivy downloads it as an OCI artifact with its own registry
client — by default from `mirror.gcr.io/aquasec/trivy-db:2`, falling back to
`ghcr.io/aquasecurity/trivy-db:2`, and `ghcr.io/aquasecurity/trivy-java-db:1`
for JAR scanning — from inside donkey-orchestrator, so the build engine's
mirrors do not apply to that pull. Name the mirrors under `build.scan`:

```yaml
build:
  registryMirrors:
    docker.io: https://registry.example.com/dockerhub     # covers aquasec/trivy
  scan:
    dbRepository: registry.example.com/ghcr/aquasecurity/trivy-db
    javaDbRepository: registry.example.com/ghcr/aquasecurity/trivy-java-db
```

Both are plain OCI repositories, so any registry that can proxy or hold OCI
artifacts serves them:

- **A remote (proxying) repository** — Artifactory "Docker remote", Nexus
  "docker (proxy)", Harbor "proxy cache project" — pointed at `https://ghcr.io`.
  The database is then fetched through it on first use and refreshed on the
  scanner's schedule (upstream publishes every six hours; the scanner checks
  the artifact's `NextUpdate` before downloading again).
- **A copy you push yourself**, for a network with no path to ghcr.io at all.
  Copy the artifacts from a machine that has internet, with the same tags:

  ```sh
  oras cp ghcr.io/aquasecurity/trivy-db:2      registry.example.com/ghcr/aquasecurity/trivy-db:2
  oras cp ghcr.io/aquasecurity/trivy-java-db:1 registry.example.com/ghcr/aquasecurity/trivy-java-db:1
  # crane cp works as well; use the tags above — they are schema versions, not releases.
  ```

  Repeat on a schedule (daily is plenty); a database older than a few days
  is what "the watch" is blind to. `donkey doctor` shows whether the
  repository answers.

Credentials and trust follow the rules every other pull already uses: when
the mirror host is donkey's own registry host, the registry login is used;
any other host is looked up in `registry.dockerConfigDir`; the registry's
`caFile` is trusted by the scanner too, in addition to the public roots. The
orchestrator's own forward proxy (`build.proxy`) is what reaches the mirror
in a network that only has one. A database that cannot be fetched fails the
build with the scanner's own message rather than publishing an unscanned
image; `build.scan.enabled: false` is the explicit way to publish without
scanning. JAR files (`.jar`, `.war`, `.ear`, `.par`) are left out of the
inventory for now: naming them takes the Java database, which only the
orchestrator can fetch through `javaDbRepository` — the inventory step runs
in the build engine, which reaches neither the mirror nor the orchestrator's
copy — and without it the scanner would refuse every image carrying a JAR.
Java dependencies bundled into an image are therefore not checked yet;
`javaDbRepository` is accepted and checked by `donkey doctor` until the
inventory step can use it.

Every stage receives `DONKEY_DEBIAN_MIRROR` and `DONKEY_ALPINE_MIRROR` as build
arguments. Each is empty when its mirror is disabled; otherwise it names only
the internal package proxy. Existing hand-written Dockerfiles must configure
APT/APK from these variables, as the assistant does for generated recipes.
The Debian endpoint serves the base archive's `main` component; it does not
provide Ubuntu or a separate Debian security archive. OS package indexes and
artifacts pass through the proxy byte-for-byte to preserve signatures and
checksums, and so does everything Maven and Go fetch: their files are
verified against published checksums, and Gradle reads its module metadata in
the order it was written. JFrog credentials stay in the proxy.

NuGet builds receive the internal v3 service index as `DONKEY_NUGET_SOURCE`.
Modern NuGet requires a `NuGet.Config` entry with
`allowInsecureConnections="true"` for the cluster-local HTTP hop; the
Dockerfile assistant creates it using `%DONKEY_NUGET_SOURCE%` and passes
`--configfile`. Maven 4 receives
`MAVEN_REPO_CENTRAL`, and Maven Wrapper bootstrap receives `MVNW_REPOURL`.
Maven 3 requires a mirror in `settings.xml`; the assistant creates one from
`DONKEY_MAVEN_MIRROR`. Existing hand-written Maven 3 or modern NuGet
Dockerfiles must consume these variables in the same way. Beside the public
Maven Central, the Maven endpoint also carries the Gradle plugin portal
(under `-/resource/…`), where Gradle finds the plugins Central does not list.
An enterprise upstream is expected to aggregate the portal, so there the same
path is answered by that upstream.

The HTTP proxy omits NuGet's optional `RepositorySignatures` index entries,
which require HTTPS. Package files and their embedded signatures pass through
unchanged, and client signature verification remains enabled. The mirror does
not advertise the upstream's signing-certificate list or its requirement that
every package be repository-signed.

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

Images pulled directly by the runtime use explicit overrides. Set
`buildkitImage` for the build engine and `busyboxImage` for the file-browser
toolbox and Docker volume initialization (default `busybox:1.37`). Docker's
optional bundled registry uses `registryImage` (default `registry:3`), only
when `registry.url` is omitted. Managed services use mirrored repositories
with the service version appended:

```yaml
buildkitImage: registry.example.com/dockerhub/moby/buildkit:v0.31.2
busyboxImage: registry.example.com/dockerhub/library/busybox:1.37
registryImage: registry.example.com/dockerhub/library/registry:3
addonImages:
  redis: registry.example.com/mirror/library/redis
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
workload namespace where a managed addon or file helper can run. Donkey
references them without reading or copying their contents; when the list is set, it replaces
the pull secret donkey would generate from `registry.url` credentials.

A separately hosted BusyBox mirror can also use its own credentials from
`registry.dockerConfigSecret` (Kubernetes) or `registry.dockerConfigDir`
(Docker). The Docker installer uses matching entries in `registry.dockerConfigDir`
for `registryImage`, `buildkitImage` and `image` too; Compose pulls use Docker's
login configuration. The BusyBox image reference travels to the server as
`DONKEY_BUSYBOX_IMAGE` and the Redis repository as `DONKEY_REDIS_IMAGE`.
Build startup verification uses the separate static image
`docker.io/library/busybox:1.37-uclibc`, already covered by
`build.registryMirrors`; `busyboxImage` only configures runtime helpers.

The same-host rule also authenticates BuildKit mirrors. A separately hosted
authenticated mirror can provide build credentials through an existing
Docker-config Secret (`registry.dockerConfigSecret`) on Kubernetes or an
absolute host directory (`registry.dockerConfigDir`) on Docker. Donkey mounts
the source into `donkey-orchestrator`, which runs the builds, selects
credentials by requested host and rereads the files for every build, so
rotation does not require a restart.
The push registry itself still uses `registry.username` plus its password; the
Docker config is for separately hosted mirrors. The registry URL remains an
image prefix: builds are pushed as
`<registry.url>/<app>:<tag>`, so a setting such as
`registry.example.com/platform/donkey` keeps every pushed app image below that
enterprise folder.

Mirror hosts inherit `registry.caSecret`, including the separate Bearer-token
request made by donkey-orchestrator on BuildKit's behalf, so an in-house
Artifactory behind a private CA needs nothing further. The referenced Secret must be in the
control-plane namespace and contain the trust bundle under exactly `ca.crt`;
the installer checks it before applying changes. There is no builder image for
`donkey doctor` to resolve — a generated Dockerfile names its own bases, and
whether those resolve is a question only the finished file can ask, which the
build engine asks through exactly these mirrors.

#### Packages on a developer's machine

The package mirror also serves the machines developers work on, for networks
where npm, PyPI or the Go proxy are blocked on a laptop but reachable from the
installation. Nothing is configured: every signed-in user can use it.

```sh
donkey packages run npm install        # or pnpm, yarn, bun
donkey packages run uv add requests    # or pip install
donkey packages run go mod download
donkey packages run mvn package        # or ./mvnw, gradle, ./gradlew
donkey packages run dotnet restore     # or dotnet build, dotnet add package
donkey packages run $SHELL             # a shell in which every command is set up
```

`donkey packages run` opens the mirror on `127.0.0.1:4874` for as long as the
command runs, and starts the command with its package manager pointed there.
The command's own flags, output and exit code pass through untouched. Each
request travels over the authenticated API to donkey-server and on to
donkey-packages, so the machine needs no registry credential and no
certificate setup per tool — `donkey login` is all of it — and upstream
credentials stay inside the installation. A forward proxy in the environment
is told to leave the local address alone (`NO_PROXY`).

| Tool | How it is pointed at the mirror | Reaches |
|---|---|---|
| npm, pnpm, yarn 1, bun | `NPM_CONFIG_REGISTRY` | whatever the command starts |
| Corepack, fetching pnpm or yarn itself | `COREPACK_NPM_REGISTRY` | whatever the command starts |
| yarn 2 and later | `YARN_NPM_REGISTRY_SERVER` | whatever the command starts |
| pip, uv | `PIP_INDEX_URL`, `UV_INDEX_URL` | whatever the command starts |
| Go | `GOPROXY` | whatever the command starts |
| .NET | `RestoreConfigFile`, naming a `NuGet.Config` donkey writes | every restore, also the one inside `dotnet build`, `run` and `test`; your own NuGet configuration is not read meanwhile |
| Maven 3.9 and later, Maven 4 | `MAVEN_ARGS`, naming a `settings.xml` donkey writes; `MAVEN_REPO_CENTRAL`, `MVNW_REPOURL` | whatever the command starts |
| Maven before 3.9 | `-gs settings.xml`, added to the command line | `mvn`, `mvnw` or `mvnd` named in the command itself |
| Gradle | `--init-script`, added to the command line | `gradle` or `gradlew` named in the command itself |

Maven, Gradle and NuGet take a mirror from a file only, so donkey writes the
three into the user's cache folder (`donkey/packages/<port>/`), the same
contents under the same paths for every run. Maven gets them as global
settings: your own `settings.xml` still applies, and a mirror or proxy it
names takes precedence. The Gradle script points every repository a build
declares at the mirror — the plugin portal at the portal behind it — and
gives builds that name no plugin repository both; `gradlew` still downloads
Gradle itself from `services.gradle.org`, which the mirror does not carry.

`donkey packages proxy` keeps the same address open until it is stopped and
prints each registry's address — for an IDE, a language server, or a tool you
would rather configure yourself. A `run` started meanwhile uses it, once the
proxy has shown it is yours: it answers a challenge with a secret kept where
only your user can read. Any other program holding the port — on a machine
people share, anyone can take it first — is left alone, and the run opens a
port of its own. The address is bound to this machine, read-only, and
answers only when addressed by its local name, so a page in a browser cannot
use it; other programs on the machine can, while it is open.

**Lockfiles.** The mirror hands out download links at the local address, and
what a package manager does with them differs:

| Package manager | What its lockfile records | A lockfile written through the mirror |
|---|---|---|
| npm | versions and checksums — `run` sets `omit-lockfile-registry-resolved`, so no address | installs anywhere; the first install drops the `resolved` lines of an existing lockfile once |
| pnpm, yarn 2 and later | versions and checksums, never an address | installs anywhere |
| pip, Go | no address (`go.sum` holds checksums) | installs anywhere |
| uv, bun, yarn 1 | the address of every package: `http://127.0.0.1:4874/…` | installs where `donkey packages` runs on the same port, and in a donkey build, which reads that address as its own mirror (see [Lockfiles in a build](#lockfiles-in-a-build)) |

Keep the default port, so the lockfiles of a team agree.

**Go's checksum database.** A machine that cannot reach the Go proxy cannot
reach `sum.golang.org` either, so `run` sets `GOSUMDB=off` unless the
environment already names a checksum database. `go.sum` still pins everything
that was downloaded.

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
authenticate through the same OIDC login the apps use. The first signed-in
user becomes a platform admin and can add others in **Account → Platform
settings**. An optional `platform.admins` in the install file seeds that list
instead; once saved, the runtime settings are authoritative and reinstalling
does not reset them. Re-running with changed installation settings updates it
idempotently (existing secrets and sessions survive; config changes roll the
affected pods).

Space permissions are always enforced. Platform admins create spaces and can
manage every space. Space admins manage their space, its membership, domains and offerings;
developers manage its apps and services, including data and credentials, and can
view membership, domains and service offerings in **Space settings** without editing them.
Space traffic is available to all three roles via **⋯ → Traffic**. Viewers
can open apps and see their health and aggregate metrics in the console. They
cannot see logs, build history, files, variables, service data or management
settings, and cannot make changes. Assign users by email (or `sub:<subject>`
when email is unavailable) or groups. Matching any entry grants that role;
empty membership lists grant no access.

Every space stores `admins`, `developers`, `viewers`, `allowPublic`, `domains`,
and optional `offerings`. **Space settings → General → Allow public apps**
controls `allowPublic`, which defaults to `false`. Turning it off switches
existing public apps and shared-address routes to login protection. New
deployments and network changes requesting `public: true` are rejected. Startup
also enforces this policy. Re-enabling the option does not automatically reopen
apps that were switched to login protection.

A space membership example:

```yaml
admins:
  users: [owner@example.com]
developers:
  groups: [engineering]
viewers:
  users: [reader@example.com]
  groups: [staff]
allowPublic: false
```

A new space starts
with `<space>.<configured-domain>` in `domains`. Apps can use that suffix or
any subdomain under it. Add more suffixes or clear the list to allow any domain.
App creation shows a hostname field with a fixed suffix or domain selector;
with no suffixes configured, it requires a full hostname. Deploys and network
changes enforce the same rule, including shared-address routes.

`commonOfferings` in the installation file selects offerings available in every
space. An empty or omitted list allows all registered offerings. A nonempty
list such as `[postgres, redis, s3, openai, exa]` limits the common catalog;
space admins can add others in **Space settings → Service offerings**:

```yaml
offerings:
  mariadb:
    defaults:
      storage: 10Gi
    config:
      storageClass: fast
```

An empty entry enables an offering without extra settings. `defaults` applies
to new services and apps may override it. `config` is admin-managed provider
configuration, described by the offering's separate schema and sent separately
over gRPC; apps cannot override it. Missing required configuration blocks new
provisioning. Existing services keep their saved parameters and configuration
for retries, resumption and removal, and existing app connections keep working
when an offering is removed. New services and new app connections use the
current space catalog. See [the provider contract](pkg/addons/v1/README.md#availability-and-space-configuration).

At startup, existing per-app users and groups move into the space's Viewer list.
Duplicates are merged across apps, and entries already assigned any space role
are skipped. Emails match without case; group names and `sub:<subject>` IDs
match exactly. This grants access to all protected apps in that space. Old
per-app lists are removed only after membership is saved. Apps without
an explicit list are now limited to space members and platform admins. Existing
spaces also default to `allowPublic: false`, so public routes become protected.
Saved space admins, developers, viewers and domains survive subsequent upgrades.
Updates use last save wins. Remove obsolete `access:` sections from donkey.yaml;
new deployments reject them as unknown fields. Manage users and groups in space
settings instead.

Space, app and service names follow one rule: lowercase letters, digits and
hyphens, starting and ending with a letter or digit, up to 40 characters (a
DNS label). `donkey`, `platform`, and `system` are reserved as space names.
Names derived from repository folders turn punctuation into hyphens;
explicitly supplied names are validated as entered.

On Kubernetes, the `donkey` ConfigMap in the control-plane namespace holds
`platform.yaml`, the console's `route.yaml`, and `buildkitd.toml`. Every space
has a `<space>-space` ConfigMap containing `config.yaml` in its workload
namespace. The `donkey` Secret stores installation credentials and session keys.
App ConfigMaps and Secrets use purpose suffixes (`api-route`, `api-env`,
`api-registry`), prefixed with the space in a shared workload namespace
(`team-api-env`). App TLS Secrets live centrally as `team-api-tls`.
See [the complete resource table and migration details](README_KUBERNETES.md#configmap-and-secret-names).

Docker persists the same settings fields as JSON under
`/var/lib/donkey/platform/config.json` and
`/var/lib/donkey/spaces/<space>/config.json` in its state volume.
See [the Docker storage layout](README_DOCKER.md#what-maps-to-what).

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

**The user's IdP token.** Services that check identity themselves (any API
behind Entra) need a real token from the identity provider; a donkey-sealed
token is useless to them. Donkey keeps the user's token for its own API, and
whoever calls a downstream API exchanges it on-behalf-of with donkey's
client ID and secret:

```yaml
auth:
  issuer: https://login.microsoftonline.com/<tenant>/v2.0
  clientId: <donkey-client-id>
  scope: api://<donkey-client-id>/<scope>
```

Logins then ask for it on top of `openid profile email`, together with
`offline_access`, and the IdP's
refresh token is sealed alongside the identity: in the session cookie (split
into `donkey_session`, `donkey_session_1`, … once it outgrows one cookie), in
`donkey login`'s token and in the OAuth tokens of MCP clients. From it, donkey
mints the user's token for its own API as needed, cached until shortly
before it expires:

- apps behind a login receive the visitor's token as
  `X-Forwarded-Access-Token`, and as `X-Auth-Request-Access-Token` (public
  routes never do, even for a signed-in visitor; inbound copies are
  dropped);
- the Trino console and `donkey addon tunnel` send the user's token as
  `Authorization: Bearer <token>` to its add-on service, which handles
  delegation to Trino.

Only the gateway talks to the token endpoint; donkey-server receives the
access token inside the sealed `X-Access-Token` and never mints one. Nothing
is stored server-side: when the IdP rotates a refresh token, the gateway
re-seals the browser's session cookie with the new one (MCP clients pick it
up on their next token refresh). In memory the gateway keeps each sign-in's
access token until shortly before it expires and its newest refresh token,
so concurrent requests share one refresh and a request still carrying an
older cookie redeems the newest. An entry lasts as long as some credential
can present it — a browser session's 12 hours, a CLI token's 30 days. After
a gateway restart, credentials that were not re-sealed (a `donkey login`
token, an idle MCP client) fall back to the refresh token they carry: fine
for IdPs that keep earlier refresh tokens valid, while IdPs that accept each
refresh token only once ask those users to sign in again. Token endpoint
authentication supports both HTTP Basic and credentials in the form body.

If apps receive user/email headers but no `X-Forwarded-Access-Token`, check
that the running gateway has `DONKEY_OIDC_SCOPE` set and sign out and in again
after changing it. Gateway logs report whether forwarding is enabled, a
sign-in lacks a refresh token, or the IdP refused the refresh grant.

The refresh token itself never leaves donkey. In Entra, donkey's app
registration exposes an API (Application ID URI `api://<donkey-client-id>`,
plus a scope) and holds a delegated permission, with admin consent, on every
downstream API exchanged for. Users who signed in before the scope was set
need to sign in once more (`donkey login` for the CLI).

**OIDC login for web apps.** A web app can use the gateway as its OpenID
Connect provider. The gateway reuses the existing upstream IdP and SSO
session, asks the visitor to approve sharing their identity, and issues an
ID token for that app. Register exact callback URLs in its `donkey.yaml`:

```yaml
name: shop
space: acme
port: 8000
oidc:
  redirectURIs:
    - https://shop.acme.apps.example.com/auth/callback
    - http://localhost:3000/auth/callback
```

Deploying this configuration registers client `donkey_web_acme_shop`.
Client IDs follow `donkey_web_<space>_<app>` and stay stable across deploys.
Registrations apply to web apps with their own gateway address. Callback
URLs use HTTPS, with HTTP allowed on loopback for local development; they
must match exactly and use a path outside the gateway's reserved `/oauth2/`
paths. Removing `oidc`, deleting the app, or changing its registration
invalidates its outstanding codes and grants at the next successful route
refresh. Restoring the old settings does not restore those grants or pending
consent forms. Console network edits preserve
the registration; remove it in `donkey.yaml` before changing the app to API
mode or internal exposure.

Configure your OIDC library with:

| Setting | Value for this example |
| --- | --- |
| Issuer / authority | `https://apps.example.com` (the installation's base domain) |
| Client ID | `donkey_web_acme_shop` |
| Redirect URI | One of the registered callback URLs |
| Flow | Authorization Code with S256 PKCE |
| Scopes | `openid profile email`; add `offline_access` for refresh tokens |

Discovery is at `/.well-known/openid-configuration`. It advertises
`/oauth2/authorize`, `/oauth2/token`, `/oauth2/userinfo`, `/oauth2/jwks`, and
`/oauth2/revoke`. Login needs no API `resource` parameter. Both public browser
clients and confidential server-side clients must use PKCE. Public clients
use token-endpoint authentication method `none` and send no client secret.
The token response includes an RS256-signed ID token, a one-hour access token
(bounded by the grant's expiry), and the granted scopes. `openid` provides a
stable subject; `profile` adds available name and preferred-username claims;
`email` adds the email address. Claims are also available from UserInfo with
the access token. No email-verification claim is invented when the upstream
login has not supplied one.

For a server-side web app, generate a random client secret of at least 32
characters. Put its SHA-256 hex digest in `oidc.clientSecretSHA256`, and put
the secret itself in the application's secret environment, for example with
`donkey env set OIDC_CLIENT_SECRET=...`. Such clients authenticate using
`client_secret_basic` or `client_secret_post`. This command generates both
values; only the digest belongs in the repository:

```sh
python3 -c 'import secrets, hashlib; s = secrets.token_urlsafe(32); print("Client secret:", s); print("SHA-256:", hashlib.sha256(s.encode()).hexdigest())'
```

The gateway checks current space membership at authorization, code exchange,
refresh, and UserInfo. Public exposure does not grant nonmembers OIDC access.
The browser can still load a public app's sign-in screen when its space allows
public apps. Upstream group claims are the snapshot from the user's sign-in;
changing upstream group membership requires a new sign-in to refresh that
snapshot.

These web-app access tokens authorize UserInfo. API/MCP access continues to
use the existing resource-bound OAuth flow, and protected backends continue
to receive `X-Forwarded-Access-Token` when `auth.scope` is configured. OIDC
login grants contain identity claims without upstream access or refresh
tokens. ID tokens name Donkey as issuer and the registered app as audience.

`offline_access` requires interactive approval and issues rotating refresh
tokens with a fixed 30-day grant lifetime. Reusing an older refresh token
revokes the whole grant, including its access tokens. Posting an access or
refresh token to `/oauth2/revoke`, with the client's usual authentication,
also revokes that grant. Already issued ID tokens remain verifiable until
they expire; applications manage their own sessions. `/oauth2/logout` ends
the gateway browser session; it is not an OIDC RP-initiated logout endpoint.

Client registration versions, signing keys, one-use codes, and grants use the
same transactional file store on both runtimes:
`/var/lib/donkey/oidc/state.json`. Kubernetes mounts
the dedicated `donkey-gateway-data` PVC (1 GiB; `data.storageClass` when set),
and Docker uses its existing state volume. An OS lock, file sync, and atomic
replacement protect transactions; expired records are removed on the next
operation. Back up this state with the installation. Signing keys rotate
after 30 days of use, retaining previous public keys until their ID tokens
have expired.

This configuration runs one gateway. Kubernetes uses `Recreate` so an
upgrade releases the RWO volume before starting its replacement; gateway
upgrades therefore briefly interrupt edge traffic. Multiple gateways on
different nodes require a shared transactional store implementation, not
independent local files. The gateway depends on a storage interface and
runtime interfaces for routes and TLS; Kubernetes and Docker adapters are
selected at startup outside the gateway package.

Authentication requests do not access the Kubernetes API. Routes and
membership refresh every 10 seconds, as do certificates in cert-manager
mode. Requests for unknown routes or certificates can trigger a refresh,
coalesced and limited to once per two seconds across callers. ACME cache
reads use memory after one initial Secret read, including cached misses;
failed reads back off for five seconds. Secret writes occur for certificate
issuance/renewal, and CA loading happens at startup.

The supported OIDC profile is the authorization-code flow with GET or form
POST authorization requests, query responses, and explicit approval. Keep
authorization state and nonce values short: requests that cannot fit the
browser's login-state cookie are rejected before redirecting to the IdP.
`prompt=login`, `prompt=consent`, and
`max_age` are supported; `max_age` requires the upstream provider to return
and honor `auth_time`. `prompt=none` returns `login_required` or
`consent_required` without showing UI. Dynamic registration remains for
API/MCP clients; web apps use the explicit registrations above. Implicit and
hybrid flows, request objects, and RP-initiated logout are not advertised.

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
| `donkey packages run COMMAND` / `packages proxy` | install with npm, pip, uv, go, Maven, Gradle, dotnet and the like through the installation's package mirror, where the public registries are blocked on your machine (see [Packages on a developer's machine](#packages-on-a-developers-machine)) |
| `donkey destroy` | remove everything donkey created for the app, its build and source history included (`--keep-data` spares disks, secrets and history — a kept database keeps its credentials; deleting data always asks — `--force` for scripts) |

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
key in `donkey` seals the browser cookies (12 h) and the CLI
tokens (30 days), all derived from an OIDC sign-in. Individual logins expire
on their own and revive with a fresh sign-in; kicking one specific person out
happens by removing every matching entry from platform admins and space roles.
Management permissions are read on every API request; the gateway reloads space
membership and public-access policy every 10 seconds and checks each app request. Changes to IdP
claims take effect when the user signs in again. The big
lever is rotating the session key itself: delete the `session-key` entry from
the Secret and re-run the install — a new key is generated, every cookie and
every CLI token everywhere is invalid the moment the pods roll, and everyone
signs in again through the browser. **There is no overlap window.**

Running app workloads are not rolled: their pods, disks and routes stay as
they are while donkey-server and donkey-orchestrator restart. A re-install
reconciles each app's Certificate and optional DNS record from its existing
route, so changing the
certificate source or DNS publication applies immediately; unchanged objects
are idempotent server-side applies. A re-install also keeps every secret it
isn't given again
(registry password, OIDC client secret, AI key), so the same settings file is
enough — rotating one thing must never blank the rest.

**Upgrading the control plane.** An upgrade replaces the API and console in
seconds and leaves running builds alone. donkey-server holds nothing that
outlives a request, so it rolls with a surge pod and a short shutdown; a
deploy stream it was relaying is cut, the deploy itself carries on, and the
CLI and console switch to following the app's status. donkey-orchestrator is
the one that waits: it finishes the deploys it already accepted (up to 10
minutes) before its replacement starts. Meanwhile the console works and plans
and deploys answer *try again in a moment*. Plans reviewed before the restart
expire with it and cost a re-upload, never a build. `donkey server install`
returns once the orchestrator is back, and says so while it waits.

**First upgrade from the single-process server on `main`.** Use the new CLI
and server image together, keeping the same install settings, installation
name, namespaces and volumes. Existing app data, credentials, platform admins,
space admins/developers and domains are retained. The orchestrator migrates
saved access settings before accepting mutations; restarting it safely retries
an interrupted migration. Source history starts with the next deployment;
previous uploads cannot be recovered from running images.

Before upgrading:

- Back up the existing settings and persistent volumes. For a rollback, retain
  the old image and the pre-upgrade settings: the migration removes per-app
  access lists after copying them to space Viewers, so simply restoring the old
  image does not restore its authorization behavior.
- On Kubernetes, stop submitting deployments and let active builds finish
  before installing. The old server owns those builds, and replacing BuildKit
  during the first upgrade can interrupt them. Docker's installer and generated
  `up.sh` drain the old server before replacing its build dependencies.
- Stop the old gateway and wait for it to exit before installing this version.
  It cannot enforce space membership once migration removes the per-app lists.
  This is a short maintenance window for app traffic; workloads keep running.
  On Docker, run `docker stop donkey-gateway`. On Kubernetes, scale
  `deployment/donkey-gateway` to zero in the system namespace and wait until its
  pods are gone. The new install restores the gateway at its normal replica
  count. Keep it stopped if an upgrade fails, then retry the install.
- Kubernetes now needs a data claim for the orchestrator: provide a default
  StorageClass, or set `data.storageClass` or `data.existingClaim`. The data
  volume is no longer optional — remove `data.disabled` from donkey-install.yaml
  if you set it.
- Review space membership. Per-app users/groups become Viewers of **all** apps
  in their space. Previously unrestricted protected apps now require space
  membership. Public apps become protected until an admin enables
  `allowPublic` and makes those apps public again.
- Remove obsolete `access:` sections from app configuration before redeploying.
  IdP `roles:` memberships are no longer supported. The upgrade removes these
  grants and logs a warning identifying each affected record; it preserves
  users/groups and never converts role names into groups. Assign users/groups
  explicitly to restore access. Keep at least one platform admin listed as a
  user or group before upgrading; removing the last admin grant does not reopen
  first-user registration.

The metadata cutover below concerns older releases; `main` already uses the
current ownership schema and does not require deleting and recreating apps.

**Metadata v2 is a clean cutover.** It deliberately does not claim resources
from versions that used the old ownership labels or selectors. Kubernetes
Deployment selectors are immutable, so adopting only their top-level labels
would create a half-migrated installation that cannot reconcile. At this
early stage, remove the old app/control-plane resources and recreate them;
new Deployments, Pods, Services, routes, services and Docker resources are
then born with one consistent schema. Back up data first if an old PVC or
Docker volume is not disposable — deleting a space intentionally deletes it.

**What to back up.** Donkey state lives in Kubernetes objects, `donkey-data`
and the telemetry PVC, and nearly
all of it is reproducible: images rebuild from the repositories, manifests
re-render from `donkey.yaml`, namespaces and routes are re-created by the next
deploy. What is *not* reproducible has to live outside the cluster:

| Keep | Why |
|---|---|
| the install settings file (`server-install.yaml`) | every install, re-install, or re-render needs it; it is the written description of the installation |
| `DONKEY_REGISTRY_PASSWORD` | donkey stores it, but never invents it; without it the server can't push and apps can't pull |
| `DONKEY_OIDC_CLIENT_SECRET` (with its client id) | belongs to a registration in your IdP — a new one means updating the IdP as well |
| `OPENAI_API_KEY` | the assistant is required; no key, no deploys |
| the `session-key` entry of `donkey` | a fresh install **generates a new one** and thereby invalidates every session and CLI token. Restoring it makes the rebuilt installation the same installation — everyone stays signed in |
| the `telemetry-key` entry of `donkey` | existing app exporters carry credentials derived from it. Restoring it keeps those exporters valid and lets a restored telemetry database continue accepting them independently of login-session rotation |
| the registry CA secret (`registry.caSecret`) | donkey only *references* that Secret in the configured control-plane namespace — it never creates it. Whatever created it has to run again |
| DNS for `*.<domain>` and `<domain>` | they point at the gateway address, which a rebuilt cluster changes |

Two more things exist only in the cluster: env values set with `donkey env
set` (the ones in `donkey.yaml` come back with the app, the rest don't), and
the generated credential of a provisioned service — which matters only as
long as its data does.

The `donkey-telemetry-data` PVC is cluster-only too; its database file is
`/var/lib/donkey/telemetry.duckdb`. Losing it does not
affect apps, but it erases the current 30-day traffic and analytics history;
include it in volume snapshots when that history
matters.

The data volume (`donkey-data`, or the claim named in
`data.existingClaim`) holds Git history, deployment records, SBOMs and current
scan reports. Losing it does not
affect apps either, but the history it held is gone; snapshot it with the
same care, or point it at storage that is already backed up.

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
`donkey-server` and `donkey-orchestrator` do the same at startup and refuse
to come up: under Kubernetes the pod then stays unready and out of rotation
with the reason in its logs, which beats a control plane that looks healthy
and fails on its first deploy.

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
  server/             REST handlers, deploy pipeline (served by the orchestrator role), env-based config
  detect/             build strategy decision (donkey.yaml > Dockerfile > the installation's builder > AI)
  build/              BuildKit gRPC builder (dockerfile.v0), startup verification, registry auth, tags
  k8s/                client-go wrapper: server-side apply, rollout wait, logs, env secrets
  k8s/manifests/      pure config→object rendering (app + control plane) — golden-tested
  addons/             service catalog: postgres, redis, s3, openai, exa (provisioned & external bindings)
  ai/                 Assistant interface (analysis + failure diagnosis), OpenAI impl, secret-safe summarizer, log redaction
  config/             donkey.yaml, client config, install settings file
  doctor/             client-side check engine behind `donkey doctor`
  machine/            what runs inside a personal machine: the front door and the agent's supervisor
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
task demo:broken     # deploy the four repair cases; each must come up
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
task verify:builder  # live AI builds: 19 samples and four repair cases, including HTTP, assets and persistence
task verify:monorepos # pinned Google microservices-demo, deployed through project review
task verify:web      # real browser upload/review/deploy, then database, storage and AI requests
task verify:admin    # PostgreSQL and Redis browsers, including isolation between spaces
```

The builder test copies the samples into temporary folders, removes incidental
root configs, and uploads each through plan review and deployment. It checks
application responses and referenced assets, and writes notes to SQLite and
Postgres before restarting their apps to verify persistence. The addon
declaration and the nested monorepo config remain part of their fixtures.
Plans and deployment streams are saved in `.build/e2e`. Use `RUNTIME=docker`
to target Docker, or `DONKEY_E2E_KEEP=true` to keep the dedicated test spaces
for inspection. Rerunning replaces those spaces.

The builder suite also supplies two broken Dockerfiles and requires automatic
recovery from a real BuildKit failure and a real runtime rollout failure.
For rendered pages and Streamlit WebSockets, install Playwright with
`npm install --prefix .build/browser --no-save playwright@1.62.1` and
`.build/browser/node_modules/.bin/playwright install chromium`, then run
`DONKEY_E2E_BROWSER=true task verify:builder`. The kind wrapper's forwarded
gateway is used by default; Docker runs can set
`DONKEY_E2E_BROWSER_GATEWAY=127.0.0.1:443`.

The monorepo test uploads Google's Online Boutique source and uses
the live assistant to discover its apps and connections and write every app's
Dockerfile. The preparation script removes upstream Dockerfiles and Docker
ignore files from the test copy; the test requires the AI build strategy for
every app. It exercises ten
application services, gRPC addresses, and Redis-backed shopping carts through
checkout. Its headless load-test client is excluded because it does not serve
an app port. The test does not install Istio or an observability stack.
Its optional Gemini/AlloyDB extension is excluded
because the repository's default deployment does not enable it.

Reviewed plans and deployment streams are saved in `.build/e2e`. Set
`DONKEY_E2E_KEEP=true` to leave the dedicated `e2e-boutique` space running for
inspection; rerunning replaces that test space.

The browser test uses the local Docker installation and demo sign-in. It uploads
`examples/demo`, checks separate exposure choices, deploys both apps, and tests
PostgreSQL writes, S3 uploads/downloads, and OpenAI triage through the frontend.
Playwright is installed under `.build/browser`; screenshots and reviewed plans
go to `.build/e2e`. `DONKEY_E2E_KEEP=true` keeps its `e2e-services` space running.

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
Go, Maven, NuGet, Debian and Alpine. The package build therefore also verifies Docker-config
credential lookup, private-CA trust, readiness authentication, cross-host
artifact routing and the real clients for all seven ecosystems. The Debian
fixture omits archive signatures to exercise the operator's unsigned-package
instructions; the Alpine fixture preserves package signatures.

`task verify:os-packages` runs isolated OS package E2E tests without a running
Donkey installation or JFrog credentials. It starts a temporary BuildKit
container and the real package proxy, then installs `libjansson4`/`jansson`
through an authenticated TLS mirror forwarding to the public archives. It
checks signed Debian and Alpine installs, rejection of unsigned Debian
archives without both APT flags, success with both flags, and rejection of
incorrect mirror credentials. Docker and internet access are required; the
test removes its container afterward.

To also verify model-generated Debian/Alpine Dockerfiles and startup of an app
that loads the installed library, run `DONKEY_E2E_BUILDER=true task
verify:os-packages` with `OPENAI_API_KEY` and `DONKEY_MODEL` set (and
`OPENAI_BASE_URL` for a custom endpoint). This live-model subtest is skipped
unless explicitly enabled.

`OPENAI_BASE_URL=http://localhost:4242/v1 task verify:repair` exercises the
security repair loop with a live model and real BuildKit/Trivy. A vulnerable
saved recipe must first fail scanning; the assistant then receives the
findings, checks replacement base images and repairs the app. The resulting
image must pass scanning and startup verification. The test uses an empty
space policy, disables assistant waivers, and makes no registry push or
runtime deployment. It needs Docker and internet access, uses `DONKEY_MODEL`
or the default model, and removes its containers afterward. An optional
`DONKEY_E2E_DNS` IP overrides DNS only for the test process and containers.

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
- Apps behind a login get the visitor's identity as `X-Forwarded-*` (and
  `X-Auth-Request-*`) headers — sound from outside (the gateway strips
  inbound copies), but an in-cluster caller who reaches an app's Service
  directly can forge them, so they are information, not authorization.
  donkey's own API accepts only its sealed token.
- With `auth.scope` set, every login-protected app a visitor opens receives
  that visitor's IdP access token for donkey's API (valid about an hour in
  Entra). Exchanging it for another API takes donkey's client secret, so
  only what holds that secret can turn it into more; donkey's own API
  accepts only its sealed tokens — but anything else configured to accept
  donkey's audience accepts these too.
  The refresh token stays sealed inside donkey. Rotating the session key
  retires donkey's copies, but the IdP's refresh tokens stay valid until
  revoked there (in Entra: revoke the user's sessions).
- donkey-server and donkey-orchestrator currently bind cluster-admin (with a
  namespace per space); scoping them down is on the list. The orchestrator is
  the one that runs builds, and the only pod the privileged build engine
  admits; it accepts requests from donkey-server alone.
- A [machine](#machines) holds its owner's donkey sign-in, and a coding
  agent runs commands in it: what the agent can do with `donkey` is what its
  owner can do, for as long as that sign-in lasts (30 days from the owner's
  console sign-in at most). The agent it embeds trusts everything that
  reaches it, so three checks stand in front: an address no app can take, a
  route that admits one subject (a platform administrator is not let in), and
  a front door inside the container that refuses requests without the
  gateway's per-machine credential. That credential is derived from the
  session key, so rotating the key locks every running machine until it is
  restarted. Cluster administrators can read a machine's Secret and disk,
  as they can any other.
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
- The source history keeps every upload as it arrived. Key material never
  gets there (uploads exclude it), but a credential written into an
  ordinary source file is committed with it and stays in the history after
  the file changes — rotate it rather than delete it. The history is
  readable by the space's members, like the builds and files they deploy,
  and is removed with the app (unless its data is kept) and with the space.
- The assistant is required, so its endpoint is part of donkey's trust
  boundary: source summaries of every deployed app go there, and redacted
  failure logs when something breaks. Point it at a model you control (the
  install takes any OpenAI-compatible base URL).
- The `openai` service never hands out the platform's real AI key: bindings
  carry a generated per-space stand-in, `donkey-ai` swaps it for the real
  one on the way upstream, and the swap only happens for tokens donkey
  itself issued — so removing the service revokes the space's access, and
  the upstream credential exists nowhere outside `donkey-system`.

## License

[MIT](LICENSE) © 2026 Adrian Liechti.
