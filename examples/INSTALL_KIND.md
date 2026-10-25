# A real installation on one server (Kubernetes/kind)

This walks through a production-shaped donkey installation on a single Debian
server that has nothing on it but Docker: [kind](https://kind.sigs.k8s.io) as
the cluster, a registry container for images, and an OIDC provider for login.
TLS and routing need nothing extra — the donkey gateway is the edge and
requests its Let's Encrypt certificates itself. The running example uses the
domain **`dihei.app`** and a [Pocket ID](https://pocket-id.org) instance at
**`https://id.dihei.ai`** — substitute your own.

(No Kubernetes needed at all? donkey also runs apps straight on the Docker
engine — the shorter [INSTALL_DOCKER.md](INSTALL_DOCKER.md) covers that.
Kubernetes buys rescheduling and the whole kubectl ecosystem; the Docker
runtime buys simplicity.)

The result:

- `https://dihei.app` — the API, the admin console, and the one OAuth
  callback every login rides through
- `https://<app>.<space>.dihei.app` — every deployed app, each with its own
  Let's Encrypt certificate and the OIDC login in front; one sign-in covers
  them all (the session cookie lives on the domain)

Traffic flows like this:

```
browser ──443──▶ server ──kind port mapping──▶ donkey-gateway
                                     (TLS by SNI, OIDC login, access rules)
                                                  │
                                                  ▼
                                               app pod
```

**Why nothing sits in front.** No Caddy or nginx on the host: the donkey
gateway has to terminate TLS itself — it picks (and issues) the certificate
by SNI per app hostname, answers its own ACME HTTP-01 challenges on port 80,
and its Secure login cookies assume it. A proxy in front would re-terminate
TLS and fight it over the same names. The kind port mappings hand the
server's 80/443 straight to the gateway, and nothing else is needed.

Sizing: builds run in the cluster (buildkit requests 4Gi of scratch space,
donkey-orchestrator 3Gi), so give the server at least 4 CPUs, 8 GB RAM and 60 GB of
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
    # Hand the server's 80/443 to the donkey gateway. Its service is
    # NodePort here, pinned to exactly these two ports in server.yaml.
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
(`registry.insecure` in step 6 tells buildkit the same):

```sh
docker exec donkey-control-plane mkdir -p "/etc/containerd/certs.d/donkey-registry:5000"
cat <<'EOF' | docker exec -i donkey-control-plane sh -c 'cat > "/etc/containerd/certs.d/donkey-registry:5000/hosts.toml"'
[host."http://donkey-registry:5000"]
EOF
```

That file lives inside the node container: it survives reboots, but re-run
this step whenever you recreate the cluster.

## 4. The OIDC client (Pocket ID)

In the Pocket ID admin (`https://id.dihei.ai`) add an OIDC client:

- **Name**: `donkey`
- **Callback URL**: `https://dihei.app/oauth2/callback` — exactly one.
  Every login under the domain rides through this central callback, whatever
  app it started on; no wildcard support needed. (Only an app with a custom
  `host:` outside dihei.app would need its own callback registered.)
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
the users/groups assigned to space admins, developers or viewers in Space
settings. Platform admin groups can be seeded through `platform.admins.groups`
in `server.yaml`.

## 5. Release donkey

Two artifacts, two pushes, both from your dev machine. The server image goes
to GHCR, amd64 and arm64 under one tag:

```sh
# once: a GitHub personal access token with the write:packages scope
echo "$GHCR_TOKEN" | docker login ghcr.io -u adrianliechti --password-stdin

task publish VERSION=0.1.0       # → ghcr.io/adrianliechti/donkey-server:0.2.0
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

## 6. Install

`donkey server install` is the one command that talks to Kubernetes directly,
so it runs on the server, where the kubeconfig lives. `server.yaml`:

```yaml
image: ghcr.io/adrianliechti/donkey-server:0.2.0
                                  # public GHCR package; the real version tag
                                  # is pulled once and then cached

registry:
  url: donkey-registry:5000/donkey
  insecure: true                  # plain HTTP — it only exists on the kind network

domain: dihei.app                 # console + API + login at https://dihei.app,
                                  # apps at <app>.<space>.dihei.app

gateway:
  serviceType: NodePort           # kind maps the host's 80/443 onto the node ports
  httpNodePort: 30080             # pinned to match the kind port mappings
  httpsNodePort: 30443

# defaultPlatform: linux/arm64   # ARM server: app builds must match the node
                                  # (the default is linux/amd64)

tls:
  mode: acme                      # the gateway requests one Let's Encrypt
                                  # certificate per hostname, on demand
  email: you@example.com          # expiry notices go here
  # directory: https://acme-staging-v02.api.letsencrypt.org/directory
                                  # while experimenting — Let's Encrypt
                                  # rate-limits the real endpoint

auth:
  issuer: https://id.dihei.ai     # exactly the issuer from step 6
  clientId: <pocket-id-client-id>
  # clientSecret comes from the environment

ai:
  baseUrl: https://api.openai.com/v1
  model: gpt-6.1-sol
  # apiKey comes from the environment
```

```sh
export OPENAI_API_KEY=sk-…
export DONKEY_OIDC_CLIENT_SECRET=…
donkey server install -f server.yaml
```

For a reviewed or GitOps-style apply, the same file can be rendered without
contacting the cluster. The generated bundle keeps Secret objects and values
out of `manifests.yaml`:

```sh
donkey server render -f server.yaml --output deploy/
# review deploy/README.md and deploy/manifests.yaml, then run deploy/apply.sh
```

It prints where to sign in — there is no API token: console and CLI use the
same Pocket ID login as the apps (add `admins:` under `auth:` to narrow who
may administer; empty means every Pocket ID account). The gateway requests
the console's certificate the moment it starts serving; the very first
https request may take a few seconds while Let's Encrypt answers.

## 7. Log in and deploy

From your laptop:

```sh
donkey login --server https://dihei.app     # opens the Pocket ID sign-in
donkey doctor
cd your-project
donkey init
donkey deploy
```

The first deploy into a space makes the gateway request that hostname's
certificate on the fly, and the app answers at
`https://<app>.<space>.dihei.app` — behind the Pocket ID login unless its
`donkey.yaml` says `public: true`. Verify with `curl -sI http://dihei.app`
from anywhere (a `301` is right; connection refused means the port mapping or
DNS record isn't in place).

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
which every sign-in and CLI token hangs off. The cluster itself is disposable: recreate it (steps 2–3),
reinstall (step 6), restore the volumes.

**Reboots.** The node and the registry both carry Docker restart policies. If
the cluster doesn't answer afterwards, kind is usually unhappy about its
container IP: `docker stop donkey-registry && docker restart
donkey-control-plane && docker start donkey-registry` brings it back in a
stable order.

**What this setup is not.** One node, no HA — a hardware failure takes
everything down until restored from backup. The registry trusts everyone on
the cluster network. And donkey-server itself holds `cluster-admin` — see the
*Security notes* in the README before opening the platform to strangers.
