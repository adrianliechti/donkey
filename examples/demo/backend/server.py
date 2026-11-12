"""An AI-triaged helpdesk for Packmule, a fictional file-sync product —
exposed twice: as a REST API for the dashboard, and as MCP tools for an
assistant that works the queue like any other agent.

Every feature runs on a space service the deploy provisions from this app's
addons (see donkey.yaml), reached only through injected env:

  postgres   the desk itself — tickets and their message threads
  s3         per-ticket attachments, plain S3 through boto3
  openai     the platform's AI proxy — it triages new tickets (category,
             priority, sentiment, a one-line summary), drafts replies as a
             live token stream, and invents realistic incoming tickets on
             demand

One thing lives on the app's own disk instead of a service: the desk's
activity log, an append-only file under /app/data (donkey mounts it from the
`storage:` block). It is the part of the app a redeploy cannot rebuild —
restart, redeploy, and the trail of who did what is still there.

Nothing connects at import time: the app has to boot before the services
exist, so each endpoint asks for its env when called and answers 503 with a
hint until the service is there. On the first request that finds postgres,
the desk seeds itself with a believable backlog — a demo that starts empty
shows nothing.
"""

from __future__ import annotations

import io
import json
import os
import random
import threading
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

import boto3
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

CATEGORIES = ["sync", "billing", "api", "account", "how-to"]
PRIORITIES = ["low", "normal", "high", "urgent"]
SENTIMENTS = ["calm", "frustrated", "angry"]


def now() -> datetime:
    return datetime.now(timezone.utc)


# ---------------------------------------------------------------------------
# The activity log, on the app's own disk. One JSON object per line, appended
# and never rewritten — `donkey files` can read it as easily as the API can.

DATA_DIR = Path(os.environ.get("DATA_DIR", "/app/data"))
_log_lock = threading.Lock()


def record(text: str, ticket_id: int | None = None) -> None:
    """Best effort on purpose: the desk must not fail a reply because its
    diary is unavailable (say, on a laptop without the mounted disk)."""
    line = json.dumps({"at": now().isoformat(), "ticket": ticket_id, "text": text})
    try:
        DATA_DIR.mkdir(parents=True, exist_ok=True)
        with _log_lock:
            with open(DATA_DIR / "activity.log", "a", encoding="utf-8") as f:
                f.write(line + "\n")
    except OSError:
        pass


def read_activity(limit: int = 12) -> list[dict[str, Any]]:
    """The newest entries, newest first. The file stays small enough that
    reading it whole beats being clever."""
    try:
        lines = (DATA_DIR / "activity.log").read_text(encoding="utf-8").splitlines()
    except OSError:
        return []
    entries = []
    for line in reversed(lines[-max(1, min(limit, 100)):]):
        try:
            entries.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return entries


# ---------------------------------------------------------------------------
# Service plumbing — the 503-with-a-hint pattern.


def missing(env: str, service: str) -> str | None:
    """A friendly sentence while the service's env is absent, None once it's
    there. The REST endpoints turn it into a 503; the MCP tools hand it to
    the model as-is, because a sentence is something a model can read and
    move on from."""
    if os.environ.get(env, ""):
        return None
    return f"the {service} service is not connected yet — `donkey deploy` provisions it from this app's addons"


def need(env: str, service: str) -> str:
    reason = missing(env, service)
    if reason:
        raise HTTPException(status_code=503, detail=reason)
    return os.environ[env]


# ---------------------------------------------------------------------------
# The desk, in the space's postgres.

_seeded = False
_seed_lock = threading.Lock()


def desk_db() -> psycopg.Connection:
    """A connection per request is plenty here. The idempotent CREATEs run
    every time on purpose: they cost one round-trip, and a remembered
    "table exists" would outlive the table (someone can drop it in the
    console)."""
    conn = psycopg.connect(need("DATABASE_URL", "postgres"), autocommit=True, row_factory=dict_row)
    conn.execute(
        """
        CREATE TABLE IF NOT EXISTS tickets (
            id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
            subject text NOT NULL,
            customer text NOT NULL,
            status text NOT NULL DEFAULT 'open',
            created timestamptz NOT NULL DEFAULT now(),
            closed timestamptz,
            -- The triage columns stay NULL until the AI has read the ticket.
            category text, priority text, sentiment text, summary text
        )
        """
    )
    conn.execute(
        """
        CREATE TABLE IF NOT EXISTS messages (
            id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
            ticket_id bigint NOT NULL REFERENCES tickets ON DELETE CASCADE,
            author text NOT NULL,
            body text NOT NULL,
            created timestamptz NOT NULL DEFAULT now()
        )
        """
    )
    global _seeded
    if not _seeded:
        with _seed_lock:
            if not _seeded:
                seed(conn)
                _seeded = True
    return conn


def seed(conn: psycopg.Connection) -> None:
    """Fill an empty desk with a believable backlog, exactly once.

    The advisory lock makes "once" hold across threads and replicas alike:
    whoever gets the lock first seeds, everyone else finds the count > 0.
    """
    with conn.transaction():
        conn.execute("SELECT pg_advisory_xact_lock(4242)")
        if conn.execute("SELECT count(*) AS n FROM tickets").fetchone()["n"] > 0:
            return
        tickets = _seed_tickets()
        record(f"seeded the desk with {len(tickets)} tickets")
        for t in tickets:
            row = conn.execute(
                """
                INSERT INTO tickets (subject, customer, status, created, closed,
                                     category, priority, sentiment, summary)
                VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s) RETURNING id
                """,
                (
                    t["subject"], t["customer"], t["status"], t["created"], t.get("closed"),
                    t.get("category"), t.get("priority"), t.get("sentiment"), t.get("summary"),
                ),
            ).fetchone()
            at = t["created"]
            for author, body in t["messages"]:
                conn.execute(
                    "INSERT INTO messages (ticket_id, author, body, created) VALUES (%s, %s, %s, %s)",
                    (row["id"], author, body, at),
                )
                at += timedelta(minutes=17, hours=2)
            # A couple of hero tickets come with the file the customer
            # attached — those live in S3, not in a table. Best effort:
            # the desk seeds fine even while the bucket isn't there yet.
            for name, content in t.get("attachments", {}).items():
                try:
                    put_attachment(row["id"], name, io.BytesIO(content.encode()), "text/plain")
                except Exception:
                    pass


