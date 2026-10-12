---
name: donkey
description: >
  Deploy and operate apps through the donkey CLI: configure donkey.yaml,
  connect services, manage environment variables and persistent files,
  verify deployments, and diagnose failures. Use for work on apps hosted
  by Donkey; platform installation is a separate administrator task.
---

# Donkey apps

Use `donkey` for deployment and operations. The server builds, pushes images,
provisions services, and starts the app on the installation's runtime. Use
CLI subcommands instead of `docker`, `kubectl`, or `helm` for resources Donkey
manages. Leave `donkey server …` and installation settings to the operator
unless the task explicitly includes platform administration.

## Connect and select the app

If the connection is unknown or failing, run `donkey config show` and
`donkey doctor`. Reuse the configured server and credentials. Browser login
requires the user. A supplied token supports unattended login with
`donkey login --server URL --token TOKEN`; add `--ca-file FILE` for a private CA.

`DONKEY_TOKEN` overrides the saved token. The saved connection is in
`~/.config/donkey/config.yaml`. Keep credentials out of repository files and
command output.

Run app commands from its folder, or pass `--app-dir <folder>`. `--space`
overrides `space:`; otherwise the space defaults to the git repository's
folder name. Use `donkey apps` and `donkey addon list` when existing app or
service names matter. Use `donkey <command> --help` for additional options.

App, space, and service names use lowercase letters, digits, and dashes,
start and end alphanumeric, and have at most 40 characters. Space names
`donkey`, `platform`, and `system` are reserved. An app and a service in the
same space cannot share a name.

Without a terminal, prompts take defaults; `--yes` also takes defaults.
Neither supplies confirmation for data deletion. See removal below for
commands requiring `--force`.

## Prepare a source app

Preserve an existing `donkey.yaml`; change only what the task needs. If you
know the app's requirements, write a minimal config directly:

```yaml
name: shop
port: 8000
healthcheck:
  path: /healthz
addons:
  - type: postgres
    name: db
```

Include only required services. Declaring `addons:` before the first deploy
lets Donkey provision them and inject bindings in the same deploy. In
unattended runs, `donkey init` and first-deploy setup infer app settings but
select no services. `donkey init` leaves an existing config intact.

Let Donkey select and run the build from the repository. Do not add a
Dockerfile or force a build strategy to prepare a source app for deployment.
Leave image mirrors, package repositories, and proxies to the installation;
app preparation should not recreate that configuration.

The app must listen on `0.0.0.0`, read the injected `PORT`, and serve any
configured health route without credentials. Omit `healthcheck.path` for a
TCP check. Donkey does not run a separate migration job: run required schema
or bucket initialization in the app's startup path and make it repeatable.

### System package requirements

Put extra native dependency requirements in the app's `README.md`, where
Donkey's builder can read them. State the library or binary, why it is needed,
and whether it belongs in the build stage or the final runtime image:

```text
Build requirements:
- PostgreSQL extension compilation needs development headers:
  Debian (apt): build-essential, libpq-dev.
  Alpine (apk): build-base, postgresql-dev.
- The final image needs the client library:
  Debian (apt): libpq5. Alpine (apk): postgresql-libs.
```

There is no app-level package-list field. `build.env` passes variables; it
does not install packages. Installation-wide guidance uses server
`build.instructions`; private OS mirrors use `build.packages.debian` and
`build.packages.alpine`. These are operator settings, not `donkey.yaml`
fields. Keep mirror endpoints and upstream credentials in server settings;
report unavailable mirrors to the operator.

### Runtime, build, and storage settings

`donkey.yaml` is parsed strictly: unknown fields fail. Explicit settings win
over source inference and console settings. Network settings omitted from
the file retain existing console choices.

| Setting | Use |
|---|---|
| `env` | Non-secret runtime values; `${db.DATABASE_URL}` maps a declared service binding to a custom variable. |
| `build.env` | Build-time values, including browser bundler variables such as `VITE_*` or `NEXT_PUBLIC_*`. |
| `build.context` | Build context relative to the app folder; CLI uploads cannot include files above that folder. |
| `build.platform` | Override the installation's target platform when needed. |
| `command` | Override the image's start command, as an argument list. |
| `storage.paths` | Absolute container paths holding data that must survive redeploys. |
| `storage.size`, `storage.class` | Disk size (default `1Gi`) and optional storage class. |
| `replicas` | Default one; must remain one with persistent storage. |
| `resources.requests`, `resources.limits` | Optional CPU/memory requirements. |

