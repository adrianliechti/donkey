# Saved out of a chat window, and it ran — because that laptop happened to
# have flask installed already.
#
# There is no requirements.txt, no lockfile, no import error until the
# container starts. The build succeeds (there is nothing to install), and the
# app dies on its first line with ModuleNotFoundError: No module named 'flask'.
#
# The fix is a file that does not exist yet, which is the interesting case:
# the patch has to create it, not change it.
from flask import Flask

app = Flask(__name__)


@app.route("/")
def home():
    return {"ok": True, "note": "deployed without ever declaring flask"}


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8000)
