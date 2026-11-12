# The tutorial version of a first Flask app, unchanged.
#
# It works perfectly on a laptop, which is why it arrives like this. In a
# container app.run() binds 127.0.0.1, and that means the app answers *itself*
# and nobody else: it starts, logs "Running on http://127.0.0.1:5000", passes
# nothing, and the rollout times out with no error in the log at all.
#
# The fix is one argument. Finding it from the outside is impossible; finding
# it by reading this file is trivial.

from flask import Flask

app = Flask(__name__)


@app.route("/")
def home():
    return "<h1>It works 🎉</h1>"


if __name__ == "__main__":
    app.run(debug=True, port=5000)
