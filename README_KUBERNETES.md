# donkey on Kubernetes

How the Kubernetes runtime marks what it creates, and the installation
modes that change an installation's shape. Everything runtime-independent —
the concept, `donkey.yaml`, services, the builder — lives in the main
[README](README.md); the Docker runtime has [its own page](README_DOCKER.md).

## Labels, annotations and selectors

Every object donkey applies carries two vocabularies. `donkey.local/*` is
donkey's own identity — what sweeps, ownership checks and selectors are built
from. `app.kubernetes.io/*` is Kubernetes'
[recommended common label set](https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/),
present on the platform's own workloads and on every deployed app alike, so
donkey's objects are legible to k9s, Lens, Backstage and every other tool
that never heard of donkey.

### Identity: `donkey.local/*`

| Label | On | Meaning |
|---|---|---|
| `donkey.local/installation` | every object | The ownership boundary: the control plane's namespace, stamped at the apply/render boundary. Every destructive sweep requires it — `managed-by=donkey` says "a donkey wrote this", this label says "this one did". |
| `donkey.local/space` | everything space-scoped | The lifecycle boundary: removing the space removes everything carrying it. |
| `donkey.local/app` | an app's objects | The logical app. Never qualified, even where object names are. |
| `donkey.local/addon` | a space service's objects | The logical service. An app's copy of a service binding deliberately carries **both** `app` and `addon`, because it represents that relationship. |
| `donkey.local/role` | every object | Donkey's immutable internal role (`workload`, `route`, `binding`, `storage`, …). This is the role selectors and ownership checks use; it is not Kubernetes API Kind or architectural component. |

### Description: `app.kubernetes.io/*`

| Label | App objects | Space service | Space | Control plane |
|---|---|---|---|---|
| `name` | app | addon | space | workload (`donkey-server`, …) |
| `instance` | space | space | space | the installation (system namespace) |
| `version` | image tag | — | — | control-plane image tag (workloads) |
| `part-of` | space | space | space | `donkey` |
| `managed-by` | `donkey` | `donkey` | `donkey` | `donkey` |

The space is the release and lifecycle instance; `name + instance` identifies
one app or addon. Shared-namespace object names still use `<space>--<name>` to
avoid collisions, but that storage detail never leaks into standard metadata.
`app.kubernetes.io/component` is omitted: donkey does not know whether an
arbitrary app is a frontend, backend or database, and will not invent the
answer. A version label is derived from the image tag and dropped when the tag
cannot be a legal label value.

