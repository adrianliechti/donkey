# examples — what a finished donkey project looks like

Where [testdata/](../testdata/) holds the awkward cases donkey has to get
right unaided, this folder holds the opposite: apps written the way you'd
write yours once you know the platform, saying everything explicitly and
using everything the platform offers. Deploy them, read them, copy from
them.

The [MariaDB addon plugin](addon-mariadb/README.md) demonstrates an independent
gRPC provisioner, with database resources in a separate namespace and explicit
app environment bindings and network policy destinations.

## demo/ — an AI-run helpdesk: two apps, one address, three services

The showcase `task demo` deploys (`hack/demo-app.sh` does the two deploys):
**Packmule Support**, a helpdesk for a fictional file-sync product, where
every platform feature does a visible job.

- **One address for two apps**: the `dashboard` (React/Tailwind) is the only
  thing on the internet; the `backend` (FastAPI + FastMCP) is
  `internal: true` — no hostname, no certificate — yet claims `/api` and
  `/mcp` on the dashboard's address via `routes:`. The gateway does the
  forwarding; there is no proxy code anywhere.
- **Three space services, put to real work** (`addons:` provisions them on
  first deploy):
  - the desk itself — tickets and threads — in `postgres`, self-seeded with
    a believable backlog on first contact,
  - per-ticket attachments in `rustfs` (plain S3 via boto3; the bucket *is*
    the file store, no table mirrors it),
  - the AI through the platform's `openai` proxy: it triages tickets
    (category, priority, sentiment, a one-line summary), drafts replies as
    a live token stream, and invents a fresh walk-in customer on the
    "Simulate a ticket" button —
  all reached only through injected env; nothing in the folder holds an
  address or a credential. Until a service exists, the affected endpoints
  answer 503 with a hint and the rest carries on.
- **The app's own disk** (`storage:`): the desk's activity log is an
  append-only file under `/app/data` — the one piece of state neither in
  postgres nor in the bucket, and the one part of an app a redeploy cannot
  rebuild.
- **The same desk again, as MCP tools**: `tickets_queue`, `ticket_read`,
  `ticket_reply`, `ticket_triage`, `attachment_read`, `desk_overview` — the
  same cores the REST endpoints call, so an assistant working the queue and
  an agent watching the dashboard always see the same desk.

```sh
cd examples/demo/backend   && donkey --yes deploy
cd examples/demo/dashboard && donkey --yes deploy
```
