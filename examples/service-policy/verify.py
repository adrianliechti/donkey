"""Seed the two demo spaces and exercise the running Donkey and proxy APIs."""
import json
import os
from urllib.error import HTTPError
from urllib.request import Request, urlopen

API = "http://127.0.0.1:8088/v1"


def api(path, body=None, expected=(200,), method=None, base=API + "/spaces"):
    headers = {"Authorization": "Bearer " + os.environ["DONKEY_DEMO_TOKEN"]}
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    try:
        response = urlopen(Request(base + path, data=data, headers=headers, method=method), timeout=30)
    except HTTPError as error:
        response = error
    with response:
        result = json.load(response)
        if response.status not in expected:
            raise RuntimeError(f"{path}: HTTP {response.status} {result}")
    return result


def denied(path, offering, message):
    result = api(path, {"type": offering}, expected=(400,))
    if message not in result["error"]:
        raise RuntimeError(f"unexpected denial for {offering}: {result}")
    print(f"Blocked {offering} at {path}: {result['error']}")


def main():
    for space in ("classified", "reporting"):
        result = api("", {"name": space}, expected=(200, 409))
        if "error" in result and "already exists" not in result["error"]:
            raise RuntimeError(result)
    offerings = {entry["type"]: entry for entry in api("/classified/catalog")["types"]}
    for name, tag, auth in (("case-files", "read", "delegated"), ("reporting", "write", "shared")):
        if tag not in offerings[name]["tags"] or offerings[name]["authorization"] != auth:
            raise RuntimeError(f"missing catalog metadata for {name}")
    for service in ("public-data", "team-data", "internal-data", "case-files", "archive"):
        api("/classified/services", {"type": service})
    print("Allowed C1–C4 readers and the delegated C4 archive in classified.")
    denied("/classified/services", "reporting", "cannot write C2")
    denied("/classified/apps/report-worker/services", "reporting", "cannot write C2")
    denied("/classified/services", "postgres", "requires delegated authorization")
    api("/reporting/services", {"type": "reporting"})
    print("Allowed the C2 writer in the separate reporting space.")
    denied("/reporting/services", "case-files", "cannot write C2")
    preview = api("/classified/service-options", {"candidates": [
        {"name": "reporting", "type": "reporting"}, {"name": "archive", "type": "archive"}]})
    if set(preview["blocked"]) != {"reporting"}:
        raise RuntimeError(f"unexpected catalog preview: {preview}")
    for token, status in [(None, 401), ("demo-reader", 403), ("demo-archivist", 200)]:
        headers = {"Authorization": "Bearer " + token} if token else {}
        result = api("/classified/services/archive/http/request",
                     {"method": "POST", "url": "/", "headers": headers})
        if result["status"] != status:
            raise RuntimeError(f"archive returned {result['status']}, expected {status}: {result['body']}")
        print(f"Archive POST with {token or 'no token'}: HTTP {status}")


if __name__ == "__main__":
    main()
