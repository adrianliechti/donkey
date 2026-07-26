# Security review

**Date:** 2026-07-26
**Scope:** the core application — `internal/`, `cmd/`, `web/src/`. Test applications and samples (`examples/`, `testdata/`, `hack/`) were excluded.
**Method:** manual review of the auth-critical core, plus three parallel deep reviews (Kubernetes manifest rendering and RBAC, CLI credential handling, addon catalog and AI subsystem). Every load-bearing claim below was verified against the tree directly; findings marked *verified* were reproduced with a test or a direct read of the code at the cited location.

> **Note on repo state.** A substantial central-SSO refactor was uncommitted and actively changing during this review (the `BaseDomain` → `Domain` rename, `cookieDomain`, `loginState.Host`). Line numbers will drift if that work is still moving. The critical finding lives inside that new code, so it is a bug that would be shipped rather than one already deployed.

---

## The structural point that frames everything else

The control plane has exactly one credential. A single static bearer token, compared constant-time at `internal/server/server.go:207-216`, guards every route — no per-user identity, no per-space scoping. Every token holder is effectively an operator.

Consequently, "space A can reach space B" is not a boundary the API attempts to enforce, and such cases are **not** reported as vulnerabilities here. What matters is that this single token sits in front of a pod holding `cluster-admin`, and that several findings below become genuine privilege-boundary violations the moment per-user or per-space authorization is introduced.

---

## Summary