def _seed_tickets() -> list[dict[str, Any]]:
    """Two years of helpdesk in miniature: ~20 hand-written tickets that read
    like real ones (the AI summarizes and answers these, so they had better),
    plus a closed background hum that gives the overview charts their shape.
    A fixed RNG keeps every fresh deploy telling the same story."""
    rng = random.Random(20260829)
    t0 = now()

    def ago(days: float = 0, hours: float = 0) -> datetime:
        return t0 - timedelta(days=days, hours=hours)

    heroes: list[dict[str, Any]] = [
        # The newest arrivals are deliberately untriaged — they are what the
        # "Triage" button (and an MCP assistant) is for.
        dict(
            subject="Files stuck at 'queued' since yesterday",
            customer="Mara Vogel", status="open", created=ago(hours=3),
            messages=[("customer",
                "Since yesterday afternoon our design folder just says 'queued — waiting to sync'. "
                "Nothing moves, restarting the app changes nothing, and the team is passing USB sticks "
                "around like it's 2005. I attached the debug log from the desktop app.")],
            attachments={"sync-debug.log": SYNC_LOG},
        ),
        dict(
            subject="App crashes on launch after today's update",
            customer="Jonas Frei", status="open", created=ago(hours=7),
            messages=[("customer",
                "After this morning's auto-update the desktop app bounces once in the dock and quits. "
                "macOS 15.6, worked fine yesterday. I'm mid-handover this week, so this is bad timing.")],
        ),
        dict(
            subject="Can I invite an external auditor read-only?",
            customer="Priya Natarajan", status="open", created=ago(days=1, hours=2),
            messages=[("customer",
                "Our annual audit starts Monday and the auditor needs to see the finance space without "
                "being able to change or download anything. Is there a read-only guest role, and does "
                "it cost a seat?")],
        ),
        # Open and already triaged — the queue as it looks mid-shift.
        dict(
            subject="Charged twice for the Team plan in July",
            customer="Elena Marchetti", status="open", created=ago(days=1, hours=6),
            category="billing", priority="urgent", sentiment="angry",
            summary="July's Team-plan charge went through twice; customer attached the card statement and wants one charge refunded.",
            messages=[
                ("customer",
                    "Your invoice 2026-07 shows up twice on our company card, same amount, one day apart. "
                    "I've attached the statement extract. Please refund one of them this week — finance "
                    "closes the quarter on Friday and I do not want to explain this line twice."),
                ("agent",
                    "You're right, and I'm sorry — I can see both charges on our side too. I've flagged the "
                    "duplicate for refund; it should reach your card within 3–5 business days. I'll write "
                    "back here the moment it clears."),
            ],
            attachments={"statement-extract.csv": STATEMENT_CSV},
        ),
        dict(
            subject="Webhooks stopped firing after we rotated keys",
            customer="Tomás Ferreira", status="open", created=ago(days=2),
            category="api", priority="high", sentiment="frustrated",
            summary="Webhook deliveries went silent after an API key rotation; integration relies on them for order intake.",
            messages=[
                ("customer",
                    "We rotated our API keys on Tuesday as part of a security review, and since then not a "
                    "single webhook has arrived. The dashboard says deliveries are 'active'. Our warehouse "
                    "integration is flying blind — is the old key still bound to the webhook config somewhere?"),
            ],
        ),
        dict(
            subject="SSO login loops back to the sign-in page",
            customer="Ingrid Halvorsen", status="open", created=ago(days=2, hours=12),
            category="account", priority="high", sentiment="frustrated",
            summary="Okta SSO redirects back to sign-in in a loop for every user since an IdP certificate update.",
            messages=[
                ("customer",
                    "Since our IT team renewed the Okta certificate, everyone who signs in gets bounced "
                    "straight back to the sign-in page. No error, just a loop. 40 people are locked out "
                    "of their files. What do you need from us — the new cert fingerprint?"),
            ],
        ),
        dict(
            subject="Large video files fail at 98%",
            customer="Kwame Mensah", status="open", created=ago(days=3),
            category="sync", priority="high", sentiment="frustrated",
            summary="Multi-gigabyte video uploads consistently fail at 98% and restart from zero; smaller files sync fine.",
            messages=[
                ("customer",
                    "Every file over about 4 GB climbs to 98%, sits there for a minute, then starts over "
                    "from zero. Smaller files are fine. We're a video production house — 98% of a 40 GB "
                    "upload is a lot of bandwidth to burn three times a day."),
                ("agent",
                    "That pattern usually means the final assembly step times out rather than the upload "
                    "itself. Could you tell me roughly how many parallel uploads run when it happens? "
                    "Meanwhile I've raised the assembly timeout for your workspace."),
            ],
        ),
        dict(
            subject="Need our VAT number on invoices",
            customer="Sofia Lindqvist", status="open", created=ago(days=4),
            category="billing", priority="low", sentiment="calm",
            summary="Customer needs the company VAT number printed on future invoices and reissues for the last quarter.",
            messages=[
                ("customer",
                    "Our accountant returned last quarter's invoices because they're missing our VAT "
                    "number (SE556677-8899). Could you add it to future invoices and reissue April–June? "
                    "No rush before month end."),
            ],
        ),
        dict(
            subject="Selective sync ignores new subfolders",
            customer="Daan Verhoeven", status="open", created=ago(days=5),
            category="sync", priority="normal", sentiment="calm",
            summary="New subfolders under a selectively-synced folder don't appear on other machines until sync is toggled off and on.",
            messages=[
                ("customer",
                    "When someone creates a new subfolder inside a folder I've selectively synced, it never "
                    "shows up on my machine until I untick and re-tick the parent. Is that by design? It "
                    "surprises everyone we onboard."),
            ],
        ),
        dict(
            subject="Getting 429s on the list endpoint at low volume",
            customer="Aiko Tanaka", status="open", created=ago(days=6),
            category="api", priority="normal", sentiment="calm",
            summary="List-files endpoint returns 429 rate limits despite roughly one request per second; asks what the actual limits are.",
            messages=[
                ("customer",
                    "Our nightly job walks the file tree at about one request per second and still hits "
                    "429s after a few minutes. The docs say 'fair use'. What are the real limits, and is "
                    "there a bulk listing endpoint we should use instead?"),
            ],
        ),
        # Recently closed, thread and resolution intact — history worth reading.
        dict(
            subject="Locked out after enabling 2FA",
            customer="Rachel Osei", status="closed", created=ago(days=8), closed=ago(days=7, hours=18),
            category="account", priority="urgent", sentiment="angry",
            summary="Customer lost access immediately after enabling 2FA — authenticator was set up on a phone that was then wiped.",
            messages=[
                ("customer",
                    "I enabled two-factor this morning, then my phone got a corporate wipe an hour later. "
                    "The recovery codes are in a file that is — of course — inside Packmule. I'm locked out "
                    "of my own lockbox. Help."),
                ("agent",
                    "That's a genuinely bad loop, and we can break it. I've sent a recovery link to your "
                    "verified work email and disabled 2FA on the account after the identity check. Please "
                    "re-enable it once your new phone is set up — and maybe print the codes this time."),
                ("customer", "Back in. Printing the codes now. Thank you for the fast save."),
            ],
        ),
        dict(
            subject="Conflict copies multiplying after laptop restore",
            customer="Marco Bianchi", status="closed", created=ago(days=10), closed=ago(days=9),
            category="sync", priority="normal", sentiment="frustrated",
            summary="A restored laptop re-uploaded stale files, producing dozens of conflict copies across the workspace.",
            messages=[
                ("customer",
                    "IT restored my laptop from a two-month-old image and now the whole team's folders are "
                    "full of 'conflicted copy (Marco's MacBook)' files. Dozens of them. Which ones are safe "
                    "to delete, and can we stop the machine from doing it again?"),
                ("agent",
                    "The restored machine believed its old files were newer, so Packmule kept both — the "
                    "conflict copies are the stale ones and are safe to delete. I've paused sync from that "
                    "device; unpause it after IT updates the image, and the versions panel can restore "
                    "anything that goes missing."),
            ],
        ),
        dict(
            subject="Downgrade didn't take effect",
            customer="Lucie Novak", status="closed", created=ago(days=12), closed=ago(days=11),
            category="billing", priority="normal", sentiment="frustrated",
            summary="Plan downgrade requested before renewal still billed at the old tier; customer expects a credit.",
            messages=[
                ("customer",
                    "I downgraded us from Business to Team on the 28th, before renewal, and we were still "
                    "billed for Business. The settings page even says Team. Please credit the difference."),
                ("agent",
                    "The downgrade was scheduled correctly but our renewal ran a few hours earlier — that's "
                    "on us. I've credited the difference to your next invoice and moved your renewal time "
                    "so this can't race again."),
            ],
        ),
        dict(
            subject="Transfer workspace ownership to a colleague",
            customer="Hannah Berg", status="closed", created=ago(days=15), closed=ago(days=14),
            category="account", priority="normal", sentiment="calm",
            summary="Departing owner needs to hand the workspace to a colleague without disturbing shares or integrations.",
            messages=[
                ("customer",
                    "I'm leaving the company at the end of the month and need to hand the workspace to my "
                    "colleague Felix. Will existing shares and API integrations survive the transfer?"),
                ("agent",
                    "Yes — ownership is a role, not a plumbing change: shares, folders and API tokens all "
                    "stay put. I've sent Felix the transfer invitation; it completes the moment he accepts."),
            ],
        ),
        dict(
            subject="Do you offer education pricing?",
            customer="Diego Fuentes", status="closed", created=ago(days=18), closed=ago(days=17, hours=12),
            category="billing", priority="low", sentiment="calm",
            summary="A university lab asks about education pricing for around 30 seats.",
            messages=[
                ("customer",
                    "We're a university research lab, about 30 people, mostly grant-funded. Is there "
                    "education pricing, and does it include the API?"),
                ("agent",
                    "There is — 40% off the Team plan for accredited institutions, API included. I've "
                    "applied the discount to your workspace; it shows on your next invoice."),
            ],
        ),
        dict(
            subject="How do I upload files over 5 GB through the API?",
            customer="Nadia Haddad", status="closed", created=ago(days=21), closed=ago(days=20),
            category="api", priority="low", sentiment="calm",
            summary="Direct API uploads fail past 5 GB; customer needs the chunked-upload flow.",
            messages=[
                ("customer",
                    "POST /files rejects anything over 5 GB with 413. We archive microscopy data — "
                    "everything is over 5 GB. What's the right way?"),
                ("agent",
                    "Past 5 GB you want the chunked flow: open an upload session, PUT the parts in "
                    "parallel, then commit. The SDK's upload_large() does the whole dance for you — "
                    "one call, resumable included."),
            ],
        ),
        dict(
            subject="Share a folder with view-only access?",
            customer="Emma Sutherland", status="closed", created=ago(days=24), closed=ago(days=23, hours=20),
            category="how-to", priority="low", sentiment="calm",
            summary="Customer wants clients to view a folder without download or edit rights.",
            messages=[
                ("customer",
                    "We share concept work with clients before contracts are signed. Can they view a "
                    "folder in the browser without being able to download or edit?"),
                ("agent",
                    "Yes — share the folder as a link, and in the link settings pick 'view in browser "
                    "only'. Downloads are disabled and every preview is watermarked with the viewer's "
                    "email."),
            ],
        ),
        dict(
            subject="Best way to move 2 TB into Packmule?",
            customer="Oliver Krause", status="closed", created=ago(days=28), closed=ago(days=27),
            category="how-to", priority="low", sentiment="calm",
            summary="New customer plans a 2 TB initial migration from a NAS and asks for the recommended path.",
            messages=[
                ("customer",
                    "We're moving off an ageing office NAS — about 2 TB. Drag it into the desktop app and "
                    "wait, or is there a smarter way?"),
                ("agent",
                    "The desktop app will get there, but the CLI's `packmule migrate` is built for exactly "
                    "this: it walks the NAS directly, parallelizes, survives restarts, and verifies "
                    "checksums at the end. Expect 2 TB to take a weekend on office fibre."),
            ],
        ),
        dict(
            subject="Does the desktop app work offline?",
            customer="Fatima Al-Rashid", status="closed", created=ago(days=33), closed=ago(days=32, hours=12),
            category="how-to", priority="low", sentiment="calm",
            summary="Field teams need offline access with sync-on-return; asks how conflicts are handled.",
            messages=[
                ("customer",
                    "Our survey teams spend days without connectivity. Can they keep working on synced "
                    "files offline, and what happens when two people edited the same sheet by the time "
                    "they're back?"),
                ("agent",
                    "Offline just works — files marked 'available offline' stay editable, and changes "
                    "upload on return. If two people changed the same file, both versions are kept and "
                    "the file gets a conflict badge so the team decides which wins. Nothing is ever "
                    "silently overwritten."),
            ],
        ),
        dict(
            subject="Sandbox API tokens expire after an hour",
            customer="Georg Weiss", status="closed", created=ago(days=38), closed=ago(days=37),
            category="api", priority="normal", sentiment="calm",
            summary="Sandbox tokens expiring hourly break long integration test runs; production tokens are unaffected.",
            messages=[
                ("customer",
                    "Our CI suite runs 90 minutes against the sandbox and the token dies at 60. Production "
                    "tokens last a year. Is the short sandbox lifetime intentional?"),
                ("agent",
                    "Intentional but adjustable — sandbox tokens default to an hour so leaked test creds "
                    "age out fast. I've raised your sandbox workspace to 4 hours, and the refresh-token "
                    "flow works there too if you'd rather never think about it."),
            ],
        ),
    ]

    # The background hum: closed, one-line tickets that give the trend and
    # category charts twelve weeks of shape. The inbox opens on the queue, so
    # these stay out of the way until someone browses history.
    subjects = {
        "sync": [
            ("Sync paused itself after a network change", "Sync stopped after switching networks and needed a manual resume."),
            ("External drive folder won't stay synced", "A folder on an external drive drops out of sync when the drive remounts."),
            ("Old versions filling the trash", "Customer asks how long deleted versions are kept and how to purge them."),
            ("Two machines show different file counts", "Two of the customer's machines disagree on a folder's file count."),
            ("Ignore pattern for build artifacts", "Customer wants node_modules-style folders excluded from sync."),
            ("Camera uploads duplicated photos", "Phone camera uploads created duplicates after a reinstall."),
        ],
        "billing": [
            ("Update the card on file", "Customer needs to replace an expiring company card."),
            ("Receipt needed for expense report", "Customer asks for a per-seat receipt for expenses."),
            ("Question about proration on upgrades", "Customer asks how mid-cycle seat additions are billed."),
            ("Annual billing discount", "Customer asks whether paying yearly is cheaper."),
            ("Seat count wrong after offboarding", "Removed users still counted as billable seats."),
            ("Currency of invoices", "Customer asks to be billed in EUR instead of USD."),
        ],
        "api": [
            ("Pagination cursor expired mid-run", "A long export hit an expired pagination cursor."),
            ("Webhook retry policy", "Customer asks how often failed webhook deliveries retry."),
            ("Scopes for a read-only integration", "Customer asks which OAuth scopes give read-only access."),
            ("Search endpoint character escaping", "Special characters in search queries return errors."),
            ("Rate limit headers missing", "Responses lack the documented rate-limit headers."),
            ("SDK support for Deno", "Customer asks whether the JS SDK runs on Deno."),
        ],
        "account": [
            ("Change the workspace URL", "Customer wants to rename their workspace subdomain."),
            ("Merge two personal accounts", "Customer accidentally created two accounts and wants them merged."),
            ("Enforce 2FA for all members", "Admin asks how to require two-factor workspace-wide."),
            ("Remove a former employee's sessions", "Admin wants a departed employee signed out everywhere."),
            ("Email change not confirmed", "The confirmation mail for an email change never arrived."),
            ("Guest seat limits", "Customer asks how many guests a Team plan allows."),
        ],
        "how-to": [
            ("Keyboard shortcuts for the web app", "Customer asks whether the web app has shortcuts."),
            ("Recover a deleted folder", "Customer needs to restore a folder deleted last week."),
            ("Set up a team space for contractors", "Customer asks how to isolate contractor access."),
            ("Preview CAD files in the browser", "Customer asks which CAD formats preview inline."),
            ("Automatic photo albums", "Customer asks whether photos can auto-group into albums."),
            ("Dark mode on Windows", "Customer can't find the dark-mode setting on Windows."),
        ],
    }
    names = [
        "Yuki Sato", "Pedro Alves", "Anna Keller", "Lars Nielsen", "Chloe Martin",
        "Omar Farouk", "Julia Weber", "Ravi Patel", "Nina Petrova", "Sam O'Brien",
        "Leila Nasser", "Felix Brunner", "Maya Cohen", "Jack Thompson", "Aline Dubois",
        "Henrik Olsen", "Carmen Ruiz", "Noah Fischer", "Grace Kim", "Ivan Horvat",
    ]
    closings = [
        "Sorted now — thanks for flagging it, and write back any time.",
        "This is resolved on our side; give it a try and shout if anything still looks off.",
        "Done! The details are in your workspace's audit log if you're curious.",
    ]
    background = []
    for _ in range(100):
        category = rng.choice(CATEGORIES)
        subject, summary = rng.choice(subjects[category])
        # Skewing toward 0 keeps recent weeks busier — a desk that looks alive.
        age = 2 + 82 * rng.random() ** 1.4
        created = ago(days=age)
        background.append(
            dict(
                subject=subject,
                customer=rng.choice(names),
                status="closed",
                created=created,
                closed=created + timedelta(hours=rng.uniform(1, 96)),
                category=category,
                priority=rng.choices(PRIORITIES, weights=[35, 45, 15, 5])[0],
                sentiment=rng.choices(SENTIMENTS, weights=[70, 25, 5])[0],
                summary=summary,
                messages=[
                    ("customer", f"{summary} Could you take a look?"),
                    ("agent", rng.choice(closings)),
                ],
            )
        )
    return heroes + background


