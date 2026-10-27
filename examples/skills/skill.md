---
name: donkey
description: >
  Deploy and operate apps through the donkey CLI: configure donkey.yaml,
  configure app authentication, connect services, manage environment variables
  and persistent files, verify deployments, and diagnose failures. Use for
  work on apps hosted by Donkey; platform installation is a separate
  administrator task.
---

# Donkey apps

- Use `donkey` for everything Donkey manages (builds, images, services,
  runtime), not `docker`, `kubectl` or `helm`.
- Leave `donkey server …` and installation settings alone unless the task is
  platform administration.
- `donkey <command> --help` lists options.

## Connection

- Unknown or failing connection: run `donkey config show` and `donkey doctor`;
  reuse the configured server and credentials.
- Browser login needs the user. With a supplied token:
  `donkey login --server URL --token TOKEN` (`--ca-file FILE` for a private CA).
- `DONKEY_TOKEN` overrides the saved token (`~/.config/donkey/config.yaml`).
- Never put credentials in repository files or command output.

## App and space

- Run from the app folder, or pass `--app-dir <folder>`.
- Space: `--space`, else `space:` in `donkey.yaml`, else the git repository's
  folder name.
- `donkey apps` and `donkey addon list` show existing names.
- Names (app, space, service): lowercase letters, digits, dashes; start and
  end alphanumeric; at most 40 characters. Reserved spaces: `donkey`,
  `platform`, `system`. An app and a service in one space cannot share a name.
- Without a terminal, and with `--yes`, prompts take their defaults. Neither
  confirms data deletion; that needs `--force` (see Removal).

## donkey.yaml

Keep an existing `donkey.yaml`; change only what the task needs. Minimal:

```yaml
name: shop
port: 8000
healthcheck:
  path: /healthz
addons:
  - type: postgres
    name: db
```

- Declare only required services. Services in `addons:` before the first
  deploy are provisioned and bound in that deploy.
- `donkey init` and unattended first deploys infer app settings but add no
  services. `donkey init` keeps an existing config.
- Parsing is strict: unknown fields fail. Explicit settings override
  inference and console settings; omitted network settings keep the console's.

| Field | Use |
|---|---|
| `env` | Non-secret runtime values; `${db.DATABASE_URL}` maps a declared service's binding to a custom variable. |
| `build.env` | Build-time values, including browser bundler variables (`VITE_*`, `NEXT_PUBLIC_*`). |
| `build.context` | Build context relative to the app folder; uploads cannot include files above that folder. |
| `build.platform` | Override the installation's target platform. |
| `command` | Start command override, as an argument list. |
| `storage.paths` | Absolute container paths whose data must survive redeploys. |
| `storage.size`, `storage.class` | Disk size (default `1Gi`), optional storage class. |
| `replicas` | Default 1; must stay 1 with persistent storage. |
| `resources.requests`, `resources.limits` | Optional CPU/memory. |
| `oidc` | Register a web app for OIDC login; see OIDC web-app login below. |

### App requirements

- Listen on `0.0.0.0`, port from `PORT`.
- `healthcheck.path` must answer without credentials; omit it for a TCP check.
- There is no migration job: run schema or bucket initialization at startup,
  idempotently.

### Build

- Do not add a Dockerfile or force a build strategy; Donkey chooses the build.
- Do not configure image mirrors, package repositories or proxies; the
  installation provides them.
- Donkey repairs scan refusals for its generated recipes, including saved
  `.donkey/Dockerfile` recipes selected automatically. Keep the repaired recipe
  and source changes it returns. For an explicitly selected Dockerfile, act
  on the reported package and fixed version; do not weaken scan policy.
- A scanner or vulnerability-database outage needs an operator fix and retry,
  not an application dependency change.
- Native or system packages: list them in the app's `README.md`, which the
  builder reads. Give the library or binary, why it is needed, build stage or
  runtime image, and package names per distro:

  ```text
  Build requirements:
  - PostgreSQL extension compilation needs development headers:
    Debian (apt): build-essential, libpq-dev.
    Alpine (apk): build-base, postgresql-dev.
  - The final image needs the client library:
    Debian (apt): libpq5. Alpine (apk): postgresql-libs.
  ```

