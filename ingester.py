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

import db
import feed_adapter

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

REQUEST_DELAY_SEC = 1.0
USER_AGENT = "RSSDigest/0.1 (self-hosted; +https://github.com/you/rss-digest)"

log = logging.getLogger(__name__)


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


def fetch_feed(source: sqlite3.Row) -> tuple[list[RawArticle], feed_adapter.FeedResult]:
    """Fetch one feed and return its parsed articles alongside the raw FeedResult.

    Passes cached ETag and Last-Modified headers so the server can respond
    with 304 Not Modified and skip retransmitting unchanged content.

    Returns the FeedResult so callers can update source metadata (etag,
    last_modified) without re-fetching.
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
        return [], feed

    if feed.bozo and not feed.entries:
        log.warning("  → Parse error for %s: %s", url, feed.bozo_exception)
        return [], feed

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

    return articles, feed


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
        try:
            articles, feed = fetch_feed(source)
            if articles:
                n = insert_articles(conn, articles)
                total_new += n
                log.info("  → Stored %d new articles from %s", n, source["name"] or source["url"])

            upsert_source_meta(conn, source["id"], feed)
        except Exception:
            log.exception("  → Failed to fetch %s, skipping", source["name"] or source["url"])

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
        default=str(db.DB_PATH),
        help=f"Path to SQLite database (default: {db.DB_PATH})",
    )
    args = parser.parse_args()

    db.setup_logging()
    conn = db.get_db(Path(args.db))
    db.init_db(conn)

    if args.add_source:
        add_source(conn, args.add_source, args.name)
    else:
        run_ingestion(conn)


if __name__ == "__main__":
    main()
