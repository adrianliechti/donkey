# streamlit-report

Streamlit is what a lot of people reach for when they are not developers: an
analysis script that grew a user interface. It is also a genuinely awkward
thing to deploy.

| What is missing | Why it matters |
|---|---|
| No start command | The app is not run by `python app.py`; it is run *by* `streamlit`. There is no `if __name__ == "__main__"` to detect. |
| No port | Streamlit defaults to 8501 and ignores `$PORT` unless told `--server.port`. |
| Binds loopback by default | Without `--server.address 0.0.0.0` nothing outside the container can reach it. |
| No health route | Streamlit has `/_stcore/health`, which nobody knows about. |
| Websockets | The page is dead without them — a gateway that only forwards plain HTTP looks "up" and is useless. |

## The test

This is the hardest sample in the suite for automatic detection, and the most
representative of its audience. Success is a deploy where the chart renders in
a browser through donkey's gateway.

If donkey cannot work it out alone, the failure has to be *legible*: name the
missing start command and offer the fix, rather than a rollout timeout. The
assistant writing a `.donkey/railpack.json` or a Dockerfile with

```
streamlit run app.py --server.port $PORT --server.address 0.0.0.0
```

is the intended path.
