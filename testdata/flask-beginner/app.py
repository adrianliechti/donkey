# A first Flask app, written the way every tutorial writes one.
#
# Three things here are perfectly normal on a laptop and wrong in a container,
# and none of them look like a mistake:
#   1. app.run() binds 127.0.0.1 — inside a container that means "nobody but
#      me", so the app starts, says "Running on http://127.0.0.1:5000", and
#      every health check still fails.
#   2. the port is hardcoded to 5000 and $PORT is ignored.
#   3. debug=True ships the interactive debugger, which is a remote shell.
#
# There is no Procfile and no health route either. This is the shape most
# non-developer apps arrive in.

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
never told it anything: no <code>Procfile</code>, no health route, a
hardcoded port, and a server that only wanted to talk to localhost.</p>
"""


@app.route("/")
def home():
    return PAGE


if __name__ == "__main__":
    app.run(debug=True, port=5000)