| # | Severity | Finding | Status |
|---|---|---|---|
| 1 | **Critical** | Gateway cookie type confusion → anonymous bypass of every login-protected app | **Fixed** |
| 2 | High | `donkey-server` runs as `cluster-admin` | Open |
| 3 | High | Privileged BuildKit with an unauthenticated TCP listener | Open |
| 4 | High | AI proxy forwards any path/method with the platform's real key | **Fixed** |
| 5 | High | AI proxy base-path escape via percent-encoded dot segments | **Fixed** |
| 6 | Medium | Push-capable registry credential copied into every tenant namespace | Open |
| 7 | Medium | No NetworkPolicy for tenant namespaces; `internal` is not enforced | Open |
| 8 | Medium | Deploy-time AI analysis sends unredacted secrets to the model provider | Open |
| 9 | Medium | CLI accepts `http://` for the token-bearing API | Open |
| 10 | Medium | `0600` on the token file is not enforced when the file already exists | Open |
| 11–20 | Low | See [Lower-severity items](#lower-severity-items) | Open |

Findings 2, 3, 6 and 7 are infrastructure changes that alter cluster behaviour (scoped RBAC can break deploys; BuildKit mTLS needs certificate provisioning; tenant NetworkPolicies need validation against a CNI that enforces them). They are deliberately left for a change that can be tested against a real cluster.

---

## 1. Critical — anonymous auth bypass on every login-protected app

**Location:** `internal/gateway/session.go:85-115`, `internal/gateway/oidc.go:113`, `internal/gateway/proxy.go:40-60`
**Status: FIXED.** Verified first with a proof-of-concept test, then fixed and covered by regression tests (`TestSealedValuesDoNotCrossPurposes`, `TestStateCookieIsNotAcceptedAsSession`, `TestSessionWithoutSubjectIsRejected` in `internal/gateway/gateway_test.go`). The description below is of the original defect.

`session.go` seals two different cookie types with the same AES-GCM key and no domain separation: the post-login `session` (`donkey_session`) and the pre-login `loginState` (`donkey_login`). `sessionFrom` (session.go:102) unseals whatever it is handed and unmarshals it into a `session`. Because `encoding/json` silently ignores unknown fields, a sealed `loginState` decodes cleanly — with empty `Subject`/`Email`/`Groups`, and `Expires` taken from the login state's own `exp` (15 minutes out, so the expiry check passes).

The gateway hands that sealed value to anyone who asks: `startLogin` sets the cookie **before** the visitor has authenticated (oidc.go:113).

```
curl -sI -H 'Accept: text/html' https://app.space.example.com/   # -> Set-Cookie: donkey_login=<sealed>
curl -H 'Cookie: donkey_session=<that same value>' https://app.space.example.com/
```

In `serveApp` (proxy.go:40) the forged session is non-nil, and `allowedAccess` (session.go:160) returns true whenever the route has no access rules, since `AccessConfig.Empty()` treats nil as "restricts nobody" (`internal/config/app.go:70-72`). The request is proxied to the app.

### Blast radius

Protection is the default: `protected()` in `internal/k8s/manifests/route.go:41-43` marks every non-`public` app as login-required, while access lists are opt-in. **Any app that requires sign-in without also naming specific users, groups, or roles is reachable with no login at all.**

- Apps with an explicit allowlist are safe — the forged session carries no email or groups.
- Apps trusting the forwarded `X-Auth-Request-User` header see it set to the empty string rather than absent.
- The admin console is **not** exposed by this: its route (`internal/k8s/manifests/system.go:142`) deliberately bypasses the gateway and keeps bearer auth.

### Same flaw, reverse direction: OIDC CSRF protection defeated

A sealed *session* decodes as a `loginState` with `State` and `Nonce` both empty. Therefore:

- an empty `?state=` query parameter satisfies the state check at `oidc.go:145`, and
- an ID token minted without a nonce claim satisfies the nonce check at `oidc.go:166`.

An attacker who can plant a cookie on the victim's browser — which any compromised tenant app under the shared parent domain can do — can complete a login as themselves inside the victim's session.

### Fix as applied

Domain separation in the sealer — each value is bound to the purpose it was minted for, using the AEAD's associated data, so a cookie sealed for one flow fails authentication when opened as the other:

```go
const (
	purposeSession = "donkey/session/v1"
	purposeLogin   = "donkey/login/v1"
)

func (s *sealer) seal(v any, purpose string) (string, error)   // aead.Seal(nonce, nonce, plain, []byte(purpose))
func (s *sealer) unseal(value, purpose string, v any) error    // aead.Open(nil, ..., []byte(purpose))
```

Two defence-in-depth checks were added alongside it, so that neither the state/nonce comparison nor the session check rests on the AAD binding alone:

- `sessionFrom` rejects a session with an empty `Subject` — every session the gateway mints carries the ID token's subject.
- `handleCallback` rejects a login state with an empty `State` or `Nonce` before comparing them, since `!=` against a blank value would wave through a callback carrying neither.

### Related: the login-state cookie was never cleared

It was set with `Path: "/oauth2/"` but cleared with `Path: "/"`, and a cookie deletion only matches when name, domain, **and** path all match — so the state cookie survived its callback and stayed replayable for its full 15-minute lifetime. **Fixed independently during the review**: `clearCookie` now takes the path explicitly and the callback passes `"/oauth2/"`.

---

## 2. High — `donkey-server` runs as `cluster-admin`

**Location:** `internal/k8s/manifests/system.go:210-220` — *verified*

```go
// serverClusterRoleBinding grants cluster-admin for the PoC: donkey-server
// creates namespaces and everything inside them. A scoped ClusterRole is the
// production follow-up.
RoleRef: rbacv1.RoleRef{..., Kind: "ClusterRole", Name: "cluster-admin"},
```

The code's own comment identifies this as a PoC shortcut. The pod it grants also offers pod-exec into arbitrary tenant namespaces (`internal/k8s/files.go:776-801`), raw SQL execution (`POST .../databases/{db}/query`), and S3 read/write — so any injection or auth bypass in that binary is a full cluster compromise rather than a single-tenant one. Its only authorization is the single static token (see the structural point above), and its route is internet-facing.

The container itself is correctly hardened (non-root, no privilege escalation, all capabilities dropped, seccomp `RuntimeDefault` — system.go:350-355), which is the right instinct given the grant.

**Fix:** the RBAC actually required is bounded and already enumerated by the code's own object universe in `internal/k8s/apply.go:25-44` — namespaces, deployments, services, secrets, PVCs, jobs, pods/exec, pods/log, httproutes, referencegrants, certificates, gateways.

---

## 3. High — privileged BuildKit with an unauthenticated TCP listener

**Location:** `internal/k8s/manifests/system.go:283, 468-476` — *verified*

```go
privileged := true
Args:            []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", buildkitPort), ...},
SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
```

Three compounding problems:

1. **`tcp://0.0.0.0:1234` with no TLS flags.** BuildKit's own documentation is explicit that an unencrypted TCP listener is unauthenticated and equivalent to handing out root.
2. **Privileged, and the only pod donkey renders that omits `AutomountServiceAccountToken: false`** — app pods (`app.go:417-421`), the pre-deploy job (`predeploy.go:58`), and the files helper (`files.go:677`) all set it.
3. **The sole compensating control is one NetworkPolicy** (system.go:517-543). It is correctly written — ingress only from the `donkey-server` pod label in `donkey-system` — but it is the *only* NetworkPolicy in the entire install, and kind's default CNI (kindnetd) does not enforce NetworkPolicy at all. Nothing in `internal/doctor` checks whether policies are actually enforced.

**Attack path on a non-enforcing CNI:** any tenant app pod → dial port 1234 → run arbitrary build steps in a privileged pod on a node → read the `donkey-server` service-account token from `/var/lib/kubelet/pods/*/volumes/...` → `cluster-admin` (finding 2). Even where policy *is* enforced, a privileged builder means a tenant Dockerfile exploiting a BuildKit or runc bug lands directly on the node.

The image is digest-pinned (`internal/config/install.go:75`), which is correct and worth keeping.

**Fix:** enable BuildKit's mTLS (`--tlscacert`/`--tlscert`/`--tlskey`), set `AutomountServiceAccountToken: false`, and have `donkey doctor` verify NetworkPolicy enforcement rather than assuming it.

---

## 4. High — AI proxy forwards any path and method with the platform's real key

**Location:** `internal/server/aiproxy.go:78-108`
**Status: FIXED** — see [Fix as applied](#fix-as-applied-for-findings-4-and-5) below.

```go
rest := strings.TrimPrefix(pr.In.URL.Path, aiProxyPrefix)
pr.Out.URL = upstream.JoinPath(rest)
pr.Out.Header.Set("Authorization", "Bearer "+upstreamKey)
```

The handler validates the space token and then forwards **any path and any method**. There is no endpoint allowlist (`/chat/completions`, `/embeddings`, `/models`) and no method restriction, so a space's binding token reaches `DELETE /ai/v1/files/{id}`, `POST /ai/v1/assistants`, and the fine-tuning and batch endpoints — destroying shared platform state.

Every app container can read its own `OPENAI_API_KEY` from env, the console hands it to the browser (`web/src/AiDialog.tsx`), and this endpoint is registered outside `s.auth` (`internal/server/server.go:140`) on an internet-facing hostname by design.

**Fix:** allowlist the inference paths and methods the addon actually advertises. Applied — see below.

---

## 5. High — AI proxy base-path escape via percent-encoded dot segments

**Location:** `internal/server/aiproxy.go:80-81` — *verified empirically*
**Status: FIXED** — see [Fix as applied](#fix-as-applied-for-findings-4-and-5) below.

`http.ServeMux` cleans the **escaped** path, so `%2e%2e` survives muxing; `URL.Path` is then decoded and `JoinPath` resolves the dot segments. Reproduced against the exact two lines of code:

```
/ai/v1/chat/completions            -> https://api.openai.com/v1/chat/completions
/ai/v1/../../admin/keys            -> REDIRECT 307 (blocked by ServeMux)
/ai/v1/%2e%2e/%2e%2e/admin/keys    -> https://api.openai.com/admin/keys      <-- escapes /v1
/ai/v1/..%2f..%2finternal/dump     -> https://api.openai.com/internal/dump   <-- escapes /v1
```

The upstream **host** is fixed from env and is not controllable, so this is not full SSRF. What it is: the platform credential is attached to arbitrary paths on the upstream host, and the response is relayed to the caller verbatim. Because the openai addon explicitly supports pointing `OPENAI_BASE_URL` at a company gateway (`internal/addons/openai.go:44-46`), on any self-hosted upstream with a debug or echo route that reflects request headers, **this discloses the real `OPENAI_API_KEY` to the caller**.

### Fix as applied (for findings 4 and 5)

A single `aiUpstreamPath` helper now gates both, and the handler additionally restricts methods to `GET` and `POST`:

- **Containment (finding 5):** the path is rejected unless `fs.ValidPath` accepts it — the standard library's definition of a clean relative path, so no `..`, no leading slash, no empty elements. This runs on the *decoded* `URL.Path`, which is where percent-encoded dot segments finally become `..`.
- **Surface (finding 4):** the first path segment must be one of `chat`, `completions`, `embeddings`, `models`, `responses`, `moderations`, `audio`, `images`. The management surface — `files`, `fine_tuning`, `assistants`, `batches`, `uploads`, `vector_stores`, `organization` — is refused with 403.
- The validated path is handed to the `Rewrite` hook through the request context, so the path that is proxied is exactly the one that passed the checks rather than a re-derivation.

Verified end-to-end through a real `ServeMux`: the three payloads that previously escaped (`%2e%2e/%2e%2e/admin/keys`, `..%2f..%2finternal/dump`, `%2e%2e/files`) now return 403, while `chat/completions`, `models`, and `models/{id}` route unchanged. Covered by `TestAIUpstreamPath` in `internal/server/aiproxy_test.go`.

Compatibility checked: donkey's own assistant does not go through this proxy (it reads `OPENAI_BASE_URL` from the server's env, which points at the real upstream), and the console's only proxied call is `GET /ai/v1/models`.

---

## 6. Medium — push-capable registry credential copied into every tenant namespace

**Location:** `internal/k8s/manifests/app.go:302-327` — *verified*

`pullSecret` embeds `in.RegistryUsername`/`in.RegistryPassword`, which come from the server's single registry config — the **same** credential `donkey-server` uses to *push* images (`internal/build/auth.go:16-28`). Every space's namespace therefore holds a Secret granting write access to the registry path all apps are built into.

Any foothold yielding secret-read in a single tenant namespace escalates to "overwrite any other app's image tag" — supply-chain compromise of every app on the platform. `RegistryImmutable` reduces but does not remove this, since new tags can still be pushed and a deploy pointed at them.

**Fix:** use a pull-only (or per-space) credential in the pull secret.

---

## 7. Medium — no NetworkPolicy for tenant namespaces; `internal` is not enforced

**Location:** the only NetworkPolicy rendered anywhere is `buildkitNetworkPolicy` (`internal/k8s/manifests/system.go:139, 517-543`) — *verified*

This makes a documented promise false. `internal/config/app.go:28-31` states that an `internal` app is reachable by "only the apps of its own space" — but it is a plain ClusterIP Service (`app.go:481-500`) that any pod in any namespace reaches at `http://<app>.donkey-<otherspace>.svc.cluster.local:80`.

The same applies to provisioned addons. Compounding it, the postgres binding pins plaintext (`internal/addons/postgres.go:39-44`):

```go
"DATABASE_URL": fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", ...)
```

So the 192-bit password crosses the pod network in the clear on every connect, where a co-tenant workload can sniff it. Usernames are fixed and public (`pgUser = "donkey"`, `rfAccessKey = "donkey"`, `pbAdminEmail = "admin@donkey.local"`), so the password is the entire barrier.

Egress is equally unrestricted, so app pods can also reach `169.254.169.254` (node IAM credentials on EKS/GKE) and `donkey-buildkitd:1234` (finding 3).

**Fix:** a default-deny ingress policy per space namespace allowing same-namespace plus the gateway namespace. This restores the stated property and closes finding 3's lateral path.

---

## 8. Medium — deploy-time AI analysis sends unredacted secrets to the model provider

**Location:** `internal/server/analyze.go:24-37`, `internal/ai/summarize.go:36-45, 76-92` — *verified*

`analyzeApp` runs on **every deploy** (`internal/server/deploy.go:288`) and on `donkey init`. It reads `donkey.yaml` — including its `env:` block (`internal/config/app.go:44`) — and passes it to `Analyze` with **no redaction**:

```go
if b, err := os.ReadFile(filepath.Join(dir, config.AppFileName)); err == nil {
    in.CurrentConfig = string(b)
}
```

`internal/ai/redact.go:113` shows the project already knows this file needs scrubbing on the *failure* path; the always-on path skips it. Alongside it, `manifestFiles` ships up to 24KB from `docker-compose.yml`, `application.properties`, `Dockerfile`, `Makefile`, and `README.md` — files that routinely carry credentials and that `deniedFile` does not match.

`deniedFile` (`summarize.go:76-92`) is the sole secret filter for explorer tool reads, and it is filename-only with concrete gaps:

- `strings.HasPrefix(lower, ".env")` — so **`local.env`**, **`prod.env`**, **`env.production`** pass
- no match for **`.npmrc`**, **`.netrc`**, **`.pgpass`**, **`.pypirc`**, **`.dockercfg`**
- the pattern list has `"password"` but not `"passwd"`, so **`.htpasswd`** passes
- **`terraform.tfvars`**, **`settings.py`**, **`wp-config.php`**, service-account JSON pass
- extension list is only `.pem .key .p12 .pfx .crt` — **`.p8`**, **`.jks`**, **`.der`**, **`.ppk`** pass

Separately, the file tree sent to the model lists every filename *before* the `deniedFile` content check (`summarize.go:129-135`), so denied filenames are still disclosed.

The CLI tells users "file names and manifest files, never secrets, go to the assistant" (`internal/cli/init.go:259`). That claim is not currently backed by a control.

**Fix:** apply the same `redact()` pass used on the failure path to the analyze path, and extend `deniedFile` (suffix matching for `.env`, plus the missing names and extensions).

---

## 9. Medium — CLI accepts `http://` for the token-bearing API

**Location:** `internal/config/global.go:155-160`, `internal/cli/login.go:61` — *verified*

```go
if u := c.Server.URL; u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
```

`http://` is explicitly accepted, and the interactive login prompt suggests it by example. The tunnel derives its URL by string replacement (`internal/apiclient/client.go:372`), so an `http://` base yields `ws://` carrying the bearer token in the clear.

It is not only the token: `GET /v1/spaces/{space}/apps/{app}/env` returns **plaintext** env values, `donkey files get` streams disk contents, and `donkey addon tunnel` carries raw database traffic. The server itself always advertises `https` (`Scheme()` is hardcoded), so `http://` exists only as something a human types.

**Fix:** reject `http://` for non-loopback hosts, or warn loudly at login and on every token-carrying command.

---

## 10. Medium — `0600` on the token file is not enforced when the file already exists

**Location:** `internal/config/global.go:163-172` — *verified*

```go
os.MkdirAll(filepath.Dir(path), 0o700)
return os.WriteFile(path, b, 0o600)
```

`os.WriteFile` maps to `OpenFile(..., O_CREATE|O_TRUNC, perm)`, and the mode argument is honoured **only at creation**. `os.MkdirAll` is likewise a no-op on an existing directory.

If `~/.config/donkey/config.yaml` pre-exists as `0644` — a dotfiles repo checkout (git preserves only the exec bit), a config-management tool, a restored backup, or a hand-written file — then `donkey login` writes a full-privilege, non-expiring token into a world-readable file and never says so.

**Fix:** `os.Chmod(path, 0o600)` after the write (or write to a `0600` temp file and rename), plus `os.Chmod` on the directory. Optionally warn on load when `mode&0o077 != 0`, as `ssh` does.

---

## Lower-severity items

Worth addressing as a batch.

| Finding | Location |
|---|---|
| HTTP client has no `CheckRedirect`, so redirects are followed with the `Authorization` header; Go's stripping rule compares hosts only, so https→http on the same host keeps the token | `internal/apiclient/client.go:35-55` |
| `donkey open` passes a server-supplied string to `open`/`xdg-open`/`rundll32` without checking the scheme; these openers act on `.desktop`/`.app` paths and arbitrary URL schemes | `internal/cli/open.go:29-54` |
| Filenames read off an app's disk are printed raw to the terminal — anything that can write to that disk can inject ANSI/OSC 52 clipboard sequences | `internal/cli/files.go:74-81` |
| AI fix proposals are confined to the app directory by `os.Root` but not restricted within it; `.git/hooks/pre-commit` is reachable, and its execution is separated in time from the user's confirmation | `internal/cli/fix.go:101-127` |
| An app's `domain:` is validated for syntax only, with no ownership check; donkey-gateway breaks hostname ties with an unstable `sort.Slice` on path length, so an alphabetically-earlier namespace deterministically wins | `internal/config/app.go:314`, `internal/gateway/gateway.go:232-236, 302-311` |
| `PathRouteName` can collide (`(a,"b-c","/d")` and `(a,"b","/c/d")` both yield `a-on-b-c-d`) and `Apply` uses `Force: true`, so one route silently overwrites the other | `internal/k8s/manifests/pathroute.go:19-25`, `internal/k8s/apply.go:79` |
| S3 download relays a user-controlled `Content-Type` without `nosniff`, unlike the file-browser download path which sets it correctly | `internal/server/s3admin.go:346-353` |
| AI proxy compares tokens with a map lookup rather than `subtle.ConstantTimeCompare`; no rate limiting, no per-space attribution | `internal/server/aiproxy.go:45` |
| Client headers other than `Authorization` (`Cookie`, custom headers) are forwarded upstream alongside the platform key | `internal/server/aiproxy.go:79-86` |
| Upload skip-list is basename-only and ignores `.gitignore`/`.dockerignore`, so `config/master.key`, `.npmrc`, `*.tfvars`, `id_rsa_work` etc. are uploaded on every deploy | `internal/config/upload.go:24-44` |
| Addon `version` is unvalidated and becomes a container image tag (not exploitable — the repository prefix is fixed by concatenation) | `internal/server/handlers.go:438-441` |
| Model-chosen volume paths auto-applied without confirmation; `excludedVolume` blocks `/tmp`, `/etc`, `/usr` but not `/app`, `/home`, `/opt`, `/var/lib` | `internal/server/analyze.go:87-96` |
| Access rules (user emails, group names) stored in a route annotation readable by anything with route-read | `internal/k8s/manifests/route.go:54-67` |
| `donkey server install` prints the token to stdout — intended, but this command commonly runs in CI where stdout is archived | `internal/cli/server.go:301-303` |

---

## Controls that hold

Worth recording, because it shapes where effort should go.

**Input handling and injection.** No string-templated YAML anywhere — every object is a typed Go struct marshalled by the Kubernetes libraries, so there is no field-escape surface for app names, domains, env values, or annotations. The database browser routes every identifier through `pgx.Identifier.Sanitize()` and every value through bind parameters. Name validation is a strict RFC-1123 label (≤40 chars) applied at the edge of every handler.

**Archive and path handling.** Build-context extraction (`internal/server/untar.go`) rejects traversal and absolute paths, drops symlinks and specials, normalizes Windows separators, and caps both unpacked size and file count. The file browser resolves every path with `realpath` inside the pod and re-checks containment; commands are argv arrays, never a shell. There is no client-side archive extraction at all, so zip-slip has no surface there. Tar packing does not follow symlinks.

**Pod hardening.** App pods, pre-deploy jobs, the files helper, and all three addon types consistently set `AutomountServiceAccountToken: false`, `RunAsNonRoot`, non-root UID/GID, `AllowPrivilegeEscalation: false`, `Capabilities: drop ALL`, and `SeccompProfile: RuntimeDefault`. App namespaces enforce Pod Security `baseline` and warn `restricted`. No hostPath, hostNetwork, hostPID, or hostIPC is rendered anywhere. (BuildKit is the sole exception — finding 3.)

**Namespace boundaries.** `SpaceNamespace` refuses names resolving to `donkey-system` or the gateway namespace, `CheckNamespaceFree` refuses to adopt any namespace lacking donkey's managed annotation, and deletion paths re-check the same annotation. Names rendered into `donkey-system` carry a content hash, so no `(space, app)` pair can collide.

**Cryptography.** All randomness is `crypto/rand` — 192-bit API tokens and addon passwords, 256-bit session key. Zero occurrences of `math/rand` or `rand.Seed` in the repo. No `InsecureSkipVerify` anywhere; `--ca-file` *adds* to the system pool rather than replacing it, and an unreadable file is a hard error rather than a silent fallback.

**Process execution.** `exec.Command` appears exactly once in the entire codebase and never through a shell. `internal/gitutil` parses `.git/HEAD`, loose refs, and `packed-refs` by hand rather than shelling out, so a hostile branch name or remote URL can never become a process argument.

**Credential hygiene.** Dry-runs render with `includeCreds=false`, so the registry password never reaches a terminal or CI log. `--tag` is constrained to donkey's own registry, so a no-build deploy cannot run an arbitrary image under an app's name. Build and startup output is scrubbed against a `KnownSecrets` list covering app env, every attached binding, the registry password, the OIDC client secret, and the API token itself. Re-install reuses existing credentials rather than silently rotating them. `donkey env` masks values unless `--show-values` is passed. Bindings are always Secrets, never ConfigMaps, and catalog/list endpoints return key *names* only.

**Gateway request handling.** The session cookie is stripped before proxying to app backends, inbound `X-Auth-Request-*` headers are deleted, and using `Rewrite` (not `Director`) means `X-Forwarded-*` values from the client are discarded. Error pages HTML-escape their input.

**AI subsystem containment.** The explorer uses `os.Root`, so `../`, absolute paths, and symlink escape all fail at the syscall level; tools are read-only with no shell, write, or network. Fix proposals require `fs.ValidPath`, are read and written through `os.Root`, require per-file confirmation, and **refuse to apply under `--yes`**. Model-proposed `preDeploy` commands are advisory only and additionally screened for shell metacharacters.

**Console.** Strict CSP with `frame-ancestors 'none'` and `base-uri 'none'`, `nosniff`, `Referrer-Policy: no-referrer`, HSTS. No `dangerouslySetInnerHTML`, `innerHTML`, or `eval` anywhere in `web/src`. The whole API mux is wrapped in `http.NewCrossOriginProtection()`.

**CLI safety.** Destructive commands (`destroy`, `files rm`, `addon remove`) default confirmation to `false`, treat `--yes` as "answer with the default" rather than "yes", and error out in non-interactive mode rather than proceeding.

---

## Remaining work, in suggested order

1. ~~**Finding 1**~~ — fixed.
2. ~~**Findings 4 and 5**~~ — fixed.
3. **Findings 9 and 10** — a few lines each in the CLI: reject `http://` for non-loopback hosts, `os.Chmod` the token file after writing.
4. **Finding 8** — reuse the existing `redact()` on the analyze path, and extend `deniedFile`.
5. **Finding 3** and **finding 7** — the NetworkPolicy work closes BuildKit's lateral path and restores the `internal` guarantee; BuildKit also needs mTLS and `AutomountServiceAccountToken: false`. Needs a cluster to validate against.
6. **Findings 2 and 6** — the larger structural changes: scoped RBAC in place of `cluster-admin`, and a pull-only registry credential.
7. The [lower-severity items](#lower-severity-items) as a batch.
