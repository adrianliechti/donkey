# MariaDB addon provider

This independent gRPC service offers one catalog entry, `mariadb`, and
provisions it in `donkey-addons`, separate from the namespaces containing
Donkey apps. It imports only the public
[addon provider API](../../pkg/addons/v1/README.md) module
(`github.com/adrianliechti/donkey/pkg/addons`), so its code can live in
another repository without changing Donkey. Copy the Go files into your own
module and depend on that module plus the gRPC and Kubernetes client
libraries.

Each `(installation, space, name)` gets its own Deployment, Service, volume,
credential Secret, and ingress NetworkPolicy. Stable names and stored
credentials make repeat provisioning safe. The example runs one replica and
serializes mutations per instance; use distributed coordination before
scaling it.

`Offerings` lists the `mariadb` offering with two parameters: `version` (a
choice of MariaDB releases) and `storage` (a disk size). The console renders
them from that schema; the CLI takes `--version`, `--storage` or `--param
storage=5Gi`. `Provision` streams progress while it applies resources and
waits for readiness, and reports the effective size: a disk grows to what is
asked but never shrinks. The offering declares `retain_data` and `update`,
so a removed instance can keep its disk and a running one can grow it.

## Install

Build from the Donkey repository root:

```sh
docker build -f examples/addon-mariadb/Dockerfile -t donkey-addon-mariadb:dev .
```

Make that image available to the cluster. For a kind cluster called `donkey`:

```sh
kind load docker-image donkey-addon-mariadb:dev --name donkey
kubectl apply -f examples/addon-mariadb/plugin.yaml
kubectl -n donkey-addons rollout status deployment/mariadb-plugin
```

For another cluster, push the image to your registry and change its reference
in `plugin.yaml`. A default StorageClass is needed for database volumes.
The manifest assumes Donkey's system namespace is `donkey-system`. If yours
differs, change `DONKEY_INSTALLATION` and both namespace/installation values
in the plugin's ingress policy.

Add this to your existing server installation settings, then run the usual
`donkey server install -f server-install.yaml`:

```yaml
addonPlugins:
  - name: mariadb
    address: mariadb-plugin.donkey-addons.svc:50051
    insecure: true
kubernetes:
  isolateSpaces: true
  restrictEgress: true
```

The example uses plaintext gRPC on a ClusterIP, with ingress limited to
`donkey-server`. Test isolation with a CNI that enforces NetworkPolicy;
kind's default networking alone does not demonstrate policy enforcement.
For TLS, mount a certificate and key, set `TLS_CERT_FILE` and `TLS_KEY_FILE`,
remove `insecure: true`, and replace the plaintext gRPC readiness probe with
a suitable probe. The certificate must be trusted by Donkey and cover the
configured plugin hostname.

To make the plugin refuse callers other than Donkey, give both sides the same
secret: mount it into the plugin as `PLUGIN_TOKEN_FILE` (or set
`PLUGIN_TOKEN`) and into donkey-server, then add `tokenFile` (or `token`) to
the `addonPlugins` entry. The health check stays open for the readiness
probe; every addon call without the bearer token is rejected.

## Connect an app

Declare the service in the app's `donkey.yaml`:

```yaml
name: api
space: shop
addons:
  - type: mariadb
    name: db
    version: "11.8"
    storage: 1Gi
env:
  DB_URL: ${db.DATABASE_URL}
```

Deploy as usual. Existing apps can also use `donkey addon add mariadb --name
db --param storage=5Gi`. Donkey injects `DATABASE_URL`, `MYSQL_HOST`,
`MYSQL_PORT`, `MYSQL_DATABASE`, `MYSQL_USER`, and `MYSQL_PASSWORD`. `DB_URL`
above is an optional app-specific alias. The database is `app` and the
non-root user is `donkey`; passwords are generated once and retained with the
data. A service that is re-added after `--keep-data` resumes with the
parameters it was created with.

The offering declares `protocol: mysql`, which selects the MySQL/MariaDB
web editor regardless of the provider or its namespace. The response
includes a `mysql://…/app` connection in `DATABASE_URL`, host and port 3306,
and a Kubernetes destination with namespace `donkey-addons` and that
instance's pod labels. Donkey permits egress from the `shop` space to those
pods on TCP 3306. The plugin creates the complementary ingress rule for
that space and for Donkey's admin server. Other spaces and plugin control
ports are excluded from these grants.

Use `donkey addon tunnel db` with a MariaDB client for interactive access.
Click the service in the space's web console to browse tables, edit rows,
create tables, or run SQL. The editor uses the same non-root database user
as the app; creating additional databases requires a user with that permission.
Removing the service with
`donkey addon remove db --space` invokes `Deprovision`; `--keep-data` keeps
the volume and its credential Secret. Re-adding resumes it with the same
password. Deleting the entire space removes its plugin instances and data.
If the plugin itself is gone, `--orphan` drops Donkey's record and leaves the
MariaDB resources for you to clean up.

## Checks

From the repository root:

```sh
go test ./internal/addons ./pkg/runtime/kubernetes/... ./examples/addon-mariadb
```

These tests cover the gRPC lifecycle, the parameter schema, persisted
provisioning intent and parameters, app bindings, policy selectors and
ports, and MariaDB credential/data retention using Kubernetes API fakes. They do not prove live CNI behavior. With your
cluster, deploy an app that uses the injected credentials and verify a SQL
query succeeds with `restrictEgress` enabled; from another space, the same
database destination should be denied.

MariaDB uses the official image's initialization variables and
[`healthcheck.sh`](https://mariadb.com/docs/server/server-management/automated-mariadb-deployment-and-administration/docker-and-mariadb/using-healthcheck-sh).
The plugin keeps credentials because MariaDB's
[initialization variables](https://mariadb.com/docs/server/server-management/automated-mariadb-deployment-and-administration/docker-and-mariadb/mariadb-server-docker-official-image-environment-variables)
do not reset passwords in an existing data directory.
