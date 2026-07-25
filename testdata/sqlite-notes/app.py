"""Notes kept in SQLite next to the app — the ordinary way a small tool
stores things, and the reason donkey has to notice folders that hold data.

Nothing here says "give me a disk". The database is created relative to the
working directory, which on a cluster is wiped on every restart and every
redeploy. Donkey's assistant should read this file, see that ./data holds
something nobody can regenerate, and say so.
"""

import os
import sqlite3
from pathlib import Path

from fastapi import FastAPI
from pydantic import BaseModel

DATA_DIR = Path("data")
DB = DATA_DIR / "notes.db"

app = FastAPI(title="sqlite notes")


class Note(BaseModel):
    text: str


def db() -> sqlite3.Connection:
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(DB)
    conn.execute("CREATE TABLE IF NOT EXISTS notes (id INTEGER PRIMARY KEY, text TEXT NOT NULL)")
    return conn


@app.get("/healthz")
def healthz() -> dict[str, bool]:
    db().close()
    return {"ok": True}


@app.get("/notes")
def list_notes() -> list[dict]:
    conn = db()
    rows = conn.execute("SELECT id, text FROM notes ORDER BY id DESC").fetchall()
    conn.close()
    return [{"id": r[0], "text": r[1]} for r in rows]


@app.post("/notes")
def add_note(note: Note) -> dict:
    conn = db()
    cur = conn.execute("INSERT INTO notes (text) VALUES (?)", (note.text,))
    conn.commit()
    new_id = cur.lastrowid
    conn.close()
    return {"id": new_id, "text": note.text}


@app.get("/")
def index() -> dict:
    return {
        "app": "sqlite-notes",
        "database": str(DB.resolve()),
        "hint": "POST /notes {\"text\": \"…\"} then restart the pod: the notes "
        "survive only if this folder is on a disk.",
        "port": os.environ.get("PORT", "unset"),
    }
