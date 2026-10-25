# Vulnerability scanning: a gate at build time, a watch at runtime

Status: phase 1 (the gate) implemented — `internal/build/scan.go` (the
inventory step in the build engine), `internal/build/trivy.go` (the
orchestrator's scanner and database), `internal/server/scan.go` (policy,
judge, record), `build.scan` settings, space waivers, the assistant's
triage, doctor checks. The scan is inventory-first: the step writes an SPDX
SBOM and the secrets report offline, the orchestrator matches the SBOM
against its database on the data volume, and the SBOM is stored with the
release, so the watch (phase 2) rescans with `Scanner.Rescan` — no image
pull, no build engine. The outcome is stamped on the workload
(`AppSecurity`, an annotation or label like the stack) and shown as a badge
on the fleet, space tiles and the app page; the Deployment tab shows the
full scan. Phase 2's loop (which updates that stamp after a rescan) and
phase 3 are open.
Decided: the gate blocks on critical findings with a fix
available, unfixed findings never block, and the assistant's verdict decides
on its own whether a blocked deploy continues (`triage: assistant` is the
default). Secrets baked into an image block by default; confirm after the
first weeks of noise.

## Goal

Two promises, stated the way the console will state them:

1. **Nothing with a known critical, fixable vulnerability is deployed.** The
   deploy refuses before the image is pushed, says which packages and which
   fixes, and the assistant says whether the app is actually affected.
2. **An app that becomes vulnerable after it was deployed is marked
   "needs action".** New advisories land daily; the image does not change, so
   the platform re-reads its images against the current database and tells the
   space.

Scope is what donkey builds and runs: app images (phase 1), then the images of
services and of the control plane itself (phase 3). Source-level SAST
(semgrep/opengrep) is deliberately out — see *What was ruled out*.

## Where the scan runs: inside BuildKit, like the startup check

donkey already has the shape this needs. A build that must prove its image
starts runs through the BuildKit gateway (`internal/build/solve.go:51` →
`buildVerified`, `internal/build/verified.go`): the frontend's result is
solved first, a probe is run on it as an LLB step with busybox mounted beside
the app (`internal/build/verify.go:53`, mount at `:75`), and only then is the
image exported. "Nothing that cannot start is ever pushed under an app's name."

The scan is the same move with a different tool mounted beside the app:

```
frontend solve ──▶ [scan]  trivy rootfs /target  ──▶ [verify] start & probe ──▶ export/push
                      │ report.json read back
                      ▼
            policy: block / warn / info  ──▶  assistant triage  ──▶ findings
```

- `llb.Image(scannerImage)` is the run step's root; the built image's state is
  mounted read-only at `/target`; `/out` is a scratch mount; the vulnerability
  database lives on `llb.AsPersistentCacheDir("donkey-scanner", CacheMountShared)`
  so it is downloaded once and refreshed on Trivy's own schedule, not per build.
- The report is read back from the `/out` mount with `gw.Solve` +
  `ref.ReadFile` — the gateway build already has `res`/`ref` in hand at
  `verified.go:43`.
- `llb.IgnoreCache` on the step, as for verify: a scan must run every time.

Why this and not a scanner service beside the control plane:

| | BuildKit step | `donkey-scanner` deployment |
|---|---|---|
| New cluster component | none | one more pod, policy, image, doctor check |
| Pre-push gate | natural — the image is not exported yet | needs the image pushed first, then a gate on the apply |
| Restricted networks | scanner image comes through `registryMirrors` like every other image | its own egress story |
| Docker runtime | identical: the same buildkitd container | a second container to install and keep healthy |
| Runtime rescans | `llb.Image(digest)` of a published image, same step | the service pulls with its own credentials |

The cost is honest, too: BuildKit is the one privileged component. Running a
scanner step in it adds nothing to its exposure (it already runs arbitrary
RUN instructions from user Dockerfiles), but rescans do consume build-engine
CPU; phase 2 bounds that.

## The scanner: Trivy

One tool, one database, one report format, for both the gate and the watch:

- **Covers the image, not just the lockfile.** OS packages (dpkg, apk, rpm)
  plus the language ecosystems donkey builds (npm, pip, Go binaries, JARs,
  .NET `deps.json`). Most of the noise and most of the criticals live in the
  base image, which `npm audit`/`pip-audit` never see.
- **`rootfs` mode** scans a mounted directory — exactly what an unexported
  BuildKit result is. The same mode serves runtime rescans.
- **Secrets in the same pass** (`--scanners vuln,secret`): AWS keys, GitHub
  tokens, private keys, `.env` files baked into the image. Zero extra cost.
