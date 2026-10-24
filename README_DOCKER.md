# donkey on Docker

`runtime: docker` runs the same control plane and the same apps straight on a
Docker engine — no cluster. The API, console, CLI and e2e suite are
identical; only the runtime underneath changes. Everything
runtime-independent lives in the main [README](README.md); the Kubernetes
runtime has [its own page](README_KUBERNETES.md). A complete walkthrough is
[examples/INSTALL_DOCKER.md](examples/INSTALL_DOCKER.md).

## What maps to what

| Kubernetes | Docker |
|---|---|
| namespace `donkey-<space>` | network `donkey-<space>` — the space's apps share it and resolve each other by name |
| app Deployment + Service | container `donkey.<space>.<app>` on the space's network |
| PersistentVolumeClaim `<app>-data` | named volume `donkey.<space>.<name>.data` |
| space service Deployment + Service | container `donkey.<space>.<service>` on the space's network |
| app route ConfigMap `<app>-route` | `/var/lib/donkey/routes/<space>/<app>.json`, read by the gateway |
| shared-address route ConfigMap `<app>-route-<target>-<slug>` | `/var/lib/donkey/routes/<space>/<app>.on.<target>.<slug>.json` |
| space service binding Secret `<service>-binding` | `/var/lib/donkey/bindings/<space>/<service>.json` |
| app service binding Secret `<app>-binding-<service>` | `/var/lib/donkey/bindings/<space>/<app>.<service>.json` |
| app environment Secret `<app>-env` | `/var/lib/donkey/env/<space>/<app>.json` |
| `<space>-space` ConfigMap / `config.yaml` | `/var/lib/donkey/spaces/<space>/config.json` |
| `donkey` ConfigMap / `platform.yaml` | `/var/lib/donkey/platform/config.json` |
| control plane in `donkey-system` | containers `donkey-server`, `donkey-gateway`, `donkey-ai`, `donkey-proxy`, `donkey-telemetry`, `donkey-packages`, `donkey-buildkitd` (and optionally `donkey-registry`) on the `donkey` network |
| `donkey-proxy` Secret / `services.json` | `/var/lib/donkey/proxy/services.json`; rendered Compose mounts private `proxy-services.json` instead |

The two runtimes share the rules, not the spelling. On both, identity is
read from labels and never from a name; object names are derived from the
logical space, app and service names and never split back apart; and no app
name is reserved for platform use. What differs is the separator, and that
is deliberate. Kubernetes joins with `-` because a Service name is a DNS
label and cannot hold a dot. Docker joins with `.` because a container name
can, and donkey names never contain one — so `donkey.my-team.web` is
unambiguous however many hyphens either half has, and the dots keep app
containers out of the control plane's `donkey-<name>` space. The same holds
for the state files: `web.postgres.json` is app `web`'s copy of the
`postgres` binding, `postgres.json` the space's own. Docker volumes cannot be
renamed, so these names are also the ones that stay.

Route, binding and console state that Kubernetes keeps in objects lives in
files on the shared state volume (`donkey-data`). Space and platform settings
use the same fields as Kubernetes, stored as JSON rather than embedded YAML. RBAC is always active. The first signed-in user becomes a platform admin and
can add others in Account → Platform settings. An optional install-file
`platform.admins` seeds the initial list. Server startup creates missing records
for existing spaces with empty membership and unrestricted domains, preserving
existing app routes. Saved roles and domains survive restarts and upgrades.

Space, app and service names follow the same DNS-label rule as on
Kubernetes (lowercase letters, digits and hyphens, up to 40 characters);
`donkey`, `platform`, and `system` are reserved as space names. Docker
container and volume names keep their dot-separated layout, and network
aliases let apps reach siblings by their logical names, such as `my-api`.

Docker has one placement mode: one network per space. It does not create
Kubernetes ConfigMaps or Secrets. State paths below are relative to
`/var/lib/donkey` in the persistent `donkey-data` volume:

