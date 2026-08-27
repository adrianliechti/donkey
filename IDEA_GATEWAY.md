# Incoming traffic and managed Gateway design

Status: reviewed design; the dynamic per-app edge exists, but the production
mode still needs the control-plane edge, configuration invariants, and status
diagnostics described below.

## Goal

Keep one application-level security boundary across every runtime while
supporting two very different operational environments:

- Small standalone Docker installations use donkey's own edge and ACME.
- Managed Kubernetes installations use platform Gateway API infrastructure
  and cert-manager, with one Gateway and exact certificate per exposed app.

In every mode, traffic passes through `donkey-gateway`. It remains responsible
for hostname/path routing, OIDC, access rules, identity-header sanitization,
and minting a short-lived sealed token for `donkey-server` only.

## Incoming traffic modes

| Mode | Public entry | TLS owner | Coverage | Intended use |
|---|---|---|---|---|
| Docker + ACME | Host ports 80/443 | donkey/ACME | Portal and apps | Small standalone public server |
| Docker + CA | Host ports 80/443 | donkey's private CA | Portal and apps | Internal or local development |
| Kubernetes native | LoadBalancer or NodePort | donkey ACME/CA | Portal and apps | Self-managed cluster |
| Shared platform Gateway | Existing Gateway plus one HTTPRoute | Platform | Portal and all domain apps | Centrally managed cluster edge |
| Dynamic per-app Gateway | One Gateway, Certificate and HTTPRoute per app | cert-manager/platform | Apps only currently | Managed production environment |

Docker ACME requires public DNS and inbound ports 80 and 443. For a laptop or
an environment ACME cannot reach, `tls.mode: ca` is the appropriate local mode.

## Common request path

The Kubernetes modes differ only in how a request reaches donkey's gateway:

```text
Native:
browser -> donkey-gateway:443 -> OIDC/access/routing -> app or portal

Shared platform:
browser -> platform Gateway:443
        -> HTTPRoute -> donkey-gateway:80
        -> OIDC/access/routing -> app or portal

Dynamic per-app:
browser -> per-app Gateway:443
        -> per-app HTTPRoute -> donkey-gateway:80
        -> OIDC/access/routing -> app
```

The hop from a platform Gateway to `donkey-gateway` is deliberately plain
HTTP. TLS is already terminated, and `donkey-gateway` remains the
application-level security boundary. It removes forged identity and access
token headers, preserves the visitor's host, and forwards a sealed token only
on the control-plane route. See `internal/gateway/proxy.go`.

## Production review

### 1. The dynamic mode does not expose the portal or central callback

Dynamic edge objects are generated only while rendering an application in
`pkg/runtime/kubernetes/manifests/appedge.go`. The portal remains an internal
donkey route ConfigMap rendered by
`pkg/runtime/kubernetes/manifests/system.go`.

An `appGateway`-only installation therefore has:

- Per-app Gateways and certificates.
- No external Gateway or certificate for `https://<domain>`.
- No external path to `/oauth2/callback` on the base domain.

The base-domain callback is required even when login starts from
`<app>.<space>.<domain>`. Until the dynamic mode provisions its own system
edge, production should combine an exact-apex shared Gateway attachment with
the dynamic per-app edge:

```yaml
domain: apps.bank.example
# Keep serverHost unset: portal and callback then share the exact apex.

gateway:
  serviceType: ClusterIP

  # The platform-owned, exact-host listener for portal/API/OIDC callback.
  httpRoute:
    name: donkey-control-plane
    namespace: infra
    sectionName: donkey-apex

  # A dedicated edge and exact certificate for every exposed app.
  appGateway:
    className: lgt-lb-standard
    routeNamespace: projectcontour
    issuerName: lgt
    # issuerKind: ClusterIssuer
    # issuerGroup: cert-manager.io

tls:
  mode: none
```

The `donkey-apex` listener must use the exact hostname
`apps.bank.example`. Donkey's shared HTTPRoute also carries
`*.apps.bank.example`, but Gateway hostname intersection limits the effective
attachment to the exact listener hostname. The per-app Gateways then own the
application hostnames.

Gateway API route wildcards match one or more DNS labels, unlike TLS
certificate wildcards, which cover only one label. This is why a static route
can describe all app hostnames while the production certificates must be
exact per-app certificates (or listener/certificate sets per space).

Longer term, `appGateway` should also render a control-plane Certificate,
Gateway and HTTPRoute for the base domain and, when distinct, `serverHost`.

### 2. `tls.mode: none` is mandatory but is not enforced

Both platform Gateway modes use the plain HTTP port of the
`donkey-gateway` Service. If donkey remains in `acme` or `ca` mode, that
listener redirects to the same public HTTPS URL:

```text
platform HTTPS -> donkey HTTP -> redirect to HTTPS
                              <- repeated request
```

The installer currently validates each field independently. It should enforce
these cross-field rules:

