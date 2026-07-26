"""Sales analytics over DuckDB, exposed twice: as a REST API for the
dashboard, and as MCP tools for an assistant.

The point of the demo is that both readers see the same numbers from the same
embedded database — no ETL, no second service. DuckDB lives on the app's disk
(donkey mounts /app/data), so the data survives restarts and redeploys.

Around that core, three space services do real work: shared notes live in
postgres, attachments in rustfs (plain S3), and the platform's openai proxy
summarizes the notes. Each arrives as injected env, and each endpoint answers
503 with a hint until its service is connected — the demo boots either way.
"""

from __future__ import annotations

import os
import random
import threading
from contextlib import asynccontextmanager
from datetime import date, timedelta
from pathlib import Path
from typing import Any

import boto3
import duckdb
import psycopg
from botocore.exceptions import ClientError
from fastapi import FastAPI, HTTPException, UploadFile
from fastapi.responses import StreamingResponse
from fastmcp import FastMCP
from openai import OpenAI
from psycopg.rows import dict_row
from pydantic import BaseModel
from starlette.middleware import Middleware
from starlette.middleware.cors import CORSMiddleware

DB_PATH = Path(os.environ.get("DUCKDB_PATH", "/app/data/demo.duckdb"))
REGIONS = ["Europe", "North America", "Asia Pacific", "Latin America"]
PRODUCTS = ["Starter", "Team", "Business", "Enterprise"]

# DuckDB allows a single writer, and FastAPI serves requests from a thread
# pool — one connection behind one lock keeps that honest.
_lock = threading.Lock()
_db: duckdb.DuckDBPyConnection | None = None


def db() -> duckdb.DuckDBPyConnection:
    if _db is None:
        raise RuntimeError("the database is not open yet")
    return _db


def query(sql: str, params: list[Any] | None = None) -> list[dict[str, Any]]:
    """Run a query and return rows as dicts, one caller at a time."""
    with _lock:
        cur = db().execute(sql, params or [])
        columns = [d[0] for d in cur.description]
        return [dict(zip(columns, row)) for row in cur.fetchall()]


def seed(conn: duckdb.DuckDBPyConnection) -> None:
    """Create and fill the orders table on first start.

    A demo that starts empty shows nothing, so the first boot generates two
    years of plausible orders. It runs once: the disk keeps them afterwards.
    """
    conn.execute(
        """
        CREATE TABLE IF NOT EXISTS orders (
            id BIGINT PRIMARY KEY,
            ordered_at DATE NOT NULL,
            region VARCHAR NOT NULL,
            product VARCHAR NOT NULL,
            units INTEGER NOT NULL,
            revenue DOUBLE NOT NULL
        )
        """
    )
    if conn.execute("SELECT count(*) FROM orders").fetchone()[0] > 0:
        return

    rng = random.Random(20260725)  # fixed seed: every demo tells the same story
    unit_price = {"Starter": 19.0, "Team": 79.0, "Business": 249.0, "Enterprise": 990.0}
    today = date.today()
    rows = []
    for i in range(1, 4001):
        ordered_at = today - timedelta(days=rng.randint(0, 730))
        product = rng.choices(PRODUCTS, weights=[45, 30, 18, 7])[0]
        units = rng.randint(1, 12 if product == "Starter" else 4)
        rows.append(
            (
                i,
                ordered_at,
                rng.choices(REGIONS, weights=[38, 34, 20, 8])[0],
                product,
                units,
                round(units * unit_price[product], 2),
            )
        )
    conn.executemany("INSERT INTO orders VALUES (?, ?, ?, ?, ?, ?)", rows)


mcp = FastMCP("donkey-demo-sales")


@mcp.tool
def sales_summary() -> dict[str, Any]:
    """Total revenue, order count and average order value across all time."""
    return query(
        """
        SELECT count(*) AS orders,
               round(sum(revenue), 2) AS revenue,
               round(avg(revenue), 2) AS average_order,
               sum(units) AS units
        FROM orders
        """
    )[0]


@mcp.tool
def revenue_by_region() -> list[dict[str, Any]]:
    """Revenue per region, largest first."""
    return query(
        """
        SELECT region, round(sum(revenue), 2) AS revenue, count(*) AS orders
        FROM orders GROUP BY region ORDER BY revenue DESC
        """
    )


