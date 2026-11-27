"""Local policy demonstration with synthetic data and fixed, scoped demo tokens."""
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Both users can read cases. Only the archivist can write the archive.
PERMISSIONS = {
    "Bearer demo-reader": {"cases:read", "internal:read"},
    "Bearer demo-archivist": {"cases:read", "internal:read", "archive:write"},
}
ENDPOINTS = {
    "/public": ("GET", None),
    "/team": ("GET", None),
    "/internal": ("GET", "internal:read"),
    "/cases": ("GET", "cases:read"),
    "/reports": ("POST", None),
    "/archive": ("POST", "archive:write"),
}


class Handler(BaseHTTPRequestHandler):
    def reply(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def handle_request(self):
        path = self.path.partition("?")[0].rstrip("/")
        if path == "" and self.command == "GET":
            return self.reply(200, {"message": "This app is serving traffic."})
        if path == "/v1/responses" and self.command == "POST":
            # Donkey checks the configured assistant on startup. This fixture
            # serves the readiness request for the service selection demo.
            return self.reply(200, {"id": "resp_demo", "object": "response",
                                    "model": "policy-demo", "status": "completed", "output": []})
        if path not in ENDPOINTS:
            return self.reply(404, {"error": "unknown endpoint"})
        method, permission = ENDPOINTS[path]
        if self.command != method:
            return self.reply(405, {"error": "method not allowed"})
        if permission:
            grants = PERMISSIONS.get(self.headers.get("Authorization"))
            if grants is None:
                return self.reply(401, {"error": "a demo bearer token is required"})
            if permission not in grants:
                return self.reply(403, {"error": "missing " + permission})
        self.reply(200, {"service": path[1:], "result": "sample record" if method == "GET" else "accepted"})

    do_GET = handle_request
    do_POST = handle_request


if __name__ == "__main__":
    ThreadingHTTPServer(("", 8000), Handler).serve_forever()
