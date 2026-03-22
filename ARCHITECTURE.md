# RSS Digest — Architecture

A self-hosted pipeline that ingests RSS/Atom feeds, deduplicates and clusters
articles by semantic similarity, ranks clusters by personal relevance, and
serves the result as an RSS feed you can subscribe to in any RSS reader.

## Goal

Aggregate many news sources into a single, deduplicated, relevance-ranked RSS
feed. Articles covering the same story get merged into one item. Stories outside
your interest profile get filtered out.

## Stack

| Concern | Technology |
|---|---|
| Main binary | Go (`cmd/digest`) |
| Feed parsing | `github.com/mmcdole/gofeed` |
| HTTP router | `github.com/go-chi/chi/v5` |
| RSS generation | `github.com/gorilla/feeds` |
| Scheduler | `github.com/robfig/cron/v3` |
| Storage | SQLite via `modernc.org/sqlite` (pure Go, no CGO) |
| Embeddings + clustering | Python microservice (`embed_service/`) |

The Python microservice is stateless — it only handles ML math (sentence
embeddings and HDBSCAN clustering).

## Architecture

```
┌─────────────────────────────────┐       ┌─────────────────────────────┐
│  Go binary (digest)             │       │  Python microservice        │
│                                 │  HTTP │  embed_service.py           │
│  ingest → embed → rank → serve  │◄─────►│  POST /embed  (texts→vecs)  │
│  cron scheduler (hourly)        │       │  POST /cluster (vecs→labels)│
│  SQLite (all persistence)       │       │  no database access         │
└─────────────────────────────────┘       └─────────────────────────────┘
```

## Project Structure

```
news-digest/
├── cmd/digest/main.go          # CLI: ingest | embed | rank | serve
├── internal/
│   ├── db/                     # SQLite open + schema DDL
│   ├── embedclient/            # HTTP client for the Python service
│   ├── pipeline/               # ingest, embed, rank logic
│   └── server/                 # chi router, RSS handler, cron scheduler
├── embed_service/
│   ├── embed_service.py        # Python microservice (~80 lines)
│   └── requirements.txt
├── go.mod
└── digest.db                   # git-ignored
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
  embedding BLOB,         -- float32 LE vector, filled by embed step
  cluster_id → clusters,  -- filled by embed step
  relevance REAL          -- cosine score vs profile, filled by rank step

clusters        -- one row per story/topic group
  id, canonical_id → articles,   -- best article in the group
  first_seen_at, last_updated_at, article_count

interest_profile  -- your relevance signal
  id, label, description, weight (default 1.0),
  embedding BLOB, updated_at

feed_items      -- materialised RSS output
  id, cluster_id → clusters,
  title, description, link, pub_date, relevance,
  published BOOL          -- whether it has been emitted in the feed yet
```

## Pipeline Steps

```
[ingest]  pipeline.Ingest  →  fetch feeds, deduplicate, write articles
[embed]   pipeline.Embed   →  embed articles, cluster them, write cluster_id
[rank]    pipeline.Rank    →  score clusters vs interest_profile, write feed_items
[serve]   server.go        →  serve /feed.xml + /health, run pipeline hourly
```

### ingest

- Reads `sources` table, fetches each active feed
- ETag/Last-Modified conditional fetching (polite, avoids re-downloading unchanged feeds)
- Two-pass deduplication: URL uniqueness, then `content_hash`
- CLI: `digest ingest [--add-source <url>] [--name <name>]`

### embed

- Embeds articles where `embedding IS NULL` via the Python `/embed` endpoint
- Sends `title + body[:1500]` for each article; stores 384-dim float32 BLOBs
- Runs HDBSCAN clustering over all embeddings via `/cluster`
- Re-clusters from scratch each run to avoid stale assignments
- Picks canonical article per cluster: highest `sources.credibility`, then most recent
- CLI: `digest embed [--min-cluster-size 3] [--max-age-days N]`

### rank

- Loads `interest_profile`; embeds any profiles missing an embedding
- Scores each cluster against profiles using cosine similarity (dot product on pre-normalised vectors)
- Writes `relevance` to `articles` for canonical articles
- Filters clusters below threshold (default 0.3), populates `feed_items`
- CLI: `digest rank [--threshold 0.3]`

### serve

- Starts the HTTP server and an hourly cron job (ingest → embed → rank)
- `GET /feed.xml` — RSS 2.0 from `feed_items WHERE published = 0`; marks items published
- `GET /health` — JSON with article/source/cluster counts and last fetch time
- CLI: `digest serve [--port 8000]`

## Python Microservice

`embed_service/embed_service.py` — stateless FastAPI app on `127.0.0.1:8001`.

- `POST /embed` — `{"texts": [...]}` → `{"vectors": [[...], ...]}`
- `POST /cluster` — `{"vectors": [...], "article_ids": [...], "min_cluster_size": N}` → `{"assignments": {"1": 0, ...}}`
- `GET /health` — `{"status": "ok"}`

Start it with:
```bash
cd embed_service
pip install -r requirements.txt
uvicorn embed_service:app --host 127.0.0.1 --port 8001
```


## Future / Backlog

- Feedback loop: mark articles interesting/not → update interest profile weights
- LLM-generated cluster summaries (Anthropic API, claude-haiku-4-5 for cost)
- Source credibility scoring based on historical click-through
- Simple web UI for managing sources and editing interest profile
- Docker / systemd service file for always-on deployment