@mcp.tool
def top_products(limit: int = 5) -> list[dict[str, Any]]:
    """The best-selling products by revenue.

    Args:
        limit: how many products to return (1-20).
    """
    return query(
        """
        SELECT product, round(sum(revenue), 2) AS revenue, sum(units) AS units
        FROM orders GROUP BY product ORDER BY revenue DESC LIMIT ?
        """,
        [max(1, min(limit, 20))],
    )


@mcp.tool
def run_sql(sql: str) -> list[dict[str, Any]]:
    """Answer a question the other tools don't cover, with a read-only query.

    Only a single SELECT is accepted, against the `orders` table
    (id, ordered_at, region, product, units, revenue).

    Args:
        sql: one SELECT statement.
    """
    statement = sql.strip().rstrip(";")
    if not statement.lower().startswith("select"):
        raise ValueError("only SELECT statements are allowed here")
    if ";" in statement:
        raise ValueError("send one statement at a time")
    return query(f"SELECT * FROM ({statement}) LIMIT 200")


# Browser-based MCP clients need CORS, and the session header has to be
# readable by JavaScript or session handling breaks.
cors = [
    Middleware(
        CORSMiddleware,
        allow_origins=[o for o in os.environ.get("CORS_ORIGINS", "*").split(",") if o],
        allow_methods=["GET", "POST", "DELETE", "OPTIONS"],
        allow_headers=["mcp-protocol-version", "mcp-session-id", "Authorization", "Content-Type"],
        expose_headers=["mcp-session-id"],
    )
]
mcp_app = mcp.http_app(path="/mcp", middleware=cors)


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Open the database, then hand over to the MCP session manager."""
    global _db
    DB_PATH.parent.mkdir(parents=True, exist_ok=True)
    _db = duckdb.connect(str(DB_PATH))
    seed(_db)
    # FastMCP's own lifespan starts its session manager — without it, the
    # mounted MCP app has no sessions.
    async with mcp_app.lifespan(app):
        yield
    _db.close()


app = FastAPI(title="donkey demo — sales analytics", lifespan=lifespan)


@app.get("/healthz")
def healthz() -> dict[str, Any]:
    """Ready once the database answers."""
    try:
        query("SELECT 1")
    except Exception as exc:  # the probe is the only place that sees this
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    return {"ok": True}


@app.get("/api/summary")
def api_summary() -> dict[str, Any]:
    return sales_summary()


@app.get("/api/revenue-by-region")
def api_revenue_by_region() -> list[dict[str, Any]]:
    return revenue_by_region()


@app.get("/api/revenue-by-month")
def api_revenue_by_month() -> list[dict[str, Any]]:
    return query(
        """
        SELECT strftime(date_trunc('month', ordered_at), '%Y-%m') AS month,
               round(sum(revenue), 2) AS revenue
        FROM orders
        WHERE ordered_at >= current_date - INTERVAL 12 MONTH
        GROUP BY month ORDER BY month
        """
    )


@app.get("/api/products")
def api_products() -> list[dict[str, Any]]:
    return top_products(10)


@app.get("/api/orders")
def api_orders(limit: int = 20) -> list[dict[str, Any]]:
    return query(
        """
        SELECT id, strftime(ordered_at, '%Y-%m-%d') AS ordered_at,
               region, product, units, revenue
        FROM orders ORDER BY ordered_at DESC, id DESC LIMIT ?
        """,
        [max(1, min(limit, 200))],
    )


# Everything below runs on the space's attached services (the addons in
# donkey.yaml). The demo has to boot before they exist, so nothing connects at
# import time — each endpoint asks for its env when called and answers 503
# with a hint until the service is there.
BUCKET = "demo"
_bucket_ready = False


def missing(env: str, service: str) -> str | None:
    """A friendly sentence while the service's env is absent, None once it's there.

    The REST endpoints turn it into a 503; the MCP tools hand it to the model
    as-is, because a sentence is something a model can read and move on from.
    """
    if os.environ.get(env, ""):
        return None
    return f"the {service} service is not connected yet — `donkey deploy` provisions it from this app's addons"


def need(env: str, service: str) -> str:
    reason = missing(env, service)
    if reason:
        raise HTTPException(status_code=503, detail=reason)
    return os.environ[env]


def notes_db() -> psycopg.Connection:
    """A connection per request is plenty here. The idempotent CREATE runs
    every time on purpose: it costs one round-trip, and a remembered "table
    exists" would outlive the table (someone can drop it in the console)."""
    conn = psycopg.connect(need("DATABASE_URL", "postgres"), autocommit=True, row_factory=dict_row)
    conn.execute(
        """
        CREATE TABLE IF NOT EXISTS notes (
            id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
            text text NOT NULL,
            created timestamptz NOT NULL DEFAULT now()
        )
        """
    )
    return conn