SYNC_LOG = """\
2026-08-28T14:02:11Z INFO  sync.queue   37 items queued (design/)
2026-08-28T14:02:11Z INFO  sync.upload  starting batch 1/5
2026-08-28T14:02:14Z WARN  sync.upload  chunk 3 rejected: quota check timed out
2026-08-28T14:02:14Z INFO  sync.queue   requeueing batch 1 (attempt 2)
2026-08-28T14:03:02Z WARN  sync.upload  chunk 3 rejected: quota check timed out
2026-08-28T14:03:02Z ERROR sync.queue   giving up on batch 1, parking queue
2026-08-28T14:03:02Z INFO  sync.state   state=queued reason=quota-check-unavailable
"""

STATEMENT_CSV = """\
date,description,amount,currency
2026-07-03,PACKMULE TEAM PLAN JUL,249.00,EUR
2026-07-04,PACKMULE TEAM PLAN JUL,249.00,EUR
2026-07-05,OFFICE COFFEE SUPPLY,84.20,EUR
"""


# ---------------------------------------------------------------------------
# Attachments, in the space's bucket. There is no attachments table: the
# bucket itself is the source of truth, one prefix per ticket.

BUCKET = "attachments"
_bucket_ready = False


def s3() -> Any:
    """The object store speaks S3, so boto3 works as-is — only the endpoint needs saying."""
    global _bucket_ready
    client = boto3.client("s3", endpoint_url=need("AWS_ENDPOINT_URL_S3", "s3"))
    if not _bucket_ready:
        try:
            client.create_bucket(Bucket=BUCKET)
        except ClientError as exc:
            # A bucket that already exists is exactly what we wanted.
            if exc.response["Error"]["Code"] not in ("BucketAlreadyOwnedByYou", "BucketAlreadyExists"):
                raise
        _bucket_ready = True
    return client


