# streamlit-report

Streamlit is what a lot of people reach for when they are not developers: an
analysis script that grew a user interface. It is also a genuinely awkward
thing to deploy, because it is not run by `python app.py` — it is run *by* the
`streamlit` binary, and it takes its address and port as flags rather than
from the environment.

That is what the `Procfile` is for, and it is the only configuration here:

```
web: streamlit run app.py --server.port ${PORT:-8501} --server.address 0.0.0.0 …
```

Without it the container starts, the script runs in what Streamlit calls bare
mode — every `st.*` call a no-op — and nothing ever listens.

| What is still missing | Why it matters |
|---|---|
| No declared port | The Procfile passes `$PORT` through; nothing states a number for donkey to match. |
| No health route | Streamlit has `/_stcore/health`, which nobody knows about. |
| Websockets | The page is dead without them — a gateway that only forwards plain HTTP looks "up" and is useless. |
| No `donkey.yaml` | Name, port and probe all have to be worked out. |

## The test

Success is a deploy where the chart renders in a browser through donkey's
gateway — which needs the websocket to survive the whole path, not just the
first HTTP request.
