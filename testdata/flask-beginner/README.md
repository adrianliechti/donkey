# flask-beginner

The single most common shape a first app arrives in: one file, copied from a
tutorial, that runs with `python app.py`.

The three lines that decide whether it survives a deploy are already right —
they are the ones a tutorial leaves out, and no platform can fix them from the
outside:

```python
app.run(
    host="0.0.0.0",                          # not 127.0.0.1
    port=int(os.environ.get("PORT", 5000)),  # the platform chooses
    debug=os.environ.get("FLASK_DEBUG") == "1",
)
```

Everything else is still unsaid, and that is what the sample tests:

| What is missing | What donkey should do |
|---|---|
| No `Procfile`, no start command | Work it out from the provider's default. |
| No declared port | The app follows `$PORT`, so any port works — donkey should notice that and not pin 5000. |
| No health route | Fall back to checking that the port answers, not a guessed path. |
| No `donkey.yaml` | Name it from the folder, and ask nothing. |

## The test

A deploy that ends with a running app here means donkey handled an app that
told it nothing. A deploy that fails should say *which* of the above it was —
"it didn't become healthy" alone is not good enough for someone who has never
seen a container.