Donkey roles: `workload` (Deployment, Service, pods), `route` and `path-route`
(the gateway's routing entries), `config` (env and telemetry Secrets),
`registry-auth` (pull secret), `storage` (the data claim), `binding` (a
service connection), `edge` (optional per-app managed edge objects), `files`
(the short-lived file-browser helper pod). Spaces are `space`, services
`addon`; control-plane roles are `server`, `gateway`, `ai`, `telemetry`,
`packages` and `buildkitd`, aligned with Docker while their standard `name`
keeps the actual `donkey-*` workload name.

### Selectors

Selectors are immutable once created, so they carry the minimum stable
identity and nothing descriptive:

- an app's Deployment and Service select `space + app + role=workload`;
- a service's select `space + addon + role=addon`;
- a control-plane workload selects its Donkey role.

The installation is added to each selector at the apply/render boundary. No
`app.kubernetes.io/*` label—including `managed-by`—ever enters a selector or
ownership comparison; those labels remain freely changeable metadata.

The apply/render boundary stamps `donkey.local/installation` into every
object, every selector and every pod template — offline-rendered manifests
get the identical stamp, so the two paths cannot drift. The descriptive
`instance` label is filled in at the same boundary where the labels name no
instance of their own (that is: on control-plane objects, whose instance the
installation is) but never enters a selector.

### Annotations

Non-identifying or potentially large configuration stays in annotations:

| Annotation | On | Meaning |
|---|---|---|
| `donkey.local/managed` | namespaces | donkey created this namespace and may delete it. A namespace donkey was given never carries it. |
| `donkey.local/image` | app Deployments | the deployed image reference |
| `donkey.local/decisions` | app Deployments | deploy-time analysis decisions (JSON), so `--no-build` redeploys keep them |
| `donkey.local/console` | app Deployments | console choices that survive deploys (until donkey.yaml settles the matter) |
| `donkey.local/config-hash` | control-plane pod templates | rolls pods that read config only at startup |
| `donkey.local/restarted-at` | app pod templates | patched to force a restart |
| `donkey.local/files-expires-at` | file-browser helper pods | when an abandoned helper may be reaped |
| `donkey.local/backend`, `host`, `path`, `login`, `access`, `token` | route ConfigMaps | one entry of the gateway's routing table: where to forward, on which hostname and path, whether visitors sign in, who may pass |

### Finding things

```sh
# one installation's objects, cluster-wide
kubectl get all -A -l donkey.local/installation=donkey-system

# everything of one space
kubectl get all -A -l donkey.local/space=acme

# one app instance, using only standard metadata
kubectl get all -A -l app.kubernetes.io/instance=acme,app.kubernetes.io/name=shop

# the platform itself
kubectl get all -A -l app.kubernetes.io/part-of=donkey
```

## Namespaces and restricted clusters

By default every space gets a namespace of its own (`donkey-<space>`), created
and deleted by donkey. That is the right shape on a cluster you own, and it is
what makes an app's Service reachable as plain `api` from its siblings.

Plenty of clusters don't work that way. Namespaces are handed out by a
platform team, through a CRD or a GitOps pipeline, and `cluster-admin` is not
something a review will approve. For those, name the namespaces donkey should
use:

```yaml
kubernetes:
  system:
    namespace: bank-donkey-system   # where the control plane lives
  workload:
    namespace: bank-donkey-apps     # where every app of every space lives
```

One rule covers both fields: **name a namespace and it is yours.** donkey
expects it to exist, applies its objects into it, and never creates, deletes
or relabels it. Leave a field empty and donkey makes and manages that
namespace itself.

Setting `workload.namespace` changes three things:

- **Every app lives in one namespace**, so object names carry their space:
  the app `api` in space `shop` becomes `shop--api` on the cluster. The
  separator is `--`, and neither a space nor an app name may contain it.
- **Siblings are addressed by that name too.** Where an app in a namespace of
  its own reaches a sibling at `http://api`, here it is `http://shop--api`.
  Addon bindings (`DATABASE_URL` and friends) are generated, so they follow
  automatically — only a hostname you hardcoded needs changing.
- **donkey needs nothing above a namespace.** No ClusterRole, no
  ClusterRoleBinding, no namespace permissions: namespaced Roles in the two
  namespaces — the server's, spelled out in `serverRoleRules`, and the
  gateway's read-only route access. This is the only arrangement that runs
  with no cluster-scoped grant at all.

(Without a workload namespace, donkey-server binds `cluster-admin`, because
it creates and deletes namespaces and no Role can express that.)

Two limits to check before you commit to a name:

- A Service name is a DNS label, so `<space>--<app>` must be **63 characters
  or fewer**. donkey refuses the pair at deploy time rather than letting the
  cluster reject it.
- Both namespaces must **already exist**, and their Pod Security Standard has
  to admit the pods that go in them. The install refuses up front rather than
  letting admission fail silently later:
  `donkey-system` runs a **privileged** BuildKit, so it needs
  `pod-security.kubernetes.io/enforce=privileged` (or no enforcement); the
  workload namespace must not be `restricted`, because most real application
  images run as root. donkey sets `baseline` on namespaces it creates itself.

## Isolating spaces from each other

Spaces are a network boundary, not just an organizational one: each space
gets one ingress NetworkPolicy, and its pods accept traffic only from their
own space, the donkey gateway and donkey-server. Cross-space calls keep
working the sanctioned way — through the gateway on an app's public hostname,
where login and access rules apply — they just don't work as anonymous
pod-to-pod traffic. Neither does anything else the cluster runs against those
pods (a cluster-wide Prometheus scraping them, say). To open the network
between spaces instead:

```yaml
kubernetes:
  isolateSpaces: false
```

The fence selects only pods carrying this installation's labels
(`donkey.local/installation` + `donkey.local/space`), so in a workload
namespace you were given it never constrains pods donkey doesn't own.
Toggling the setting takes effect on the next `donkey server install` for
existing spaces, and immediately for new ones. One prerequisite: the
cluster's network plugin must enforce NetworkPolicy — on one that doesn't,
the policies apply cleanly and change nothing (which also means the default
is safe on such clusters, just not protective).

### Restricting egress

By default only *ingress* is fenced; a space's pods may still reach out
anywhere, the internet included. To close that side too:

```yaml
kubernetes:
  restrictEgress: true
```

Now a space's pods reach only their own space (sibling apps and its
services, postgres and rustfs included), the platform endpoints an app
actually uses — donkey-ai for the openai addon and donkey-telemetry for
metrics — and DNS. Everything else, the public internet and every other
space, is cut.

