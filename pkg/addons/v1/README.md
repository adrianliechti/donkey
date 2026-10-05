# Addon provider API v1

An addon provider is an independently deployed gRPC service that serves a
catalog of offerings (databases, caches, APIs, anything an app can be
connected to) and provisions instances of them. The public
[protobuf contract](addon.proto) and generated Go client/server bindings live
in this module (`github.com/adrianliechti/donkey/pkg/addons`); other
languages can generate bindings from the same proto. The module depends only
on gRPC and protobuf, so a provider does not inherit Donkey's dependencies.
The provider owns its backing resources and credentials. Donkey owns the
catalog listing, app bindings, and consumer-side network policy.

Register providers in the installation settings:

```yaml
addonPlugins:
  - name: acme
    address: acme-provider.donkey-addons.svc:50051
    insecure: true
    tokenFile: /var/run/secrets/donkey/acme-provider-token
```

`name` identifies the provider in logs and listings; the catalog ids come
from the provider itself. The installer passes this array as JSON in
`DONKEY_ADDON_PLUGINS`. TLS with system trust roots is the default; `caFile`
replaces the roots and `certFile`/`keyFile` present a client certificate.
`insecure: true` explicitly chooses plaintext for a trusted private
connection, as in the isolated example. `token` or `tokenFile` adds a bearer
token to every call (`authorization: Bearer …`), so the provider can tell
Donkey from anyone else who reaches it; the file paths are read by
donkey-server. The example provider checks the token when `PLUGIN_TOKEN` or
`PLUGIN_TOKEN_FILE` is set.

## Availability and space configuration

Provider registration is installation-wide. `commonOfferings` in the install
file chooses the offerings available in every space:

```yaml
commonOfferings: [postgres, redis, rustfs, openai]
```

An omitted or empty list allows **all** offerings, including plugins. With a
nonempty list, space admins can enable additional offerings in space settings,
beside membership and domains. The space's `config.yaml` can contain:

```yaml
offerings:
  mariadb:
    defaults:
      storage: 10Gi
    config:
      storageClass: fast
  corporate-api:
    config:
      tenant: finance
      plan: enterprise
```

An entry enables the offering in this space, including an empty entry `{}`.
An entry for a common offering customizes it. `defaults` overrides the
provider's parameter defaults for **new** services; explicit app parameters
win, and existing instances keep their saved parameters.

`Offering.space_configuration` is a separate JSON Schema, using the same
subset as `parameters`. Its values come only from `offerings.<id>.config`,
which only space and platform admins can edit. Required values without
defaults show **Needs configuration** in the space catalog and block new
provisioning. Partial configuration can be saved for completion later;
unknown names and invalid values are rejected.

`ProvisionRequest.space_configuration` carries the resolved settings;
`previous_space_configuration` carries the settings of the previous request.
Apps cannot supply them through parameters, and Donkey does not include them
in service listings, app bindings, or the app environment. The provider
chooses the app's environment through its declared response contract.

Donkey records configuration before calling the provider. If the space later
removes the offering entry, existing instances reuse their saved configuration
for retries and resumption and remain removable. Existing app connections keep
working; new instances and new connections must use the current space catalog.
Changes to admin settings take effect on the next explicit provisioning request;
they do not automatically reprovision existing services. Immutable settings and
the offering's `update` capability apply to admin configuration too.

`GET /v1/catalog` describes the registered catalog.
`GET /v1/spaces/{space}/catalog` returns the available offerings for that space,
with its parameter defaults and configuration readiness, without configuration
values. Installers pass `commonOfferings` as JSON in `DONKEY_COMMON_OFFERINGS`.

## When Donkey calls a provider

Nothing is called at startup: registering a provider only prepares a lazy
connection. `Offerings` is called the first time something needs the
catalog (a listing, a deploy naming an offering) and the answer is cached.
A listing serves the cached catalog immediately and refreshes it in the
background after half a minute; a provider that has never answered delays a
listing by at most three seconds and is shown once, under its name, as
unavailable. A failing provider is probed again with growing gaps (30
seconds doubling to five minutes), so a dead one costs one probe per gap.
The last good catalog outlives an outage.

