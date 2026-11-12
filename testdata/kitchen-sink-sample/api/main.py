import os

from fastapi import FastAPI

app = FastAPI()


@app.get("/healthz")
def healthz():
    return {"ok": True}


@app.get("/")
def root():
    return {"service": "api", "greeting": os.environ.get("GREETING", "none")}


@app.get("/db-check")
def db_check():
    url = os.environ.get("DATABASE_URL")
    if not url:
        return {"database": "not configured"}
    import psycopg

    with psycopg.connect(url, connect_timeout=5) as conn:
        with conn.cursor() as cur:
            cur.execute("SELECT 1")
            cur.fetchone()
    return {"database": "ok"}
