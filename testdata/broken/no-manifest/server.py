# Downloaded from a chat window, saved, and it ran — because flask happened to
# be installed on that laptop. Nothing here says so: no requirements.txt, no
# pyproject.toml, no lockfile, no imports pinned anywhere.
#
# A build has no way to know what to install. donkey should either work it out
# from the imports, or stop with a sentence a non-developer can act on —
# "this app needs flask, but nothing lists it" — never a stack trace.

from flask import Flask, jsonify

app = Flask(__name__)

TASKS = [
    {"id": 1, "title": "Write the thing", "done": True},
    {"id": 2, "title": "Deploy the thing", "done": False},
]


@app.get("/")
def index():
    return jsonify({"tasks": TASKS, "note": "no requirements.txt anywhere"})


@app.get("/healthz")
def healthz():
    return jsonify({"ok": True})


app.run(host="0.0.0.0", port=8000)