Every instance records the offering it was provisioned with. Removing,
stopping, resuming or retrying an instance reads its capabilities and
contract from that record and only calls `Provision` or `Deprovision`, so an
instance never waits on its provider describing itself again; if the
provider is down, the provisioning call itself reports it. Offering ids must
be unique across the installation: the built-in ids (`postgres`, `redis`,
`rustfs`, `openai`) are reserved, and a catalog that repeats an id, or one
another provider already serves, is rejected as a whole.

## Lifecycle

1. `Offerings` returns the catalog: for each offering its `id`, `title`,
   `description`, `category`, `tags`, optional `icon` and
   `documentation_url`, the application `protocol`, the JSON Schema of its
   `parameters` and `space_configuration`, the environment contract `env`,
   its `capabilities`, and
   whether it is `shared` (provisioning grants access to one
   installation-wide service, such as an API, rather than creating a
   dedicated instance) or `deprecated` (usable for existing instances,
   hidden from the catalog). It must not create resources or return
   secrets.
2. `Provision` receives a stable identity (`installation`, `space`, `name`,
   `offering`), on Kubernetes the consumers' namespace and pod labels, the
   `parameters` validated against the schema with defaults applied, the
   `previous_parameters` of the last request for the instance, separate admin
   settings in `space_configuration` and `previous_space_configuration`, and a
   `reason`: `CREATE` for a new instance, `UPDATE` for a running one whose
   parameters may have changed, `RESUME` for one stopped with `keep_data`,
   `RETRY` after a failed or lost attempt. It streams `ProvisionEvent`s:
   progress with `message`, then one final event carrying `result` once the
   service is ready for connections. Donkey bounds the whole stream to five
   minutes and shows the messages in its log.
3. The result contains `env`, `endpoints` and, when they differ from the
   request, the effective `parameters` (a grown disk, a resolved version).
   `env` keys must be declared in the offering's `env`; non-optional keys
   are required. The values become the app's environment through existing
   bindings, including `${db.DATABASE_URL}` aliases. Endpoint metadata and
   parameters are stored separately and never injected as env variables.
4. `Deprovision` removes the backing resources. With `keep_data`, retain
   both data and the credentials needed to reopen it; the next `Provision`
   with reason `RESUME` for the same identity must reuse them. Donkey sends
   `keep_data` only for offerings declaring `capabilities.retain_data` and
   bounds this RPC to two minutes.

Capabilities are explicit flags. `retain_data` says `Deprovision` honours
`keep_data`. `update` says a running instance accepts changed parameters and
admin configuration in place; without it, Donkey refuses a change and asks for the service to be
removed and added again (values marked immutable in the schema are refused
regardless).

All mutations must be idempotent and safe under concurrent retries. A timeout
or lost stream does not prove that provisioning failed: Donkey records the
intent, the offering, the parameters and the last error before and after
calling the provider, and reuses that identity for retry and removal. A
repeat request for a retained or failed instance carries the parameters it
was created with unless the caller changes them, so a provider's defaults
moving on does not silently upgrade an existing service. Removing an app
attachment leaves the service alone. Removing the space also deprovisions
provider instances, including incomplete ones. Instances without a binding
show in the space's service list as `pending`, `failed` (with the message)
or `stopped`, so they can be retried or removed from the console and the CLI.

If a provider is decommissioned or keeps failing, `donkey addon remove NAME
--space --orphan` drops Donkey's record of an instance and leaves its backing
service to the operator without calling `Deprovision`; it cannot be combined
with `--keep-data`. Removing a space still requires each provider to
answer, so orphan such instances first.

Provision responses and saved state contain credentials. Treat them as
secrets and avoid logging them. Providers generate and persist credentials;
Donkey does not send its registry, cluster, or installation credentials.
Each provider should have its own scoped permissions.

