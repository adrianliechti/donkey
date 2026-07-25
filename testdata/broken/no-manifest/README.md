# no-manifest

A Python app with **no dependency file of any kind** — the state a file
arrives in when someone saves it out of a chat window and it happens to run on
their machine.

`import flask` is the only evidence that a dependency exists.

## The test

Two acceptable outcomes, one unacceptable one:

- ✅ donkey works it out (the assistant reads the imports and writes a
  `.donkey/` build file listing flask), or
- ✅ donkey stops and says plainly: *this app imports flask, but nothing in
  the folder lists it — add a requirements.txt with `flask`*.
- ❌ a build that fails deep inside a provider with `ModuleNotFoundError`,
  or a rollout that times out with no explanation.

Note the app also calls `app.run(...)` at import time rather than under
`if __name__ == "__main__"`, which is normal in generated code and breaks
some runners (gunicorn imports the module and the call blocks).
