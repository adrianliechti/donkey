# Incoming traffic and managed edge design

Status: implemented, including the Project Contour shared-Gateway fallback.
Gateway/Certificate readiness and DNS diagnostics remain future work.

## One application boundary, several TLS owners

Every request still passes through `donkey-gateway`. External infrastructure
may terminate TLS, but it does not replace donkey's hostname/path routing,
OIDC flow, access rules, identity-header sanitization, or the sealed token used
for `donkey-server`.

The configuration names the resource contract instead of using a generic
`mode` string:

| Configuration | Public entry | Objects owned by donkey | Host coverage | TLS owner |
|---|---|---|---|---|
| no edge block | `donkey-gateway` Service | no external-edge CRDs | portal and apps | donkey ACME/CA |
| `gateway.httpRoute` | existing Gateway | one aggregate `HTTPRoute` | portal and all domain apps | platform listeners |
| `gateway.appGateway` | GatewayClass | `Certificate`, `Gateway`, `HTTPRoute` per app | apps | cert-manager + app Gateway |
| `gateway.contourHTTPProxy` | one existing Contour Gateway/Envoy | root `HTTPProxy` per host, app Certificates, one system Certificate | portal and apps | cert-manager + HTTPProxy |

This keeps the meaning visible in YAML and avoids pretending that a
vendor-specific resource is portable Gateway API. The blocks also hold only
the settings that their resource needs.

Compatibility rules are enforced before cluster access:

- Every external-TLS block requires `tls.mode: none`; otherwise the plain
  upstream hop redirects back to the same public HTTPS URL forever.
- `appGateway` and `contourHTTPProxy` are alternative per-host strategies.
- `httpRoute` and `contourHTTPProxy` both expose the complete installation and
  cannot be combined.
- `httpRoute` and `appGateway` may be combined: the first can own the exact
  portal/OAuth callback host while the second owns application hosts.
- Docker rejects all Kubernetes edge blocks.

`gateway.serviceType: ClusterIP` is recommended for all external modes. It is
not mandatory because some installations intentionally keep a second private
entry to the service.

## Why not ListenerSet on Project Contour?

Gateway API `ListenerSet` is the portable resource intended for this ownership
split: a tenant attaches hostname/TLS listeners to a platform-owned Gateway.
It became Standard-channel GA in Gateway API v1.5. Project Contour's current
main line still consumes Gateway API v1.3 and does not advertise ListenerSet
support, so emitting one would create an unhandled resource.

A certificate cannot move to an `HTTPRoute` as a workaround. HTTPRoute begins
after TLS is terminated; its API has no downstream certificate field. In
portable Gateway API, the certificate belongs to a listener on a `Gateway` or
`ListenerSet`.

Contour already has a supported native equivalent. A Gateway listener using
`protocol: projectcontour.io/https` carries no TLS details; root HTTPProxy
objects define `spec.virtualhost.fqdn` and
`spec.virtualhost.tls.secretName`. This adds SNI virtual hosts to one Envoy
data plane without adding Gateways or load balancers.

## Contour shared-Gateway contract

The platform owns a single Gateway using Contour's documented hybrid listener
shape:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: main
  namespace: gateway-infra
spec:
  gatewayClassName: contour
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: All
    - name: https
      protocol: projectcontour.io/https
      port: 443
      allowedRoutes:
        namespaces:
          from: All
```

`from: All` is part of Contour's documented Gateway/HTTPProxy hybrid contract.
Contain who can claim root virtual hosts through Contour's restricted root
namespaces and Kubernetes RBAC. The allowed list must include `donkey-system`
(or the chosen system namespace), and that Contour instance must accept a
distinct ingress class for HTTPProxy.

This is a real delegation of virtual-host authority. The broad custom HTTPS
listener does not constrain HTTPProxy hostnames, so a platform that needs a
hard domain boundary should pair the namespace restriction with cert-manager
issuer policy and admission validation of `spec.virtualhost.fqdn`.

Donkey selects it as follows:

```yaml
gateway:
  serviceType: ClusterIP
  contourHTTPProxy:
    ingressClassName: contour-main
    issuerName: corporate-pki
    # issuerKind: ClusterIssuer
    # issuerGroup: cert-manager.io
tls:
  mode: none
```

Per exposed app, donkey creates in its system namespace:

```text
Certificate -> generated kubernetes.io/tls Secret
                         |
root HTTPProxy (exact fqdn + secretName)
                         |
                         +-> donkey-gateway Service :80
