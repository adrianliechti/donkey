# Pluggable builders + mirror-aware builds (restricted / JFrog-only environments)

## Context

Goal: run donkey in a restricted environment whose only egress is a JFrog Artifactory
providing mirrors.

**Railpack cannot run offline as shipped, and one of its network calls is inside
`donkey-server` itself.** Three independent dependencies, verified in
`railpack v0.33.0` (`go.mod:17`):

| # | Where | What it fetches | Can it be pointed at JFrog? |
|---|---|---|---|
| 1 | **`donkey-server` process** — `internal/build/railpack.go:30` → `GenerateBuildPlan` | Downloads the mise binary from `github.com/jdx/mise/releases/...` into `/tmp/railpack/mise`, then execs `mise latest <pkg>@<ver>` per package against mise's backends | **No.** `core/mise/mise.go:187-207` builds `cmd.Env` from scratch (only `HOME`, `MISE_*` dirs, `PATH`, `GITHUB_TOKEN`) and passes `MISE_NO_CONFIG=1`. `HTTPS_PROXY`, `MISE_OFFLINE` and every mise config file are dropped. |
| 2 | **buildkitd**, solve time | `ghcr.io/railwayapp/railpack-{builder,runtime}:mise-2026.7.11`, plus `alpine:latest` / `composer:latest` / `dunglas/frankenphp` | Yes — registry mirrors + the exported vars `generate.RailpackBuilderImage` / `plan.RailpackRuntimeImage` are reassignable |
| 3 | **inside build steps** | `apt-get`, `mise install`, `npm ci`, `pip install`, `go mod download` … | Yes — `build_graph.go:263-270` mounts **every** plan secret as an env var on **every** `RUN`, so `NPM_CONFIG_REGISTRY`/`PIP_INDEX_URL`/`GOPROXY`/`HTTPS_PROXY` injected via build env reach all of them |

Note #1 also fires for `donkey init`, which runs detection client-side
(`internal/cli/init.go:59`) — developer laptops need the same egress today.

**CNB buildpacks have none of these problems.** Detection and build both run
*inside* the builder image, every toolchain is pre-baked, and the only pull is the
builder + run images. Behind a JFrog-only network, buildpacks work; railpack
needs a workaround around railpack internals that upstream can break.

So: make the builder **selectable in `server.yaml`**, ship buildpacks as the
offline-native option, and make railpack mirror-aware as a best-effort path for
sites that also have a forward proxy.

---

## Design

`server.yaml` gains one `build:` section covering every builder and every mirror:

```yaml
build:
  # Default builder for apps with no Dockerfile. railpack (current default) | buildpacks
  builder: buildpacks

  # OCI mirrors — written into buildkitd.toml
  registryMirrors:
    docker.io: https://jfrog.corp/docker-remote
    ghcr.io:   https://jfrog.corp/ghcr-remote
    gcr.io:    https://jfrog.corp/gcr-remote

  # Injected into every app build (reaches every RUN as an env var)
  env:
    NPM_CONFIG_REGISTRY: https://jfrog.corp/api/npm/npm
    PIP_INDEX_URL:       https://jfrog.corp/api/pypi/pypi/simple
    GOPROXY:             https://jfrog.corp/api/go/go

  # Optional forward proxy for donkey-server and buildkitd
  proxy:
    https:   http://proxy.corp:3128
    noProxy: .corp,10.0.0.0/8,.svc.cluster.local

  buildpacks:
    builderImage: jfrog.corp/gcr/buildpacks/builder:google-24
    runImage:     ""      # empty → the builder's own default, read from its metadata label
    platformAPI:  "0.14"

  railpack:
    builderImage: jfrog.corp/ghcr/railwayapp/railpack-builder:mise-2026.7.11
    runtimeImage: jfrog.corp/ghcr/railwayapp/railpack-runtime:mise-2026.7.11
```

Per-app `donkey.yaml` `build.type` still wins; `auto` resolves
Dockerfile → `.donkey/Dockerfile` → the installation's configured builder.

---

## Phase 1 — Builder abstraction + config plumbing

The strategy surface is already small: two switch sites,
`internal/detect/detect.go:38` and `internal/build/solve.go:112`.

