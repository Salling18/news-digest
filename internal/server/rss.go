package server

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/gorilla/feeds"
)

// generateRSS queries unpublished feed items, builds an RSS document, marks
// them as published, and returns the XML bytes.
func generateRSS(ctx context.Context, db *sql.DB) ([]byte, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, title, description, link, pub_date, relevance
		 FROM feed_items
		 WHERE published = 0
		 ORDER BY relevance DESC`)
	if err != nil {
		return nil, fmt.Errorf("generateRSS: query feed_items: %w", err)
	}
	defer rows.Close()

	feed := &feeds.Feed{
		Title: "News Digest",
		Link:  &feeds.Link{Href: "http://localhost:8000/feed.xml"},
	}

	for rows.Next() {
		var (
			id          int64
			title       sql.NullString
			description sql.NullString
			link        sql.NullString
			pubDate     sql.NullString
			relevance   sql.NullFloat64
		)
		if err := rows.Scan(&id, &title, &description, &link, &pubDate, &relevance); err != nil {
			return nil, fmt.Errorf("generateRSS: scan: %w", err)
		}

		item := &feeds.Item{
			Title:       title.String,
			Description: description.String,
			Link:        &feeds.Link{Href: link.String},
		}

		if pubDate.Valid {
			t, err := time.Parse(time.RFC3339, pubDate.String)
			if err == nil {
				item.Created = t
			}
		}

		feed.Items = append(feed.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("generateRSS: rows: %w", err)
	}

	rss, err := feed.ToRss()
	if err != nil {
		return nil, fmt.Errorf("generateRSS: ToRss: %w", err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE feed_items SET published = 1 WHERE published = 0`); err != nil {
		return nil, fmt.Errorf("generateRSS: mark published: %w", err)
	}

	return []byte(rss), nil
}