- **Offline-capable.** The database is an OCI artifact
  (`ghcr.io/aquasecurity/trivy-db`, plus `trivy-java-db`); `--db-repository`
  points it at a JFrog remote. The scanner image itself rides
  `build.registryMirrors`. Nothing new for a restricted install beyond two
  more mirrored repositories.
- Single static binary, Apache-2.0, pinned by digest like
  `DefaultBuildkitImage` (`internal/config/install.go:829`).

Report fields donkey keeps per finding: id, severity, CVSS score, package,
installed version, fixed version, status (`fixed`, `affected`, `will_not_fix`,
`end_of_life`), target path (which layer/file), title, published date.

### What was ruled out

- **Syft + Grype.** donkey already opts into BuildKit's `attest:sbom`
  (`solve.go:174`, syft-based) — but that attestation is produced by the
  exporter, i.e. after the point where a gate can still refuse the push, and
  it splits the job over two tools. Trivy can emit the same CycloneDX SBOM
  from its pass if the attestation is wanted; keep the existing opt-in.
- **Package-manager audits** (`npm audit`, `pip-audit`, `govulncheck`,
  `dotnet list --vulnerable`). Inconsistent coverage and output, each needs
  its toolchain and its own network path (npm audit posts to the registry;
  donkey-packages would have to proxy that), and none sees the OS layer.
  `govulncheck` is the one worth keeping in mind: for Go it answers the
  "really affected?" question statically through the call graph. It can be a
  later, Go-only sharpening of the assistant's verdict, not the backbone.
- **OSV.dev API.** Clean and batch-friendly, but an internet dependency for
  every scan; the offline variant is a per-ecosystem zip download. Trivy's
  mirrored OCI database is the better fit for the restricted installs
  `IDEA_BUILDER.md` targets.
- **semgrep / opengrep.** A different product: finds bug patterns in the
  app's own code (injection, unsafe deserialization), not known
  vulnerabilities in what it ships. High false-positive rate without tuning,
  rules licensing moved under semgrep, minutes per run. Neither stated goal
  needs it. If donkey ever does a code-level security read, the "two readers"
  rule applies: a scanner produces evidence, the assistant judges — and there
  is no good evidence-producing scanner for this yet.
- **TruffleHog** is not ruled out, but it is not phase 1 — see phase 3. Its
  distinctive features (live verification of found credentials, git-history
  scanning) are separate decisions with their own trade-offs; Trivy's secret
  scanner already catches credentials baked into images.

## Policy

Installation-wide, in the install file beside the other build settings
(`BuildSettings`, `internal/config/install.go:388`):

```yaml
build:
  scan:
    enabled: true
    image: aquasec/trivy:<version>@sha256:…     # mirrored like buildkitImage
    dbRepository: ""                             # e.g. jfrog.corp/ghcr/aquasecurity/trivy-db
    javaDbRepository: ""
    # Findings at or above this severity, with a fix available, refuse the
    # deploy. none | critical | high
    block: critical
    # Unfixed findings never block: nothing the developer can do about them.
    ignoreUnfixed: true
    # Secrets found in the image: block | warn | off
    secrets: block
    # assistant | off — the assistant's "not affected" verdict (high
    # confidence) turns a blocking finding into a warning and the deploy
    # continues. off: the verdict is shown but only a space admin's waiver
    # lets a blocked image through.
    triage: assistant
    # Re-read running images against the current database. 0 disables.
    rescan: 24h
```

Three rules keep the gate meaningful:

1. **Severity comes from the scanner, never from the assistant.** The
   assistant can *waive* (block → warn, with reasons on the record); it can
   never raise or lower a severity, and `triage: off` makes the gate purely
   mechanical for installations that want it so.
2. **Unfixed findings inform, they do not block.** A critical in Debian's
   libc with no fix is in every `python:*-slim` image on earth; blocking on it
   blocks everyone and teaches people to ignore the gate.
3. **Human waivers are a space-admin action, not a `donkey.yaml` key.** A
   developer who can waive a gate from the app's own config has no gate.
   Waivers live in space settings (`config.SpaceConfig`,
   `internal/config/space_config.go:13`): app, finding id, reason, who, expiry
   (default 30 days). The console lists them and the deploy stream names
   them when it skips one.

```yaml
# space settings (console-managed)
securityWaivers:
  - app: shop
    id: CVE-2026-12345
    reason: "ImageMagick is installed but never invoked; removal tracked in #412"
    by: adrian@…
    expires: 2026-11-24
```

## The assistant's verdict