def list_attachments(ticket_id: int) -> list[dict[str, Any]]:
    prefix = f"tickets/{ticket_id}/"
    objects = s3().list_objects_v2(Bucket=BUCKET, Prefix=prefix).get("Contents", [])
    return [
        {"name": o["Key"][len(prefix):], "size": o["Size"], "modified": o["LastModified"].isoformat()}
        for o in sorted(objects, key=lambda o: o["Key"])
    ]


def put_attachment(ticket_id: int, name: str, fileobj: Any, content_type: str) -> str:
    # Whatever path the client claims, only the basename becomes the key.
    name = Path(name or "upload").name
    s3().upload_fileobj(
        fileobj, BUCKET, f"tickets/{ticket_id}/{name}", ExtraArgs={"ContentType": content_type}
    )
    return name


def get_attachment(ticket_id: int, name: str) -> Any:
    try:
        return s3().get_object(Bucket=BUCKET, Key=f"tickets/{ticket_id}/{Path(name).name}")
    except ClientError as exc:
        if exc.response["Error"]["Code"] in ("NoSuchKey", "404"):
            raise HTTPException(status_code=404, detail="no attachment by that name") from exc
        raise


# ---------------------------------------------------------------------------
# The AI, through the platform's openai proxy. Base URL and key arrive as
# injected env; the OpenAI client picks both up on its own.


