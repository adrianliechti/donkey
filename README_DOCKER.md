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
| PersistentVolumeClaim | named volume `donkey.<space>.<name>.data` |
| route ConfigMaps | route records in the state volume, read by the gateway |
| `donkey.space.<space>` settings ConfigMap | `/var/lib/donkey/spaces/<space>/config.json` |
| `donkey.platform` settings ConfigMap | `/var/lib/donkey/platform/config.json` |
| control plane in `donkey-system` | containers `donkey-server`, `donkey-gateway`, `donkey-ai`, `donkey-telemetry`, `donkey-packages`, `donkey-buildkitd` (and optionally `donkey-registry`) on the `donkey` network |

The dots in `donkey.<space>.<app>` keep app containers out of the control
plane's `donkey-<name>` namespace. Route, binding and console state that
Kubernetes keeps in objects lives in meta files on the shared state volume
(`donkey-data`). Space and platform settings use the same JSON as Kubernetes;
RBAC is always active. The first signed-in user becomes a platform admin and
can add others in Account → Platform settings. An optional install-file
`platform.admins` seeds the initial list. Server startup creates missing records
for existing spaces with empty membership and unrestricted domains, preserving
existing app routes. Saved roles and domains survive restarts and upgrades.

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
`telemetry-storage` and `registry-storage`.

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
