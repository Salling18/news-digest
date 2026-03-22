package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

const (
	requestDelay = 1 * time.Second
	userAgent    = "RSSDigest/0.1 (self-hosted; +https://github.com/you/rss-digest)"
)

type source struct {
	id           int64
	url          string
	name         sql.NullString
	etag         sql.NullString
	lastModified sql.NullString
}

// ContentHash returns the SHA-256 hex digest of the normalised title and body,
// byte-identical to the Python version.
func ContentHash(title, body string) string {
	payload := strings.ToLower(strings.TrimSpace(title)) + "|" + strings.ToLower(strings.TrimSpace(body))
	h := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", h)
}

// Ingest fetches all active sources and stores new articles.
func Ingest(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		"SELECT id, url, name, etag, last_modified FROM sources WHERE active = 1")
	if err != nil {
		return fmt.Errorf("querying sources: %w", err)
	}
	defer rows.Close()

	var sources []source
	for rows.Next() {
		var s source
		if err := rows.Scan(&s.id, &s.url, &s.name, &s.etag, &s.lastModified); err != nil {
			return fmt.Errorf("scanning source row: %w", err)
		}
		sources = append(sources, s)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating sources: %w", err)
	}

	if len(sources) == 0 {
		slog.Warn("no active sources; add one with --add-source")
		return nil
	}

	totalNew := 0
	for i, src := range sources {
		n, err := fetchFeed(ctx, db, src)
		if err != nil {
			slog.Error("failed to fetch source, skipping",
				"name", src.name.String, "url", src.url, "err", err)
		} else {
			totalNew += n
		}

		if i < len(sources)-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(requestDelay):
			}
		}
	}

	slog.Info("ingestion complete", "new_articles", totalNew)
	return nil
}

func fetchFeed(ctx context.Context, db *sql.DB, src source) (int, error) {
	displayName := src.url
	if src.name.Valid {
		displayName = src.name.String
	}
	slog.Info("fetching", "name", displayName, "url", src.url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.url, nil)
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	if src.etag.Valid {
		req.Header.Set("If-None-Match", src.etag.String)
	}
	if src.lastModified.Valid {
		req.Header.Set("If-Modified-Since", src.lastModified.String)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	newETag := resp.Header.Get("ETag")
	newLastModified := resp.Header.Get("Last-Modified")

	if resp.StatusCode == http.StatusNotModified {
		slog.Info("not modified (304), skipping", "name", displayName)
		return 0, nil
	}

	feed, err := gofeed.NewParser().Parse(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("parsing feed: %w", err)
	}

	slog.Info("parsed entries", "name", displayName, "count", len(feed.Items))

	inserted := 0
	for _, item := range feed.Items {
		if item.Link == "" {
			continue
		}

		title := strings.TrimSpace(item.Title)
		body := item.Content
		if body == "" {
			body = item.Description
		}

		var publishedAt *string
		if item.PublishedParsed != nil {
			s := item.PublishedParsed.UTC().Format(time.RFC3339)
			publishedAt = &s
		} else if item.UpdatedParsed != nil {
			s := item.UpdatedParsed.UTC().Format(time.RFC3339)
			publishedAt = &s
		}

		hash := ContentHash(title, body)

		var exists int
		if err := db.QueryRowContext(ctx, "SELECT 1 FROM articles WHERE url = ?", item.Link).Scan(&exists); !errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err := db.QueryRowContext(ctx, "SELECT 1 FROM articles WHERE content_hash = ?", hash).Scan(&exists); !errors.Is(err, sql.ErrNoRows) {
			slog.Debug("duplicate content, skipping", "url", item.Link)
			continue
		}

		if _, err = db.ExecContext(ctx,
			"INSERT INTO articles (source_id, url, title, body, published_at, content_hash) VALUES (?, ?, ?, ?, ?, ?)",
			src.id, item.Link, title, body, publishedAt, hash); err != nil {
			return inserted, fmt.Errorf("inserting article %s: %w", item.Link, err)
		}
		inserted++
	}

	if _, err = db.ExecContext(ctx,
		"UPDATE sources SET etag = ?, last_modified = ?, last_fetched_at = datetime('now') WHERE id = ?",
		toNullString(newETag), toNullString(newLastModified), src.id); err != nil {
		return inserted, fmt.Errorf("updating source meta: %w", err)
	}

	if inserted > 0 {
		slog.Info("stored new articles", "name", displayName, "count", inserted)
	}
	return inserted, nil
}

// AddSource registers a new feed URL in the sources table.
func AddSource(ctx context.Context, db *sql.DB, url string, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching feed: %w", err)
	}
	defer resp.Body.Close()

	feed, err := gofeed.NewParser().Parse(resp.Body)
	if err != nil {
		return fmt.Errorf("parsing feed: %w", err)
	}

	if name == "" {
		name = feed.Title
	}
	if name == "" {
		name = url
	}

	if _, err = db.ExecContext(ctx,
		"INSERT INTO sources (url, name, home_url) VALUES (?, ?, ?)",
		url, name, feed.Link); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			slog.Warn("source already exists", "url", url)
			return nil
		}
		return fmt.Errorf("inserting source: %w", err)
	}

	slog.Info("added source", "name", name, "url", url)
	return nil
}

func toNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
