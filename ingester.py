"""
ingester.py — Layer 1: Feed fetching and raw article storage

Responsibilities:
  - Fetch RSS/Atom feeds from configured sources
  - Deduplicate by URL and content hash before inserting
  - Store raw articles in SQLite for downstream processing
  - Track per-source fetch metadata (last_fetched_at, etag, etc.)

Usage:
  python ingester.py                  # run once
  python ingester.py --source-url <url>  # add a new source then run
"""

import argparse
import hashlib
import logging
import sqlite3
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

import feed_adapter

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

DB_PATH = Path("digest.db")
REQUEST_DELAY_SEC = 1.0
USER_AGENT = "RSSDigest/0.1 (self-hosted; +https://github.com/you/rss-digest)"

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
log = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Database setup
# ---------------------------------------------------------------------------

def get_db(path: Path = DB_PATH) -> sqlite3.Connection:
    """Open the SQLite database, enabling WAL mode for safe concurrent reads and writes."""
    conn = sqlite3.connect(path)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def init_db(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS sources (
            id              INTEGER PRIMARY KEY,
            url             TEXT UNIQUE NOT NULL,
            name            TEXT,
            home_url        TEXT,
            credibility     REAL DEFAULT 0.5,
            active          INTEGER DEFAULT 1,
            etag            TEXT,
            last_modified   TEXT,
            last_fetched_at TEXT,
            created_at      TEXT DEFAULT (datetime('now'))
        );

        CREATE TABLE IF NOT EXISTS articles (
            id            INTEGER PRIMARY KEY,
            source_id     INTEGER NOT NULL REFERENCES sources(id),
            url           TEXT UNIQUE NOT NULL,
            title         TEXT,
            body          TEXT,
            published_at  TEXT,
            fetched_at    TEXT DEFAULT (datetime('now')),
            content_hash  TEXT NOT NULL,
            embedding     BLOB,           -- filled by embedder.py (Layer 2)
            cluster_id    INTEGER,        -- filled by embedder.py (Layer 2)
            relevance     REAL            -- filled by ranker.py   (Layer 3)
        );

        CREATE INDEX IF NOT EXISTS idx_articles_source
            ON articles(source_id);
        CREATE INDEX IF NOT EXISTS idx_articles_published
            ON articles(published_at DESC);
        CREATE INDEX IF NOT EXISTS idx_articles_cluster
            ON articles(cluster_id);
    """)
    conn.commit()


# ---------------------------------------------------------------------------
# Data classes
# ---------------------------------------------------------------------------

@dataclass
class RawArticle:
    source_id: int
    url: str
    title: str
    body: str
    published_at: str | None
    content_hash: str


# ---------------------------------------------------------------------------
# Core logic
# ---------------------------------------------------------------------------

def content_hash(title: str, body: str) -> str:
    """SHA-256 of normalised title+body — catches exact duplicates cheaply."""
    payload = f"{title.strip().lower()}|{body.strip().lower()}"
    return hashlib.sha256(payload.encode()).hexdigest()


def parse_published(entry: feed_adapter.FeedEntry) -> str | None:
    """Return ISO-8601 UTC string or None from a feed entry."""
    if entry.published_parsed:
        dt = datetime(*entry.published_parsed[:6], tzinfo=timezone.utc)
        return dt.isoformat()
    if entry.updated_parsed:
        dt = datetime(*entry.updated_parsed[:6], tzinfo=timezone.utc)
        return dt.isoformat()
    return None


def fetch_feed(source: sqlite3.Row) -> list[RawArticle]:
    """Fetch one feed and return its parsed articles.

    Passes cached ETag and Last-Modified headers so the server can respond
    with 304 Not Modified and skip retransmitting unchanged content.
    """
    url = source["url"]
    source_id = source["id"]

    log.info("Fetching %s (%s)", source["name"] or url, url)

    feed = feed_adapter.parse(
        url,
        agent=USER_AGENT,
        etag=source["etag"],
        modified=source["last_modified"],
    )

    if feed.status == 304:
        log.info("  → Not modified (304), skipping")
        return []

    if feed.bozo and not feed.entries:
        log.warning("  → Parse error for %s: %s", url, feed.bozo_exception)
        return []

    log.info("  → %d entries", len(feed.entries))

    articles = []
    for entry in feed.entries:
        if not entry.link:
            continue

        title = entry.title.strip()
        hash_ = content_hash(title, entry.body)

        articles.append(RawArticle(
            source_id=source_id,
            url=entry.link,
            title=title,
            body=entry.body,
            published_at=parse_published(entry),
            content_hash=hash_,
        ))

    return articles


def upsert_source_meta(conn: sqlite3.Connection, source_id: int, feed: feed_adapter.FeedResult) -> None:
    """Update ETag/Last-Modified so we can do conditional fetches next time."""
    conn.execute(
        """
        UPDATE sources SET
            etag            = ?,
            last_modified   = ?,
            last_fetched_at = datetime('now')
        WHERE id = ?
        """,
        (feed.etag, feed.modified, source_id),
    )


def insert_articles(conn: sqlite3.Connection, articles: list[RawArticle]) -> int:
    """Insert new articles, skipping duplicates. Returns the number of rows inserted.

    Deduplication is two-pass:
      1. URL match — the same article from the same source.
      2. Content-hash match — identical text published under a different URL
         (e.g. syndicated content or cross-posted stories).
    """
    inserted = 0
    for a in articles:
        exists = conn.execute(
            "SELECT 1 FROM articles WHERE url = ?", (a.url,)
        ).fetchone()
        if exists:
            continue

        hash_exists = conn.execute(
            "SELECT 1 FROM articles WHERE content_hash = ?", (a.content_hash,)
        ).fetchone()
        if hash_exists:
            log.debug("  Duplicate content, skipping %s", a.url)
            continue

        conn.execute(
            """
            INSERT INTO articles (source_id, url, title, body, published_at, content_hash)
            VALUES (?, ?, ?, ?, ?, ?)
            """,
            (a.source_id, a.url, a.title, a.body, a.published_at, a.content_hash),
        )
        inserted += 1

    conn.commit()
    return inserted


def run_ingestion(conn: sqlite3.Connection) -> None:
    """Fetch all active sources and store new articles.

    Iterates over every active source, fetching and inserting articles,
    then updates per-source ETag/Last-Modified metadata. A polite delay
    of REQUEST_DELAY_SEC is inserted between requests.
    """
    sources = conn.execute(
        "SELECT * FROM sources WHERE active = 1"
    ).fetchall()

    if not sources:
        log.warning("No active sources. Add one with --add-source.")
        return

    total_new = 0
    for i, source in enumerate(sources):
        articles = fetch_feed(source)
        if articles:
            n = insert_articles(conn, articles)
            total_new += n
            log.info("  → Stored %d new articles from %s", n, source["name"] or source["url"])

        feed = feed_adapter.parse(source["url"], agent=USER_AGENT)
        upsert_source_meta(conn, source["id"], feed)

        if i < len(sources) - 1:
            time.sleep(REQUEST_DELAY_SEC)

    log.info("Ingestion complete. %d new articles total.", total_new)


def add_source(conn: sqlite3.Connection, url: str, name: str | None = None) -> None:
    """Add a new feed source to the database.

    Fetches the feed once to infer the source name and home URL from its
    metadata. Silently skips if the URL is already registered.
    """
    feed = feed_adapter.parse(url, agent=USER_AGENT)
    home_url = feed.feed_link
    inferred_name = name or feed.feed_title or url

    try:
        conn.execute(
            "INSERT INTO sources (url, name, home_url) VALUES (?, ?, ?)",
            (url, inferred_name, home_url),
        )
        conn.commit()
        log.info("Added source: %s (%s)", inferred_name, url)
    except sqlite3.IntegrityError:
        log.warning("Source already exists: %s", url)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="RSS Digest — ingestion layer")
    parser.add_argument(
        "--add-source",
        metavar="URL",
        help="Add a new feed source and exit",
    )
    parser.add_argument(
        "--name",
        help="Human-readable name for --add-source",
    )
    parser.add_argument(
        "--db",
        default=str(DB_PATH),
        help=f"Path to SQLite database (default: {DB_PATH})",
    )
    args = parser.parse_args()

    conn = get_db(Path(args.db))
    init_db(conn)

    if args.add_source:
        add_source(conn, args.add_source, args.name)
    else:
        run_ingestion(conn)


if __name__ == "__main__":
    main()