This one is **off by default and deliberate**, unlike the ingress fence:
cutting outbound traffic breaks any app that calls a third-party API,
webhook or email service at runtime, and even calling another donkey app by
its public hostname (that leaves the cluster and comes back through the
edge). Turn it on for spaces whose apps only talk to their own database and
the platform. It needs `isolateSpaces` on, since the egress rules ride the
same per-space policy.

## Using the cluster's edge

donkey needs no Gateway API — but a cluster that standardizes on one can put
its edge in front of donkey with one setting, checked at install: naming a
gateway on a cluster that doesn't serve HTTPRoute is refused up front
instead of failing mid-apply.

**Enter through the cluster's Gateway API gateway** instead of exposing
donkey's edge Service:

```yaml
gateway:
  serviceType: ClusterIP
  httpRoute:
    name: shared-gateway      # the Gateway to attach to
    namespace: infra
tls:
  mode: none                  # that gateway terminates TLS before routes match
```

donkey renders one HTTPRoute sending `<domain>` and `*.<domain>` (a Gateway
API wildcard matches any depth) to the donkey gateway, which keeps doing what
it always does — routing, login, access — one hop further in. Custom app
hostnames outside the domain are not covered by this route.

TLS in this arrangement is the platform's prerequisite, not donkey's job:
the cluster gateway's listeners terminate it with certificates provisioned
however the platform likes — cert-manager, a PKI process, a ReferenceGrant
when the certificate Secrets live outside the gateway's namespace. Remember
that a TLS certificate wildcard covers one label while apps live at
`<app>.<space>.<domain>`, so the gateway wants a listener per
`*.<space>.<domain>`.

Managed platforms can also let donkey request a dedicated edge and
certificate for every exposed app:

```yaml
gateway:
  serviceType: ClusterIP
  appGateway:
    className: private-app-edge
    issuerName: corporate-pki
    # issuerKind: ClusterIssuer       # default
    # issuerGroup: cert-manager.io    # default
    # routeNamespace: gateway-infra   # only when the platform mirrors Gateways
```

This renders a cert-manager `Certificate`, Gateway API `Gateway`, and
`HTTPRoute` per hostname. All three tenant-owned objects live in donkey's
system namespace, where the route can reach the `donkey-gateway` Service
without a cross-namespace backend grant. In the confined two-namespace setup,
the server Role can manage those three resource kinds only in the system
namespace; it receives no permission in `gateway-infra`.

Leave `routeNamespace` empty when the configured GatewayClass watches tenant
Gateways directly. Set it only for a platform that mirrors a tenant Gateway
into its controller namespace. That platform must also make the generated TLS
Secret available to the mirrored Gateway (by mirroring it, or by rewriting the
certificate reference and arranging the appropriate `ReferenceGrant`). The
mirrored listener must allow routes from donkey's system namespace. donkey
puts that namespace selector on the source Gateway, but whether it is preserved
is part of the platform's mirroring contract. Installation checks for Gateway,
HTTPRoute and Certificate CRDs before changing the cluster.

## Several donkeys on one cluster

Supported, with one requirement: **every installation needs its own
`kubernetes.system.namespace`.** That namespace is the installation's
identity — it is stamped on every object as `donkey.local/installation`, every sweep
donkey makes requires it, and every cluster-scoped object it applies is named
after it. Without a distinct one the second install would apply the first's
`ClusterRoleBinding`, and because server-side apply forces the subject list,
the first installation would silently lose every permission it has.

`kubectl get all -A -l donkey.local/installation=bank-donkey-system` shows one
installation's objects.

The rest is not enforceable from inside a single installation — donkey can
only see its own configuration — so these are yours to get right:

| Must differ per installation | Why |
|---|---|
| `kubernetes.system.namespace` | the installation's identity — every object is stamped and swept by it; the install warns when it finds no existing installation there |
| `domain` | two gateways cannot divide one wildcard, and DNS resolves to one of them |
| `kubernetes.workload.namespace`, or `kubernetes.workload.namespacePrefix` | otherwise both installations write into the same namespaces |
| `gateway.httpNodePort` / `httpsNodePort` | node ports are cluster-wide |
| `registry.url` | see below |

The registry one is the subtlest. An image is pushed to
`<registry.url>/<app>:<tag>`, and the repository path carries neither the
space nor the installation. Two installations sharing a `registry.url`
therefore share the repository for every app of the same name — and
`--no-build --tag` only checks that an image sits under the configured
registry prefix, which is what stops one app from being deployed with
another's image. Sharing the prefix defeats that check across installations.
Give each one its own.
