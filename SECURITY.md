# Security

How donkey's security works, what protects it, and what does not yet. This
describes the current state of the tree — it is not a changelog. Scope is the
core application (`internal/`, `pkg/`, `cmd/`, `web/src/`); samples and helper
scripts (`examples/`, `testdata/`, `hack/`) are out of scope.

donkey is a proof of concept. The gaps below are real and named so an
operator can decide what an installation may hold; several are deliberate
trade-offs for a small platform, marked as such.

## The trust model

**One credential underlies everything: an OIDC login.** The gateway runs the
browser sign-in, checks the admin rules, and mints a short-lived sealed token
from the session (`X-Access-Token`, ≤10 min). The CLI carries the same kind of
token in `Authorization: Bearer` (30 days). `donkey-server` verifies the seal
itself under `SessionKey` and applies the admin rules — so a request that never
touched the gateway is still authorized on its own, which matters because
tenant workloads can reach the server's AI proxy by design. There is no static
API token.

Identity headers (`X-Auth-Request-*`) are forwarded to *apps* as information
only; no donkey component authorizes on them. The server's `auth`
(`internal/server/server.go`) accepts only a verified sealed token.

**Two things follow from this model, and both are load-bearing:**

- **`auth.admins` empty means every account the IdP admits is an operator**
  (`AccessConfig.Empty()`). An installation that does not set it grants full
  platform rights to everyone the IdP will issue a token for. Set it.
- **There is no per-space authorization.** Any operator reaches every space.
  This is by design at PoC stage; several risks below become privilege-boundary
  violations the day it changes, and are marked.

## Boundaries that hold

These were verified against the current tree and are where donkey is sound.

**Authentication and the API surface.** The sealed-token check is the only way
into `/v1/*`; a bare identity header yields 401. The sealed token is minted for
exactly one backend — routes carry `donkey.local/token: forward`, set only on
the console/API route, and the gateway strips inbound `X-Access-Token` on every
route. On Kubernetes, `serverNetworkPolicy` additionally admits only the
gateway to `donkey-server`'s port. The openai addon's endpoint runs as its own
workload, **donkey-ai** (`ai` entrypoint) — holding the upstream AI credential
and read-only access to binding Secrets, and nothing else (no session key, no
registry login, no write verb, no Docker socket) — so the one process tenants
can reach is not the one that can write to the cluster.

**Telemetry ingestion.** donkey-telemetry accepts OTLP over scope-specific
tokens: an app token is bound to its own `service.namespace`/`service.name`, the
gateway and runtime tokens are domain-separated, and the private query API is a
separate control-plane-only token. Tokens are compared in constant time; SQL is
fully parameterized; a single-writer DuckDB connection serializes ingestion.

**Input handling and injection.** No string-templated YAML anywhere — every
object is a typed Go struct marshalled by the Kubernetes libraries, so names,
domains, env values and annotations have no field-escape surface. The database
browser routes every identifier through `pgx.Identifier.Sanitize()` and every
value through bind parameters; `LIMIT`/`OFFSET` are range-clamped `Atoi`
results; DSN injection is closed by re-serializing through `url.String()`. Name
validation is a strict RFC-1123 label at the edge of every handler.

**Archive and path handling.** Build-context extraction (`internal/server/untar.go`)
rejects traversal, absolute and drive-relative paths, normalizes backslashes,
drops symlinks/hardlinks/device nodes in both tar and zip, masks setuid/setgid,
and caps unpacked size and file count. The file browser resolves every path
with `realpath` inside the pod and re-checks containment on the resolved output
(leaf included, so a pre-existing symlink cannot be written through); the Docker
provider mirrors this. There is no client-side extraction.

**Command execution.** Every helper invocation is an argv slice, never a shell
line, in both runtimes. The only `sh -c` site is `initVolumeDirs`, whose
interpolated values are `StorageSubPath` slugs restricted to `[a-z0-9-]`. donkey
runs no command of its own against an app image — an app that must migrate does
so in its own start path.