- `internal/config/app.go:211` — add `BuildBuildpacks BuildStrategy = "buildpacks"`;
  extend the validation switch at `:391` and the `auto` defaulting at `:480`.
- `internal/config/install.go` — new `InstallBuild` struct on `InstallFile`
  (alongside the existing `BuildkitImage`, which already documents the mirroring
  case at `:20-22`).
- `internal/server/config.go:51` — read `DONKEY_BUILD_*` env into a `BuildConfig`
  on `server.Config`, same shape as the existing registry block.
- `pkg/runtime/kubernetes/manifests/system.go`:
  - `SystemInput` (`:27`) gains the build fields.
  - `buildkitTOML` (`:293`) emits `[registry."<host>"] mirrors = [...]` per entry,
    plus `[registry."<mirror-host>"]` inheriting the existing `RegistryCASecret`
    CA path — JFrog behind a private CA is the common case.
  - `serverDeployment` env (`:344`) gains `DONKEY_BUILD_*`; `buildkitPodSpec`
    (`:490`) gains `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`.
  - `buildkitConfigHash` (`:181`) must cover the mirrors so buildkitd rolls when
    they change — it already does this for the registry settings.
- `internal/cli/serverdocker.go:121` — same TOML additions for the Docker runtime.
- `internal/cli/server.go:40` — flags mirroring the new fields.
- `internal/build/solve.go` — merge the installation `build.env` under the app's
  own `build.env` (app wins) before both the railpack secrets path (`:165`) and
  the Dockerfile `build-arg:` path (`:131`). One change covers layer #3 for
  every builder.

## Phase 2 — Buildpacks builder (the offline-native path)

New `internal/build/buildpacks.go`, generating LLB that runs the CNB lifecycle in
the existing buildkitd — no new cluster components, no shelled-out `pack`.

Graph, from `llb.Image(builderImage)`:
1. `llb.Mkdir` `/workspace`, `/layers`, `/platform/env` owned by uid/gid 1000, then
   `llb.Copy` from `llb.Local("context")` into `/workspace`.
2. One `llb.Mkfile` per build-env entry at `/platform/env/<KEY>` — the CNB platform
   contract for build-time env.
3. `llb.User("cnb")` + `llb.Run` of:
   `/cnb/lifecycle/creator -app /workspace -layers /layers -platform /platform
    -cache-dir /cache -report /layers/report.toml -run-image <runImage> <ref>`
   with `CNB_PLATFORM_API` set, `/cache` on
   `llb.AsPersistentCacheDir("donkey-cnb-<app>", CacheMountShared)`.
4. `CNB_REGISTRY_AUTH` passed as a BuildKit secret with `SecretAsEnv(true)` —
   reuse the credentials `internal/build/auth.go:15` already assembles, formatted
   as `{"<host>":"Basic <b64 user:pass>"}`.

The lifecycle pushes the image itself, so this case sets **no image exporter**.
Instead export `/layers/report.toml` via `ExporterLocal` into a temp dir and parse
`[image] digest` — that is the authoritative digest, and it gives real error text
when export fails. `internal/build/solve.go:82` currently reads the digest from
`ExporterResponse`; the buildpacks case returns it from the report instead.

Detection (`internal/detect/buildpacks.go`) does **no work locally** — CNB detects
inside the image at build time. Consequences to handle:
- `Detection.Summary` becomes "Cloud Native Buildpacks (<builder image>)".
- Port guessing loses `Railpack.Providers` (`internal/server/analyze.go:82-95`,
  `internal/cli/init.go:283`). Default to 8080, the CNB launcher convention, and
  keep `donkey.yaml`'s explicit port as the override.
- `donkey init` no longer runs railpack locally in this mode — which removes the
  CLI's mise egress requirement entirely. This is the answer to the open
  CLI-scope question: **choose buildpacks and the CLI needs no egress.**

Both `builderImage` and `runImage` stay pure config, so any CNB builder works —
`gcr.io/buildpacks/builder:google-24`, Paketo, or an in-house one.

## Phase 3 — Railpack behind mirrors (best-effort)

