# A real installation on one server (Docker, no Kubernetes)

donkey's Docker runtime runs apps straight on the Docker engine: spaces are
Docker networks, apps are containers, disks are named volumes. No kind, no
kubectl — one server with Docker is the whole story. The install brings its
own registry and build engine as containers; TLS and routing come from the
donkey gateway itself.

The running example uses the domain **`dihei.app`** and a
[Pocket ID](https://pocket-id.org) instance at **`https://id.dihei.ai`** —
substitute your own. (Want Kubernetes instead — rescheduling, the kubectl
ecosystem? That's [INSTALL_KIND.md](INSTALL_KIND.md).)

The result:

- `https://dihei.app` — the API, the admin console, and the one OAuth
  callback every login rides through
- `https://<app>.<space>.dihei.app` — every deployed app, each with its own
  Let's Encrypt certificate and the OIDC login in front

What the install creates on the engine:

```
browser ──443──▶ donkey-gateway (TLS by SNI, OIDC login, access rules)
                      │                             donkey network
                      ▼
                app container ── donkey-<space> network ── service containers
```

- `donkey-gateway` — the edge, publishing ports 80/443
- `donkey-server` — the API (and runtime provider: it holds the Docker
  socket), publishing the API on `127.0.0.1:8088` for direct token access
- `donkey-buildkitd` — builds images
- `donkey-registry` — receives them (loopback-published for the daemon's
  pulls; never reachable from outside)
- volumes: `donkey-data` (routes, env, bindings, the gateway's
  certificates), `donkey-registry` (images), `donkey-buildkit` (build cache)

Sizing: builds run on the server (buildkit wants real scratch space), so
give it at least 4 CPUs, 8 GB RAM and 60 GB of disk.

## 1. DNS and firewall

Two records:

```
dihei.app.     A   <server-ip>
*.dihei.app.   A   <server-ip>
```

The wildcard covers every app (a DNS wildcard matches any depth); the apex
record exists because a wildcard never covers the apex — and the console,
API and login callback live there. The records must be plain DNS — a
proxying CDN in front (Cloudflare's orange cloud) would terminate TLS itself
and break the gateway's certificates.

Open ports 80 and 443 (plus SSH). Let's Encrypt reaches the gateway's own
HTTP-01 challenges through port 80.

## 2. Docker and the donkey CLI

```sh
curl -fsSL https://get.docker.com | sh

ARCH=$(case "$(uname -m)" in arm64|aarch64) echo arm64;; *) echo amd64;; esac)
curl -fsSL "https://github.com/adrianliechti/donkey/releases/latest/download/donkey_linux_${ARCH}.tar.gz" \
  | tar -xz -C /usr/local/bin donkey
```

## 3. The OIDC client (Pocket ID)

Exactly as on Kubernetes — the identity story doesn't change with the
runtime. In the Pocket ID admin add an OIDC client with the **one** callback
URL `https://dihei.app/oauth2/callback`, copy the client id, generate a
client secret, and read the issuer from the discovery document:

```sh
curl -s https://id.dihei.ai/.well-known/openid-configuration | jq -r .issuer
```

(The longer version — why one callback covers every app, how groups map to
access rules — is in [INSTALL_KIND.md](INSTALL_KIND.md) section 4.)

## 4. Install

`server.yaml`:

```yaml
runtime: docker                   # THE line that skips Kubernetes

image: ghcr.io/adrianliechti/donkey-server:0.2.0

domain: dihei.app                 # console + API + login at https://dihei.app,
                                  # apps at <app>.<space>.dihei.app

# defaultPlatform: linux/arm64   # ARM server: app builds must match the host
                                  # (the default is linux/amd64)

tls:
  mode: acme                      # the gateway requests one Let's Encrypt
                                  # certificate per hostname, on demand
  email: you@example.com          # expiry notices go here
  # directory: https://acme-staging-v02.api.letsencrypt.org/directory
                                  # while experimenting — Let's Encrypt
                                  # rate-limits the real endpoint

auth:
  issuer: https://id.dihei.ai     # exactly the issuer from step 3
  clientId: <pocket-id-client-id>
  # clientSecret comes from the environment
  # admins:                       # narrow who may administer; empty means
  #   users: [you@example.com]    # everyone the IdP admits

ai:
  baseUrl: https://api.openai.com/v1
  model: gpt-5.5
  # apiKey comes from the environment
```

No registry section: the install runs its own (an explicit `registry:` block
still wins if you have one). Then:

```sh
export OPENAI_API_KEY=sk-…
export DONKEY_OIDC_CLIENT_SECRET=…
donkey server install -f server.yaml
```

Re-running with changed settings updates the installation; the session key
(and with it every login and CLI token) survives re-installs.

## 5. Log in and deploy

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
`donkey.yaml` says `public: true`.

## Day 2

**Upgrading donkey.** Change `image:` in `server.yaml` and re-run
`donkey server install -f server.yaml`. The containers are recreated;
secrets, certificates and app containers survive.

**Backups.** Everything lives in Docker volumes: `donkey-data` (above all —
routes, bindings, the session key rides in the container env, the gateway's
certificates), `donkey-registry` (images — losing it only costs rebuilds),
and one `donkey.<space>.<app>.data` volume per app disk plus
`donkey.<space>.<service>.data` per provisioned service.

**What this setup is not.** One machine, one copy of everything — no
rescheduling, no HA: a hardware failure takes it down until restored from
backup. If an app crashes, Docker's restart policy brings it back; if the
machine dies, nothing does. That trade — and `cluster-admin`-equivalent
power in donkey-server (it holds the Docker socket) — is the price of the
simplest possible installation. See the *Security notes* in the README.