**Namespace and space boundaries.** `ValidateSpace` reserves the mapping onto
`donkey-system`; a namespace lacking donkey's `donkey.local/managed` annotation
is never adopted or deleted; both deletion paths re-check ownership. Every
Kubernetes sweep is narrowed by installation plus logical-scope labels
(`donkey.local/installation` + space/app/addon); Docker verifies the managed
network and space before cascading. Name collisions are guarded both ways (an
addon cannot take an app's name or vice versa) and the `shared` owner is
reserved. Selectors and ownership checks use only immutable `donkey.local/*`
identity labels; the descriptive `app.kubernetes.io/*` labels never grant
authority.

**Optional certificate and DNS authority stays namespaced.** In the confined
two-namespace topology, donkey-server may write cert-manager Certificate and
external-dns DNSEndpoint objects only in its system namespace. It receives no
access outside the namespaces assigned to the installation. Cluster operators
should still constrain issuer and DNS policy when the selected controllers
serve unrelated tenants.

**Cryptography.** All randomness is `crypto/rand` (no `math/rand`, no
`rand.Seed`). No `InsecureSkipVerify` anywhere; `--ca-file` *adds* to the
system pool rather than replacing it, an unreadable file is a hard error, and
the `SystemCertPool` fallback is an empty (more restrictive) pool. CLI tokens
are AES-GCM sealed identities with purpose binding, a length guard before the
nonce slice, a non-empty-subject requirement and an expiry check — so
verification is the AEAD tag check, constant-time by construction. Generated
service passwords are 192-bit.

**Gateway request handling.** Session and login cookies are stripped before
proxying; inbound `X-Auth-Request-*` are deleted unconditionally (case-
insensitively); using `Rewrite` rather than `Director` discards client
`X-Forwarded-*`. ID token verification is complete — signature, `iss`, `aud`,
`exp`, with nonce and state bound, no `Skip*` flags. The post-login return has
no open redirect (`RequestURI()` escapes backslashes; cross-host returns are
confined to the SSO host). Certificate issuance is gated by the routing table,
so a hostname donkey does not serve cannot obtain a certificate. Error pages
HTML-escape their input.

**AI subsystem containment.** The source explorer uses `os.Root`, so `..`,
absolute paths and symlink escape fail at the syscall level. Fix proposals
require `fs.ValidPath`, are read and written through `os.Root`, require per-file
confirmation, and refuse to apply under `--yes`. `--tag`/`--no-build` cannot
point an app at a foreign image (`SameRegistry` is prefix-anchored with a
trailing slash). A Dockerfile path from an attacker-controlled `donkey.yaml`
cannot escape the build context. Registry credentials are released only to the
matching host. Every published image carries a SLSA provenance attestation; the
default `min` mode deliberately keeps build-arg *values* out of it (`max` would
embed them, which is why it is opt-in).

**Console.** Strict CSP — `script-src 'self'`, no `unsafe-inline`/`unsafe-eval`,
no wildcards, plus `frame-ancestors 'none'` and `base-uri 'none'` (the only
weakening is `'unsafe-inline'` in *style*-src). No `dangerouslySetInnerHTML`,
`innerHTML`, `eval`, `new Function`, `document.write`, `srcdoc`, `localStorage`,
`sessionStorage` or `document.cookie` anywhere in `web/src`. Markdown never
enables raw HTML; the highlighter escapes. The API mux is wrapped in
`http.NewCrossOriginProtection()`; the tunnel upgrader keeps gorilla's
same-origin default. The console holds no bearer token — it rides the gateway's
`HttpOnly; Secure; SameSite=Lax` session cookie.

**Credential hygiene.** Dry-runs render with `includeCreds=false`, so the
registry password never reaches a terminal or CI log. Addon credentials are
always `secretKeyRef` on Kubernetes, never inlined into a pod spec, and a
re-provision reuses the existing credential rather than rotating it. `donkey
env` masks values unless `--show-values`; catalog and list endpoints return key
*names* only. Destructive CLI commands default confirmation to false, treat
`--yes` as "answer with the default", and refuse in non-interactive mode.

## Open risks

Ranked by severity. Line-stable symbol references; a few are deliberate PoC
trade-offs, marked so.

### High

**`donkey-server` runs as `cluster-admin`.** In the namespace-per-space
arrangement the server binds the `cluster-admin` ClusterRole
(`manifests/rbac.go`, `serverAccessObjects`) — it creates and deletes
namespaces, which no Role expresses. The pod is otherwise correctly hardened
(`systemSecurityContext`), but combined with the fact that tenant apps can
reach it, the blast radius of any server-side flaw is the whole cluster. The
confined two-namespace mode (`kubernetes.workload.namespace`) already runs on
two namespaced Roles with no cluster-scoped grant — that is the scoped-RBAC
shape the default should move toward. *Required RBAC is already enumerated by
the object universe in `apply.go`.*

**Privileged BuildKit with an unauthenticated TCP listener.**
`buildkitPodSpec` runs `--privileged` with `--addr tcp://0.0.0.0:1234` and no
mTLS — anything that reaches the port runs code on a node as root. Its only
control is `buildkitNetworkPolicy` (admits donkey-server alone), which depends
on the CNI actually enforcing NetworkPolicy; kind's default CNI does not, and
nothing in `internal/doctor` checks that it does. The buildkitd pod is also the
one workload that omits `AutomountServiceAccountToken: false` without needing
API access. The image is digest-pinned, which is correct. *Fix: BuildKit mTLS,
`AutomountServiceAccountToken: false`, and a doctor check that NetworkPolicy is
enforced.*

**Docker runtime containers get no capability drop, no `no-new-privileges`, no
non-root user.** `replaceContainer` (`pkg/runtime/docker/deploy.go`) builds a
bare `container.HostConfig{Mounts: mounts}` — no `CapDrop`, no `SecurityOpt`, no
`User` — while the Kubernetes path sets all of it. Docker's default caps include
`CAP_NET_RAW`, so a compromised app can put a raw socket on the space bridge and
ARP-poison it; that bridge also carries donkey-server and donkey-gateway
(needed for probing and routing), sibling postgres traffic (`sslmode=disable`,
below), and identity headers stamped for signed-in admins. Docker's default
seccomp still applies, so seccomp parity holds; the gap is capabilities and
privilege escalation. *Fix: `CapDrop: ["ALL"]`, `SecurityOpt:
["no-new-privileges"]`, and a `User` field, re-adding only `NET_BIND_SERVICE`
where an app needs a low port.*

### Medium

**`donkey-gateway`'s Role covers every Secret in `donkey-system`.**
`donkeyGatewaySecretsRole` grants `get/list/create/update` on `secrets` with no
`resourceNames`. ACME and CA modes create or update named state Secrets;
cert-manager mode lists Secrets and filters installation and edge labels in
the client because Kubernetes RBAC cannot constrain a list by label. The grant
also covers `donkey-system-config` (registry password, OpenAI key, OIDC client
secret, session key). The gateway is the most exposed component donkey runs,
so any RCE/SSRF in it becomes one API call for the registry *push* credential
— write access to the path every app is built from. Its ClusterRole also reads
every ConfigMap cluster-wide (RBAC cannot filter by label;
`ListManagedRoutes` filters client-side). *Fix: put gateway-readable TLS state
in a dedicated namespace or mount a controller-maintained certificate bundle;
then restrict ACME/CA state by `resourceNames` and remove broad Secret access
from the system namespace.*

**Access allowlists match an unverified `email` claim.** The OIDC claims struct
decodes `email` but not `email_verified`, and `AccessConfig.Allows` compares it
against `access.users` and the console `admins` rule. On an IdP where the email
attribute is user-settable (guest/B2B accounts, open sign-up), a user the IdP
admits can set their profile email to an allowlisted address and match. `groups`
and `roles` compare exactly and are unaffected. *Fix: decode `email_verified`
and refuse `email` matching when it is present and false; warn at install when
`admins` uses `users:` against an issuer that does not assert it.*

**Push-capable registry credential copied into every tenant namespace.**
`RegistryPullSecret` (`manifests/app.go`) encodes the server's single registry
credential — the same one used to *push* — into a dockerconfigjson Secret in
every workload namespace (visible in `testdata/golden/app-objects.yaml`). Any
secret-read foothold in one namespace can overwrite another app's image tag;
`registry.immutable` reduces but does not remove this. *Fix: a pull-only or
per-space credential.*

**Deploy-time AI analysis sends unredacted config to the model provider.**
`analyzeApp` reads `donkey.yaml` including its `env:` block into `CurrentConfig`
and ships it verbatim; the project's `redact()` is wired only on the *failure*
path. `deniedFile` is filename-only with gaps (`local.env`, `prod.env`,
`.npmrc`, `.netrc`, `.pgpass`, `terraform.tfvars`, service-account JSON, …), and
the file tree lists every filename before the content check, so denied names are
disclosed regardless. `app.Build.Env` is fed to the build as build-args/secrets
that package managers echo into logs, and is not in `KnownSecrets`. *Fix: apply
`redact()` on the analyze path; extend `deniedFile` (suffix match `.env`); add
`app.Env`/`app.Build.Env` to `KnownSecrets`; prefer `donkey env set` over
`env:` in `donkey.yaml`.*

**Docker addon binding hosts are bare aliases that collide across spaces.**
`rewriteBindingHosts` turns `postgres.donkey-<space>.svc` into the bare alias
`postgres`; the addon name defaults to its type, so every space's postgres
carries the same alias, and donkey-server is attached to every space network.
An operator browsing space B's database can have the server resolve `postgres`
to space A's container and send space B's `DATABASE_URL` there. *Fix: rewrite to
the globally unique `ctrName(space, name)`, or take the host from
`ServiceEndpoint`.*

**Domain-wide session cookie can be overwritten by a sibling app.**
`donkey_session` and `donkey_login` carry `Domain=<base domain>` so one login
covers every app. Reading is blocked (`HttpOnly`, stripped before proxy), but a
sibling host under the base domain can *set* a cookie with the same name/domain
and replace the victim's — silently signing every visitor in as the attacker
(session), or forcing a login CSRF (login cookie). *Fix: bind the session to
its host (seal the issuing host and reject a mismatch), or mint per-app
`__Host-` cookies from the SSO cookie.*

**CLI loopback login callback has no `state` binding.** `browserLogin` listens
on `127.0.0.1:0` and accepts the first `?token=` anything delivers, with no
`state` generated or checked. A local process or a malicious page can scan the
ephemeral port during the login window and fix the CLI to the attacker's account
(`donkey deploy` then pushes into attacker-controlled spaces). *Fix: generate a
`state`, pass it through the login URL, reject callbacks that do not match.*

**CLI accepts `http://` for the token-bearing API.** `http://` is accepted, and
the tunnel derives `ws://` from it by string replacement, carrying the sealed
token and raw database traffic in the clear. The server always advertises
`https`, so this only exists as something a human types. *Fix: reject `http://`
for non-loopback hosts, or warn on every token-carrying command.*

**`0600` on the token file is not enforced when it already exists.**
`WriteFile(path, b, 0o600)` honours the mode only at creation, and `MkdirAll`
no-ops on an existing directory — so a pre-existing `0644`
`~/.config/donkey/config.yaml` (dotfiles checkout, config management, a
hand-written file with `kubeContext:`) receives a 30-day token in a
world-readable file. *Fix: `os.Chmod` after write (or temp-file-and-rename),
and warn on load when `mode&0o077 != 0`.*

### Residuals on the space network fence

Kubernetes installations fence each space with a NetworkPolicy
(`SpaceIsolationPolicy`): ingress is on by default (a space's pods accept
traffic only from their own space, the gateway and the server, so an `internal`
app is reachable only from its space), and optional egress restriction
(`kubernetes.restrictEgress`) confines outbound traffic to the same space,
donkey-ai, donkey-telemetry and DNS. Two residuals remain:

- **Enforcement is the CNI's.** On a plugin that ignores NetworkPolicy the
  fence is a silent no-op — the same caveat as BuildKit's policy, still
  unchecked by `internal/doctor`.
- **Egress is opt-in.** With the default (ingress-only), an app pod can still
  reach `169.254.169.254` and the internet; only `restrictEgress: true` closes
  that, and it is off by default because it breaks apps that call external APIs.
- **Provisioned postgres pins `sslmode=disable`** (`internal/addons/postgres.go`),
  so the password and rows cross the pod network in the clear — now confined to
  same-space traffic. The provisioned database is documented as development-grade;
  the production path is `donkey addon attach` to a managed database.

The Docker runtime isolates spaces structurally (one Docker network per space);
finding 5 (no `NET_RAW` drop) is what undermines that *within* a space.

## Lower-severity items

A batch worth clearing together.

| Item | Location |
|---|---|
| An unparseable access annotation fails **open**: the unmarshal error is discarded, `Access` stays nil, `Allows` then returns true for every signed-in identity | `internal/gateway/gateway.go` |
| Identity headers are forwarded to **public** apps that never asked for a login, handing a visitor's email and full group/role membership to a tenant app | `internal/gateway/proxy.go` |
| A route path with a trailing slash never matches its own sub-paths, so a protected app's traffic can fall through to a neighbouring public one | `internal/gateway/gateway.go`, `internal/config/app.go` |
| AI proxy: a double-encoded dot segment (`%252e%252e`) survives `fs.ValidPath` and reaches the upstream still encoded — inert against a compliant upstream, live against one behind a normalizing proxy | `internal/aiproxy/aiproxy.go` |
| The API HTTP client sets no `CheckRedirect`, so an https→http redirect on the same host keeps the `Authorization` header | `internal/apiclient/client.go` |
| `donkey open` passes a server-supplied string to the OS opener without a scheme check (`file://`, `smb://`); argv arrays, so no command injection | `internal/cli/open.go` |
| Server-supplied strings are printed raw to the terminal — ANSI/OSC-52 injection surface; `ui.Printf` is a bare `Fprintf` | `internal/ui/ui.go` |
| AI fix proposals preserve an existing file's mode, so a checked-in `0755` script or a `.git/hooks/` file is a reachable target — mitigated by per-file confirmation and `--yes` refusal | `internal/cli/fix.go` |
| Model-chosen volume paths are auto-applied server-side; `excludedVolume` still permits `/app`, `/home`, `/opt`, `/var/lib`, `/var/run` | `internal/server/analyze.go`, `internal/ai/openai.go` |
| Model-authored Dockerfile is written and built with no confirmation on the server path; the CLI path confirms with default yes, so `--yes` writes it unreviewed | `internal/server/analyze.go`, `internal/cli/init.go` |
| An app's `domain:` is validated for syntax only, with no ownership check; the gateway breaks ties with an unstable sort, and a route claiming the base domain at `/` collides with the console route | `internal/config/app.go`, `internal/gateway/gateway.go` |
| `PathRouteName` can collide across apps and `Apply` uses `Force: true`, so one route silently overwrites another — intra-space only | `manifests/pathroute.go`, `manifests/../apply.go` |
| S3 download relays a user-controlled `Content-Type` without `nosniff` — mitigated by a well-formed `Content-Disposition: attachment` | `internal/server/s3admin.go` |
| Upload skip-list is basename-only and ignores `.gitignore`/`.dockerignore`, so `.npmrc`, `.git-credentials`, `terraform.tfvars`/`.tfstate`, `kubeconfig`, service-account JSON are uploaded | `internal/config/upload.go` |
| Access rules (user emails, group names) are stored in a route annotation readable by anything with route-read | `manifests/route.go` |
| `donkey-system` carries no Pod Security Standard labels while tenant namespaces enforce `baseline` — necessary today for privileged BuildKit | `manifests/system.go` |
| The per-space `OPENAI_API_KEY` is pulled into the browser and held in React state — memory-only, and realising it needs a script-execution primitive the CSP denies | `web/src/AiDialog.tsx` |

## Hardening priorities

1. **Decide what an empty `auth.admins` means.** "Every account the IdP admits
   is an operator" is a surprising default; refuse to start, or set one.
2. **Docker runtime parity** — `CapDrop`, `SecurityOpt`, `User` in
   `replaceContainer`. Three lines, closes the highest Docker-side risk.
3. **Small and self-contained** — isolate gateway-readable TLS Secrets;
   `email_verified` in the claims struct; a `state` on the CLI login callback;
   reject `http://` for non-loopback; `os.Chmod` the token file.
4. **Redaction on the analyze path** — reuse `redact()`, extend `deniedFile`,
   add `app.Env`/`app.Build.Env` to `KnownSecrets`.
5. **Network** — a `donkey doctor` check that NetworkPolicy is enforced,
   BuildKit mTLS and token automount, and the Docker binding-host rewrite.
   Needs a cluster to validate.
6. **Structural** — scoped RBAC in place of `cluster-admin`, and a pull-only
   registry credential.
7. **The lower-severity batch** — the fail-open access annotation and the
   trailing-slash route are the two with real consequences.
