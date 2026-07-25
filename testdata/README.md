# testdata — apps to deploy, and the challenges they pose

Every folder here is a real app you can deploy against a running donkey
(`task demo` sets one up). They are not a test suite: they are the awkward
cases donkey has to get right, each isolating one of them so a regression
tells you *what* broke.

Deploy any of them the same way:

```sh
cd testdata/<sample> && donkey deploy
```

## The demo

| Sample | Stack | The challenge |
|---|---|---|
| `demo/` | FastAPI + FastMCP + DuckDB, and a React/Tailwind dashboard | **One** address for two apps: the dashboard is the only thing on the internet and forwards `/api` and `/mcp` to a backend marked `internal: true` — no hostname, no certificate, reachable only as `http://backend` from inside the space. Plus MCP and REST over one embedded database, and a disk that survives redeploys. This is what `task demo` deploys. |

## Simple

| Sample | Stack | The challenge |
|---|---|---|
| `static-site/` | Plain HTML and CSS | No build, no framework, no config, no `donkey.yaml`. The smallest thing anyone can deploy — and the case where any question donkey asks is one question too many. |
| `dockerfile-sample/` | A `Dockerfile` | The user's own Dockerfile always wins over automatic detection. |
| `fastapi-sample/` | Python | Ordinary detection: a `Procfile`, `$PORT`, and a real `/healthz`. |

## Not written by developers

The apps most likely to arrive at a PaaS were not written by someone thinking
about containers. They run on a laptop, and every one of these breaks in a way
that is invisible from the outside: the container starts, the log says
"Running on…", and nothing answers.

| Sample | Stack | The challenge |
|---|---|---|
| `flask-beginner/` | Python, one file | `app.run(debug=True, port=5000)` — binds **127.0.0.1**, hardcodes the port, ships the debugger, and declares no start command or health route. The single most common shape a first app arrives in. |
| `streamlit-report/` | Streamlit | An analyst's script that grew a UI. Not run by `python app.py` but by the `streamlit` binary, defaults to 8501, binds loopback unless told otherwise, needs websockets, and has a health route nobody knows about (`/_stcore/health`). The hardest detection case here. |
| `no-manifest/` | Python | Imports `flask` and lists it **nowhere** — no requirements.txt, no lockfile. Saved out of a chat window and it ran, because that laptop happened to have flask. |
| `My Cool App/` | Static files | A folder named the way people name folders: spaces, capitals, `main style.css`, `IMG_2024 (1).svg`. The name has to become `my-cool-app` and every file has to survive the round trip unmangled. |

## Awkward on purpose

| Sample | Stack | The challenge |
|---|---|---|
| `local-habits/` | React + Vite | Written the way people write things on a laptop: a `fetch('http://localhost:3001/api')` that runs in the **browser** (always wrong once deployed), a vite devServer proxy that is dev-only (donkey must *not* warn about it), and a `.env` that must never leave the machine. Distinguishing the first from the second is the whole test. |
| `sqlite-notes/` | Python + SQLite | Writes to `./data` and never says so. Without a disk the notes vanish on the next restart, and nothing about the app looks wrong until they do. |
| `monorepo-root/` | Repository root | The app is not at the top: it lives in `services/api/`. Uploading the root must find it, say which folder it picked, and refuse to guess when several are candidates. |
| `odd-port-sample/` | Node | Ignores `$PORT` and pins an unusual one. Provider defaults would guess wrong; only reading the source gets it right. |
| `kitchen-sink-sample/` | Python, React, Go, Dockerfile, **Tcl** | Five folders, no `donkey.yaml` anywhere. The Tcl one is recognized by nothing, so the assistant has to write the build itself. |

## What a good run looks like

- `static-site/` and `fastapi-sample/` deploy without a single question.
- `flask-beginner/` and `streamlit-report/` are warned about **before** they
  fail: "this app listens on 127.0.0.1, which in a container means itself and
  nobody else". A rollout timeout with no explanation is a failure of donkey,
  not of the sample.
- `no-manifest/` either gets its dependency worked out, or is told which one
  is missing — never a `ModuleNotFoundError` from inside a build.
- `My Cool App/` lands at `my-cool-app.<space>.…` with the stylesheet and the
  image both loading.
- `local-habits/` deploys **and warns** about `App.jsx`, while staying silent
  about `vite.config.js`.
- `sqlite-notes/` proposes a disk for `./data` before the first deploy.
- `monorepo-root/` builds `services/api` and says that it did.
- `kitchen-sink-sample/legacy` needs the assistant; without one configured,
  it should fail with a clear reason rather than a stack trace.

## Using them as a suite

These are deliberately awkward, so failures are information. When one of them
cannot be deployed at all, the question to ask is not "how do we fix the
sample" — it is whether the person who wrote that app would have understood
what donkey told them.