Keeps the current default working, and gets railpack as far as a proxy-less JFrog
network allows.

- New `internal/build/railpack_images.go` — reassign the exported vars
  `rpgenerate.RailpackBuilderImage` (`core/generate/mise_step_builder.go:26`) and
  `rpplan.RailpackRuntimeImage` (`core/plan/plan.go:10`) from config at server
  startup. Solves layer #2.
- New `internal/build/mise.go` — `EnsureMise()`:
  - Computes the exact path railpack execs, `/tmp/railpack/mise/mise-<mise.Version>`,
    from railpack's exported `mise.Version`, so it stays correct across bumps.
  - Copies a **baked-in** mise binary there. `ensureInstalled`
    (`core/mise/install.go:71`) short-circuits on `os.Stat`, so the GitHub download
    never happens. `hack/Dockerfile.server` bakes it at image build time
    (it already documents this dependency at `:3-4` and `:15-18`).
  - Optionally writes a **shim** at that path instead — a script that re-injects
    proxy/mirror env (`HTTPS_PROXY`, `MISE_NODE_MIRROR_URL`, `GITHUB_TOKEN`) and
    execs the real binary. This is the only injection point railpack leaves open,
    and it is what makes version resolution work through a forward proxy.
  - Fail loudly at startup (and in doctor) if the baked binary's version does not
    match `mise.Version` — better than a mid-deploy timeout.
- **Honest limit:** `resolver.ResolvePackages` calls `GetLatestVersion`
  unconditionally, even for exact pins, and most mise backends resolve versions via
  the GitHub API, which JFrog cannot mirror. Without a forward proxy, railpack's
  plan step still fails. Document this and point restricted installs at buildpacks.

## Phase 4 — Doctor + docs

- `internal/server/handlers.go:657` — add checks: configured builder; builder/run
  (or railpack builder/runtime) images resolvable through the mirrors; mise binary
  present and version-matched when `builder: railpack`.
- `examples/server-install.yaml` and `examples/INSTALL.md` — a "restricted network"
  section: the JFrog repo types needed (Docker remote for `gcr.io`/`ghcr.io`/
  `docker.io`, npm/PyPI/Go/apt remotes), and the note that `buildkitImage`
  (`internal/config/install.go:20`) must be mirrored too.

---

## Verification

1. `task build && task test` — unit tests for `buildkitTOML` mirror rendering,
   `InstallFile` round-trip, build-env merge precedence, and `report.toml` parsing.
2. Local end-to-end on the kind stack (see `examples/INSTALL.md`): install with
   `build.builder: buildpacks` and `builderImage: gcr.io/buildpacks/builder:google-24`,
   then `donkey deploy` a Node and a Python sample. Confirm the pushed image runs
   and the digest in the deploy output matches `crane digest <ref>`.
3. **Prove the offline claim**: add a temporary egress NetworkPolicy on
   `donkey-buildkitd` and `donkey-server` allowing only the registry/mirror host and
   kube-dns — `pkg/runtime/kubernetes/manifests/system.go:544` currently sets
   `PolicyTypes: [Ingress]` only. A buildpacks deploy must still succeed; a railpack
   deploy is expected to fail at `GeneratePlan`, which is the documented behaviour.
4. `donkey doctor` — all checks green with mirrors configured; flip a mirror to a
   bad host and confirm the builder-image check reports it.
5. `donkey init` against a buildpacks installation — confirm it completes with no
   outbound traffic (no `/tmp/railpack` created).

## Risks / open items

- **CNB platform contract details** are the main unknown: exact `CNB_PLATFORM_API`
  the google-24 lifecycle accepts, whether running directly as `cnb` works or the
  `-uid/-gid` drop-privileges form is required, and cache-mount ownership. Spike
  this first with a hand-written LLB before wiring config.
- **Run image discovery**: leaving `runImage` empty means reading the builder's
  `io.buildpacks.builder.metadata` label. Simplest first cut is to require it
  explicitly in config and add label lookup after.
- **Railpack's mise shim** depends on railpack internals (exec path, arg shapes).
  Pin the railpack version deliberately and cover the shim with a test that asserts
  the path railpack would exec.