- `donkey.yaml` has no package-list field; `build.env` only sets variables.
  `build.instructions` and `build.packages.debian`/`.alpine` are operator
  settings, not app fields. Report unavailable mirrors to the operator.

### Env and secrets

- Secrets: `donkey env set KEY=VALUE` (restarts the app). Never in
  `donkey.yaml`, browser variables or build arguments.
- `donkey env list` masks values. `PORT` is managed by Donkey and cannot be set.
- `${service.KEY}`: runtime `env` only, service must be in `addons:`, never in
  browser-exposed variables.
- Browser code calls a public or same-origin API URL, never a container
  hostname or with a service credential.
- Sibling apps in the space: `http://<app>`.

### Storage and uploads

- Each `storage.paths` entry mounts an empty folder that hides the image's
  contents at that path. Seed it at startup or with `donkey files put`.
- Other paths are ephemeral. `paths: []` means no disk.
- Uploads skip dependency/virtualenv directories, VCS/editor files, symlinks,
  `.env*` and key material. Check the deploy's skipped-file output when build
  inputs are missing.

### Exposure

Choose exposure for the intended audience; do not change it just to test.
Default address: `<app>.<space>.<domain>`.

| Setting | Effect |
|---|---|
| default | Login-protected web app. |
| `public: true` | No login; requires `allowPublic: true` in space settings (off by default). |
| `internal: true` | No public address; siblings still reach it. |
| `mode: api` | Protected API/MCP server: OAuth access tokens instead of browser login. |
| Space membership | Admins, developers and viewers may open protected apps; assign users and groups in space settings. |
| `cors: allowAll` | Cross-origin browser calls, at the gateway. |
| `host` | Full hostname, a label under the space's domain, or `@` for the space's apex. |
| `routes` | Serve this app under a path on another app's hostname in the same space. |

Invalid:

- per-app `access` (use space membership instead)
- `internal` with `host`
- `internal` with `public` or `routes`
- `public: true` when the space has not enabled `allowPublic`
- `oidc` with `internal: true` or `mode: api`
- a route path of `/`, or a route targeting the app itself

Frontend and an externally reachable backend on one origin:

```yaml
name: backend
port: 8000
routes:
  - app: frontend
    path: /api
    stripPrefix: true
```

`stripPrefix: true`: the backend receives `/…` for `/api/…`. Omit it when the
backend's own routes start with `/api`.

### Visitor identity

Protected routes receive (read `X-Forwarded-*`; `X-Auth-Request-*` repeats
them; client-sent copies are dropped):

- `X-Forwarded-User`: stable user ID (Entra object ID, else the IdP subject)
- `X-Forwarded-Email`, `-Preferred-Username`, `-Name`, `-Groups`
- `X-Forwarded-Access-Token`: the visitor's IdP access token, when the
  installation has `auth.scope` configured and the sign-in has a refresh token

Public routes receive none, even for a signed-in visitor. Keep routes
protected when the app relies on these headers or delegates with the user's
IdP token. A public web app can instead manage its own login using OIDC below;
its OIDC tokens provide identity and UserInfo access. Sibling apps bypass the
gateway; these headers describe the visitor, they do not authorize calls
from other apps.

### OIDC web-app login

Use this when a web app needs standard OIDC login through its authentication
library. Register callbacks in that app's `donkey.yaml` and deploy:

```yaml
oidc:
  redirectURIs:
    - https://shop.acme.apps.example.com/auth/callback
    - http://localhost:3000/auth/callback
```

- The app must be in web mode and have its own gateway address. Callback
  URLs match exactly, use HTTPS (HTTP is allowed on loopback), and must stay
  outside the gateway's reserved `/oauth2/` paths.
- Issuer: the installation's base domain, e.g. `https://apps.example.com`.
  Discovery: `/.well-known/openid-configuration`. Client ID:
  `donkey_web_<space>_<app>` (`donkey_web_acme_shop` in this example).
