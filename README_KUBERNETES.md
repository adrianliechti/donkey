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
one app or addon. Shared-namespace object names still use `<space>-<name>` to
avoid collisions, but that storage detail never leaks into standard metadata.
`app.kubernetes.io/component` is omitted: donkey does not know whether an
arbitrary app is a frontend, backend or database, and will not invent the
answer. A version label is derived from the image tag and dropped when the tag
cannot be a legal label value.

Donkey roles: `workload` (Deployment, Service, pods), `route` and `path-route`
(the gateway's routing entries), `config` (env and telemetry Secrets),
`registry-auth` (pull secret), `storage` (the data claim), `binding` (a
service connection), `edge` (per-app certificate and DNS objects), `files`
(the short-lived file-browser helper pod). Spaces are `space`, services
`addon`; control-plane roles are `server`, `gateway`, `ai`, `proxy`, `telemetry`,
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

Operational metadata stays in annotations. Routing and membership settings
live in ConfigMap data:

| Annotation | On | Meaning |
|---|---|---|
| `donkey.local/managed` | namespaces | donkey created this namespace and may delete it. A namespace donkey was given never carries it. |
| `donkey.local/image` | app Deployments | the deployed image reference |
| `donkey.local/decisions` | app Deployments | deploy-time analysis decisions (JSON), so `--no-build` redeploys keep them |
| `donkey.local/console` | app Deployments | console choices that survive deploys (until donkey.yaml settles the matter) |
| `donkey.local/config-hash` | control-plane pod templates | rolls pods that read config only at startup |
| `donkey.local/restarted-at` | app pod templates | patched to force a restart |
| `donkey.local/files-expires-at` | file-browser helper pods | when an abandoned helper may be reaped |

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

## Console API reads

The portal refreshes apps and spaces together through `GET /v1/fleet` every
10 seconds. Workload summaries use bulk lists for Deployments, pods, routes,
storage claims and app service bindings, rather than individual reads for
every app. A shared five-second cache coalesces concurrent portal requests;
completed writes invalidate it. External workload changes appear after the
cache expires and the portal next refreshes.

Platform and space permissions are read fresh for every request. The display
cache contains no credentials or membership policies, and access revocation
does not wait for it to expire. Resource sampling also selects only this
installation's app pods when reading the Metrics API.

To check request counts without a running cluster:

```sh
go test ./internal/server -run '^$' -bench '^Benchmark(Portal|ResourceSampling)KubernetesAPI$' -benchtime=1x
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
  the app `api` in space `shop` becomes `shop-api` on the cluster, the way a
  Helm release prefixes its chart. Names are derived and never parsed back —
  identity is always read from the `donkey.local/*` labels — so both halves
  may contain hyphens. Two pairs can spell the same name (`a-b`/`c` and
  `a`/`b-c` both give `a-b-c`); the second is refused when applied, by the
  first's labels, with an error naming who holds it.
- **Siblings are addressed by that name too.** Where an app in a namespace of
  its own reaches a sibling at `http://api`, here it is `http://shop-api`.
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

- A Service name is a DNS label, so `<space>-<app>` must be **63 characters
  or fewer**. donkey refuses the pair at deploy time rather than letting the
  cluster reject it.
- Both namespaces must **already exist**, and their Pod Security Standard has
  to admit the pods that go in them. The install refuses up front rather than
  letting admission fail silently later:
  `donkey-system` runs a **privileged** BuildKit, so it needs
  `pod-security.kubernetes.io/enforce=privileged` (or no enforcement); the
  workload namespace must not be `restricted`, because most real application
  images run as root. donkey sets `baseline` on namespaces it creates itself.

## Space and platform settings

Platform configuration lives in the `donkey` ConfigMap in the control-plane
namespace (`donkey-system` by default). Each entry keeps the format expected by
its consumer: YAML for donkey settings and TOML for BuildKit.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: donkey
  namespace: donkey-system
  labels:
    donkey.local/installation: donkey-system
    donkey.local/role: server
data:
  platform.yaml: |
    admins:
      users:
        - owner@example.com
  route.yaml: |
    host: apps.example.com
    path: /
    backend: http://donkey-server.donkey-system.svc:80
    login: true
    token: true
  buildkitd.toml: |
    # BuildKit configuration generated from the installation settings.
```

The first signed-in user initializes `platform.yaml` unless `platform.admins`
was seeded in the install file. Platform admins manage the saved list through
Account → Platform settings. Saving replaces the list; no revision is required.
BuildKit mounts only `buildkitd.toml` from this ConfigMap.

Every space has a `<space>-space` ConfigMap in its workload namespace, with
settings under `config.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: team-space
  namespace: donkey-team
  labels:
    donkey.local/installation: donkey-system
    donkey.local/space: team
    donkey.local/role: space
data:
  config.yaml: |
    admins:
      users:
        - owner@example.com
    developers:
      groups:
        - engineering
    domains:
      - team.example.com
```

The name is the same in a shared workload namespace: `team-space`. The namespace name
and the labels identify the space; the old `donkey.space.<space>` name is migrated.
Missing records for existing spaces get empty membership and unrestricted domains
at server startup, preserving existing app routes. Saved memberships and domains
survive upgrades and deployments.

Routing configuration uses `route.yaml` in the ConfigMap's `data`. App visitor
access rules use a separate `access.yaml` entry, so changing a route does not
overwrite membership maintained through the console. Labels describe identity
and ownership; routing settings are no longer stored in annotations.

A Secret also named `donkey` holds installation credentials and session keys.
Kubernetes distinguishes the ConfigMap and Secret by resource type.

## ConfigMap and Secret names

`<space>`, `<app>`, and `<service>` always mean logical names entered in
Donkey. For example, space `team`, app `my-api`, and service `postgres`.
All three follow one rule: a DNS label of lowercase letters, digits and
hyphens, 1–40 characters. `donkey`, `platform`, and `system` are reserved as
space names. Object names are built from these by joining with `-`; they are
never split back apart. To find out what an object is, read its labels. The
Docker runtime follows the same rules with `.` as its separator; its
[mapping table](README_DOCKER.md#what-maps-to-what) lists the counterpart of
every row below.

Kubernetes has two workload placements. Choosing a custom system namespace
changes the location of platform resources, not their names. Optional resources
exist only when their feature is configured.

| Resource | Namespace per space | Shared workload namespace | Content |
|---|---|---|---|
| Space ConfigMap | `<space>-space` | `<space>-space` | `config.yaml`: `admins`, `developers`, `domains` |
| App route ConfigMap | `<app>-route` | `<space>-<app>-route` | `route.yaml`, optional `access.yaml` |
| Shared-address route ConfigMap | `<app>-route-<target>-<path-slug>` | `<space>-<app>-route-<target>-<path-slug>` | `route.yaml`, optional `access.yaml` |
| App environment Secret | `<app>-env` | `<space>-<app>-env` | Environment variable names and values |
| App telemetry Secret | `<app>-telemetry` | `<space>-<app>-telemetry` | OpenTelemetry environment variables |
| App registry Secret | `<app>-registry` | `<space>-<app>-registry` | `.dockerconfigjson` |
| Service registry Secret | `<service>-registry` | `<space>-<service>-registry` | `.dockerconfigjson` |
| Space service binding Secret | `<service>-binding` | `<space>-<service>-binding` | The service's own environment variables, such as `DATABASE_URL` |
| App service binding Secret | `<app>-binding-<service>` | `<space>-<app>-binding-<service>` | The app's copy of a service binding |

All rows above live in the workload namespace: normally `donkey-<space>`, or
`kubernetes.workload.namespace` when configured. For `team` / `my-api`, the
environment Secret is `my-api-env` in `donkey-team`, or `team-my-api-env` in
the shared namespace. A service's binding belongs to the service, not to a
pseudo-app: postgres in `team` keeps its credentials in `postgres-binding`,
and an app that connects gets `my-api-binding-postgres`. No app name is
reserved for this.

Platform resources always live in `kubernetes.system.namespace`
(`donkey-system` by default), with the same names in both placements:

| Resource | Name | Content / Secret type |
|---|---|---|
| ConfigMap | `donkey` | `platform.yaml`, `route.yaml`, `buildkitd.toml` |
| Secret | `donkey` | Installation environment values; `Opaque` |
| Secret | `donkey-proxy` | `services.json`: configured HTTP targets and outbound proxies; `Opaque`, reloaded by the shared proxy |
| Secret | `donkey-registry` | `.dockerconfigjson`; `kubernetes.io/dockerconfigjson` |
| Secret | `donkey-gateway-ca` | Built-in CA: `ca.crt`, `ca.key`; `Opaque` |
| Secret | `donkey-gateway-acme` | ACME account, certificates and challenge cache; `Opaque` |
| Secret | `donkey-gateway-tls` | Platform certificate from cert-manager: `tls.crt`, `tls.key`; `kubernetes.io/tls` |
| Secret | `<space>-<app>-tls` | App certificate from cert-manager: `tls.crt`, `tls.key`; `kubernetes.io/tls` |
| Certificate | `donkey-gateway` | Platform certificate request; writes `donkey-gateway-tls` |
| Certificate | `<space>-<app>` | App certificate request; writes `<space>-<app>-tls` |
| DNSEndpoint | `donkey-gateway-dns` | Platform DNS records |
| DNSEndpoint | `<space>-<app>-dns` | App DNS records |

App TLS Secrets always include the space because all certificates share the
platform namespace. The `team` / `my-api` certificate is `team-my-api-tls` in
either placement. ACME and built-in CA modes do not create per-app TLS Secrets.
Registry Secrets use `kubernetes.io/dockerconfigjson`; environment, telemetry
and service credential Secrets use `Opaque`. Environment keys retain their
standard names, such as `DATABASE_URL` and `OTEL_EXPORTER_OTLP_ENDPOINT`.

For space `team` and app `my-api`, the Certificate is `team-my-api`, its Secret
is `team-my-api-tls`, and the DNSEndpoint is `team-my-api-dns`. These names and
their system namespace are the same in both workload placements. Certificate
resources exist only in cert-manager mode; DNSEndpoints require external DNS
publication to be configured.

The installation Secret uses the names consumed by the control-plane pods:
`DONKEY_SESSION_KEY`, `DONKEY_TELEMETRY_KEY`, `DONKEY_REGISTRY_PASSWORD`,
`DONKEY_OIDC_CLIENT_ID`, `DONKEY_OIDC_CLIENT_SECRET`, `OPENAI_API_KEY`,
`DONKEY_PACKAGES_PASSWORD`, `DONKEY_BUILD_ENV`, `DONKEY_BUILDER_INSTRUCTIONS`,
`HTTP_PROXY`, and `HTTPS_PROXY`. `DONKEY_BUILD_ENV` is a JSON environment map
because that environment variable's consumer expects JSON. Configuration files
use YAML; BuildKit's native configuration remains TOML.

The local kind demo also creates Secrets `donkey-registry-ca`,
`package-registry-auth`, and `private-packages-tls` for its registry/package setup.
These are inputs to the installation, not resources generated by Donkey.
Existing Secrets referenced in installation settings keep their operator-chosen
names. `kube-root-ca.crt` is created by Kubernetes.

Upgrades migrate previous ConfigMap/Secret names and JSON settings to the YAML
entries above. Credentials, CA keys, platform admins, space roles, domains and
app visitor access are preserved. Route readers accept old annotations during
the rollout; the installer removes those annotations after the new gateway is
ready. Old credential Secrets remain while any Pod still references them;
subsequent installs retry cleanup.

App Certificates and DNSEndpoints migrate from `donkey-app-<space>.<app>` and
`donkey-app-<space>.<app>-dns` during server startup, installation, or the app's
next deployment. Donkey applies each replacement before removing the old object.
The TLS Secret keeps its name and certificate data; an upgrade from the original
TLS Secret name copies its existing data first. Old Certificates are deleted
with orphan propagation so cert-manager owner references cannot cause the
issued TLS Secret to be garbage-collected. Migration refuses resources owned by
another installation and can be rerun after an interrupted upgrade.

When migrating a shared namespace from the former `space--app` names, workload
Deployments move to `space-app`. Existing PVCs retain their names and data;
later deploys and the file browser continue using them. Legacy Services remain
as DNS aliases so stored connection URLs keep working. Donkey does not rename
spaces automatically: an existing space with a reserved name must be moved to
a valid space name before upgrading.

Space NetworkPolicies are named `<space>-space`, like the settings ConfigMap;
policies still called `donkey.space.<space>` are replaced at server startup.
A space's own service bindings move from the former pseudo-app names
(`shared-binding-<service>`, `<space>-shared-binding-<service>`, and the
older `-addon-` and `-service-` spellings) to `<service>-binding`; app copies
keep `<app>-binding-<service>`. Deployments are repointed first, and an old
Secret stays until no Pod references it.

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
services, postgres and S3 storage included), the platform endpoints an app
actually uses — donkey-ai for the openai addon, donkey-proxy for configured
HTTP offerings and donkey-telemetry for metrics — and DNS. Configured addon plugins can additionally declare specific
destination namespaces/pods/ports or external CIDRs. Donkey persists these
destinations with the provisioning info and maintains separate additive
egress policies for the owning space. Everything else, including undeclared
public internet destinations and other spaces, is cut. See the
[plugin API](pkg/addons/v1/README.md) and
[cross-namespace MariaDB example](examples/addon-mariadb/README.md).

This one is **off by default and deliberate**, unlike the ingress fence:
cutting outbound traffic breaks any app that calls a third-party API,
webhook or email service at runtime, and even calling another donkey app by
its public hostname (that leaves the cluster and comes back through the
edge). Turn it on for spaces whose apps only talk to their own database and
the platform. It needs `isolateSpaces` on, since the egress rules ride the
same per-space policy.

## Incoming traffic, certificates and DNS

Kubernetes has one incoming traffic path:

```text
one load-balancer VIP :80/:443
             |
             v
      donkey-gateway Service
             |
             +-- <domain> and serverHost --> donkey control plane
             +-- exact app hostname ------> app Service
```

`donkey-gateway` is the edge. It terminates TLS, redirects HTTP to HTTPS,
routes by host and path, and enforces login and access rules. The default
`gateway.serviceType: LoadBalancer` exposes it directly on ports 80 and 443.
`NodePort` remains useful for local clusters and `ClusterIP` when another
explicitly managed network path reaches the Service.

Certificate issuance is independent from the traffic path. Choose one source:

| `tls.mode` | Certificate source | Kubernetes objects |
|---|---|---|
| `acme` (default) | Let's Encrypt or `tls.directory` | gateway-managed Secret |
| `ca` | donkey's built-in CA | gateway-managed CA Secret |
| `cert-manager` | a Kubernetes Issuer or ClusterIssuer | one `Certificate` per app plus one for the control plane |

ACME and CA modes are intentionally available on Kubernetes as well as Docker.
They are useful when a cluster has no cert-manager installation, when an
internal ACME service already exists, or for private environments where
trusting donkey's generated CA is the simplest contract.

To use the cluster's cert-manager:

```yaml
tls:
  mode: cert-manager
  issuerName: corporate-pki
  # issuerKind: ClusterIssuer       # default
  # issuerGroup: cert-manager.io    # default
```

The installer verifies `cert-manager.io/v1` `Certificate` support before
changing the cluster. It creates an exact-host Certificate for each public app
in donkey's system namespace. A single SAN Certificate covers `<domain>` and
a distinct `serverHost`. cert-manager writes the referenced TLS Secrets;
`donkey-gateway` reads the Secrets directly and reloads renewed certificates
without a restart. A namespaced `Issuer` must live in the system namespace;
a `ClusterIssuer` is normally the simpler platform contract.

DNS publication is optional and orthogonal. If external-dns runs with its CRD
source, donkey can create exact `externaldns.k8s.io/v1alpha1` `DNSEndpoint`
objects for the same hosts:

```yaml
gateway:
  externalDNS:
    targets: [10.204.166.5]  # stable load-balancer VIP
    # recordType: A          # inferred as A, AAAA, or CNAME
    # ttl: 300
```

The external-dns deployment must include `--source=crd`. Its `--domain-filter`
must cover donkey's domain—for example,
`az.lgt.services` covers both `donkey.az.lgt.services` and
`donkey-test.az.lgt.services`. A compatible Azure private DNS controller might
include:

```text
--source=crd
--domain-filter=az.lgt.services
--provider=azure-private-dns
--policy=sync
--registry=txt
--txt-owner-id=<cluster-owner-id>
```

Provider, policy, registry, and TXT owner ID remain external-dns concerns. The
target is explicit because a DNSEndpoint cannot refer to a Service's eventual
status address. For an Azure private DNS setup this will normally be the
private load-balancer VIP.

With no `gateway.externalDNS` block, donkey creates no DNS CRDs; existing
wildcard/apex records or another external-dns source continue to work. During
install, deploy, network changes, and destroy, donkey reconciles only the
Certificate and DNSEndpoint objects belonging to the affected hosts.

An exposed app's **Overview** in the admin console follows this setup every
ten seconds. It shows cert-manager's Ready/Issuing reason and renewal dates (or
the gateway's ACME/CA readiness), whether external-dns observed the current
DNSEndpoint generation, and the A/AAAA/CNAME answer actually returned to the
control plane. That last check intentionally reflects the cluster—including
private DNS zones—not every recursive resolver on the internet.

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
