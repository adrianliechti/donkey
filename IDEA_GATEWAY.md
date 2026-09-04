# Incoming traffic and certificate design

Status: implemented. Gateway/Certificate readiness and DNS diagnostics remain
future work.

## One edge and one address

Every public request enters the `donkey-gateway` LoadBalancer Service. The
usual production shape is one stable VIP exposing HTTP and HTTPS:

```text
client -> VIP :80/:443 -> donkey-gateway -> control plane or app Service
```

The gateway redirects HTTP to HTTPS and owns host/path routing, OIDC, access
rules, identity-header sanitization, and the sealed token accepted by
`donkey-server`. A request for the installation domain or `serverHost` reaches
the control plane; an exact app hostname reaches that app's Service.

This single path removes duplicate routing state and ensures that the
component enforcing authentication is also the component that receives the
original request.

## TLS is a source choice, not a traffic mode

The gateway always terminates TLS. `tls.mode` selects only where its
certificates originate:

- `acme` is the default and requests exact-host certificates from Let's
  Encrypt or an internal ACME directory.
- `ca` issues exact-host certificates from donkey's built-in CA. It is useful
  for private installations whose clients can trust that CA.
- `cert-manager` creates `cert-manager.io/v1` Certificate objects. The
  resulting TLS Secrets are consumed directly by `donkey-gateway`.

ACME and CA remain first-class choices on Kubernetes. cert-manager is useful
when the platform already centralizes issuer policy and renewal there; it does
not require a separate traffic controller.

In cert-manager mode, every exposed app gets one exact-host Certificate in
the system namespace. The installation domain and a distinct server host
share one SAN Certificate. The generated Secret inherits donkey ownership
labels. The gateway lists only its installation's labeled TLS Secrets,
validates each key pair, selects by SNI, and periodically reloads renewals.

```yaml
tls:
  mode: cert-manager
  issuerName: corporate-pki
  # issuerKind: ClusterIssuer
  # issuerGroup: cert-manager.io
```

The installer checks for the Certificate CRD only when this source is chosen.
A namespaced Issuer must exist in donkey's system namespace; a ClusterIssuer
is the usual cluster contract.

## Optional DNS records

DNS is also independent from routing and certificates. When configured,
donkey creates `externaldns.k8s.io/v1alpha1` DNSEndpoint objects with exact
records for every exposed app, the installation domain, and a distinct server
host:

```yaml
gateway:
  externalDNS:
    targets: [10.204.166.5]
    # recordType: A
    # ttl: 300
```

The target is the stable gateway VIP. Record type is inferred from the target
as A, AAAA, or CNAME unless set explicitly. The external-dns controller must
run with `--source=crd`; provider configuration, domain filters, synchronization
policy, registry, and TXT ownership remain platform concerns. If the block is
absent, donkey emits no DNS object and existing records remain untouched.

## Lifecycle and authority

- Install creates the control-plane Certificate and DNSEndpoint, when enabled,
  alongside the direct LoadBalancer Service.
- Deploying or exposing an app creates its exact-host Certificate and DNS
  record. Making it internal or destroying it removes them and its generated
  TLS Secret.
- Reinstall reconciles all existing app hosts from their route ConfigMaps, so
  changing between ACME, CA, and cert-manager or enabling DNS does not wait for
  another app deployment.
- In the confined two-namespace topology, Certificate and DNSEndpoint write
  access exists only in donkey's system namespace. The gateway reads Secrets
  there because both its managed certificates and ACME/CA state live there.

Reconciliation is event-driven except for certificate Secret reloads.
cert-manager and external-dns remain the controllers of their own CRDs.

## Remaining production diagnostics

The object lifecycle is supported, but deploy and `donkey doctor` do not yet
report cert-manager Certificate readiness or external-dns publication state.
A pending Certificate can therefore coexist with a healthy gateway process;
TLS handshakes for that hostname fail clearly until its Secret is ready.