def s3() -> Any:
    """rustfs speaks S3, so boto3 works as-is — only the endpoint needs saying."""
    global _bucket_ready
    client = boto3.client("s3", endpoint_url=need("AWS_ENDPOINT_URL_S3", "rustfs"))
    if not _bucket_ready:
        try:
            client.create_bucket(Bucket=BUCKET)
        except ClientError as exc:
            # A bucket that already exists is exactly what we wanted.
            if exc.response["Error"]["Code"] not in ("BucketAlreadyOwnedByYou", "BucketAlreadyExists"):
                raise
        _bucket_ready = True
    return client


# The dashboard's REST endpoints and the MCP tools further down both go
# through these, so the two faces of each feature cannot drift apart.
def list_notes() -> list[dict[str, Any]]:
    with notes_db() as conn:
        return conn.execute(
            "SELECT id, text, created FROM notes ORDER BY id DESC LIMIT 50"
        ).fetchall()


def add_note(text: str) -> dict[str, Any]:
    with notes_db() as conn:
        return conn.execute(
            "INSERT INTO notes (text) VALUES (%s) RETURNING id, text, created",
            (text,),
        ).fetchone()


def delete_note(note_id: int) -> bool:
    with notes_db() as conn:
        return (
            conn.execute("DELETE FROM notes WHERE id = %s RETURNING id", (note_id,)).fetchone()
            is not None
        )


def list_files() -> list[dict[str, Any]]:
    objects = s3().list_objects_v2(Bucket=BUCKET).get("Contents", [])
    return [
        {"key": o["Key"], "size": o["Size"], "modified": o["LastModified"].isoformat()}
        for o in sorted(objects, key=lambda o: o["LastModified"], reverse=True)
    ]


def delete_file(key: str) -> bool:
    """True when the file was there. S3's delete never tells, so ask first."""
    client = s3()
    try:
        client.head_object(Bucket=BUCKET, Key=key)
    except ClientError as exc:
        if exc.response["Error"]["Code"] in ("NoSuchKey", "404"):
            return False
        raise
    client.delete_object(Bucket=BUCKET, Key=key)
    return True


class NoteIn(BaseModel):
    text: str


@app.post("/api/notes")
def api_add_note(note: NoteIn) -> dict[str, Any]:
    text = note.text.strip()
    if not text:
        raise HTTPException(status_code=422, detail="an empty note is not worth keeping")
    return add_note(text)


@app.get("/api/notes")
def api_notes() -> list[dict[str, Any]]:
    return list_notes()


@app.delete("/api/notes/{note_id}")
def api_delete_note(note_id: int) -> dict[str, Any]:
    if not delete_note(note_id):
        raise HTTPException(status_code=404, detail="no note with that id")
    return {"deleted": note_id}


@app.post("/api/notes/summary")
def api_notes_summary() -> dict[str, str]:
    """Hand the recent notes to the platform's model, return its short read."""
    need("OPENAI_BASE_URL", "openai")
    with notes_db() as conn:
        rows = conn.execute("SELECT text FROM notes ORDER BY id DESC LIMIT 20").fetchall()
    if not rows:
        return {"summary": "No notes yet — write a few and ask again."}

    # Base URL and key come from the injected env; a short timeout because a
    # demo click should fail politely, not hang.
    client = OpenAI(timeout=30)
    model = os.environ.get("OPENAI_MODEL", "")
    if not model:
        try:
            model = client.models.list().data[0].id
        except Exception:
            model = "gpt-4o-mini"
    reply = client.chat.completions.create(
        model=model,
        messages=[
            {
                "role": "system",
                "content": "You summarize a team's shared notes in two or three plain, warm sentences.",
            },
            # Oldest first, so the story reads in the order it was written.
            {"role": "user", "content": "\n".join(f"- {r['text']}" for r in reversed(rows))},
        ],
    )
    return {"summary": (reply.choices[0].message.content or "").strip()}