def ai() -> tuple[OpenAI, str]:
    need("OPENAI_BASE_URL", "openai")
    client = OpenAI(timeout=45)
    model = os.environ.get("OPENAI_MODEL", "")
    if not model:
        try:
            model = client.models.list().data[0].id
        except Exception:
            model = "gpt-4o-mini"
    return client, model


def ai_json(text: str) -> dict[str, Any]:
    """The model was asked for bare JSON, but models decorate — take the
    outermost object and ignore any prose or fences around it."""
    start, end = text.find("{"), text.rfind("}")
    if start < 0 or end <= start:
        raise ValueError("the model did not answer with JSON")
    return json.loads(text[start:end + 1])


def pick(value: Any, allowed: list[str], fallback: str) -> str:
    value = str(value or "").strip().lower()
    return value if value in allowed else fallback


def thread_text(ticket: dict[str, Any], messages: list[dict[str, Any]]) -> str:
    lines = [f"Subject: {ticket['subject']}", f"From: {ticket['customer']}", ""]
    lines += [f"[{m['author']}] {m['body']}" for m in messages]
    return "\n".join(lines)


TRIAGE_PROMPT = f"""You triage support tickets for Packmule, a file-sync product for teams.
Read the ticket and answer with only a JSON object, no other text:
{{"category": one of {CATEGORIES}, "priority": one of {PRIORITIES},
"sentiment": the customer's tone, one of {SENTIMENTS},
"summary": one plain sentence a busy agent can act on}}"""


