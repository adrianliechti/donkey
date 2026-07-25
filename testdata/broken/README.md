# broken — apps that are supposed to fail

Every app in `testdata/` is meant to deploy. Every app in **this** folder is
meant not to.

They exist because half of what donkey does only runs when something goes
wrong. A deploy tool is easy to judge on the happy path and almost never
judged on the other one, which is the path its users actually remember: the
rollout timed out, the log says nothing useful, and the person who wrote the
app has no idea what to do next.

So these four are broken the way real apps are broken — invisibly. Each one
builds, starts, logs something reassuring, and never answers.

| App | What is wrong | Where it fails | The fix |
|---|---|---|---|
| `flask-loopback/` | `app.run(debug=True, port=5000)` binds 127.0.0.1 | rollout — the probe can't connect, and the log says "Running on…" | one argument: `host="0.0.0.0"` |
| `express-loopback/` | `app.listen(3000, '127.0.0.1')` | rollout — same failure, other language | drop the host argument |
| `streamlit-bare/` | no Procfile, so it is started with `python app.py` | rollout — Streamlit runs in bare mode, every `st.*` a no-op, no server ever created | a start command that uses the `streamlit` binary |
| `missing-dependency/` | imports flask, declares it nowhere | runtime — `ModuleNotFoundError` on the first line | a `requirements.txt` that does not exist yet |

The last one is the interesting case for the patch itself: the fix has to
**create** a file rather than change one, so the diff is a pure addition.

## Running them

```sh
task demo:broken
```

The assertions are inverted. For each app it checks that:

1. **the deploy failed** — one that deploys is no longer broken, and is
   testing nothing;
2. **donkey explained it** — a "What happened:" line;
3. **donkey proposed a patch** — an actual diff, not advice to go and look.

All three have to hold. A failure with no explanation is the outcome this
folder exists to catch: it is what every deploy tool does, and it leaves the
author with a rollout timeout and nowhere to go.

Expect it to be slow. The loopback cases have to wait out a full rollout
timeout before there is anything to diagnose — that wait *is* the condition
being tested.

## Applying a fix

`task demo:broken` runs with `--yes`, which prints the patch but never writes
it: rewriting someone's source is not a question donkey answers on its own.
To go through the whole loop — fail, read the diff, accept it, deploy again —
run one by hand:

```sh
cd testdata/broken/flask-loopback && donkey deploy
```

Then say yes when it offers the change. The second deploy should succeed, and
`git diff` shows exactly what donkey wrote.