@app.post("/api/files")
def api_upload_file(file: UploadFile) -> dict[str, Any]:
    # Whatever path the browser claims, only the basename becomes the key.
    key = Path(file.filename or "upload").name
    s3().upload_fileobj(
        file.file,
        BUCKET,
        key,
        ExtraArgs={"ContentType": file.content_type or "application/octet-stream"},
    )
    return {"key": key}


@app.get("/api/files")
def api_list_files() -> list[dict[str, Any]]:
    return list_files()


@app.get("/api/files/{key:path}")
def api_download_file(key: str) -> StreamingResponse:
    try:
        obj = s3().get_object(Bucket=BUCKET, Key=key)
    except ClientError as exc:
        if exc.response["Error"]["Code"] in ("NoSuchKey", "404"):
            raise HTTPException(status_code=404, detail="no file by that name") from exc
        raise
    return StreamingResponse(
        obj["Body"].iter_chunks(),
        media_type=obj.get("ContentType") or "application/octet-stream",
        headers={"content-disposition": f'attachment; filename="{Path(key).name}"'},
    )


@app.delete("/api/files/{key:path}")
def api_delete_file(key: str) -> dict[str, Any]:
    if not delete_file(key):
        raise HTTPException(status_code=404, detail="no file by that name")
    return {"deleted": key}


# The same features again, for an assistant — the tools call the same cores
# the REST endpoints do, so both faces always agree.
@mcp.tool
def notes_list() -> list[dict[str, Any]] | str:
    """The team's shared notes, newest first."""
    reason = missing("DATABASE_URL", "postgres")
    if reason:
        return reason
    return list_notes()


@mcp.tool
def notes_add(text: str) -> dict[str, Any] | str:
    """Leave a note for the team.

    Args:
        text: what to say.
    """
    reason = missing("DATABASE_URL", "postgres")
    if reason:
        return reason
    text = text.strip()
    if not text:
        raise ValueError("an empty note is not worth keeping")
    return add_note(text)


@mcp.tool
def notes_delete(id: int) -> dict[str, Any] | str:
    """Remove a note.

    Args:
        id: the note's id, as notes_list reports it.
    """
    reason = missing("DATABASE_URL", "postgres")
    if reason:
        return reason
    if not delete_note(id):
        raise ValueError(f"no note with id {id}")
    return {"deleted": id}


@mcp.tool
def files_list() -> list[dict[str, Any]] | str:
    """The files in the space's bucket, newest first."""
    reason = missing("AWS_ENDPOINT_URL_S3", "rustfs")
    if reason:
        return reason
    return list_files()


@mcp.tool
def files_read(key: str) -> str:
    """Read a small text file from the bucket.

    Only text up to 64 KB comes back — anything bigger or binary belongs to
    the dashboard's download link, not a tool reply.

    Args:
        key: the file's key, as files_list reports it.
    """
    reason = missing("AWS_ENDPOINT_URL_S3", "rustfs")
    if reason:
        return reason
    try:
        obj = s3().get_object(Bucket=BUCKET, Key=key)
    except ClientError as exc:
        if exc.response["Error"]["Code"] in ("NoSuchKey", "404"):
            raise ValueError("no file by that name — files_list shows what's there") from exc
        raise
    if obj["ContentLength"] > 64 * 1024:
        raise ValueError("that file is bigger than 64 KB — too much for a tool reply")
    try:
        return obj["Body"].read().decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("that file is not text") from exc


@mcp.tool
def files_delete(key: str) -> dict[str, Any] | str:
    """Remove a file from the bucket.

    Args:
        key: the file's key, as files_list reports it.
    """
    reason = missing("AWS_ENDPOINT_URL_S3", "rustfs")
    if reason:
        return reason
    if not delete_file(key):
        raise ValueError("no file by that name — files_list shows what's there")
    return {"deleted": key}


# The MCP endpoint lands at /mcp. Mounting last means the API routes above
# match first and everything else goes to the MCP app.
app.mount("/", mcp_app)
