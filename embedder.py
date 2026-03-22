"""
embedder.py — Layer 2: Article embedding and clustering

Responsibilities:
  - Embed unprocessed articles (title + body[:512]) using all-MiniLM-L6-v2
  - Store float32 embedding vectors in articles.embedding
  - Re-cluster all articles from scratch each run using HDBSCAN
  - Pick a canonical article per cluster (highest credibility, then most recent)
  - Upsert the clusters table with fresh metadata

Usage:
  python embedder.py
  python embedder.py --db path/to/digest.db
  python embedder.py --model all-mpnet-base-v2 --min-cluster-size 5
"""

import argparse
import logging
import sqlite3
from datetime import datetime, timezone
from pathlib import Path

import numpy as np
from sentence_transformers import SentenceTransformer
from sklearn.cluster import HDBSCAN

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

DB_PATH = Path("digest.db")
MODEL_NAME = "all-MiniLM-L6-v2"
EMBED_BATCH_SIZE = 64
MIN_CLUSTER_SIZE = 3

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
)
log = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Database setup
# ---------------------------------------------------------------------------

def get_db(path: Path = DB_PATH) -> sqlite3.Connection:
    """Open (or create) the SQLite database with WAL mode and FK enforcement."""
    conn = sqlite3.connect(path)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA foreign_keys=ON")
    return conn


