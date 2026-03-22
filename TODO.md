# RSS Digest — TODO

## ✅ Go rewrite

- [x] `internal/db` — SQLite open + schema DDL
- [x] `internal/embedclient` — HTTP client for Python embed service
- [x] `internal/pipeline` — ingest, embed, rank
- [x] `internal/server` — chi router, RSS handler, cron scheduler
- [x] `cmd/digest` — CLI subcommands: ingest, embed, rank, serve
- [x] `embed_service/embed_service.py` — stateless FastAPI microservice

## ✅ Pipeline features

- [x] ETag/Last-Modified conditional fetching
- [x] Two-pass deduplication (URL + content hash)
- [x] Sentence embeddings via Python service (all-MiniLM-L6-v2, 384-dim)
- [x] HDBSCAN clustering with canonical article selection
- [x] Cosine relevance scoring against interest profiles
- [x] feed_items materialisation + RSS output
- [x] Hourly cron scheduler in `serve` subcommand
- [x] `/health` endpoint with DB stats

## ⬜ Testing

- [ ] Add a source and run `digest ingest`
- [ ] Start embed service and run `digest embed`
- [ ] Add an interest profile and run `digest rank`
- [ ] Start `digest serve` and verify `/feed.xml`

## Backlog

- [ ] Feedback loop: mark articles interesting/not, update interest profile weights
- [ ] LLM-generated cluster summaries in RSS description field
- [ ] Source credibility scoring based on click-through history
- [ ] Web UI for managing sources and interest profile
- [ ] Docker / systemd service file for always-on deployment
