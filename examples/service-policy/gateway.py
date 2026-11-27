"""Deploy harmless fixtures and exercise them over the local HTTPS gateway."""
from datetime import datetime, timezone
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import time
from urllib.parse import urlencode
from urllib.request import Request, urlopen

from verify import API, api


WORK = Path(".build/service-policy-demo")
APPS = {"gateway-demo": ("healthy", "critical", "stale"), "gateway-drift": ("web",)}


def deploy(space, app, tag):
    manifest = json.dumps({"name": app, "space": space, "port": 8000, "public": True,
                           "healthcheck": {"path": "/public"}}).encode()
    archive = io.BytesIO()
    with tarfile.open(fileobj=archive, mode="w:gz") as tar:
        entry = tarfile.TarInfo("donkey.yaml")
        entry.size = len(manifest)
        tar.addfile(entry, io.BytesIO(manifest))
    query = urlencode({"space": space, "noBuild": "true", "tag": "donkey-registry:5000/donkey/gateway/sample:" + tag})
    req = Request(API + "/deploys?" + query, data=archive.getvalue(), headers={
        "Authorization": "Bearer " + os.environ["DONKEY_DEMO_TOKEN"], "Content-Type": "application/gzip"})
    with urlopen(req, timeout=150) as response:
        events = [json.loads(line) for line in response if line.strip()]
    result = next((e["result"] for e in events if "result" in e), {})
    if not result.get("ok"):
        raise RuntimeError(f"deploying {app} in {space}: {result.get('error', 'missing deploy result')}")
    print("Deployed", result["url"], flush=True)


def configure(enabled):
    settings = api("/platform/settings", base=API)
    settings["gatewayMaintenance"] = ({"criticalDays": 30, "unmaintainedDays": 7, "noncompliantDays": 7}
                                      if enabled else {})
    api("/platform/settings", settings, method="PUT", base=API)


def publish(tag):
    image = "127.0.0.1:15000/donkey/gateway/sample:" + tag
    subprocess.run(["docker", "tag", "donkey/policy-upstream:local", image], check=True)
    subprocess.run(["docker", "push", "--quiet", image], check=True)


def seed():
    fresh = datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S")
    old = "20000101000000"
    for tag in (fresh, old):
        publish(tag)
    for space, apps in APPS.items():
        api("", {"name": space, "allowPublic": True}, expected=(200, 409))
        if space == "gateway-drift":
            # Replaying the fixtures reverses the sample repair only.
            for service in api("/gateway-drift/services")["addons"]:
                if service["type"] == "public-data":
                    if service["name"] != "demo-remedy" or service["attachedTo"]:
                        raise RuntimeError("gateway-drift already uses public-data; its compliance requirement is satisfied")
                    api("/gateway-drift/services/demo-remedy", method="DELETE")
        for app in apps:
            deploy(space, app, old if app == "stale" else fresh)


def repair():
    fresh = datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S")
    publish(fresh)
    deploy("gateway-demo", "stale", fresh)
    api("/gateway-drift/services", {"type": "public-data", "name": "demo-remedy"})


def policy_overlay():
    # Read the rendered base policy so edits to server.yaml or its Rego also
    # apply to the demo. The override only adds a rule for the fixture space.
    base = json.load(sys.stdin)["services"]["orchestrator"]["environment"]["DONKEY_SERVICE_POLICY"]
    policy = json.loads(base)
    policy["rego"] += '''
deny contains "This space requires the public-data service" if {
    input.space == "gateway-drift"
    not has_public_data
}
has_public_data if { some s in input.services; s.id == "public-data" }
'''
    patch = {"environment": {"DONKEY_SERVICE_POLICY": json.dumps(policy).replace("$", "$$")}}
    (WORK / "gateway-policy.json").write_text(json.dumps({"services": {"server": patch, "orchestrator": patch}}))


def check(enabled):
    if enabled:
        policy = api("/gateway-drift/services")["servicePolicy"]
        if policy["status"] != "noncompliant" or "requires the public-data" not in policy["reason"]:
            raise RuntimeError(f"unexpected service policy for gateway-drift: {policy}")
    deadline = time.monotonic() + 100
    for space, apps in APPS.items():
        for app in apps:
            host = f"{app}.{space}.policy.localhost"
            expected = 503 if enabled and app != "healthy" else 200
            while True:
                probe = subprocess.run(["curl", "--silent", "--show-error", "--noproxy", "*",
                                        "--max-time", "3", "--cacert", str(WORK / "ca.crt"),
                                        "--resolve", host + ":443:127.0.0.1", "-H", "Accept: text/html",
                                        "--write-out", "\n%{http_code}", "https://" + host + "/"],
                                       text=True, capture_output=True)
                body, _, status = probe.stdout.rpartition("\n")
                marker = "owner" if expected == 503 else "This app is serving traffic."
                if probe.returncode == 0 and status == str(expected) and marker in body:
                    print(f"https://{host}/  HTTP {status}", flush=True)
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError(f"{host}: expected {expected}, got {status} {probe.stderr or body[:200]}")
                time.sleep(1)


if __name__ == "__main__":
    action = sys.argv[1]
    if action == "seed":
        seed()
    elif action == "repair":
        repair()
    elif action == "policy":
        policy_overlay()
    elif action in ("enable", "disable"):
        configure(action == "enable")
    elif action == "check-repaired":
        if api("/gateway-drift/services")["servicePolicy"]["status"] != "compliant":
            raise RuntimeError("gateway-drift is still noncompliant after repair")
        rules = api("/platform/settings", base=API).get("gatewayMaintenance", {})
        if not all(rules.get(key) for key in ("criticalDays", "unmaintainedDays", "noncompliantDays")):
            raise RuntimeError("run gateway to enable the maintenance rules before checking repairs")
        check(False)
    elif action in ("check", "check-off"):
        check(action == "check")
    else:
        raise SystemExit("unknown gateway demo step")