This is the piece that makes the gate bearable. A scanner says *the package is
present*; whether the *app* is affected depends on what the code does with it.
The assistant already reads source with containment the OS enforces
(`internal/ai/explore.go:46`, `os.Root`; tools at `internal/ai/search.go:18`)
and already turns failure evidence into reasoned proposals
(`DiagnoseFailure`, `internal/ai/assistant.go:18`). A third job of the same
kind:

```go
// internal/ai/assistant.go
TriageVulnerabilities(ctx, *TriageInput) (*Triage, error)

type TriageInput struct {
    Root      string            // build workspace, or a source-history checkout
    Recipe    string            // the Dockerfile that produced the image
    Exposure  string            // public | protected | internal
    Findings  []Vulnerability   // only the ones that would block, after waivers
}

type Verdict struct {
    ID         string   // finding id
    Verdict    string   // affected | not-affected | unsure
    Confidence string   // high | medium | low
    Reason     string   // one paragraph, in the user's language
    Evidence   []string // files and lines the reason rests on
    Fix        string   // what would make it go away: bump X to Y, drop package Z, newer base tag
}
```

What it is asked, per finding: is the vulnerable package imported, invoked,
or on a request path? Is it a build-only dependency that happens to be in the
image? Is the vulnerable function reachable given how this app is exposed?
For an OS package: does anything in the image exec it? The prompt carries the
advisory text and the finding's target path, and the explorer tools do the
reading — the same pattern as the Dockerfile writer, no new plumbing.

Only `not-affected` with `high` confidence waives a block, and only under
`triage: assistant`. Every verdict — including the ones that change nothing —
is stored with the scan record and shown next to the finding, so a reviewer
can disagree with it. The cost is bounded: triage runs only for findings that
would otherwise block, and at runtime only for findings that are *new* for
that digest (cache by `(digest, id)`).

Honest framing for the docs: a verdict is a reading, not a proof. The install
option exists so an operator who wants "mechanical only" has it in one line.

## Findings, records and where the state lives

**Findings.** The scan produces `Finding`s (`internal/server/findings.go:10`)
with stable codes the console keys off — `vuln-blocking` (level `block`),
`vuln-waived` (`warn`, with the verdict or the waiver named), `vuln-fixable`
(`warn`), `vuln-unfixed` (`info`, collapsed to one line with a count),
`secret-in-image` (`block`/`warn` per policy). They travel the existing
stream, so CLI, console and the release record all say the same thing, and
`Blocking` (`findings.go:51`) is what stops the deploy.

**Scan records.** The telemetry store already versions build records per app
(`internal/telemetry/store.go:174`, `InsertBuild`/`BuildByImage`
at `builds.go:43,132`). A `scan_records` table beside it:

```sql
CREATE TABLE IF NOT EXISTS scan_records (
  space TEXT NOT NULL, app TEXT NOT NULL,
  digest TEXT NOT NULL,          -- the image scanned
  scanned BIGINT NOT NULL,       -- when
  db_version TEXT NOT NULL,      -- trivy-db UpdatedAt, so "new since" is answerable
  critical INT, high INT, fixable INT, secrets INT,   -- summary for the fleet view
  state TEXT NOT NULL,           -- clean | needs-action | blocked | failed
  record TEXT NOT NULL,          -- full report + verdicts, JSON
  PRIMARY KEY (space, app, digest, scanned)
);
```

Bounded retention like build records. The release record (`AppRelease`,
`internal/server/release.go:25`) carries the build-time scan's summary and
sequence so the Deployment tab can link to it.

**Runtime state.** `AppSummary` (`pkg/runtime/provider.go:174`) and the app
status response (`internal/server/handlers.go:53`) gain one block:

```json
"security": { "state": "needs-action", "critical": 1, "high": 3, "fixable": 2,
              "scanned": "2026-10-25T06:00:00Z", "newSince": "2026-10-24T…" }
```

`state` is computed from the latest record and the current policy and
waivers, so changing the policy re-labels the fleet without a rescan.
`needs-action` means: a finding at or above `block` severity, fixable, not
waived, not judged not-affected — i.e. exactly what would refuse this image if
it were deployed today.

## Phases

### Phase 1 — the gate

- `internal/build/scan.go` — `ScanSpec` (image, db repositories, severities,
  scanners) and `scanState(target llb.State, spec ScanSpec) llb.State`
  mirroring `verifyState`; `readScanReport(gw, ref)`; a parser for Trivy's
  JSON into `[]Vulnerability` + `[]Secret`. Registry auth for the database
  pull: a BuildKit secret mount of a `config.json` at `DOCKER_CONFIG`, built
  from the credentials `internal/build/auth.go` already assembles.
