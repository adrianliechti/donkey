# A first Flask app — the tutorial shape, with the three lines changed that
# every tutorial leaves out and every deploy needs:
#
#   1. host="0.0.0.0". app.run() binds 127.0.0.1 by default, which inside a
#      container means "nobody but me": the app starts, says "Running on
#      http://127.0.0.1:5000", and every health check still fails.
#   2. the port comes from $PORT, which is how the platform tells the app
#      where to listen. The 5000 stays as the local default.
#   3. debug is off unless asked for. The debugger is an interactive shell on
#      a public address.
#
# What is still missing is deliberate: no Procfile, no health route, no
# donkey.yaml. Those are the things donkey works out on its own.

import os

from flask import Flask

app = Flask(__name__)

PAGE = """<!doctype html>
<title>My first app</title>
<style>
  body { font: 16px/1.6 system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; }
  code { background: #f4f1ea; padding: .1rem .3rem; }
</style>
<h1>It works 🎉</h1>
<p>A single-file Flask app with no configuration of any kind.</p>
<p>If you can read this through donkey, the deploy understood an app that
never told it anything: no <code>Procfile</code>, no health route and no
config file — just a single Python file.</p>
"""


@app.route("/")
def home():
    return PAGE


if __name__ == "__main__":
    app.run(
        host="0.0.0.0",
        port=int(os.environ.get("PORT", 5000)),
        debug=os.environ.get("FLASK_DEBUG") == "1",
    )
