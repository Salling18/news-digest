package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"news-digest/internal/embedclient"
)

// Embed embeds pending articles and re-clusters all embeddings.
func Embed(ctx context.Context, db *sql.DB, client *embedclient.Client, minClusterSize int, maxAgeDays int) error {
	n, err := embedPending(ctx, db, client)
	if err != nil {
		return fmt.Errorf("embedPending: %w", err)
	}
	slog.Info("embedded pending articles", "count", n)

	ids, vectors, err := loadAllEmbeddings(ctx, db, maxAgeDays)
	if err != nil {
		return fmt.Errorf("loadAllEmbeddings: %w", err)
	}

	if len(ids) < minClusterSize {
		slog.Warn("not enough articles to cluster", "have", len(ids), "need", minClusterSize)
		return nil
	}

	assignments, err := client.Cluster(ctx, vectors, ids, minClusterSize)
	if err != nil {
		return fmt.Errorf("cluster: %w", err)
	}
	slog.Info("clustering complete", "assignments", len(assignments))

	return upsertClusters(ctx, db, assignments)
}

func embedPending(ctx context.Context, db *sql.DB, client *embedclient.Client) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, title, body FROM articles WHERE embedding IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("query pending: %w", err)
	}
	defer rows.Close()

	type pending struct {
		id   int64
		text string
	}

	var items []pending
	for rows.Next() {
		var id int64
		var title, body sql.NullString
		if err := rows.Scan(&id, &title, &body); err != nil {
			return 0, fmt.Errorf("scan: %w", err)
		}
		b := body.String
		if len(b) > 1500 {
			b = b[:1500]
		}
		items = append(items, pending{id: id, text: strings.TrimSpace(title.String + " " + b)})
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("rows iteration: %w", err)
	}
	if len(items) == 0 {
		return 0, nil
	}

	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = it.text
	}

	vecs, err := client.Embed(ctx, texts)
	if err != nil {
		return 0, fmt.Errorf("embed call: %w", err)
	}

	for i, it := range items {
		blob, err := encodeVector(vecs[i])
		if err != nil {
			return 0, fmt.Errorf("encode vector for article %d: %w", it.id, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE articles SET embedding = ? WHERE id = ?`, blob, it.id); err != nil {
			return 0, fmt.Errorf("store embedding for article %d: %w", it.id, err)
		}
	}

	return len(items), nil
}

func loadAllEmbeddings(ctx context.Context, db *sql.DB, maxAgeDays int) ([]int64, [][]float32, error) {
	var query string
	var args []any

	if maxAgeDays > 0 {
		query = `SELECT id, embedding FROM articles WHERE embedding IS NOT NULL AND fetched_at >= datetime('now', ?)`
		args = append(args, fmt.Sprintf("-%d days", maxAgeDays))
	} else {
		query = `SELECT id, embedding FROM articles WHERE embedding IS NOT NULL`
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("query embeddings: %w", err)
	}
	defer rows.Close()

	var ids []int64
	var vectors [][]float32
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, nil, fmt.Errorf("scan: %w", err)
		}
		vec, err := decodeVector(blob)
		if err != nil {
			return nil, nil, fmt.Errorf("decode vector for article %d: %w", id, err)
		}
		ids = append(ids, id)
		vectors = append(vectors, vec)
	}
	return ids, vectors, rows.Err()
}

func upsertClusters(ctx context.Context, db *sql.DB, assignments map[int64]int) error {
	groups := make(map[int][]int64)
	for articleID, label := range assignments {
		if label != -1 {
			groups[label] = append(groups[label], articleID)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `UPDATE articles SET cluster_id = NULL`); err != nil {
		return fmt.Errorf("clear article clusters: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM clusters`); err != nil {
		return fmt.Errorf("delete clusters: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	for _, articleIDs := range groups {
		placeholders, args := buildInClause(articleIDs)

		var canonicalID int64
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT a.id FROM articles a JOIN sources s ON a.source_id = s.id WHERE a.id IN (%s) ORDER BY s.credibility DESC, a.published_at DESC NULLS LAST LIMIT 1`,
			placeholders,
		), args...).Scan(&canonicalID); err != nil {
			return fmt.Errorf("select canonical: %w", err)
		}

		var firstSeen sql.NullString
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT MIN(fetched_at) FROM articles WHERE id IN (%s)`,
			placeholders,
		), args...).Scan(&firstSeen); err != nil {
			return fmt.Errorf("select first_seen: %w", err)
		}

		fs := now
		if firstSeen.Valid {
			fs = firstSeen.String
		}

		res, err := tx.ExecContext(ctx,
			`INSERT INTO clusters (canonical_id, first_seen_at, last_updated_at, article_count) VALUES (?, ?, ?, ?)`,
			canonicalID, fs, now, len(articleIDs),
		)
		if err != nil {
			return fmt.Errorf("insert cluster: %w", err)
		}

		clusterID, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("last insert id: %w", err)
		}

		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE articles SET cluster_id = ? WHERE id IN (%s)`, placeholders,
		), append([]any{clusterID}, args...)...); err != nil {
			return fmt.Errorf("update article cluster_id: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	slog.Info("upserted clusters", "count", len(groups))
	return nil
}

func buildInClause(ids []int64) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ","), args
}
