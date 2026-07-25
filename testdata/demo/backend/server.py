"""Sales analytics over DuckDB, exposed twice: as a REST API for the
dashboard, and as MCP tools for an assistant.

The point of the demo is that both readers see the same numbers from the same
embedded database — no ETL, no second service. DuckDB lives on the app's disk
(donkey mounts /app/data), so the data survives restarts and redeploys.
"""

from __future__ import annotations

import os
import random
import threading
from contextlib import asynccontextmanager
from datetime import date, timedelta
from pathlib import Path
from typing import Any

import duckdb
from fastapi import FastAPI, HTTPException
from fastmcp import FastMCP
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


# The MCP endpoint lands at /mcp. Mounting last means the API routes above
# match first and everything else goes to the MCP app.
app.mount("/", mcp_app)