```

The control plane is not an afterthought in this mode. Install creates one SAN
Certificate and a root HTTPProxy for the base `domain`, where the central
`/oauth2/callback` lives. When `serverHost` differs, a second HTTPProxy uses the
same Certificate and Secret. Internal apps have no hostname and create no
external edge.

The HTTPProxy and Service share a namespace, as do the HTTPProxy and TLS
Secret. No Contour TLS delegation or cross-namespace backend permission is
needed. A namespaced cert-manager `Issuer` must therefore exist in the system
namespace; a `ClusterIssuer` is the usual platform contract.

## Other platform contracts

### Aggregate HTTPRoute

```yaml
gateway:
  serviceType: ClusterIP
  httpRoute:
    name: shared-gateway
    namespace: gateway-infra
    sectionName: donkey
tls:
  mode: none
```

Donkey creates one HTTPRoute for `<domain>` and `*.<domain>`. The platform owns
listeners, Certificates, Secrets, DNS, and route admission. A route wildcard
can match app names at several depths, but an X.509 wildcard covers only one
label; the listener certificate plan must account for that difference.

### Dedicated app Gateways

```yaml
gateway:
  serviceType: ClusterIP
  httpRoute:                         # portal and OAuth callback
    name: donkey-control-plane
    namespace: gateway-infra
    sectionName: donkey-apex
  appGateway:
    className: platform
    nameTemplate: "$namespace-$hash"
    routeNamespace: projectcontour   # only for a mirroring platform
    issuerName: platform-ca
tls:
  mode: none
```

The per-app Certificate, source Gateway, TLS Secret, and HTTPRoute live in
donkey's system namespace. `routeNamespace` is for platforms that mirror the
Gateway into a controller namespace; that platform must also mirror/rewrite
the Secret reference and retain route admission from donkey's namespace.
Donkey receives no write permission in that infrastructure namespace.

`appGateway` currently covers applications only, so an installation needs the
aggregate HTTPRoute (or another platform-owned exact-apex route) for the portal
and central OAuth callback.

## Lifecycle and authority

- Install preflight checks the exact optional APIs a selected contract needs:
  Gateway/HTTPRoute, Certificate, or HTTPProxy/Certificate.
- Managed edge objects and generated TLS Secrets carry the installation,
  logical space/app, and edge-role labels used for guarded cleanup.
- Write permissions for Gateway, HTTPRoute, HTTPProxy, and Certificate stay in
  the system namespace in the restricted two-namespace topology.
- Deploy and network updates apply the active edge first, then remove only the
  inactive strategy. The common Certificate/Secret survives a switch.
- Making an app internal or destroying it retracts every edge kind and its
  generated Secret. Removing Contour mode retracts its system edge.
- Re-installing with a different mode reconciles every already-deployed app
  from its route ConfigMap: the replacement is applied before the inactive
  strategy is removed. Deploy and network updates keep reconciling one app at
  a time afterward.
- This is event-driven reconciliation, not a second always-running operator.
  cert-manager continuously owns Certificate/Secret issuance and renewal, and
  Contour continuously consumes HTTPProxy/Secret changes. Deleting a donkey-
  owned top-level object is repaired by the next install, deploy, or network
  update rather than by a polling loop.

## Common request paths

```text
Native:
browser -> donkey-gateway:443 -> OIDC/access/routing -> portal or app

Aggregate Gateway API:
browser -> platform Gateway:443 -> HTTPRoute -> donkey-gateway:80
        -> OIDC/access/routing -> portal or app

Dedicated app Gateway:
browser -> app Gateway:443 -> app HTTPRoute -> donkey-gateway:80
        -> OIDC/access/routing -> app

Shared Contour:
browser -> main Envoy:443 -> root HTTPProxy -> donkey-gateway:80
        -> OIDC/access/routing -> portal or app
```

The internal hop is deliberately plain HTTP after platform TLS termination.
The external proxy must preserve the original Host header; donkey-gateway uses
it as the routing and login boundary.

## Remaining production diagnostics

Object creation is supported, but deploy and `donkey doctor` do not yet read:

- cert-manager `Certificate` Ready status;
- Gateway Accepted/Programmed conditions;
- HTTPRoute Accepted/ResolvedRefs conditions;
- HTTPProxy validity status;
- Gateway address or DNS readiness.

A rejected certificate or route can therefore coexist with a healthy internal
donkey-gateway. Status checks should be the next managed-edge increment.

References:

- <https://gateway-api.sigs.k8s.io/reference/api-types/listenerset/>
- <https://gateway-api.sigs.k8s.io/guides/user-guides/tls/>
- <https://projectcontour.io/docs/main/config/gateway-api/>
- <https://projectcontour.io/docs/main/config/tls-termination/>
- <https://cert-manager.io/docs/usage/certificate/>