- Use Authorization Code with S256 PKCE, query responses, and scopes
  `openid profile email`. Public browser clients use token authentication
  method `none`, with no client secret.
- Confidential server-side clients also use PKCE. Store a random secret of
  at least 32 characters with `donkey env set`; put only its SHA-256 hex
  digest in `oidc.clientSecretSHA256`. Use `client_secret_basic` or
  `client_secret_post`. Keep the secret out of browser code and command output.
- Web-app access tokens authorize `/oauth2/userinfo`; they do not authorize
  API/MCP routes or downstream IdP APIs. Do not send `resource` for this flow.
  Protected `mode: api` routes use their existing resource-bound OAuth flow.
- OIDC login checks space membership even when the web app is public. Public
  exposure still requires the space's `allowPublic`; do not change exposure
  merely to add OIDC.
- Request `offline_access` only when refresh tokens are needed. Approval is
  interactive; refresh tokens rotate within a fixed 30-day grant lifetime.
  Serialize refreshes: reuse of an old token revokes the entire grant.
- `/oauth2/revoke` revokes the supplied access or refresh token's grant using
  the client's usual authentication. The app manages its own session;
  `/oauth2/logout` ends only the gateway browser session and is not an OIDC
  RP-initiated logout endpoint. Silent `prompt=none` returns an interaction
  error; use an interactive login when needed.
- Console network edits preserve the registration. Remove `oidc` and deploy
  before switching the app to API mode or internal exposure. Once the gateway
  observes a registration removal or change, old grants stay invalid even if
  the previous configuration is restored; start a fresh login.

## Services

- Services belong to the space; the same name means one shared instance.
- Read endpoints and credentials from env.
- Keep service names when editing config: they identify data and
  `${name.KEY}` references.
- Storage type is `s3`; `rustfs` is a legacy alias, not a reason to rename.

| Type | Binding and client use |
|---|---|
| `postgres` | `DATABASE_URL`; `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE`; Java: `JDBC_DATABASE_URL`, `JDBC_DATABASE_USERNAME`, `JDBC_DATABASE_PASSWORD`. |
| `redis` | Prefer `REDIS_URL` (credentials, database, TLS scheme). Split clients must honor `REDIS_PASSWORD`, `REDIS_USERNAME`, `REDIS_DB`, `REDIS_TLS` too, not just `REDIS_ADDR`/`REDIS_HOST`. |
| `s3` | `AWS_ENDPOINT_URL_S3` (= `S3_URL`), `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`. Use path-style addressing (JS SDK: `forcePathStyle: true`). |
| `openai` | `OPENAI_BASE_URL`, `OPENAI_API_KEY` (bearer), `OPENAI_MODEL`. Streaming works. |
| `exa` | Set `EXA_BASE_URL` explicitly; `EXA_API_KEY` as `x-api-key` or bearer; `POST /search`, `POST /contents` relative to the base. |
| `trino` | `TRINO_URL`, optional `TRINO_CATALOG`, `TRINO_SCHEMA`. No stored credential: send the request's `X-Forwarded-Access-Token` as the Trino client's bearer token. Without one (jobs, public routes, local runs), connect without credentials instead of failing: locally `donkey addon tunnel` adds the developer's token; in the cluster there is no Trino access. |

- Managed PostgreSQL: single instance, no backups. Use an external database
  when production needs more.
- S3 starts without buckets: create an app-specific one; tolerate
  "already owned".
- Managed OpenAI/Exa bindings hold proxy tokens; upstream keys stay on the
  platform.
- The installation may offer more services and restrict offerings per space.
  Pass catalog parameters as `--param name=value` on `addon add`. An
  unavailable offering needs the operator or space admin; do not substitute
  another by protocol.
- Trino comes only from a provider offering: `addon add` with the catalog's
  type; never `addon attach`.

On an existing app, `addon add` (platform service) and `addon attach`
(external service) both update `donkey.yaml` (may reformat it, drop comments)
and redeploy a deployed app with its current image:

| Command | Use |
|---|---|
| `donkey addon add postgres --name db --storage 5Gi` | Create/connect a managed database. |
| `donkey addon add exa` | Connect the platform's Exa proxy. |
| `donkey addon attach exa --url URL` | Attach an external endpoint; set its `EXA_API_KEY` with `donkey env set`. |

