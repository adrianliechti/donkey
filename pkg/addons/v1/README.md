# Addon plugin API v1

An addon plugin is an independently deployed gRPC service. The public
[protobuf contract](addon.proto) and generated Go client/server bindings live
in this module (`github.com/adrianliechti/donkey/pkg/addons`); other
languages can generate bindings from the same proto. The module depends only
on gRPC and protobuf, so a plugin does not inherit Donkey's dependencies.
The plugin owns its backing resources and credentials. Donkey owns the
catalog, app bindings, and consumer-side network policy.

Register plugins in the installation settings:

```yaml
addonPlugins:
  - type: mariadb
    address: mariadb-plugin.donkey-addons.svc:50051
    insecure: true
    tokenFile: /var/run/secrets/donkey/mariadb-plugin-token
```

The installer passes this array as JSON in `DONKEY_ADDON_PLUGINS`. TLS with
system trust roots is the default; `caFile` replaces the roots and
`certFile`/`keyFile` present a client certificate. `insecure: true`
explicitly chooses plaintext for a trusted private connection, as in the
isolated example. `token` or `tokenFile` adds a bearer token to every call
(`authorization: Bearer …`), so the plugin can tell Donkey from anyone else
who reaches it; the file paths are read by donkey-server. The example plugin
checks the token when `PLUGIN_TOKEN` or `PLUGIN_TOKEN_FILE` is set.

A configured plugin is registered at startup whether or not it answers.
Donkey calls `Describe` lazily, caches the answer for half a minute, and
keeps the last good description through an outage, so a plugin restart never
takes the control plane down and a plugin release that changes its
description needs no Donkey restart. Until a plugin answers, the catalog
lists it as unavailable. A plugin whose description is invalid, or which
answers with another type, stays unavailable. Plugin types cannot replace
built-in types.

## Lifecycle

1. `Describe` returns the catalog entry: `type`, `title`, `description`, the
   application `protocol`, the JSON Schema of the plugin's `parameters`, the
   environment contract `env`, and `capabilities`. It must not create
   resources or return secrets.
2. `Provision` receives a stable identity (`installation`, `space`, `name`),
   runtime, consumer namespace and labels, and the `parameters` — validated
   against the schema with defaults applied. It streams `ProvisionEvent`s:
   progress with `message`, then one final event carrying `result` once the
   service is ready for connections. Donkey bounds the whole stream to five
   minutes and shows the messages in its log.
3. The result contains `env`, `urls`, `endpoints` and, when they differ from
   the request, the effective `parameters` (a grown disk, a resolved
   version). `env` keys must be declared in `Describe.env`; non-optional
   keys are required. The values become the app's environment through
   existing bindings, including `${db.DATABASE_URL}` aliases. URLs, endpoint
   metadata and parameters are stored separately and never injected as env
   variables.
4. `Deprovision` removes the backing resources. With `keep_data`, retain both
   data and the credentials needed to reopen it; the next provision for the
   same identity must reuse them. Donkey sends `keep_data` only to plugins
   declaring the `keep-data` capability and bounds this RPC to two minutes.

All mutations must be idempotent and safe under concurrent retries. A timeout
or lost stream does not prove that provisioning failed: Donkey records the
intent, the parameters and the last error before and after calling the
plugin, and reuses that identity for retry and removal. A repeat request for
a retained or failed instance carries the parameters it was created with
unless the caller changes them, so a plugin's defaults moving on does not
silently upgrade an existing service. Removing an app attachment leaves the
service alone. Removing the space also deprovisions plugin instances,
including incomplete ones. Instances without a binding show in the space's
service list as `pending`, `failed` (with the message) or `stopped`, so they
can be retried or removed from the console and the CLI.

If a plugin is decommissioned or keeps failing, `donkey addon remove NAME
--space --orphan` drops Donkey's record of an instance and leaves its backing
service to the operator. Removing a space still requires each plugin to
answer, so orphan such instances first.

Provision responses and saved state contain credentials. Treat them as
secrets and avoid logging them. Plugins generate and persist credentials;
Donkey does not send its registry, cluster, or installation credentials.
Each plugin should have its own scoped permissions.

