# Addon plugin API v1

An addon plugin is an independently deployed gRPC service. The public
[protobuf contract](addon.proto) and generated Go client/server bindings live
in this package; other languages can generate bindings from the same proto.
The plugin owns its backing resources and credentials. Donkey owns the
catalog, app bindings, and consumer-side network policy.

Register plugins in the installation settings:

```yaml
addonPlugins:
  - type: mariadb
    address: mariadb-plugin.donkey-addons.svc:50051
    insecure: true
```

The installer passes this array as JSON in `DONKEY_ADDON_PLUGINS`. TLS with
system trust roots is the default. `insecure: true` explicitly chooses
plaintext for a trusted private connection, as in the isolated example.
Use an authenticated private network or a TLS proxy to control who can call
your provisioner. Donkey contacts `Describe` at startup and refuses to start
if a configured plugin is unavailable, returns another type, or conflicts
with an existing addon. Plugin types cannot replace built-in types.

## Lifecycle

1. `Describe` returns catalog metadata, the application `protocol`, defaults, and the exact environment
   keys the service supplies. It must not create resources or return secrets.
2. `Provision` receives a stable identity (`installation`, `space`, `name`),
   runtime, consumer namespace and labels, version, and storage. It returns
   once the backing service is ready. Donkey bounds this RPC to five minutes.
3. The response contains `env`, `urls`, `endpoints`, and an optional `protocol`
   that defaults to the description's value (if both are set, they must agree). `env` must match the
   keys from `Describe`; its values become the app's environment through
   existing bindings, including `${db.DATABASE_URL}` aliases. URLs and endpoint
   metadata are stored separately and never injected as extra env variables.
4. `Deprovision` removes the backing resources. With `keep_data`, retain both
   data and the credentials needed to reopen it. The next provision for the
   same identity must reuse them. Donkey bounds this RPC to two minutes.

All mutations must be idempotent and safe under concurrent retries. A timeout
or lost response does not prove that provisioning failed: Donkey records
intent before calling the plugin and reuses that identity for retry/removal.
Removing an app attachment leaves the service alone. Removing the space also
deprovisions plugin instances, including incomplete ones. Failed deletion
retains the lifecycle record for retry; keep the plugin configured until all
its instances, including retained data, have been removed.

Provision responses and saved state contain credentials. Treat them as
secrets and avoid logging them. Plugins generate and persist credentials;
Donkey does not send its registry, cluster, or installation credentials.
Each plugin should have its own scoped permissions.

## Application protocol and web editors

`protocol` describes how clients talk to the service, independently of its
type or location. Use `postgres` for PostgreSQL, `mysql` for MySQL/MariaDB,
`redis`, `s3`, or `openai`. Custom lowercase names are also allowed; they do
not select a built-in editor. `postgresql` and `mariadb` normalize to
`postgres` and `mysql`. This field is separate from `Endpoint.protocol`,
which still means TCP, UDP, or SCTP.

The catalog and service listings expose the protocol without credentials;
the authorized binding API includes it alongside connection values. It is
saved outside the app environment. Built-in external
bindings keep the protocol of their addon type, so an externally hosted
PostgreSQL service still opens the database editor. Old bindings without
metadata use their connection URL scheme for compatibility.

The same protocol selects the catalog icon and console for every provider:

| Protocol | Connection environment | Console |
| --- | --- | --- |
| `postgres` | `DATABASE_URL` (`postgres://` or `postgresql://`) | SQL editor |
| `mysql` | `DATABASE_URL` (`mysql://` or `mariadb://`) | SQL editor |
| `redis` | `REDIS_URL` (`redis://` or `rediss://`) | Key browser/editor |
| `s3` | `AWS_ENDPOINT_URL_S3` (HTTP/HTTPS origin), `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, optional `AWS_SESSION_TOKEN` and `AWS_REGION` | Bucket/object browser |
| `openai` | `OPENAI_BASE_URL` (HTTP/HTTPS base URL), optional `OPENAI_API_KEY` and `OPENAI_MODEL` | Model chat |

For known protocols, `Describe.env_keys` must include the connection URL key;
`Provision.env` must contain a usable URL for that protocol. Additional app
variables are allowed when declared. Custom protocols keep their own env
contract and display a generic service icon without a specialized console.
An explicit protocol always takes precedence over legacy environment inference.

OpenAI-compatible consoles forward requests through an authenticated service
route, using the saved endpoint and credential. This supports private and
external providers without browser CORS access. Operator sessions stay at
Donkey; provider credentials never authorize access to Donkey's platform AI
proxy. The S3 console also supports providers issuing temporary credentials.

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
also declare one of:

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
egress and rejected when egress is fenced. Cross-namespace Service traffic
uses pod selectors because Service address translation and IP-based policy
evaluation vary with the networking implementation. See the Kubernetes
[NetworkPolicy documentation](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

Docker also supports plugin provisioning and environment bindings; the
plugin must return addresses reachable from the app's Docker network. The
Kubernetes policy fields are only enforced by the Kubernetes runtime.

## Building a plugin

Go services import `github.com/adrianliechti/donkey/pkg/addons/v1`, embed
`UnimplementedAddonPluginServer`, implement the three RPCs, and register with
`RegisterAddonPluginServer`. No Donkey internal package is required. To
regenerate bindings with `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc`
installed:

```sh
go generate ./pkg/addons/v1
```

The [MariaDB example](../../../examples/addon-mariadb/README.md) includes a
standalone service, container build, namespace-scoped RBAC, ingress policies,
and app configuration. The initial API supports plugin-managed provisioning;
URL-only `addon attach --url` remains a built-in addon capability. The
console selects its editor by protocol. The generic service tunnel also works
with a native database client.
