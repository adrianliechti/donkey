# examples — what a finished donkey project looks like

Where [testdata/](../testdata/) holds the awkward cases donkey has to get
right unaided, this folder holds the opposite: apps written the way you'd
write yours once you know the platform, saying everything explicitly and
using everything the platform offers. Deploy them, read them, copy from
them.

## demo/ — two apps, one address, three services

The showcase `task demo` deploys (`hack/demo-app.sh` does the two deploys):

- **One address for two apps**: the `dashboard` (React/Tailwind) is the only
  thing on the internet; the `backend` (FastAPI + FastMCP + DuckDB) is
  `internal: true` — no hostname, no certificate — yet claims `/api` and
  `/mcp` on the dashboard's address via `routes:`. The gateway does the
  forwarding; there is no proxy code anywhere.
- **MCP and REST over one embedded database**: sales analytics in DuckDB on
  the app's disk (`storage:`), served to the dashboard as JSON and to
  assistants as MCP tools — both readers see the same numbers.
- **Three space services, put to real work** (`addons:` provisions them on
  first deploy):
  - shared notes in `postgres`,
  - attachments in `rustfs` (plain S3 via boto3),
  - note summaries through the platform's `openai` proxy —
  all reached only through injected env; nothing in the folder holds an
  address or a credential. The notes and files are also MCP tools, so an
  assistant can do what the dashboard does. Until a service exists, the
  affected endpoints answer 503 with a hint and the rest carries on.

```sh
cd examples/demo/backend   && donkey --yes deploy
cd examples/demo/dashboard && donkey --yes deploy
```