def run_triage(ticket_id: int) -> dict[str, Any]:
    """Have the model read the thread, then write its verdict onto the ticket."""
    ticket, messages = fetch_thread(ticket_id)
    client, model = ai()
    reply = client.chat.completions.create(
        model=model,
        messages=[
            {"role": "system", "content": TRIAGE_PROMPT},
            {"role": "user", "content": thread_text(ticket, messages)},
        ],
    )
    verdict = ai_json(reply.choices[0].message.content or "")
    with desk_db() as conn:
        updated = conn.execute(
            """
            UPDATE tickets SET category = %s, priority = %s, sentiment = %s, summary = %s
            WHERE id = %s
            RETURNING id, subject, customer, status, created, closed,
                      category, priority, sentiment, summary
            """,
            (
                pick(verdict.get("category"), CATEGORIES, "how-to"),
                pick(verdict.get("priority"), PRIORITIES, "normal"),
                pick(verdict.get("sentiment"), SENTIMENTS, "calm"),
                str(verdict.get("summary") or "").strip() or ticket["subject"],
                ticket_id,
            ),
        ).fetchone()
        record(
            f"AI triaged #{ticket_id}: {updated['category']} · {updated['priority']} · {updated['sentiment']}",
            ticket_id,
        )
        return updated


DRAFT_PROMPT = """You are a support agent at Packmule, a file-sync product for teams.
Write the next reply to the customer in this ticket: warm, concrete and short —
two paragraphs at most. If something must be checked or done on Packmule's side,
say so plainly instead of promising magic. Address the customer by first name,
no subject line, sign off as "— the Packmule team"."""


def stream_draft(ticket_id: int):
    """Yield the model's reply as it is written — the dashboard types it into
    the reply box token by token, which is the whole charm."""
    ticket, messages = fetch_thread(ticket_id)
    client, model = ai()
    stream = client.chat.completions.create(
        model=model,
        messages=[
            {"role": "system", "content": DRAFT_PROMPT},
            {"role": "user", "content": thread_text(ticket, messages)},
        ],
        stream=True,
    )
    for chunk in stream:
        if chunk.choices and chunk.choices[0].delta.content:
            yield chunk.choices[0].delta.content


SCENARIOS = [
    "a shared folder that two departments see with different contents",
    "an invoice charged to a card that was cancelled last month",
    "webhook deliveries arriving twice since yesterday",
    "a laptop stolen on a train and what to do about the files on it",
    "the mobile app draining battery since the last release",
    "a polite but firm refund request after an accidental annual renewal",
    "a security questionnaire from an enterprise prospect's IT department",
    "sync conflicts after two people edited the same spreadsheet offline",
    "a full data export before a company merger",
    "the browser preview showing an old version of a presentation",
    "a new hire who cannot see any of the team's folders",
    "an API integration that started timing out this morning",
]

SIMULATE_PROMPT = """Invent one realistic inbound support ticket for Packmule,
a file-sync product for teams. Write in the customer's voice — specific, human,
2 to 4 sentences, mild details invented freely (OS, plan, team size). Answer
with only a JSON object: {"customer": a full name, "subject": a short subject
line, "message": the ticket text}"""

FALLBACK_TICKETS = [
    ("Robin Eriksson", "Shared link opens to an empty folder",
     "I shared our press-kit folder with a journalist and she says the link opens to an empty page. "
     "On my side the folder has 14 files. Same link, different worlds — what's going on?"),
    ("Carla Mendes", "Need yesterday's version of a contract",
     "A colleague overwrote contract_final.docx with the wrong draft. I can see version history "
     "exists but the restore button is greyed out for me. Time-sensitive, signing is tomorrow."),
    ("Ben Carter", "Desktop app stuck 'checking for changes'",
     "Since Monday the desktop app on my Windows 11 machine says 'checking for changes' forever "
     "and nothing syncs. My colleague's Mac is fine. Reinstalling didn't help."),
]


def simulate_ticket() -> dict[str, Any]:
    """A fresh customer walks in: the model invents the ticket, the desk files
    it, and triage runs on it right away — the demo's one-button story. With
    no AI connected the walk-in still happens, from a small canned pool, and
    lands untriaged (which is honest: nobody read it yet)."""
    if missing("OPENAI_BASE_URL", "openai"):
        customer, subject, message = random.choice(FALLBACK_TICKETS)
    else:
        client, model = ai()
        reply = client.chat.completions.create(
            model=model,
            messages=[
                {"role": "system", "content": SIMULATE_PROMPT},
                {"role": "user", "content": f"Scenario: {random.choice(SCENARIOS)}"},
            ],
        )
        invented = ai_json(reply.choices[0].message.content or "")
        customer = str(invented.get("customer") or "Alex Miller").strip()
        subject = str(invented.get("subject") or "A question about Packmule").strip()
        message = str(invented.get("message") or "Something is off — can you take a look?").strip()

    with desk_db() as conn:
        ticket = conn.execute(
            "INSERT INTO tickets (subject, customer) VALUES (%s, %s) RETURNING id",
            (subject, customer),
        ).fetchone()
        conn.execute(
            "INSERT INTO messages (ticket_id, author, body) VALUES (%s, 'customer', %s)",
            (ticket["id"], message),
        )
    record(f"walk-in ticket #{ticket['id']} filed: {subject}", ticket["id"])
    try:
        run_triage(ticket["id"])
    except Exception:
        pass  # an untriaged walk-in is a fine outcome — the Triage button exists
    return {"id": ticket["id"]}


# ---------------------------------------------------------------------------
# The desk's core reads and writes. The REST endpoints and the MCP tools both
# go through these, so the two faces of the desk cannot drift apart.

TICKET_COLUMNS = """t.id, t.subject, t.customer, t.status, t.created, t.closed,
                    t.category, t.priority, t.sentiment, t.summary"""