| Data | Docker storage |
|---|---|
| Platform admins | `platform/config.json` |
| Space admins, developers, domains | `spaces/<space>/config.json` |
| App environment | `env/<space>/<app>.json` |
| Gateway routes and app visitor access | `routes/<space>/<app>.json`; shared-address routes as `routes/<space>/<app>.on.<target>.<slug>.json` |
| Service credentials | `bindings/<space>/<service>.json` for the space's own binding, `bindings/<space>/<app>.<service>.json` for an app's copy (older `shared.<service>.json` files are moved at startup) |
| App metadata | `meta/<space>/<app>.json` |
| Saved deployment configuration | `deploy/<space>/<app>.json` |
| Console route | `routes/system/donkey-server.json` |
| Built-in CA | `ca/ca.crt`, `ca/ca.key` |
| ACME certificates and account | `certs/` |

Installation credentials are passed to control-plane containers as environment
variables. Registry authentication is passed to the Docker API when pulling
images; app telemetry values are injected into the app container environment.
The rendered Compose bundle provides a `secrets.env` input and mounts
`buildkitd.toml`. These files and Docker volumes are distinct from Kubernetes
Secret and ConfigMap objects.

## Labels

Docker labels use reverse-DNS keys, so donkey's are `local.donkey.*` — the
same identity dimensions as the Kubernetes runtime's `donkey.local/*`,
spelled the way Docker tooling expects:

| Label | Meaning |
|---|---|
| `local.donkey.managed` | `"true"` — the coarse discovery marker |
| `local.donkey.installation` | ownership boundary (always `donkey` — one installation per engine) |
| `local.donkey.space` | the lifecycle boundary |
| `local.donkey.app` | the logical app |
| `local.donkey.addon` | the logical space service |
| `local.donkey.role` | the resource's immutable Donkey role |
| `local.donkey.image`, `local.donkey.port` | descriptive: the deployed image reference and app port |

App roles are `workload`, `addon`, `storage`, `space` (the network), `files`
and `init`. Control-plane containers follow one naming rule: the container
`donkey-<role>` carries that role (`server`, `gateway`, `ai`,
`telemetry`, `packages`, `buildkitd`, `registry`); the infrastructure network
is `network`, and the volumes are `state-storage`, `buildkit-storage`,
`telemetry-storage` and `registry-storage`. The state volume also holds
donkey-server's data: every app's source history, in its `source/` folder.

Every list and delete donkey performs filters on `managed` +
`installation` (+ the exact scope), and destructive operations verify the
full immutable identity first — a same-named container someone else created
is refused, never adopted:

```sh
# everything of one space
docker ps --filter label=local.donkey.space=acme

# every donkey-managed resource
docker ps --filter label=local.donkey.managed=true
docker volume ls --filter label=local.donkey.managed=true
```

## Isolation model

Spaces are separate Docker networks, so isolation is structural rather than
policy-based: an app container joins only its own space's network and
reaches exactly its siblings and the space's services. Donkey's own
`donkey-server`, `donkey-gateway`, `donkey-ai` and `donkey-telemetry`
containers attach to every space network (routing, tunnels, the AI endpoint
and OTLP ingestion need to reach the apps); the build engine, package proxy
and registry stay on the infrastructure network alone.

The Kubernetes modes do not apply here: `kubernetes.isolateSpaces` needs no
equivalent (the network boundary is the default and cannot be turned off),
and there is **no equivalent of `kubernetes.restrictEgress`** — outbound
traffic from app containers is unrestricted.

## The Compose bundle

For hosts where the CLI must not drive the Docker engine directly, render
the installation instead of installing it:

```sh
donkey server render -f server-install.yaml --output deploy/
```

The bundle contains `compose.yaml`, `buildkitd.toml`, `up.sh` and a
`secrets.env.example` naming the required secrets — no secret values are
ever written. `up.sh` sources `secrets.env`, recovers secrets a running
installation already holds (so a re-run without the file keeps the session
key), and refuses to touch any container, network or volume whose identity
labels are not this installation's — the same ownership check the direct
installer makes. Re-running the bundle is the upgrade path: same names,
fresh configuration, preserved volumes.

The direct installer (`donkey server install` with `runtime: docker`) and
the bundle create identically named and labelled resources, so either can
take over from the other.
