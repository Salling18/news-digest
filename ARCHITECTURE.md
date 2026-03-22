# RSS Digest — Architecture Briefing

A self-hosted Python pipeline that ingests RSS/Atom feeds, deduplicates and
clusters articles by semantic similarity, ranks clusters by personal relevance,
and serves the result as an RSS feed you subscribe to in any RSS reader.

## Goal

Aggregate many news sources into a single, deduplicated, relevance-ranked RSS
feed. Articles covering the same story get merged into one item. Stories outside
your interest profile get filtered out.

## Stack

| Concern | Library | Version |
|---|---|---|
| Feed parsing | `feedparser` | 6.0.12 |
| Embeddings | `sentence-transformers` | 5.3.0 |
| Clustering | `scikit-learn` (HDBSCAN) | 1.8.0 |
| RSS generation | `feedgen` | 1.0.0 |
| HTTP server | `fastapi[standard]` | 0.135.1 |
| Scheduler | `apscheduler` | 3.11.2 (NOT 4.x — still alpha) |
| Storage | SQLite (via stdlib `sqlite3`) | — |

**Python minimum: 3.10** (required by scikit-learn, sentence-transformers, FastAPI).

Install everything:
```bash
pip install feedparser sentence-transformers scikit-learn feedgen "fastapi[standard]" "apscheduler<4"
```

## Data Model

```sql
sources         -- feed URLs you subscribe to
  id, url, name, home_url, credibility (0–1), active,
  etag, last_modified, last_fetched_at

articles        -- one row per feed entry, raw
  id, source_id → sources,
  url (UNIQUE), title, body, published_at, fetched_at,
  content_hash,           -- SHA-256(title+body), cheap exact dedup
  embedding BLOB,         -- float32 vector, filled by Layer 2
  cluster_id → clusters,  -- filled by Layer 2
  relevance REAL          -- cosine score vs profile, filled by Layer 3

clusters        -- one row per story/topic group
  id, canonical_id → articles,   -- best article in the group
  first_seen_at, last_updated_at, article_count,
  summary TEXT                    -- LLM-generated later, nullable

interest_profile  -- your relevance signal
  id, label, description, weight (default 1.0),
  embedding BLOB, updated_at

feed_items      -- materialised RSS output
  id, cluster_id → clusters,
  title, description, link, pub_date, relevance,
  published BOOL          -- whether it's been emitted in the feed yet
```

Key relationships:
```
sources ──< articles >── clusters
                │              │
           embedding      canonical_id
                │
       interest_profile (compared at ranking time)
                │
           feed_items (output layer, read by server)
```

## Pipeline Layers

```
[Layer 1] ingester.py   →  fetches feeds, writes articles (raw)
[Layer 2] embedder.py   →  embeds articles, clusters them, writes cluster_id
[Layer 3] ranker.py     →  scores clusters vs interest_profile, writes feed_items
[Layer 4] server.py     →  serves /feed.xml (RSS) + triggers pipeline via scheduler
```

Each layer is a standalone script. They share only the SQLite database.

### Layer 1 — ingester.py ✅ DONE

- Reads `sources` table, fetches each active feed
- Uses ETag/Last-Modified headers for conditional fetches (polite, fast)
- Two-pass deduplication: URL uniqueness first, then `content_hash`
- Writes to `articles` (url, title, body, published_at, content_hash)
- CLI: `python ingester.py --add-source <url> [--name <name>]`

### Layer 2 — embedder.py ⬜ TODO

- Reads articles where `embedding IS NULL`
- Embeds `title + " " + body[:512]` with `all-MiniLM-L6-v2` (384-dim, fast)
  - Alternative: `all-mpnet-base-v2` (768-dim, better quality, slower)
- Stores serialised `numpy.float32` array in `articles.embedding`
- Runs HDBSCAN (`metric='cosine'`, `min_cluster_size=3`) over all embeddings
- Reassigns `cluster_id` for all articles (re-clusters from scratch each run)
- Picks canonical article per cluster: highest `sources.credibility`, then most recent
- Upserts `clusters` table

Key note: HDBSCAN is preferred over DBSCAN here because it doesn't require
tuning the `eps` parameter, which is brittle with high-dimensional embeddings.

### Layer 3 — ranker.py ⬜ TODO

- Reads `interest_profile` table; embeds any profiles missing an embedding
- Scores each cluster's canonical article against the profile: cosine similarity
- Writes `relevance` score to `articles.relevance` for canonical articles
- Filters out clusters below a threshold (configurable, default 0.3)
- Populates `feed_items` with surviving clusters, ranked by relevance DESC

### Layer 4 — server.py ⬜ TODO

- FastAPI app with lifespan-based APScheduler (runs ingestion hourly)
- `GET /feed.xml` — generates RSS 2.0 from `feed_items WHERE published = 0`,
  marks them published, returns XML
- `GET /health` — returns item counts and last-run timestamps
- Uses `feedgen` for RSS generation; `rss_str()` returns bytes, not str

## Design Decisions

**Why materialise `feed_items`?**
Decouples the pipeline from the server. The server just reads rows; the pipeline
writes them independently. Avoids re-clustering on every HTTP request.

**Why SQLite?**
Zero ops overhead for a personal tool. WAL mode gives safe concurrent
reads (server) + writes (pipeline). Swap to Postgres later if needed —
the schema is intentionally portable.

**Why store embeddings as BLOBs?**
Keeps the stack simple. If you later want fast ANN search across millions of
articles, drop in `sqlite-vec` or ChromaDB without changing the rest of the schema.

**Embeddings are re-clustered from scratch each run.** This avoids stale
cluster assignments when new articles arrive that should merge with old clusters.
It's fast enough at personal scale (hundreds to low thousands of articles).

## Project Structure

```
digest/
├── ingester.py       # Layer 1 ✅
├── embedder.py       # Layer 2 ⬜
├── ranker.py         # Layer 3 ⬜
├── server.py         # Layer 4 ⬜
├── digest.db         # SQLite database (git-ignored)
├── ARCHITECTURE.md   # this file
├── TODO.md           # task checklist
└── requirements.txt  # pip dependencies
```

## Future / Backlog

- Feedback loop: mark articles interesting/not → update interest profile weights
- LLM-generated cluster summaries (Anthropic API, claude-haiku-4-5 for cost)
- Source credibility scoring based on historical click-through
- Simple web UI for managing sources and editing interest profile
- Docker / systemd service file for always-on deployment
