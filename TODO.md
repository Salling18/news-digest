# RSS Digest — Project TODO

## ✅ Layer 1: Ingestion (`ingester.py`)
- [x] Fetch RSS/Atom feeds via feedparser
- [x] SQLite schema (sources, articles)
- [x] Content-hash deduplication (exact duplicates)
- [x] ETag / Last-Modified conditional fetching (polite to servers)
- [x] CLI: `--add-source <url>`, `--name`, `--db`

## ⬜ Layer 2: Embedding + Clustering (`embedder.py`)
- [ ] Load unembedded articles from DB
- [ ] Generate embeddings with sentence-transformers (`all-MiniLM-L6-v2`)
- [ ] Serialize + store embeddings in `articles.embedding` (BLOB)
- [ ] Run HDBSCAN clustering over all embeddings (cosine distance)
- [ ] Assign `cluster_id` to each article
- [ ] Pick canonical article per cluster (highest source credibility)
- [ ] Insert/update rows in `clusters` table

## ⬜ Layer 3: Relevance Ranking (`ranker.py`)
- [ ] Load/embed `interest_profile` entries
- [ ] Score each cluster against profile (cosine similarity)
- [ ] Write `relevance` score back to `articles`
- [ ] Apply threshold filter (discard low-relevance clusters)
- [ ] Populate `feed_items` table with ranked output

## ⬜ Layer 4: RSS Output (`server.py`)
- [ ] FastAPI app with lifespan + APScheduler (hourly ingestion trigger)
- [ ] `GET /feed.xml` — generate RSS 2.0 via feedgen from `feed_items`
- [ ] `GET /health` — status + item count
- [ ] Serve on configurable port (default 8000)
- [ ] Optional: `POST /sources` to add feeds via HTTP

## Backlog (future layers)
- [ ] Feedback loop: mark articles interesting/not, update interest profile weights
- [ ] LLM-generated cluster summaries in RSS description field
- [ ] Source credibility scoring based on click-through history
- [ ] Web UI for managing sources and interest profile
- [ ] Docker / systemd service file for always-on deployment