def list_tickets(status: str | None = None, q: str | None = None, limit: int = 400) -> list[dict[str, Any]]:
    where, params = [], []
    if status in ("open", "closed"):
        where.append("t.status = %s")
        params.append(status)
    if q:
        where.append("(t.subject ILIKE %s OR t.customer ILIKE %s)")
        params += [f"%{q}%", f"%{q}%"]
    with desk_db() as conn:
        return conn.execute(
            f"""
            SELECT {TICKET_COLUMNS}, count(m.id) AS messages, max(m.created) AS last_activity
            FROM tickets t LEFT JOIN messages m ON m.ticket_id = t.id
            {"WHERE " + " AND ".join(where) if where else ""}
            GROUP BY t.id ORDER BY t.created DESC LIMIT %s
            """,
            params + [max(1, min(limit, 400))],
        ).fetchall()


def fetch_thread(ticket_id: int) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    with desk_db() as conn:
        ticket = conn.execute(
            f"SELECT {TICKET_COLUMNS} FROM tickets t WHERE t.id = %s", (ticket_id,)
        ).fetchone()
        if not ticket:
            raise HTTPException(status_code=404, detail="no ticket with that id")
        messages = conn.execute(
            "SELECT id, author, body, created FROM messages WHERE ticket_id = %s ORDER BY created, id",
            (ticket_id,),
        ).fetchall()
    return ticket, messages


def add_reply(ticket_id: int, body: str, close: bool = False) -> dict[str, Any]:
    body = body.strip()
    if not body:
        raise HTTPException(status_code=422, detail="an empty reply helps nobody")
    with desk_db() as conn:
        message = conn.execute(
            """
            INSERT INTO messages (ticket_id, author, body) VALUES (%s, 'agent', %s)
            RETURNING id, author, body, created
            """,
            (ticket_id, body),
        ).fetchone()
        if close:
            conn.execute("UPDATE tickets SET status = 'closed', closed = now() WHERE id = %s", (ticket_id,))
    record(f"reply sent on #{ticket_id}" + (" and closed it" if close else ""), ticket_id)
    return message


def set_status(ticket_id: int, status: str) -> dict[str, Any]:
    if status not in ("open", "closed"):
        raise HTTPException(status_code=422, detail="status is either open or closed")
    with desk_db() as conn:
        row = conn.execute(
            """
            UPDATE tickets SET status = %s, closed = CASE WHEN %s = 'closed' THEN now() END
            WHERE id = %s RETURNING id, status, closed
            """,
            (status, status, ticket_id),
        ).fetchone()
    if not row:
        raise HTTPException(status_code=404, detail="no ticket with that id")
    record(f"ticket #{ticket_id} {'closed' if status == 'closed' else 'reopened'}", ticket_id)
    return row


def desk_stats() -> dict[str, Any]:
    with desk_db() as conn:
        counts = conn.execute(
            """
            SELECT count(*) FILTER (WHERE status = 'open') AS open,
                   count(*) FILTER (WHERE status = 'open' AND priority IN ('urgent', 'high')) AS attention,
                   count(*) FILTER (WHERE created >= now() - interval '7 days') AS new_this_week
            FROM tickets
            """
        ).fetchone()
        resolution = conn.execute(
            """
            SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM closed - created)) AS median
            FROM tickets WHERE closed IS NOT NULL AND closed >= now() - interval '30 days'
            """
        ).fetchone()
        weekly = conn.execute(
            """
            SELECT to_char(w, 'YYYY-MM-DD') AS week, count(t.id) AS count
            FROM generate_series(date_trunc('week', now()) - interval '11 weeks',
                                 date_trunc('week', now()), interval '1 week') AS w
            LEFT JOIN tickets t ON date_trunc('week', t.created) = w
            GROUP BY w ORDER BY w
            """
        ).fetchall()
        categories = conn.execute(
            """
            SELECT category, count(*) AS total, count(*) FILTER (WHERE status = 'open') AS open
            FROM tickets WHERE category IS NOT NULL GROUP BY category ORDER BY total DESC
            """
        ).fetchall()
        priorities = conn.execute(
            """
            SELECT priority, count(*) AS open FROM tickets
            WHERE status = 'open' AND priority IS NOT NULL GROUP BY priority
            """
        ).fetchall()
    return {
        "open": counts["open"],
        "attention": counts["attention"],
        "new_this_week": counts["new_this_week"],
        "median_resolution_seconds": resolution["median"],
        "weekly": weekly,
        "categories": categories,
        "priorities": {p["priority"]: p["open"] for p in priorities},
    }


# ---------------------------------------------------------------------------
# The REST face, for the dashboard.

app = FastAPI(title="Packmule Support — donkey demo")


@app.get("/healthz")
def healthz() -> dict[str, Any]:
    """Alive is enough: the desk boots before its services exist on purpose,
    so readiness must not depend on them."""
    return {"ok": True}


@app.get("/api/tickets")
def api_tickets(status: str | None = None, q: str | None = None) -> list[dict[str, Any]]:
    return list_tickets(status, q)


@app.get("/api/tickets/{ticket_id}")
def api_ticket(ticket_id: int) -> dict[str, Any]:
    ticket, messages = fetch_thread(ticket_id)
    # The thread renders even while the bucket is missing — the attachments
    # strip degrades to its own hint instead of taking the page down.
    hint = missing("AWS_ENDPOINT_URL_S3", "s3")
    return {
        "ticket": ticket,
        "messages": messages,
        "attachments": None if hint else list_attachments(ticket_id),
        "attachments_hint": hint,
    }


class ReplyIn(BaseModel):
    body: str
    close: bool = False


@app.post("/api/tickets/{ticket_id}/messages")
def api_reply(ticket_id: int, reply: ReplyIn) -> dict[str, Any]:
    fetch_thread(ticket_id)  # 404 before insert, not a foreign-key stacktrace
    return add_reply(ticket_id, reply.body, reply.close)


