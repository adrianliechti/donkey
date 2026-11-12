"""A small API that happens to live two levels below the repository root."""

import os

from fastapi import FastAPI

app = FastAPI(title="monorepo api")


@app.get("/healthz")
def healthz() -> dict[str, bool]:
    return {"ok": True}


@app.get("/")
def index() -> dict[str, str]:
    return {
        "service": "api",
        "where": "services/api — donkey found me below the repository root",
        "port": os.environ.get("PORT", "unset"),
    }