## Parameters

`Offering.parameters` is a JSON Schema (draft 2020-12) for one object.
Donkey validates every request against it, applies the top-level
`default`s, renders it as the console's form and accepts it on the CLI as
`--param name=value` or in `donkey.yaml` under `addons[].parameters`
(`version` and `storage` keep their shorthand). Built-in services describe
themselves the same way, so postgres, redis and rustfs offer `version` and
`storage`, and the console does not know about disks in particular.

The supported subset, which the console can render:

- one `object` with `properties`; nested objects and `$ref` are rejected,
- property types `string`, `integer`, `number`, `boolean`, or an `array` of
  those,
- `title`, `description`, `default`, `enum`, `minimum`, `maximum`,
  `pattern`, `required`, and `format: quantity` for a Kubernetes-style size,
- only declared parameters exist: unknown names are rejected, and a stored
  value the schema no longer names is dropped.

Values arriving as strings are coerced to the declared type (`--param
replicas=3`, an unquoted `version: 16` in YAML), and arrays accept
comma-separated strings.

Presentation hints, all optional:

| Keyword | Meaning |
| --- | --- |
| `x-donkey-widget` | `disk-size` offers a ladder of sizes; `select` forces a dropdown. |
| `x-donkey-immutable` | The value cannot change once the service exists (a disk cannot shrink). Donkey refuses such a change; the provider need not. |
| `x-donkey-advanced` | Hidden behind the console's "advanced settings" toggle. |
| `x-donkey-order` | Ascending sort key; unordered fields follow by name. |

A MariaDB offering might describe:

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "version": { "type": "string", "title": "Version", "enum": ["10.11", "11.4", "11.8"], "default": "11.8" },
    "storage": { "type": "string", "title": "Disk", "format": "quantity", "default": "1Gi",
                 "pattern": "^[0-9]+(\\.[0-9]+)?([KMGTPE]i?)?$", "x-donkey-widget": "disk-size" }
  }
}
```

## Catalog metadata

`category` groups the console's catalog: use `database`, `cache`,
`storage`, `messaging`, `ai`, `api`, or a custom lowercase name. `tags` are
free lowercase words. `icon` names an icon when the protocol's is not the
right one (a protocol name such as `postgres`, or a generic one such as
`database`); consoles fall back to the protocol. `documentation_url` links
to the service's own documentation.

## Application protocol and consoles

`protocol` describes how clients talk to the service, independently of its
id or location. Use `postgres` for PostgreSQL, `mysql` for MySQL/MariaDB,
`redis`, `s3`, or `openai`. Custom lowercase names are also allowed; they do
not select a built-in console. `postgresql` and `mariadb` normalize to
`postgres` and `mysql`. This field is separate from `Endpoint.protocol`,
which means TCP, UDP, or SCTP.

The catalog, service listings, and binding API expose the protocol and the
parameters without credentials. They are saved outside the app environment.
Built-in external bindings keep the protocol of their addon type, so an
externally hosted PostgreSQL service still opens the database editor. Old
bindings without metadata use their connection URL scheme for compatibility.

The same protocol selects the catalog icon and console for every provider:

| Protocol | Connection environment | Console |
| --- | --- | --- |
| `postgres` | `DATABASE_URL` (`postgres://` or `postgresql://`) | SQL editor |
| `mysql` | `DATABASE_URL` (`mysql://` or `mariadb://`) | SQL editor |
| `redis` | `REDIS_URL` (`redis://` or `rediss://`) | Key browser/editor |
| `s3` | `AWS_ENDPOINT_URL_S3` (HTTP/HTTPS origin), `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN` and `AWS_REGION` | Bucket/object browser |
| `openai` | `OPENAI_BASE_URL` (HTTP/HTTPS base URL), optional `OPENAI_API_KEY` and `OPENAI_MODEL` | Model chat |

