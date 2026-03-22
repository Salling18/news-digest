package db

const schemaDDL = `
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
	id           INTEGER PRIMARY KEY,
	source_id    INTEGER NOT NULL REFERENCES sources(id),
	url          TEXT UNIQUE NOT NULL,
	title        TEXT,
	body         TEXT,
	published_at TEXT,
	fetched_at   TEXT DEFAULT (datetime('now')),
	content_hash TEXT NOT NULL,
	embedding    BLOB,
	cluster_id   INTEGER,
	relevance    REAL
);

CREATE INDEX IF NOT EXISTS idx_articles_source       ON articles(source_id);
CREATE INDEX IF NOT EXISTS idx_articles_published     ON articles(published_at DESC);
CREATE INDEX IF NOT EXISTS idx_articles_cluster       ON articles(cluster_id);
CREATE INDEX IF NOT EXISTS idx_articles_content_hash  ON articles(content_hash);

CREATE TABLE IF NOT EXISTS clusters (
	id              INTEGER PRIMARY KEY,
	canonical_id    INTEGER REFERENCES articles(id),
	first_seen_at   TEXT DEFAULT (datetime('now')),
	last_updated_at TEXT DEFAULT (datetime('now')),
	article_count   INTEGER DEFAULT 0,
	summary         TEXT
);

CREATE TABLE IF NOT EXISTS interest_profile (
	id          INTEGER PRIMARY KEY,
	label       TEXT NOT NULL,
	description TEXT,
	weight      REAL DEFAULT 1.0,
	embedding   BLOB,
	updated_at  TEXT DEFAULT (datetime('now'))
);

CREATE TABLE IF NOT EXISTS feed_items (
	id          INTEGER PRIMARY KEY,
	cluster_id  INTEGER REFERENCES clusters(id),
	title       TEXT,
	description TEXT,
	link        TEXT,
	pub_date    TEXT,
	relevance   REAL,
	published   INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_feed_items_published ON feed_items(published);
`