## Parameters

`DescribeResponse.parameters` is a JSON Schema (draft 2020-12) for one
object. Donkey validates every request against it, applies the top-level
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
| `x-donkey-immutable` | The value cannot change once the service exists (a disk cannot shrink). Donkey refuses such a change; the plugin need not. |
| `x-donkey-advanced` | Hidden behind the console's "advanced settings" toggle. |
| `x-donkey-order` | Ascending sort key; unordered fields follow by name. |

A MariaDB plugin might describe:

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

Capabilities are free-form lowercase strings with two well-known values:
`runtime:kubernetes` and `runtime:docker` limit the plugin to those runtimes
(none listed means every runtime; the catalog marks a plugin unavailable
elsewhere), and `keep-data` says `Deprovision` honours `keep_data`.

## Application protocol and consoles

`protocol` describes how clients talk to the service, independently of its
type or location. Use `postgres` for PostgreSQL, `mysql` for MySQL/MariaDB,
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

For known protocols, `Describe.env` must include the connection URL key;
the provisioning result must contain a usable URL for that protocol.
Additional app variables are allowed when declared; mark credentials
`secret` so consoles and logs mask them, and `optional` for keys a result may
leave out. Custom protocols keep their own env contract and display a generic
service icon without a specialized console. An explicit protocol always
takes precedence over legacy environment inference.

Database plugins supply `DATABASE_URL`: `postgres://…` / `postgresql://…`
for PostgreSQL, or `mysql://user:password@host:3306/database` for MySQL/MariaDB.
Percent-encode credentials in URLs. MySQL URLs accept `tls`, `charset`,
`collation`, `timeout`, `readTimeout`, `writeTimeout`, `parseTime`, and `loc`;
`tls=true` verifies the server certificate using system trust roots.

Both SQL editors support database/table listing, table creation, row
insertion/editing/deletion, sorting/paging, and SQL statements. Grid edits
require a primary key; SQL queries return at most 500 rows and requests are
bounded to 30 seconds. Every action uses the binding's database user and
permissions. Donkey's server must be able to reach the database as well as
the application; a plugin's ingress policy should admit it.

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
configuration changes and service removal. Plugins manage destination-side
ingress using `Instance.namespace` and `Instance.consumer_labels`. Donkey
needs no permission to write in the plugin's namespace.

Standard Kubernetes NetworkPolicy cannot target a URL or DNS name. Donkey
does not resolve hostnames into an IP snapshot, which would go stale, or open
all egress as a fallback. A hostname-only endpoint is accepted with open
egress and rejected when egress is fenced; the instance then shows as
`failed` with that reason until the plugin answers with a target. Cross-
namespace Service traffic uses pod selectors because Service address
translation and IP-based policy evaluation vary with the networking
implementation. See the Kubernetes
[NetworkPolicy documentation](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

Docker also supports plugin provisioning and environment bindings; the
plugin must return addresses reachable from the app's Docker network. The
Kubernetes policy fields are only enforced by the Kubernetes runtime.

## Building a plugin

Go services depend on `github.com/adrianliechti/donkey/pkg/addons`, embed
`UnimplementedAddonPluginServer`, implement the three RPCs, and register with
`RegisterAddonPluginServer`. `Provision` writes to its stream: any number of
`ProvisionEvent{Message: …}` followed by one `ProvisionEvent{Result: …}`.
No Donkey internal package is required. To regenerate bindings with `protoc`,
`protoc-gen-go`, and `protoc-gen-go-grpc` installed:

```sh
go generate ./pkg/addons/v1
```

The [MariaDB example](../../../examples/addon-mariadb/README.md) includes a
standalone service, container build, namespace-scoped RBAC, ingress policies,
bearer-token authentication, and app configuration. The API supports
plugin-managed provisioning; URL-only `addon attach --url` remains a built-in
addon capability. The console selects PostgreSQL or MySQL/MariaDB editors by
protocol. The generic service tunnel also works with a native database
client.
