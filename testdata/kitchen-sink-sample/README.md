# kitchen-sink-sample

One monorepo that exercises every automatic behavior donkey has. No
`donkey.yaml` anywhere — each folder deploys with `donkey --yes deploy`.

| Folder | Stack | What it proves |
|---|---|---|
| `api/` | Python / FastAPI | railpack detection, `$PORT` via Procfile, HTTP probes once `/healthz` is known, Postgres addon binding (`/db-check`), env vars |
| `web/` | React / Vite | node detection with a build step, static serving on injected `$PORT` |
| `worker/` | Go | golang detection, `$PORT` convention, **no** health route → zero-config TCP probe |
| `gateway/` | Dockerfile + decoy `package.json` | a user Dockerfile always beats railpack detection |
| `legacy/` | Tcl (!) | nothing recognizes it → AI detection fallback writes `.donkey/Dockerfile` (needs AI configured on the server) |

Each folder deploys against a running donkey server with a plain
`donkey --yes deploy` (`task demo` sets one up locally).