Set secrets with `donkey env set KEY=VALUE`; this updates stored env and
restarts the app. `donkey env list` masks values by default. Do not put
secrets in `donkey.yaml`, browser variables, or build arguments. `PORT` is
managed by Donkey and cannot be changed with `env set`.

`${service.KEY}` references belong only in runtime `env`, require the service
in `addons:`, and cannot be used in browser-exposed variables. Browser code
must use a public or same-origin API URL, never a container hostname or a
service credential. Apps in one space reach siblings at `http://<app>`.

Each storage path mounts an initially empty folder and hides image contents
at that path. Seed it at startup or with `donkey files put`. Paths omitted
from `storage.paths` use ephemeral container storage; `paths: []` explicitly
selects no disk.

Uploads exclude dependency/virtualenv directories, VCS/editor files,
symlinks, `.env*`, and key material. Read the deploy's skipped-file output
when diagnosing missing build inputs.

### Exposure and routing

Choose exposure for the requested audience; keep it unchanged just to test
an app. The default address is `<app>.<space>.<domain>`.

| Setting | Behavior |
|---|---|
| Default | Login-protected web app. |
| `public: true` | Public access without the login gateway. |
| `internal: true` | No public address; siblings can still reach the app. |
| `mode: api` | Protected APIs/MCP servers use OAuth access tokens instead of browser login. |
| `access` | Restrict protected access by `users`, `groups`, or `roles`; any match permits access. |
| `cors: allowAll` | Enable cross-origin browser calls at the gateway. |
| `host` | Full hostname, a label under the space's domain, or `@` for the space's apex. |
| `routes` | Serve this app under a path on another app's hostname in the same space. |

`public` cannot combine with `access`; `internal` cannot combine with `host`.
An internal app can use `public` or `access` only when it has `routes`, where
those settings apply. `internal` + `access` + `mode: api` is invalid. Route
paths must be below `/` and target another app. For a frontend and internal
backend sharing an origin:

```yaml
name: backend
port: 8000
internal: true
routes:
  - app: frontend
    path: /api
    stripPrefix: true
```

Here the backend receives `/…` for requests under `/api/…`; omit
`stripPrefix` when its own routes include `/api`.

## Service bindings

Services belong to the space; apps with the same service name share one
instance. Read endpoints and credentials from env. Preserve service names
when updating configs: names identify existing data and `${name.KEY}` refs.
Use `s3` as the storage type; `rustfs` remains a legacy alias, not a reason
to rename an existing instance.

