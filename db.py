"""
db.py — Shared database setup and schema management.

Single source of truth for the SQLite connection, PRAGMA configuration,
and all CREATE TABLE / CREATE INDEX statements. Every layer imports from
here instead of defining its own get_db / init_db.
"""

import logging
import sqlite3
from pathlib import Path

DB_PATH = Path("digest.db")

log = logging.getLogger(__name__)


def get_db(path: Path = DB_PATH) -> sqlite3.Connection:
    """Open the SQLite database, enabling WAL mode for safe concurrent reads and writes."""
    conn = sqlite3.connect(path)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def init_db(conn: sqlite3.Connection) -> None:
    """Create all tables and indexes if they don't already exist."""
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
        CREATE INDEX IF NOT EXISTS idx_articles_content_hash
            ON articles(content_hash);

        CREATE TABLE IF NOT EXISTS clusters (
            id              INTEGER PRIMARY KEY,
            canonical_id    INTEGER REFERENCES articles(id),
            first_seen_at   TEXT DEFAULT (datetime('now')),
            last_updated_at TEXT DEFAULT (datetime('now')),
            article_count   INTEGER DEFAULT 0,
            summary         TEXT        -- reserved for LLM-generated summaries (Layer 4+)
        );
    """)
    conn.commit()


def setup_logging() -> None:
    """Configure logging once for the whole process."""
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(message)s",
    )