def init_db(conn: sqlite3.Connection) -> None:
    """Create the clusters table if it doesn't exist yet."""
    conn.executescript("""
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


# ---------------------------------------------------------------------------
# Model loading
# ---------------------------------------------------------------------------

def load_model(model_name: str = MODEL_NAME) -> SentenceTransformer:
    """
    Load the sentence-transformers model from cache (downloads on first run).

    all-MiniLM-L6-v2  — 384 dimensions, fast, good quality (default)
    all-mpnet-base-v2 — 768 dimensions, slower, higher quality
    """
    log.info("Loading embedding model '%s' ...", model_name)
    return SentenceTransformer(model_name)


# ---------------------------------------------------------------------------
# Embedding
# ---------------------------------------------------------------------------

def embed_pending(conn: sqlite3.Connection, model: SentenceTransformer) -> int:
    """
    Compute and store embeddings for all articles that don't have one yet.

    Text fed to the model: title + first 512 characters of body.
    Vectors are stored as raw float32 bytes (BLOB) in articles.embedding.

    Returns the number of articles newly embedded.
    """
    rows = conn.execute(
        "SELECT id, title, body FROM articles WHERE embedding IS NULL"
    ).fetchall()

    if not rows:
        log.info("No new articles to embed.")
        return 0

    log.info("Embedding %d new articles ...", len(rows))

    ids = [r["id"] for r in rows]
    texts = [
        f"{r['title']} {(r['body'] or '')[:512]}"
        for r in rows
    ]

    vectors: np.ndarray = model.encode(
        texts,
        batch_size=EMBED_BATCH_SIZE,
        show_progress_bar=len(texts) > 20,
        convert_to_numpy=True,
    ).astype(np.float32)

    for article_id, vec in zip(ids, vectors):
        conn.execute(
            "UPDATE articles SET embedding = ? WHERE id = ?",
            (vec.tobytes(), article_id),
        )

    conn.commit()
    log.info("Stored %d embeddings.", len(ids))
    return len(ids)


# ---------------------------------------------------------------------------
# Clustering
# ---------------------------------------------------------------------------

def load_all_embeddings(conn: sqlite3.Connection) -> tuple[list[int], np.ndarray]:
    """
    Load every embedded article from the database.

    Returns a tuple of:
      - article_ids: list of article IDs in row order
      - matrix: float32 ndarray of shape (N, embedding_dim)
    """
    rows = conn.execute(
        "SELECT id, embedding FROM articles WHERE embedding IS NOT NULL"
    ).fetchall()

    if not rows:
        return [], np.empty((0, 0), dtype=np.float32)

    ids = [r["id"] for r in rows]
    first = np.frombuffer(rows[0]["embedding"], dtype=np.float32)
    dim = len(first)

    matrix = np.zeros((len(rows), dim), dtype=np.float32)
    matrix[0] = first
    for i, row in enumerate(rows[1:], start=1):
        matrix[i] = np.frombuffer(row["embedding"], dtype=np.float32)

    return ids, matrix


def run_clustering(
    article_ids: list[int],
    matrix: np.ndarray,
    min_cluster_size: int,
) -> dict[int, int | None]:
    """
    Run HDBSCAN over the full embedding matrix.

    HDBSCAN is preferred over DBSCAN because it doesn't require tuning the
    eps parameter, which is brittle in high-dimensional cosine space.

    Returns a dict mapping article_id → cluster label (or None for noise).
    Noise articles (label == -1) are not assigned to any cluster.
    """
    log.info(
        "Clustering %d articles (min_cluster_size=%d) ...",
        len(article_ids),
        min_cluster_size,
    )

    clusterer = HDBSCAN(metric="cosine", min_cluster_size=min_cluster_size)
    labels: np.ndarray = clusterer.fit_predict(matrix)

    assignments = {
        article_id: (None if label == -1 else int(label))
        for article_id, label in zip(article_ids, labels)
    }

    n_clusters = len({label for label in labels if label != -1})
    n_noise = int((labels == -1).sum())
    log.info("  → %d clusters, %d noise articles", n_clusters, n_noise)

    return assignments


# ---------------------------------------------------------------------------
# Canonical article selection
# ---------------------------------------------------------------------------

def pick_canonical(conn: sqlite3.Connection, article_ids: list[int]) -> int:
    """
    Select the best representative article from a cluster.

    Selection criteria (in order):
      1. Highest source credibility score
      2. Most recent published_at (NULLS sorted last)

    Returns the winning article_id.
    """
    if len(article_ids) == 1:
        return article_ids[0]

    placeholders = ",".join("?" * len(article_ids))
    row = conn.execute(
        f"""
        SELECT a.id
        FROM articles a
        JOIN sources s ON a.source_id = s.id
        WHERE a.id IN ({placeholders})
        ORDER BY s.credibility DESC, a.published_at DESC NULLS LAST
        LIMIT 1
        """,
        article_ids,
    ).fetchone()

    return int(row["id"]) if row else article_ids[0]


# ---------------------------------------------------------------------------
# Cluster persistence
# ---------------------------------------------------------------------------

def upsert_clusters(
    conn: sqlite3.Connection,
    assignments: dict[int, int | None],
) -> None:
    """
    Rebuild the clusters table from the current HDBSCAN label assignments.

    Because clustering is done from scratch each run, all existing cluster rows
    and article cluster_id values are cleared first, then rewritten.

    For each cluster:
      - Picks the canonical article (see pick_canonical)
      - Sets first_seen_at to the earliest fetched_at among cluster members
      - Writes article_count
    """
    now = datetime.now(timezone.utc).isoformat()

    # Group article IDs by HDBSCAN label, ignoring noise
    cluster_map: dict[int, list[int]] = {}
    for article_id, label in assignments.items():
        if label is not None:
            cluster_map.setdefault(label, []).append(article_id)

    # Wipe existing data — re-clustering always produces a fresh assignment
    conn.execute("UPDATE articles SET cluster_id = NULL")
    conn.execute("DELETE FROM clusters")

    for ids in cluster_map.values():
        canonical_id = pick_canonical(conn, ids)

        placeholders = ",".join("?" * len(ids))
        first_seen = conn.execute(
            f"SELECT MIN(fetched_at) FROM articles WHERE id IN ({placeholders})",
            ids,
        ).fetchone()[0] or now

        cursor = conn.execute(
            """
            INSERT INTO clusters (canonical_id, first_seen_at, last_updated_at, article_count)
            VALUES (?, ?, ?, ?)
            """,
            (canonical_id, first_seen, now, len(ids)),
        )
        cluster_id = cursor.lastrowid

        conn.execute(
            f"UPDATE articles SET cluster_id = ? WHERE id IN ({placeholders})",
            [cluster_id] + ids,
        )

    conn.commit()

    assigned = sum(len(ids) for ids in cluster_map.values())
    noise = sum(1 for label in assignments.values() if label is None)
    log.info(
        "Upserted %d clusters covering %d articles (%d noise / unclustered).",
        len(cluster_map),
        assigned,
        noise,
    )


# ---------------------------------------------------------------------------
# Main pipeline entry point
# ---------------------------------------------------------------------------

def run_embedding(
    conn: sqlite3.Connection,
    model: SentenceTransformer,
    min_cluster_size: int,
) -> None:
    """
    Full Layer 2 pipeline: embed new articles, then re-cluster everything.

    Steps:
      1. Embed any articles missing an embedding vector
      2. Load all embeddings into memory
      3. Run HDBSCAN clustering
      4. Persist cluster assignments and update the clusters table
    """
    embed_pending(conn, model)

    article_ids, matrix = load_all_embeddings(conn)
    if len(article_ids) < min_cluster_size:
        log.warning(
            "Only %d embedded articles — need at least %d for clustering. Skipping.",
            len(article_ids),
            min_cluster_size,
        )
        return

    assignments = run_clustering(article_ids, matrix, min_cluster_size)
    upsert_clusters(conn, assignments)


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def main() -> None:
    parser = argparse.ArgumentParser(description="RSS Digest — embedding and clustering layer")
    parser.add_argument(
        "--db",
        default=str(DB_PATH),
        help=f"Path to SQLite database (default: {DB_PATH})",
    )
    parser.add_argument(
        "--model",
        default=MODEL_NAME,
        help=f"Sentence-transformers model name (default: {MODEL_NAME})",
    )
    parser.add_argument(
        "--min-cluster-size",
        type=int,
        default=MIN_CLUSTER_SIZE,
        help=f"HDBSCAN min_cluster_size (default: {MIN_CLUSTER_SIZE})",
    )
    args = parser.parse_args()

    conn = get_db(Path(args.db))
    init_db(conn)
    model = load_model(args.model)
    run_embedding(conn, model, args.min_cluster_size)


if __name__ == "__main__":
    main()