| Type | Client contract |
|---|---|
| `postgres` | `DATABASE_URL`; split `PGHOST`, `PGPORT`, `PGUSER`, `PGPASSWORD`, `PGDATABASE`; Java uses `JDBC_DATABASE_URL`, `JDBC_DATABASE_USERNAME`, `JDBC_DATABASE_PASSWORD`. |
| `redis` | Prefer `REDIS_URL` (includes credentials, database, and TLS scheme). Split clients must also honor `REDIS_PASSWORD`, `REDIS_USERNAME`, `REDIS_DB`, `REDIS_TLS`, not just `REDIS_ADDR`/`REDIS_HOST`. |
| `s3` | `AWS_ENDPOINT_URL_S3`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`; `S3_URL` is the same endpoint. Configure path-style addressing, e.g. `forcePathStyle: true` for the JS SDK. |
| `openai` | Use `OPENAI_BASE_URL`, `OPENAI_API_KEY`, and `OPENAI_MODEL`. HTTP calls use a bearer token; streaming is supported. |
| `exa` | Configure `EXA_BASE_URL` explicitly; send `EXA_API_KEY` as `x-api-key` or bearer for `POST /search` and `POST /contents` relative to that base. |

Managed PostgreSQL is a single instance without backups. Use an external
database when production requirements need more. S3 has no pre-created
bucket: create an app-specific bucket and tolerate an already-owned bucket.
Managed OpenAI and Exa bindings carry proxy tokens; their upstream keys
remain on the platform.

The installation may offer additional provider or HTTP services and restrict
which offerings a space can create. Use the catalog's parameters (`--param
name=value` on `addon add`); an unavailable offering needs operator/space
admin configuration, not a replacement guessed from its protocol.

For existing apps, choose `addon add` for a platform service or `addon attach`
for an external service. Both update `donkey.yaml` (formatting/comments may
change) and redeploy a deployed app using its current image:

| Command | Use |
|---|---|
| `donkey addon add postgres --name db --storage 5Gi` | Create/connect a managed database. |
| `donkey addon add exa` | Connect the platform's Exa proxy. |
| `donkey addon attach exa --url URL` | Attach an external endpoint; set its supplied `EXA_API_KEY` with `donkey env set`. |

External PostgreSQL/Redis URLs carry their connection credentials. External
S3, OpenAI, and Exa URLs carry only endpoints; supply credentials with
`donkey env set`, plus `AWS_REGION` or `OPENAI_MODEL` as needed by the client.
Use endpoints reachable from Donkey's runtime, not just
your workstation. URL paths can prefix OpenAI/Exa gateways. Attaching a new
URL to an existing service updates every attached app; use `--name` with a
distinct name when only one app should use the new endpoint.

## Deploy and verify

```sh
donkey deploy
donkey status
donkey logs --tail 100
```

Read deployment findings and build/rollout errors. A successful deploy waits
for health unless `--no-wait` is used. Confirm the intended image is running;
healthy status alone may describe the previous version after a failed deploy.

| Option | Use |
|---|---|
| `--dry-run` | Print proposed Kubernetes objects without deploying. |
| `--build-only` | Build and push; return the image reference. |
| `--no-build --tag REF` | Deploy a previously built image from Donkey's registry. |
| `-v` | Full build output. |

For a monorepo, put a config in each app folder and use the same space.
Deploy individually with `--app-dir`, or from the root: without a root
config, the CLI finds configs up to three levels down and deploys the
shallowest matching level in path order. A failure stops the run; earlier
apps remain deployed. Each CLI upload contains only that app's folder.

Use `donkey status` for the URL and `donkey open` to open it. An internal app
has no public URL. For HTTP checks:

- Protected web routes accept the CLI login token as bearer; a `401` without
  it is an authentication result, not evidence that the app failed.
- Protected `mode: api` routes need an OAuth access token, not the CLI token.
  MCP clients can complete the advertised OAuth flow.
- Private-CA installations require the configured CA file for HTTPS clients.

If HTTP credentials are unavailable, report verification from status/logs
and the remaining HTTP check; do not make the app public to bypass login.

On failure, read the diagnosis, logs, and proposed edits. Unattended runs
and `--yes` do not apply proposed file edits; apply relevant fixes yourself
and redeploy. After `Image ready: REF`, a deployment retry can use
`--no-build --tag REF` if the fix needs no source rebuild. Use `donkey doctor`
for platform failures; do not repeatedly retry missing credentials or
unavailable operator-managed dependencies.

## Inspect services and persistent files

`donkey logs <service-name>` shows a managed service declared by this app.
`donkey addon tunnel <name> --port <local-port>` prints binding variables
rewritten for localhost; the tunnel lasts while the command runs. External
and binding-only services have no tunnel or managed logs.

File paths are disk-relative: `/app/data` maps to `app-data/`, and
`/var/lib/app` to `var-lib-app/`. Start with `donkey files ls`; leading
slashes are rejected. Files remain accessible while the app is stopped.

```sh
donkey files put seed.db app-data/seed.db
donkey files get app-data/result.csv -o result.csv
donkey files mv app-data/old.csv app-data/new.csv
```

## Remove only what was requested

| Command | Effect |
|---|---|
| `donkey addon remove db` | Disconnect this app; the shared service and data remain. |
| `donkey addon remove db --space --keep-data` | Remove the shared service, disconnect all apps, retain its data. |
| `donkey addon remove db --space --force` | Remove the shared service and delete its data. |
| `donkey destroy --keep-data --force` | Remove the app, retain its disk and credentials. |
| `donkey destroy --force` | Remove the app and its data; space services remain. |
| `donkey files rm app-data/file --force` | Delete the selected persistent file. |

Use data-deleting forms only when data deletion is requested. Donkey has no
built-in snapshots/restore. `--force` skips a CLI confirmation; it does not
change the requested scope. OpenAI, Exa, and HTTP proxy services have no data
to retain and reject `--keep-data`. Removing an app leaves its local source
and `donkey.yaml` intact.
