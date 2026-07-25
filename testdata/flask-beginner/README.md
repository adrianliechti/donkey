# flask-beginner

The single most common shape a first app arrives in: one file, copied from a
tutorial, that runs fine with `python app.py`.

| In the code | Why it breaks in a container | What donkey should do |
|---|---|---|
| `app.run(debug=True, port=5000)` | Flask's default host is `127.0.0.1`. Inside a container that means the app answers *itself* and nothing else — it starts, logs "Running on…", and every probe still fails. | Notice the loopback bind and say so before the deploy fails. |
| Port `5000`, `$PORT` ignored | donkey hands the app a port; this app doesn't look. | Read 5000 out of the source and use it. |
| No `Procfile`, no start command | Nothing declares how to run it. | Work it out (the provider's default). |
| No health route | There is no `/healthz` to probe. | Fall back to checking the port answers, not a guessed path. |
| `debug=True` | The Werkzeug debugger is a remote code execution console. | Worth a warning; never a silent "fix". |

## The test

A deploy that ends with a running app here means donkey handled an app that
told it nothing. A deploy that fails should say *which* of the above it was —
"it didn't become healthy" alone is not good enough for someone who has never
seen a container.
