"""Advance the demo clock only for conditions the real orchestrator observed."""
import json
import os
from pathlib import Path
import sys
import time

import yaml


def update(path, edit):
    data = yaml.safe_load(path.read_text())
    edit(data)
    temp = path.with_suffix(".demo-tmp")
    temp.write_text(yaml.safe_dump(data))
    temp.chmod(0o600)
    os.replace(temp, path)


root = Path("/data")
if sys.argv[1] in ("scan", "clean"):
    critical = sys.argv[1] == "scan"
    badge = {"state": "needs-action" if critical else "clean", "summary": {"critical": int(critical)},
             "scannedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    update(root / "deploy/gateway-demo/critical.yaml",
           lambda record: record.update(security=json.dumps(badge)))
elif sys.argv[1] == "age":
    def age(record, key):
        if not record.get(key):
            raise ValueError(f"waiting for {key}")
        record[key] = int(time.time()) - 31 * 86400

    def age_apps(record):
        age(record["apps"]["critical"], "criticalSince")
        age(record["apps"]["stale"], "unmaintainedSince")

    for attempt in range(90):
        try:
            update(root / "spaces/gateway-demo/gateway-maintenance.yaml", age_apps)
            update(root / "spaces/gateway-drift/gateway-maintenance.yaml",
                   lambda record: age(record, "noncompliantSince"))
            break
        except (FileNotFoundError, ValueError, KeyError):
            if attempt == 89:
                raise
            time.sleep(1)
else:
    raise SystemExit("expected scan, clean or age")
