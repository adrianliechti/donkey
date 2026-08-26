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

So only two samples ship one, each for reasons that have nothing to do with
convenience:

- **`monorepo-root/services/api/`** — the file *is* the test. Subfolder
  discovery finds an app by looking for a `donkey.yaml`, so removing it would
  remove the thing the sample exercises.
- **`addons-sample/`** — same reason, different feature: addons are the one
  thing donkey never provisions from detection alone (a `psycopg` import only
  earns a suggestion), so `addons:` in the file is the only way to say them —
  and the deploy provisioning what the file lists is what's under test.

(The showcase that says everything explicitly — `internal`, `routes`,
`storage`, `addons`, a fixed `space` — lives in [examples/](../examples/):
it stopped being a detection challenge long ago.)

Everything else is bare. When you add a sample, add it bare.

## Attached services

| Sample | Stack | The challenge |
|---|---|---|
| `addons-sample/` | An Express guestbook on Postgres | The `donkey.yaml` lists an addon — the deploy has to provision it for the space with its own disk, wait for it, and only then start the app with `DATABASE_URL` injected. Notes land in Postgres. Nothing in the folder holds an address or a credential. |

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
| `django-blog/` | Django 5 + gunicorn | A Django project set up the way a deployable one is: gunicorn instead of `runserver`, `ALLOWED_HOSTS`/`DEBUG`/`SECRET_KEY` from the environment, and `migrate` in front of the server. What it still doesn't say is its port, its health route or that `db.sqlite3` sits in the project folder — that last one is real data in a place no restart survives, and finding it is donkey's job. |
| `express-api/` | Express 5 | `const port = 3000`, `app.listen(port)` — `$PORT` never consulted, so the port has to be read out of the source. No health route, which is the norm, not an oversight. |
| `fastapi-sample/` | Python | The well-behaved case, for contrast: a `Procfile`, `$PORT`, and a real `/healthz`. Should deploy without a single question. |

## Simple

| Sample | Stack | The challenge |
|---|---|---|
| `static-site/` | Plain HTML and CSS | No build, no framework, no config. The smallest thing anyone can deploy — and the case where any question donkey asks is one question too many. |
| `dockerfile-sample/` | A `Dockerfile` | The user's own Dockerfile always wins over automatic detection; the port still has to come from `EXPOSE` or the source. |

## Not written by developers

The apps most likely to arrive at a PaaS were not written by someone thinking
about containers. They are small, they have no configuration to speak of, and
what they leave unsaid is everything a deploy needs to know: the port, the
health route, which folder holds data, and sometimes what to install.

| Sample | Stack | The challenge |
|---|---|---|
| `flask-beginner/` | Python, one file | One file, one route, `requirements.txt`, nothing else: no Procfile, no health route, no config. It binds `0.0.0.0` and reads `$PORT`, so it is deployable — everything else about it has to be worked out. |
| `streamlit-report/` | Streamlit | An analyst's script that grew a UI. It is run by the `streamlit` binary rather than by python, which its Procfile says — and that line is also where the address and port are settled, because Streamlit takes them as flags. Nothing declares the port, the health route (`/_stcore/health`) or the websocket the page needs. |
| `My Cool App/` | Static files | A folder named the way people name folders: spaces, capitals, `main style.css`, `IMG_2024 (1).svg`. The name has to become `my-cool-app` and every file has to survive the round trip unmangled. |

## Awkward on purpose

| Sample | Stack | The challenge |
|---|---|---|
| `local-habits/` | React + Vite | A `fetch('http://localhost:3001/api')` that runs in the **browser** (always wrong once deployed), a vite devServer proxy that is dev-only (donkey must *not* warn about it), and a `.env` that must never leave the machine. Distinguishing the first from the second is the whole test. |
| `sqlite-notes/` | Python + SQLite | Writes to `./data` and never says so. Without a disk the notes vanish on the next restart, and nothing about the app looks wrong until they do. |
| `monorepo-root/` | Repository root | The app is not at the top: it lives in `services/api/`. Uploading the root must find it, say which folder it picked, and refuse to guess when several are candidates. |
| `odd-port-sample/` | Node | Ignores `$PORT` and pins an unusual one. Provider defaults would guess wrong; only reading the source gets it right. |
| `kitchen-sink-sample/` | Python, React, Go, Dockerfile, **Tcl** | Five apps in one repository, no `donkey.yaml` anywhere — so an upload of the root has to refuse to guess, and each app is deployed from its own folder. Three of the five (`api`, `web`, `worker`) are ordinary apps railpack recognizes; `gateway` only works if its port is read from `EXPOSE`, because its `httpd` cannot see `$PORT`; `legacy` is Tcl, which nothing recognizes, so the assistant has to write the build itself. |

## What a good run looks like

- `static-site/` and `fastapi-sample/` deploy without a single question.
- `nextjs-app/` builds and starts on `$PORT` — one app, two commands, no help.
- `django-blog/` deploys, runs its migrations, and gets a disk proposed for
  `db.sqlite3` — the one thing about it that is still silently wrong.
- `express-api/` gets port 3000 from `app.listen`, not from a guess.
- `flask-beginner/` and `streamlit-report/` deploy with no questions asked.
  Both say how they start and nothing else; a rollout timeout with no
  explanation is a failure of donkey, not of the sample.
- `My Cool App/` lands at `my-cool-app.<space>.…` with the stylesheet and the
  image both loading.
- `local-habits/` deploys **and warns** about `App.jsx`, while staying silent
  about `vite.config.js`.
- `sqlite-notes/` proposes a disk for `./data` before the first deploy.
- `addons-sample/` provisions its postgres first; signing the guestbook writes
  to it, and the notes survive a redeploy.
- `monorepo-root/` builds `services/api` and says that it did.
- `kitchen-sink-sample/{api,web,worker}` each deploy on their own — railpack
  recognizes all three, and finding them separately is the actual test.
- `kitchen-sink-sample/legacy` needs the assistant; without one configured,
  it should fail with a clear reason rather than a stack trace.

## The ones that are supposed to fail

`broken/` is the other half, and it is a separate suite with inverted
assertions — see [broken/README.md](broken/README.md). Those apps exist
to exercise what only runs when a deploy goes wrong: donkey reading the code,
explaining the cause, and proposing a patch. `task demo:broken` fails if one
of them *deploys*, or fails without an explanation and a diff.

That folder is also where the awkward cases go now. When a sample here is
fixed so it deploys, the broken version usually still earns its place over
there: `flask-loopback/` is the `app.run(debug=True, port=5000)` that
`flask-beginner/` used to be, and `streamlit-bare/` is `streamlit-report/`
without its Procfile.

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
- **challenges** — `kitchen-sink-sample/gateway`, whose Dockerfile cannot read
  `$PORT`, and `kitchen-sink-sample/legacy`, which no provider recognizes.
  These are reported but never fatal. A failure here is only a real failure if
  donkey said *nothing useful* before it happened.

Clean up with `kubectl delete namespace donkey-samples donkey-kitchen-sink`.