- `internal/build/verified.go` — run the scan between the frontend solve
  (`:36`) and the startup check (`:59`). Return the report on `Result`, and a
  typed `ScanBlockedError{Findings}` so the repair loop can tell "build
  failed" from "build fine, image refused".
- `internal/build/solve.go:51` — route through the gateway when
  `req.Scan != nil || req.Verify != nil`. Note `deploy.go:553` drops `Verify`
  when stored env exists; the scan must not be dropped with it.
- `internal/config/install.go` — `ScanSettings` on `BuildSettings` with the
  YAML above; validation; `DefaultScannerImage` pinned by digest.
  `internal/server/config.go` reads `DONKEY_BUILD_SCAN_*`;
  `pkg/runtime/kubernetes/manifests/system.go` and
  `internal/cli/serverdocker.go` pass them through. The scanner image is not
  a pod, so no manifest changes beyond env.
- `internal/server/deploy.go:547` — set `req.Scan` from policy and the
  space's waivers; after `buildGenerated` (`:563`), turn the report into
  findings, print them, persist the scan record.
- `internal/server/findings.go` — the codes above.
- Rollback and `--no-build` deploys (`deploy.go:406`): apply the same policy
  to the stored record for that image; rescan first when it is older than
  `rescan`. Deploying is deploying.
- Assistant triage, build time: `TriageVulnerabilities` over the build
  workspace, invoked only when the scan would block.
