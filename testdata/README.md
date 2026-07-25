# testdata — apps to deploy, and the challenges they pose

Every folder here is a real app you can deploy against a running donkey
(`task demo` sets one up). They are not a test suite: they are the awkward
cases donkey has to get right, each isolating one of them so a regression
tells you *what* broke.

Deploy any of them the same way:

```sh
cd testdata/<sample> && donkey deploy
```

## Almost none of them have a donkey.yaml — that is the point

The thing under test is **automatic configuration**: donkey reads the source
and works out the name, the port, the build, the start command, the health
route, and whether the app needs a disk. A sample that declares those in a
`donkey.yaml` is not testing detection; it is testing YAML parsing.

So only two samples ship one, both for reasons that have nothing to do with
convenience:

- **`demo/`** — it is the showcase for what you *can* only say explicitly:
  `internal`, `routes`, `storage`, a fixed `space`.
- **`monorepo-root/services/api/`** — the file *is* the test. Subfolder
  discovery finds an app by looking for a `donkey.yaml`, so removing it would
  remove the thing the sample exercises.

Everything else is bare. When you add a sample, add it bare.

## The demo

| Sample | Stack | The challenge |
|---|---|---|
| `demo/` | FastAPI + FastMCP + DuckDB, and a React/Tailwind dashboard | **One** address for two apps: the dashboard is the only thing on the internet, and the backend claims `/api` and `/mcp` on that same address while being `internal: true` — no hostname, no certificate, reachable only as `http://backend` from inside the space. Plus MCP and REST over one embedded database, and a disk that survives redeploys. This is what `task demo` deploys. |

## The way apps are actually generated

What arrives at a PaaS is overwhelmingly the untouched output of a scaffolding
tool — `create-next-app`, `django-admin startproject`, `express-generator` — or
of an assistant asked for "a web app", which produces the same thing. These
have a shared property that makes them hard: **the generator's defaults are
tuned for a laptop**, and nothing in the project marks which of them will not
survive a deploy.

| Sample | Stack | The challenge |
|---|---|---|
| `nextjs-app/` | Next.js 15, App Router, TypeScript | Two-phase by nature: `next build`, then `next start` — not `node index.js`, and the build output alone is not runnable because `output: 'standalone'` is left commented out. Port 3000 by default but `$PORT` is honoured, so nothing needs rewriting if donkey passes it. |
| `django-blog/` | Django 5 + gunicorn | The richest one. `manage.py runserver` is dev-only, so the start command has to become `gunicorn blog.wsgi`. `ALLOWED_HOSTS = []` makes Django answer **400 to every request through a gateway** — the app is up, healthy-looking, and refuses everyone. `DEBUG = True` ships the settings-printing error page. The sqlite file sits in the project folder, so it needs a disk. And it needs `migrate` before it serves, which is what `preDeploy` is for. Four separate ways to be broken, none visible from the outside. |
| `express-api/` | Express 5 | `const port = 3000`, `app.listen(port)` — `$PORT` never consulted, so the port has to be read out of the source. No health route, which is the norm, not an oversight. |
| `fastapi-sample/` | Python | The well-behaved case, for contrast: a `Procfile`, `$PORT`, and a real `/healthz`. Should deploy without a single question. |

## Simple

| Sample | Stack | The challenge |
|---|---|---|
| `static-site/` | Plain HTML and CSS | No build, no framework, no config. The smallest thing anyone can deploy — and the case where any question donkey asks is one question too many. |
| `dockerfile-sample/` | A `Dockerfile` | The user's own Dockerfile always wins over automatic detection; the port still has to come from `EXPOSE` or the source. |

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
| `local-habits/` | React + Vite | A `fetch('http://localhost:3001/api')` that runs in the **browser** (always wrong once deployed), a vite devServer proxy that is dev-only (donkey must *not* warn about it), and a `.env` that must never leave the machine. Distinguishing the first from the second is the whole test. |
| `sqlite-notes/` | Python + SQLite | Writes to `./data` and never says so. Without a disk the notes vanish on the next restart, and nothing about the app looks wrong until they do. |
| `monorepo-root/` | Repository root | The app is not at the top: it lives in `services/api/`. Uploading the root must find it, say which folder it picked, and refuse to guess when several are candidates. |
| `odd-port-sample/` | Node | Ignores `$PORT` and pins an unusual one. Provider defaults would guess wrong; only reading the source gets it right. |
| `kitchen-sink-sample/` | Python, React, Go, Dockerfile, **Tcl** | Five folders, no `donkey.yaml` anywhere. The Tcl one is recognized by nothing, so the assistant has to write the build itself. |

## What a good run looks like

- `static-site/` and `fastapi-sample/` deploy without a single question.
- `nextjs-app/` builds and starts on `$PORT` — one app, two commands, no help.
- `django-blog/` is the benchmark: gunicorn instead of `runserver`, a disk for
  the sqlite file, `migrate` as a `preDeploy` step, and — the one that decides
  whether the deploy is usable at all — `ALLOWED_HOSTS` handled or called out
  before the first request returns 400.
- `express-api/` gets port 3000 from `app.listen`, not from a guess.
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

To run the whole set against a live donkey:

```sh
task demo:samples
```

Every sample is deployed from a **throwaway copy**, never from `testdata/`
itself. That matters more than it sounds: `donkey deploy` writes a
`donkey.yaml` (and sometimes a `.donkey/Dockerfile`) into the folder it
deploys. Run the suite in place once and every sample carries a config, so the
next run tests YAML parsing instead of detection — the samples quietly stop
being samples. The copy keeps the original folder's exact basename, because
that name is what becomes the app's name.

Apps land in two spaces. The single-app samples share `samples`. The five apps
of `kitchen-sink-sample/` get their own `kitchen-sink` space, because they are
one repository whose apps belong together — which is what a space *is* — and
because names like `api` and `gateway` have no business sitting beside
`static-site` as if they were peers.

The samples are split into two groups, because they answer different
questions:

- **should work** — donkey is expected to get these right unaided, so one of
  them failing fails the run.
- **challenges** — `django-blog`, `flask-beginner`, `streamlit-report`,
  `no-manifest` and the five `kitchen-sink-sample/` apps. These are reported
  but never fatal. A failure here is only a real failure if donkey said
  *nothing useful* before it happened.

Clean up with `kubectl delete namespace donkey-samples donkey-kitchen-sink`.