For known protocols, the offering's `env` must include the connection URL
key; the provisioning result must contain a usable URL for that protocol.
Additional app variables are allowed when declared; mark credentials
`secret` so consoles and logs mask them, and `optional` for keys a result may
leave out. Custom protocols keep their own env contract and display a generic
service icon without a specialized console. An explicit protocol always
takes precedence over legacy environment inference.

Database offerings supply `DATABASE_URL`: `postgres://…` / `postgresql://…`
for PostgreSQL, or `mysql://user:password@host:3306/database` for MySQL/MariaDB.
Percent-encode credentials in URLs. MySQL URLs accept `tls`, `charset`,
`collation`, `timeout`, `readTimeout`, `writeTimeout`, `parseTime`, and `loc`;
`tls=true` verifies the server certificate using system trust roots.

Both SQL editors support database/table listing, table creation, row
insertion/editing/deletion, sorting/paging, and SQL statements. Grid edits
require a primary key; SQL queries return at most 500 rows and requests are
bounded to 30 seconds. Every action uses the binding's database user and
permissions. Donkey's server must be able to reach the database as well as
the application; a provider's ingress policy should admit it.

## Network destinations

Each endpoint declares the host/IP and port the app dials, with a TCP
(default), UDP, or SCTP protocol. For restricted Kubernetes egress, it must
also declare one `target`:

- `kubernetes`: the destination namespace and nonempty pod labels. If the
  Service maps to a different pod port, set `kubernetes.port` to that port.
- `cidrs`: explicit, stable external IP ranges. IPv4 and IPv6 are supported;
  default routes are rejected.

Put the primary client connection first among TCP endpoints: the CLI tunnel
uses that destination. Other endpoints may describe additional ports required
by the service. Tunnel output rewrites address fields without changing
credentials, URL paths, or connections to other destinations.

Donkey grants those destinations to the owning space, bounded by installation
and space labels. It only installs these additive policies when both
`isolateSpaces` and `restrictEgress` are enabled, and reconciles them on
configuration changes and service removal. Providers manage destination-side
ingress using `Instance.kubernetes.namespace` and
`Instance.kubernetes.consumer_labels`, which Donkey sets on Kubernetes
installations only; a runtime-agnostic provider ignores them. Donkey needs
no permission to write in the provider's namespace.

Standard Kubernetes NetworkPolicy cannot target a URL or DNS name. Donkey
does not resolve hostnames into an IP snapshot, which would go stale, or open
all egress as a fallback. A hostname-only endpoint is accepted with open
egress and rejected when egress is fenced; the instance then shows as
`failed` with that reason until the provider answers with a target. Cross-
namespace Service traffic uses pod selectors because Service address
translation and IP-based policy evaluation vary with the networking
implementation. See the Kubernetes
[NetworkPolicy documentation](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

Docker also supports provider provisioning and environment bindings; the
provider must return addresses reachable from the app's Docker network. The
Kubernetes policy fields are only enforced by the Kubernetes runtime. A
provider is deployed for one installation and its runtime, so it need not
handle both.

## Building a provider

Go services depend on `github.com/adrianliechti/donkey/pkg/addons`, embed
`UnimplementedServiceProviderServer`, implement the three RPCs, and register
with `RegisterServiceProviderServer`. `Provision` writes to its stream: any
number of `ProvisionEvent{Message: …}` followed by one
`ProvisionEvent{Result: …}`. No Donkey internal package is required. To
regenerate bindings with `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc`
installed:

```sh
cd pkg/addons
go generate ./v1
```

The [MariaDB example](../../../examples/addon-mariadb/README.md) includes a
standalone service with one offering, container build, namespace-scoped
RBAC, ingress policies, bearer-token authentication, and app configuration.
URL-only `addon attach --url` remains a built-in addon capability. The
console selects PostgreSQL or MySQL/MariaDB editors by protocol. The generic
service tunnel also works with a native database client.