- Repair loop (`internal/server/aibuild.go:34`): a `ScanBlockedError` on a
  *generated* recipe is failure evidence like any other — the next attempt's
  `DockerfileInput` carries the findings ("critical in `openssl` 3.0.11,
  fixed in 3.0.13: prefer a current patch tag or a slimmer base"). For an
  app-owned Dockerfile or a lockfile pin, no silent rewrite: the assistant
  proposes `FixAction`s with diffs, as `diagnoseWith` does today, shown in the
  console and CLI for the developer to apply.
- CLI: findings already stream as lines; a blocked deploy ends with the
  refusal, the list, and the one-line reason per waived finding. Exit code
  non-zero.
- Console: `PlanReview`/`ReleaseView` render the new finding codes with
  `FindingsList`; a blocked release shows the findings as the failure.
- Doctor (`internal/server/handlers.go:1026`, the orchestrator's checks):
  scanner image pullable through the mirrors; database repository reachable;
  database age.

### Phase 2 — the watch

- Scan loop in the orchestrator — it owns BuildKit access, the deploy board
  and the source history. Shape of `resourceTelemetryLoop`
  (`internal/server/resources.go:40`, started at `server.go:250`), but with
  `rescan` as the interval. Each tick: list running apps with their image
  digests (`AppSummaries`); for each whose latest record is older than
  `rescan` *or* was made against an older database, rescan from the
  inventory stored with its release (`AppScan.SBOM`, via
  `BuildKitBuilder.Rescan`) — a database lookup, no image pull; store the
  record. One scan at a time, and the loop yields while a deploy is in
  progress, so rescans never queue behind a developer's build. Trivy's
  database refresh happens inside the cache mount on its own schedule. An
  image with no stored inventory (built before scanning existed) is scanned
  once from `llb.Image(digest)` with the same step, which then stores one.
- New findings only go to the assistant for triage, against a checkout of the
  commit that built the image (the source history lives on the orchestrator's
  volume, `internal/server/config.go:109`).
- API: `GET /v1/spaces/{space}/apps/{app}/security` (latest record with
  verdicts and waivers, plus history); `POST …/security/scan` (rescan now,
  an orchestrated route — `internal/server/orchestrator.go:46`); waivers via
  the existing `PUT /v1/spaces/{space}/settings`. `GET /v1/fleet` carries the
  summary block.
- Console: `SpaceIssue` (`web/src/fleet.ts:6`) gains a `security` variant so
  the fleet's *Needs attention* rail (`FleetOverview.tsx:358`) and the
  switcher list vulnerable apps beside unhealthy ones, with their own tone —
  the app is running fine, it is not *down*. A **Security** tab on the app
  (`AppDetail.tsx:58`): findings grouped as *needs action / fixable /
  unfixed / waived*, each with package, versions, the advisory link, the
  assistant's verdict and evidence, and the actions: **Rescan**, **Waive**
  (space admins; reason and expiry required), **Rebuild**. Platform settings
  show the policy read-only, and System view shows the scanner's database
  age.
- **Rebuild** is new ground: deploy the app's last built commit again from
  the source history, no upload. `POST …/rebuild` in the orchestrator, taking
  the commit's archive (`handleCommitArchive` exists) through `deployApp`.
  This is the fix for most base-image findings — the `FROM` tag moves
  upstream — and the console can say which findings a rebuild is likely to
  clear (OS packages with a fix) versus which need a lockfile change.
- Feedback into the recipe writer: when a base image carried blocking
  findings last time, the next `WriteDockerfile` input says so, so the
  assistant prefers a current patch tag or a slimmer base on its own.

### Phase 3 — everything else that runs

- **Services and the platform itself.** The same loop over addon images
  (postgres, redis, minio, …) and the control plane's images (server,
  buildkitd, gateway, telemetry). Results in the space's service card and in
  System view; no gate, since nothing is "deployed" by a user here, but the
  same `needs action` label and a one-line "upgrade available" when donkey
  ships a newer pinned image.
- **TruffleHog over the source history.** A second step with
  `trufflesecurity/trufflehog` mounted and the app's history repository as
  the target: `trufflehog git file:///target` finds secrets committed at any
  point, not only those in the latest image. Verification (calling the
  provider to test whether a found key is live) stays **off by default** —
  the platform sending found credentials to third parties is an operator's
  choice. AGPL-3.0: it runs as its own image, never linked into the binary.
- **Upload-time secret check**, independent of the scanners: a short list of
  high-confidence patterns (AWS `AKIA…`, GitHub `ghp_…`, private-key
  headers, `.env`-family files) applied in `analyzeApp` *before* the source
  reaches the model. `SECURITY.md` already names the analyze path as sending
  unredacted config; this closes part of that and produces a
  `secret-in-source` warning in the plan.
- **`govulncheck` for Go apps** as scanner evidence for the assistant's
  verdict: it knows whether the vulnerable function is reachable.

## Verification

1. Unit: Trivy JSON → findings (fixtures for fixed/unfixed/secret results);
   policy evaluation incl. waivers, expiry and verdicts; `ScanSettings`
   round-trip; `scan_records` insert/latest/retention; `security.state`
   derivation.
2. E2E in `internal/build` (the `os_packages_e2e_test.go` harness already owns
   a disposable buildkitd): build a deliberately old base (`node:16` or
   `debian:bullseye` + an old `lodash`), assert the refusal names the
   packages and nothing was pushed (`crane ls` on the tag is empty); build a
   current `python:3.12-slim` app, assert it passes with unfixed findings
   reported as info only.
3. Restricted-network proof, as in `IDEA_BUILDER.md`: with an egress policy
   on buildkitd allowing only the mirror host, a scan must succeed with
   `dbRepository` set and fail with a clear doctor finding without it.
4. Watch: deploy, then replace the cache mount's database with an older
   snapshot and tick the loop — the app must flip to `needs-action` with
   `newSince` set; waive it in the console and confirm it clears without a
   rescan.
5. Triage: a fixture app with a critical in a package it imports and one in a
   package only present in the image; assert `affected` and `not-affected`
   verdicts respectively against the live model (corpus-style test like
   `internal/ai/corpus_test.go`), and that only the latter waives.

## Risks / open items

- **Build time.** Expect +10–40 s per build after the first (database
  download ~70 MB once per cache). Measure on the kind stack before choosing
  defaults; `enabled: false` stays available but should not be the default
  on a platform whose README promises "private by default".
- **Noise calibration.** `node:20` (non-slim) carries hundreds of findings
  incl. criticals; `*-slim`/distroless bases carry few. The assistant already
  prefers slim bases; `build.instructions` can say so explicitly. The first
  weeks will tell whether `block: critical` + `ignoreUnfixed` is quiet enough
  or needs the phase-2 recipe feedback to be.
- **Trivy inside BuildKit.** The step runs as an unprivileged user in the
  scanner image; `rootfs` mode on a read-only mount needs no capabilities.
  Spike first with a hand-written LLB graph, as `IDEA_BUILDER.md` does for
  buildpacks: database pull through the cache mount, `DOCKER_CONFIG` secret
  mount, report read-back.
- **Rescan cost.** `rootfs` mode walks the filesystem each time (no layer
  cache in that mode); one at a time, yielding to deploys, at PaaS scale
  (tens of apps) this is minutes per day. If it grows, switch rescans to
  `trivy image` by digest with the layer cache on the same mount.
- **The verdict is a reading.** It is stored, shown, and overridable in
  policy; the docs must say so plainly rather than call it "reachability
  analysis".
- **Database freshness as a dependency.** A stale database makes the watch
  silently blind; the doctor check and the System view must show its age,
  and a database older than, say, 7 days should mark every result `unknown`
  rather than `clean`.