class StatusIn(BaseModel):
    status: str


@app.patch("/api/tickets/{ticket_id}")
def api_set_status(ticket_id: int, body: StatusIn) -> dict[str, Any]:
    return set_status(ticket_id, body.status)


@app.post("/api/tickets/{ticket_id}/triage")
def api_triage(ticket_id: int) -> dict[str, Any]:
    need("OPENAI_BASE_URL", "openai")
    fetch_thread(ticket_id)
    try:
        return run_triage(ticket_id)
    except (ValueError, json.JSONDecodeError) as exc:
        raise HTTPException(status_code=502, detail="the model's triage answer made no sense — try again") from exc


@app.post("/api/tickets/{ticket_id}/draft")
def api_draft(ticket_id: int) -> StreamingResponse:
    need("OPENAI_BASE_URL", "openai")
    fetch_thread(ticket_id)
    return StreamingResponse(stream_draft(ticket_id), media_type="text/plain; charset=utf-8")


@app.post("/api/simulate")
def api_simulate() -> dict[str, Any]:
    return simulate_ticket()


@app.get("/api/stats")
def api_stats() -> dict[str, Any]:
    return desk_stats()


@app.get("/api/activity")
def api_activity(limit: int = 12) -> list[dict[str, Any]]:
    """What just happened on the desk — read from the app's own disk, the one
    piece of state that is neither in postgres nor in the bucket."""
    return read_activity(limit)


@app.post("/api/tickets/{ticket_id}/attachments")
def api_upload(ticket_id: int, file: UploadFile) -> dict[str, Any]:
    fetch_thread(ticket_id)
    name = put_attachment(
        ticket_id, file.filename or "upload", file.file, file.content_type or "application/octet-stream"
    )
    record(f"attachment '{name}' added to #{ticket_id}", ticket_id)
    return {"name": name}


@app.get("/api/tickets/{ticket_id}/attachments/{name}")
def api_download(ticket_id: int, name: str) -> StreamingResponse:
    obj = get_attachment(ticket_id, name)
    return StreamingResponse(
        obj["Body"].iter_chunks(),
        media_type=obj.get("ContentType") or "application/octet-stream",
        headers={"content-disposition": f'attachment; filename="{Path(name).name}"'},
    )


# ---------------------------------------------------------------------------
# The MCP face, for an assistant. Same cores as the REST endpoints, so an
# assistant working the queue and an agent watching the dashboard always see
# the same desk.

mcp = FastMCP("packmule-support")


@mcp.tool
def tickets_queue(status: str = "open", q: str = "", limit: int = 20) -> list[dict[str, Any]] | str:
    """The support queue, newest first.

    Args:
        status: open, closed, or all.
        q: optional text to match against subject and customer name.
        limit: how many tickets to return (1-100).
    """
    reason = missing("DATABASE_URL", "postgres")
    if reason:
        return reason
    return list_tickets(status if status in ("open", "closed") else None, q or None, max(1, min(limit, 100)))


@mcp.tool
def ticket_read(id: int) -> dict[str, Any] | str:
    """One ticket in full: the thread, the triage verdict, the attachments.

    Args:
        id: the ticket's id, as tickets_queue reports it.
    """
    reason = missing("DATABASE_URL", "postgres")
    if reason:
        return reason
    ticket, messages = fetch_thread(id)
    attachments: Any = missing("AWS_ENDPOINT_URL_S3", "s3") or list_attachments(id)
    return {"ticket": ticket, "messages": messages, "attachments": attachments}


@mcp.tool
def ticket_reply(id: int, body: str, close: bool = False) -> dict[str, Any] | str:
    """Answer the customer as a support agent, optionally closing the ticket.

    Args:
        id: the ticket's id.
        body: the reply text the customer will read.
        close: also close the ticket once the reply is sent.
    """
    reason = missing("DATABASE_URL", "postgres")
    if reason:
        return reason
    fetch_thread(id)
    return add_reply(id, body, close)


@mcp.tool
def ticket_triage(id: int) -> dict[str, Any] | str:
    """Run AI triage on a ticket: category, priority, sentiment and a
    one-line summary get written onto it.

    Args:
        id: the ticket's id.
    """
    for env, service in (("DATABASE_URL", "postgres"), ("OPENAI_BASE_URL", "openai")):
        reason = missing(env, service)
        if reason:
            return reason
    return run_triage(id)


@mcp.tool
def attachment_read(id: int, name: str) -> str:
    """Read a small text attachment from a ticket.

    Only text up to 64 KB comes back — anything bigger or binary belongs to
    the dashboard's download link, not a tool reply.

    Args:
        id: the ticket's id.
        name: the attachment's name, as ticket_read reports it.
    """
    for env, service in (("DATABASE_URL", "postgres"), ("AWS_ENDPOINT_URL_S3", "s3")):
        reason = missing(env, service)
        if reason:
            return reason
    obj = get_attachment(id, name)
    if obj["ContentLength"] > 64 * 1024:
        raise ValueError("that attachment is bigger than 64 KB — too much for a tool reply")
    try:
        return obj["Body"].read().decode("utf-8")
    except UnicodeDecodeError as exc:
        raise ValueError("that attachment is not text") from exc


@mcp.tool
def desk_overview() -> dict[str, Any] | str:
    """The numbers on the wall: open tickets, what needs attention, this
    week's volume, median time to resolution, and the breakdowns."""
    reason = missing("DATABASE_URL", "postgres")
    if reason:
        return reason
    return desk_stats()


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
# FastMCP's own lifespan starts its session manager — without it, the mounted
# MCP app has no sessions. Mounting last means the API routes above match
# first and everything else goes to the MCP app.
app.router.lifespan_context = mcp_app.lifespan
app.mount("/", mcp_app)