- External PostgreSQL/Redis URLs carry their credentials. External S3,
  OpenAI and Exa URLs carry only endpoints: set credentials, plus
  `AWS_REGION` or `OPENAI_MODEL` if the client needs them, with
  `donkey env set`.
- External endpoints must be reachable from Donkey's runtime, not only from
  this machine. A URL path may prefix OpenAI/Exa gateways.
- Attaching a new URL to an existing service changes it for every attached
  app; use a distinct `--name` to change one app only.

## Deploy and verify

```sh
donkey deploy
donkey status
donkey logs --tail 100
```

- Deploy waits for health unless `--no-wait`. Read findings and build/rollout
  errors.
- After a failed deploy, a healthy status may be the previous version:
  confirm the running image.

| Option | Use |
|---|---|
| `--dry-run` | Print the Kubernetes objects; deploy nothing. |
| `--build-only` | Build and push; prints the image reference. |
| `--no-build --tag REF` | Deploy an image already in Donkey's registry. |
| `-v` | Full build output. |

Monorepo: one `donkey.yaml` per app folder, same space. Deploy each with
`--app-dir`, or from the root without a root config: the CLI finds configs up
to three levels down, deploys the shallowest level in path order, and stops
at the first failure (earlier apps stay deployed). Each upload holds only
its app's folder.

HTTP checks (`donkey status` gives the URL, `donkey open` opens it; internal
apps have none):

- Protected web routes accept the CLI login token as bearer. A `401` without
  it is authentication, not an app failure.
- Protected `mode: api` routes need an OAuth access token, not the CLI token;
  MCP clients can run the advertised OAuth flow.
- Private CA: give HTTPS clients the configured CA file.
- No credentials available: report status/logs and the pending HTTP check.
  Never make the app public to get past login.

On failure:

- Read the diagnosis, logs and proposed edits. Unattended runs and `--yes`
  never apply proposed edits: apply relevant fixes yourself and redeploy.
- After `Image ready: REF`, retry with `--no-build --tag REF` when the fix
  needs no source change.
- Platform failures: `donkey doctor`. Do not retry missing credentials or
  unavailable operator-managed dependencies.

## Tunnels, logs, files

- `donkey logs <service-name>`: managed services declared by this app only.
- `donkey addon tunnel <name> [--port N]`: reach a service from this machine
  while the command runs. Prints the binding rewritten for localhost and a
  ready-to-run command.
- Trino, platform OpenAI/Exa and configured HTTP proxy services tunnel over
  HTTP: point `*_URL`/`*_BASE_URL` at the printed address and keep
  credentials as they are. donkey adds the user's token (Trino) or the
  space's key (OpenAI, Exa).
- Services attached from elsewhere have no tunnel; use their own address.

Files: paths are disk-relative without a leading slash (`/app/data` →
`app-data/`, `/var/lib/app` → `var-lib-app/`). Start with `donkey files ls`.
Works while the app is stopped.

```sh
donkey files put seed.db app-data/seed.db
donkey files get app-data/result.csv -o result.csv
donkey files mv app-data/old.csv app-data/new.csv
```

## Removal

Remove only what was requested.

| Command | Effect |
|---|---|
| `donkey addon remove db` | Disconnect this app; the shared service and data remain. |
| `donkey addon remove db --space --keep-data` | Remove the shared service from all apps; keep its data. |
| `donkey addon remove db --space --force` | Remove the shared service and delete its data. |
| `donkey destroy --keep-data --force` | Remove the app; keep its disk and credentials. |
| `donkey destroy --force` | Remove the app and its data; space services remain. |
| `donkey files rm app-data/file --force` | Delete that persistent file. |

- Use data-deleting forms only when deletion is requested. There are no
  snapshots or restore.
- `--force` only skips the confirmation; it does not change scope.
- OpenAI, Exa and HTTP proxy services have no data and reject `--keep-data`.
- Removing an app keeps its local source and `donkey.yaml`.
