# A real installation on one server

This walks through a production-shaped donkey installation on a single Debian
server that has nothing on it but Docker: [kind](https://kind.sigs.k8s.io) as
the cluster, Envoy Gateway as the traffic layer, cert-manager with
Let's Encrypt for certificates, a registry container for images, and an OIDC
provider for login. The running example uses the domain **`dihei.app`** and a
[Pocket ID](https://pocket-id.org) instance at **`https://id.dihei.ai`** —
substitute your own.

The result:

- `https://dihei.app` — the API, the admin console, and the one OAuth
  callback every login rides through
- `https://<app>.<space>.dihei.app` — every deployed app, each with its own
  Let's Encrypt certificate and the OIDC login in front; one sign-in covers
  them all (the session cookie lives on the domain)

Traffic flows like this:

```
browser ──443──▶ server ──kind port mapping──▶ Envoy (certificate by SNI)
                                                  │
                                                  ▼
                                        donkey-gateway (OIDC login, access rules)
                                                  │
                                                  ▼
                                               app pod
```

**Why nothing sits in front.** No Caddy or nginx on the host: donkey's shared
Gateway has to terminate TLS itself — Envoy picks the certificate by SNI per
app hostname, cert-manager answers HTTP-01 challenges through the same
listener, and the login gateway's Secure cookies assume it. A proxy in front
would re-terminate TLS and fight cert-manager over the same names. The kind
port mappings hand the server's 80/443 straight to Envoy, and nothing else is
needed.

Sizing: builds run in the cluster (buildkit requests 4Gi of scratch space,
donkey-server 3Gi), so give the server at least 4 CPUs, 8 GB RAM and 60 GB of
disk.

## 0. DNS and firewall

Two records:

```
dihei.app.     A   <server-ip>
*.dihei.app.   A   <server-ip>
```

The wildcard covers every app — a DNS wildcard matches *any* depth
(`web.acme.dihei.app` included), unlike a TLS wildcard, which is why donkey
issues one certificate per hostname instead. Two caveats on that: an explicit
record at some `x.dihei.app` shadows the wildcard for everything *beneath* it
(so a space named like an existing subdomain would stop resolving), and the
records must be plain DNS — a proxying CDN in front (Cloudflare's orange
cloud) would terminate TLS itself and break donkey's certificates. The apex
record exists because a wildcard never covers the apex — and the console, API
and login callback live there.

Open ports 80 and 443 (plus SSH). The Kubernetes API stays on the server's
localhost — administration happens over SSH, everyone else talks to
`https://dihei.app`.

## 1. Tools on the server

Plain binaries — works as root, on amd64 and arm64 alike (`ARCH` picks the
right downloads):

```sh
ARCH=$(case "$(uname -m)" in arm64|aarch64) echo arm64;; *) echo amd64;; esac)

# kind — check https://github.com/kubernetes-sigs/kind/releases for the current version
curl -Lo /usr/local/bin/kind "https://kind.sigs.k8s.io/dl/v0.32.0/kind-linux-${ARCH}"
chmod +x /usr/local/bin/kind

# kubectl
curl -Lo /usr/local/bin/kubectl "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/${ARCH}/kubectl"
chmod +x /usr/local/bin/kubectl

# helm (its install script detects the architecture itself)
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

## 2. The cluster

`kind.yaml`:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: donkey
nodes:
  - role: control-plane
    # Hand the server's 80/443 to the gateway. donkey's traffic service is
    # NodePort here, pinned to exactly these two ports in step 9.
    extraPortMappings:
      - containerPort: 30080
        hostPort: 80
      - containerPort: 30443
        hostPort: 443
    # Volumes (the default StorageClass) live on the host, not inside the
    # node container — a rebuilt node keeps app and addon data.
    extraMounts:
      - hostPath: /data/donkey/volumes
        containerPath: /var/local-path-provisioner
containerdConfigPatches:
  # Per-registry settings from /etc/containerd/certs.d — step 3 drops the
  # plain-HTTP registry's file there.
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
```

```sh
mkdir -p /data/donkey/volumes
kind create cluster --config kind.yaml
```

This writes the kubeconfig to `~/.kube/config` on the server.

## 3. The registry

donkey pushes every build to a registry and never installs one, so run the
official registry as a container **on the kind Docker network** — it gets no
host port, which makes it unreachable from the internet and safe to leave
credential-less:

```sh
mkdir -p /data/donkey/registry
docker run -d --name donkey-registry --restart=always \
  --net kind -v /data/donkey/registry:/var/lib/registry registry:3
```

The name `donkey-registry` resolves everywhere it matters: Docker's DNS
answers for the node, and the cluster's CoreDNS forwards there for pods — so
buildkit pushes and the kubelet pulls under one and the same name. What's
left is telling containerd that this registry speaks plain HTTP
(`registry.insecure` in step 8 tells buildkit the same):

```sh
docker exec donkey-control-plane mkdir -p "/etc/containerd/certs.d/donkey-registry:5000"
cat <<'EOF' | docker exec -i donkey-control-plane sh -c 'cat > "/etc/containerd/certs.d/donkey-registry:5000/hosts.toml"'
[host."http://donkey-registry:5000"]
EOF
```

That file lives inside the node container: it survives reboots, but re-run
this step whenever you recreate the cluster.

## 4. Envoy Gateway

```sh
# check https://github.com/envoyproxy/gateway/releases for the current version
helm install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version v1.8.3 --namespace envoy-gateway-system --create-namespace

kubectl wait --for=condition=Available deployment/envoy-gateway \
  --namespace envoy-gateway-system --timeout=5m
```

The chart brings the Gateway API CRDs with it. donkey creates the
`GatewayClass` and `Gateway` itself at install time — only the controller has
to be here first.

## 5. cert-manager and Let's Encrypt

`config.gatewayAPI.enabled` is the one non-default: cert-manager answers
HTTP-01 challenges by attaching a temporary route to donkey's Gateway, and
its Gateway API support is off out of the box.

```sh
# check https://cert-manager.io/docs/releases/ for the current version
helm install cert-manager oci://quay.io/jetstack/charts/cert-manager \
  --namespace cert-manager --create-namespace \
  --version v1.21.0 \
  --set crds.enabled=true \
  --set config.gatewayAPI.enabled=true
```

`issuer.yaml`:

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt
spec:
  acme:
    email: you@example.com          # expiry notices go here
    server: https://acme-v02.api.letsencrypt.org/directory
    privateKeySecretRef:
      name: letsencrypt-account
    solvers:
      - http01:
          gatewayHTTPRoute:
            parentRefs:
              - kind: Gateway
                name: donkey
                namespace: donkey-system
```

The `parentRefs` point at a Gateway that doesn't exist yet — donkey creates
it in step 8. That's fine: the reference is only followed when a challenge
actually runs, and the issuer turns Ready as soon as its Let's Encrypt
account is registered, which is what `donkey server install` checks.

```sh
kubectl apply -f issuer.yaml
kubectl wait --for=condition=Ready clusterissuer/letsencrypt
```

While experimenting, point `server:` at
`https://acme-staging-v02.api.letsencrypt.org/directory` instead —
Let's Encrypt rate-limits the real endpoint.

## 6. The OIDC client (Pocket ID)

In the Pocket ID admin (`https://id.dihei.ai`) add an OIDC client:

- **Name**: `donkey`
- **Callback URL**: `https://dihei.app/oauth2/callback` — exactly one.
  Every login under the domain rides through this central callback, whatever
  app it started on; no wildcard support needed. (Only an app with a custom
  `domain:` outside dihei.app would need its own callback registered.)
- Copy the **client ID** and generate a **client secret** — donkey's gateway
  is a confidential client and keeps the secret server-side.

The issuer donkey needs is the one from the discovery document, character for
character:

```sh
curl -s https://id.dihei.ai/.well-known/openid-configuration | jq -r .issuer
```

This one client is the whole identity story: apps, the **admin console** and
the **CLI** (`donkey login` opens the browser) all sign in through it. Every
app requires login unless its `donkey.yaml` says `public: true`, and because
the session cookie lives on the domain, signing in once covers everything —
console included (and signing out anywhere signs you out everywhere).

Group-based access works out of the box: donkey requests the `groups` scope
whenever the provider advertises it, and Pocket ID's user groups then match
`access.groups` in an app's `donkey.yaml` — or `admins.groups` in
`server.yaml`, which narrows who may administer (`access.roles` is for
Entra/Keycloak-style role claims, which Pocket ID doesn't have).

## 7. Release donkey

Two artifacts, two pushes, both from your dev machine. The server image goes
to GHCR, amd64 and arm64 under one tag:

```sh
# once: a GitHub personal access token with the write:packages scope
echo "$GHCR_TOKEN" | docker login ghcr.io -u adrianliechti --password-stdin

task publish VERSION=0.1.0       # → ghcr.io/adrianliechti/donkey-server:0.1.0
```

After the very first push, flip the GHCR package to **public** in its
settings on github.com — donkey's control-plane pods carry no pull secret.

The CLI releases with goreleaser from a git tag — binaries onto the GitHub
release, plus the Homebrew tap and the Scoop bucket:

```sh
export GITHUB_TOKEN=…            # repo scope
git tag v0.1.0
goreleaser release --clean
```

The server gets its CLI straight from the release (`ARCH` as in step 1):

```sh
curl -fsSL "https://github.com/adrianliechti/donkey/releases/latest/download/donkey_linux_${ARCH}.tar.gz" \
  | tar -xz -C /usr/local/bin donkey
```

This anonymous download needs the repository to be public — a private repo
answers 404 to unauthenticated curl (the same URL works in a signed-in
browser, which is confusing the first time). While the repo is private, ship
the binary from the goreleaser output on your dev machine instead:

```sh
scp dist/*_linux_amd64*/donkey root@<server>:/usr/local/bin/donkey
```

(The local registry from step 3 stays what it is — the home of *app* builds
inside the cluster. GHCR and the GitHub release only distribute donkey
itself.)

## 8. Install

`donkey server install` is the one command that talks to Kubernetes directly,
so it runs on the server, where the kubeconfig lives. `server.yaml`:

```yaml
image: ghcr.io/adrianliechti/donkey-server:0.1.0
                                  # public GHCR package; the real version tag
                                  # is pulled once and then cached

registry:
  url: donkey-registry:5000/donkey
  insecure: true                  # plain HTTP — it only exists on the kind network

domain: dihei.app                 # console + API + login at https://dihei.app,
                                  # apps at <app>.<space>.dihei.app

gateway:
  serviceType: NodePort           # kind maps the host's 80/443 onto the node ports

# defaultPlatform: linux/arm64   # ARM server: app builds must match the node
                                  # (the default is linux/amd64)

tls:
  issuer: letsencrypt

auth:
  issuer: https://id.dihei.ai     # exactly the issuer from step 6
  clientId: <pocket-id-client-id>
  # clientSecret comes from the environment

ai:
  baseUrl: https://api.openai.com/v1
  model: gpt-5.5
  # apiKey comes from the environment
```

```sh
export OPENAI_API_KEY=sk-…
export DONKEY_OIDC_CLIENT_SECRET=…
donkey server install -f server.yaml
```

It prints where to sign in — there is no API token: console and CLI use the
same Pocket ID login as the apps (add `admins:` under `auth:` to narrow who
may administer; empty means every Pocket ID account). A warning that the
server's certificate is still pending is expected here: issuance needs port
80, which the next step wires up.

## 9. Pin the gateway's ports

donkey renders the `EnvoyProxy` resource but leaves the node ports to
Kubernetes, which picks random ones — while the kind mappings from step 2 are
fixed at 30080/30443. Pinning happens in **two steps**, because the envoy
service doesn't have its https port yet: Envoy Gateway only adds `443` once
the server certificate exists, and the certificate needs a working port 80
first. A patch that names a port the service doesn't have yet is appended as
an invalid (nameless) entry, and Kubernetes then rejects the whole service
update — including the port-80 half.

First, port 80 alone:

```sh
cat > envoyproxy-ports.yaml <<'EOF'
spec:
  provider:
    kubernetes:
      envoyService:
        patch:
          type: StrategicMerge
          value:
            spec:
              ports:
                - port: 80
                  nodePort: 30080
EOF
kubectl -n donkey-system patch envoyproxy donkey --type merge --patch-file envoyproxy-ports.yaml
```

Within a minute the envoy service shows `80:30080`, Let's Encrypt reaches its
challenge, and the server's certificate issues:

```sh
kubectl -n donkey-system get certificate -w   # donkey-tls-server → Ready=True
```

That makes the https listener valid, Envoy Gateway adds port 443 to the
service — and now the full patch merges cleanly:

```sh
cat > envoyproxy-ports.yaml <<'EOF'
spec:
  provider:
    kubernetes:
      envoyService:
        patch:
          type: StrategicMerge
          value:
            spec:
              ports:
                - port: 80
                  nodePort: 30080
                - port: 443
                  nodePort: 30443
EOF
kubectl -n donkey-system patch envoyproxy donkey --type merge --patch-file envoyproxy-ports.yaml
```

The patch adds a field donkey's server-side apply never owns, so re-running
`donkey server install` later keeps it. Verify:

```sh
kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=donkey
kubectl -n donkey-system get certificate
```

The service now shows `80:30080` and `443:30443`, and within a minute or two
`donkey-tls-server` goes Ready and `https://dihei.app` serves the console.
If issuance hangs, `kubectl -n donkey-system describe challenge` tells you
why — and `curl -sI http://dihei.app` from anywhere must reach Envoy (a
`301` is right; connection refused means the port pinning or DNS record
isn't in place).

## 10. Log in and deploy

From your laptop:

```sh
donkey login --server https://dihei.app     # opens the Pocket ID sign-in
donkey doctor
cd your-project
donkey init
donkey deploy
```

The first deploy into a space requests that hostname's certificate on the
fly, and the app answers at `https://<app>.<space>.dihei.app` — behind the
Pocket ID login unless its `donkey.yaml` says `public: true`.

## Day 2

**Upgrading donkey.** Rebuild and push under a new version tag (step 7),
change `image:` in `server.yaml` and re-run `donkey server install -f
server.yaml`. Secrets, tokens and certificates survive; changed settings roll
the affected pods. The details — token rotation, what a re-install preserves —
are in the README under *Running it in production*.

**Backups.** Three things on the host: `/data/donkey/volumes` (app disks and
addon data), `/data/donkey/registry` (images — losing it only costs rebuilds),
and the `donkey-system` secrets
(`kubectl -n donkey-system get secret -o yaml`) — above all the session key,
which every sign-in and CLI token hangs off. The cluster itself is disposable: recreate it (steps 2–5),
reinstall (step 8–9), restore the volumes.

**Reboots.** The node and the registry both carry Docker restart policies. If
the cluster doesn't answer afterwards, kind is usually unhappy about its
container IP: `docker stop donkey-registry && docker restart
donkey-control-plane && docker start donkey-registry` brings it back in a
stable order.

**What this setup is not.** One node, no HA — a hardware failure takes
everything down until restored from backup. The registry trusts everyone on
the cluster network. And donkey-server itself holds `cluster-admin` — see the
*Security notes* in the README before opening the platform to strangers.
