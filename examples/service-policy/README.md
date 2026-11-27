# Space-wide service policy demo

This local Donkey installation applies one Rego policy to each space. Six fixed
HTTP proxies provide the sample data sources and destinations.

| Offering | Classification | Access | Backend authorization |
| --- | --- | --- | --- |
| `public-data` | C1 | Read | Shared |
| `team-data` | C2 | Read | Shared |
| `internal-data` | C3 | Read | Delegated |
| `case-files` | C4 | Read | Delegated |
| `reporting` | C2 | Write | Shared |
| `archive` | C4 | Write | Delegated |
| `postgres` | C4 | Read and write | Shared database credentials |

## Run locally

Use a Docker engine available for a Donkey installation, Docker Compose
v2.24.4 or newer, Go as specified in go.mod, Python 3, tar, OpenSSL and curl.
The demo uses the standard Donkey container names and binds its published
ports to loopback. Ports 80, 443, 5556, 8088 and 15000 must be available.
Run this command from the repository root.

```sh
examples/service-policy/demo.sh up
```

The script builds Donkey from this checkout, renders the installation, starts
Compose, creates the `classified` and `reporting` spaces, and exercises both.
It keeps generated settings and local signing keys in
`.build/service-policy-demo/bundle`. The identity provider signs you in as
`demo@example.com`. All records and bearer tokens in this demo are synthetic.
The upstream acknowledges writes without storing them and supplies a fixture
for the model readiness check.

Open the [local console](https://policy.localhost). The gateway uses a local
certificate authority. If your browser asks, accept the local demo certificate.

## Try the policy

Open the `classified` space and choose **Add a service**. The space already has
one reader at each level and the C4 archive. Choosing `reporting` shows the
C4-to-C2 denial. Choosing `postgres` shows the requirement for delegated
authorization on writers alongside `case-files`. The `reporting` space has
its own C2 writer. Adding the C4 reader there shows the reverse selection
order. Changing the app or renaming the service preserves the decision.

In the `classified` space, open the `archive` service’s HTTP console and send
a `POST` to `/`. An empty Authorization header returns HTTP 401.
`Authorization: Bearer demo-reader` returns HTTP 403.
`Authorization: Bearer demo-archivist` returns HTTP 200. The upstream checks
these demo permissions on every request.

```sh
examples/service-policy/demo.sh verify
examples/service-policy/demo.sh status
examples/service-policy/demo.sh stop
```

`stop` preserves both spaces. Run `up` again after editing the
[installation settings](server.yaml) or the [Rego policy](../service-policy.rego).

## Try the gateway maintenance page

The base installation starts with every gateway maintenance rule off. To opt in
locally, run the following after `up`.

```sh
examples/service-policy/demo.sh gateway
```

This deploys four public sample apps through the Donkey API. The demo configures
30 days for critical vulnerabilities and seven days each for unmaintained apps
and noncompliant spaces. It supplies a synthetic scan finding, deploys an old
build tag, and tightens the service policy on an existing space. It waits for
the orchestrator to record each condition, then backdates those three demo
timestamps by 31 days. The running gateway serves the following results.

| App | HTTPS response |
| --- | --- |
| [Healthy control](https://healthy.gateway-demo.policy.localhost/) | 200 from the app |
| [Critical vulnerability](https://critical.gateway-demo.policy.localhost/) | 503 owner-action page |
| [Unmaintained build](https://stale.gateway-demo.policy.localhost/) | 503 owner-action page |
| [Noncompliant space](https://web.gateway-drift.policy.localhost/) | 503 owner-action page |

Open `gateway-drift` in the [console](https://policy.localhost) to see the
policy's reason. Adding a `public-data` service satisfies the new rule.
Platform settings are available from the account menu. The demo exports its
public CA certificate to `.build/service-policy-demo/ca.crt` for local HTTPS checks.

```sh
examples/service-policy/demo.sh gateway-check   # verify the overdue fixtures
examples/service-policy/demo.sh gateway-repair  # fix all three and verify all four return 200
examples/service-policy/demo.sh gateway         # recreate the overdue fixtures
examples/service-policy/demo.sh gateway-off     # disable all three rules and verify traffic
```

`gateway-repair` supplies a clean synthetic scan, deploys a fresh build tag, and
adds a proxy named `demo-remedy` to satisfy the new policy. Replaying `gateway`
redeploys the sample apps and removes that disposable proxy if it has no connections.