- `gateway.httpRoute` or `gateway.appGateway` requires `tls.mode: none`.
- A platform Gateway mode should normally require
  `gateway.serviceType: ClusterIP` so no second public edge is exposed.
- `appGateway` without a control-plane edge should be rejected until it can
  provision one itself.
- Docker should reject Kubernetes-only Gateway settings instead of silently
  ignoring them.

Until those rules exist, production configuration must explicitly set
`tls.mode: none` and `gateway.serviceType: ClusterIP`.

### 3. Gateway and certificate readiness is not checked

Install checks that the necessary CRDs exist, but deploy does not inspect:

- `Certificate` Ready status.
- `Gateway` Accepted and Programmed status.
- `HTTPRoute` Accepted and ResolvedRefs status.
- Gateway address or DNS readiness.

`donkey doctor` currently probes only the internal `donkey-gateway` health
Service. A Certificate or Route can therefore be rejected while deploy and
doctor still describe the internal gateway as healthy.

The managed edge needs status diagnostics in deploy and doctor. Deploy should
either wait for the edge or return a clear note that certificate/Gateway
provisioning is still in progress. The provider interface already permits
such a note.

### 4. `routeNamespace` is a platform contract

With `routeNamespace: projectcontour`:

- Donkey creates the source Gateway, Certificate, TLS Secret and HTTPRoute in
  its system namespace.
- The HTTPRoute points at the expected mirrored Gateway in
  `projectcontour`.
- The platform must mirror the Gateway and TLS Secret, or rewrite the
  certificate reference and arrange an appropriate `ReferenceGrant`.
- The mirrored listener must retain the `allowedRoutes` selector admitting
  donkey's system namespace.

Cross-namespace Route-to-Gateway attachment is authorized by the target
listener's `allowedRoutes`. Cross-namespace Gateway-to-Secret references are
authorized with a `ReferenceGrant` in the Secret namespace.

DNS is another part of this contract. Either every mirrored Gateway uses a
shared platform VIP reachable through wildcard DNS, or the platform must
create/update DNS from each Gateway's assigned address. Donkey does not
currently manage DNS or verify it.

References:

- <https://gateway-api.sigs.k8s.io/docs/concepts/hostnames/>
- <https://gateway-api.sigs.k8s.io/docs/concepts/security/>
- <https://gateway-api.sigs.k8s.io/guides/user-guides/tls/>
- <https://cert-manager.io/docs/usage/certificate/>

### 5. Production NetworkPolicies must remain narrow

Workload pods must not receive ingress to system port 8088: that is
`donkey-server`. The separated `donkey-ai` endpoint listens on 8089. A broad
policy selecting every system pod would weaken the rendered gateway-only
policy protecting the admin API.

The relevant traffic is:

```text
platform dataplane namespace -> donkey-gateway pod :8090
donkey-gateway pod            -> workload app pods :application ports
workload app pods             -> donkey-ai pod :8089
donkey-gateway pod            -> donkey-server pod :8088
donkey-server pod             -> donkey-ai pod :8089
```

Donkey already renders narrow policies for server, BuildKit and AI traffic.
A platform policy should add only the environment-specific flows, selecting
`donkey-gateway` rather than every pod in the system namespace. A workload
namespace with blanket default-deny also needs a narrow rule allowing the
Donkey gateway to reach application pods.

## What is already in good shape

- Every mode still uses `donkey-gateway` for application-level routing and
  authentication.
- The external Gateway never receives direct access to `donkey-server` or
  app routing configuration; it forwards to one ClusterIP Service.
- Per-app Gateway, HTTPRoute and Certificate write permissions are confined
  to donkey's system namespace in the two-namespace deployment.
- The server receives no permission in the infrastructure namespace named by
  `routeNamespace`.
- The per-app Certificate follows the enterprise issuer's key, lifetime,
  renewal and usage policy instead of hard-coding public-CA defaults.
- Internal apps create no dynamic edge objects, and destroy/network changes
  clean up the Gateway, Route, Certificate and TLS Secret.
- The generated listener restricts route attachment to donkey's system
  namespace with the standard immutable namespace-name label.

## Recommended implementation order

1. Render the control-plane Gateway, Certificate and HTTPRoute in dynamic
   mode, covering the base callback domain and `serverHost`.
2. Add cross-field validation for TLS mode, Service type and incompatible or
   incomplete edge combinations.
3. Read Gateway API and cert-manager status conditions during deploy and in
   `donkey doctor`.
4. Document the narrow platform NetworkPolicies alongside the final mode.
5. Add an end-to-end managed-edge test covering portal login, central OAuth
   callback, protected app traffic, public app traffic and cleanup.

The intended end state is one explicit production mode, not an accidental
combination of independent knobs:

```text
platform TLS/Gateway/cert-manager
        -> donkey-gateway (plain HTTP inside the cluster)
        -> OIDC, access policy and hostname/path routing
        -> portal or application Service
```
