package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/salling18/news-digest/internal/embedclient"
)

const DefaultThreshold = 0.3

type profile struct {
	id          int64
	label       string
	description string
	weight      float64
	embedding   []float32
}

type cluster struct {
	id          int64
	canonicalID int64
	embedding   []float32
}

// Rank scores every cluster against the interest profiles and populates
// feed_items with clusters whose relevance meets the threshold.
func Rank(ctx context.Context, db *sql.DB, client *embedclient.Client, threshold float64) error {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}

	profiles, err := loadProfiles(ctx, db)
	if err != nil {
		return fmt.Errorf("loadProfiles: %w", err)
	}
	if len(profiles) == 0 {
		slog.Warn("no interest profiles defined, skipping ranking")
		return nil
	}

	if err := embedMissingProfiles(ctx, db, client, profiles); err != nil {
		return fmt.Errorf("embedMissingProfiles: %w", err)
	}

	clusters, err := loadClusters(ctx, db)
	if err != nil {
		return fmt.Errorf("loadClusters: %w", err)
	}
	if len(clusters) == 0 {
		slog.Warn("no clusters with embeddings found")
		return nil
	}

	scores := make(map[int64]float32, len(clusters))
	for _, cl := range clusters {
		scores[cl.id] = maxWeightedCosine(cl.embedding, profiles)
	}

	for _, cl := range clusters {
		if _, err := db.ExecContext(ctx,
			`UPDATE articles SET relevance = ? WHERE id = ?`,
			scores[cl.id], cl.canonicalID); err != nil {
			return fmt.Errorf("update relevance for article %d: %w", cl.canonicalID, err)
		}
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM feed_items`); err != nil {
		return fmt.Errorf("delete feed_items: %w", err)
	}

	inserted := 0
	for _, cl := range clusters {
		if scores[cl.id] < float32(threshold) {
			continue
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO feed_items (cluster_id, title, description, link, pub_date, relevance)
			 SELECT ?, a.title, a.body, a.url, a.published_at, ?
			 FROM articles a WHERE a.id = ?`,
			cl.id, scores[cl.id], cl.canonicalID); err != nil {
			return fmt.Errorf("insert feed_item for cluster %d: %w", cl.id, err)
		}
		inserted++
	}

	slog.Info("ranking complete", "clusters", len(clusters), "feed_items", inserted)
	return nil
}

func loadProfiles(ctx context.Context, db *sql.DB) ([]profile, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, label, description, weight, embedding FROM interest_profile`)
	if err != nil {
		return nil, fmt.Errorf("query profiles: %w", err)
	}
	defer rows.Close()

	var profiles []profile
	for rows.Next() {
		var p profile
		var desc sql.NullString
		var blob []byte
		if err := rows.Scan(&p.id, &p.label, &desc, &p.weight, &blob); err != nil {
			return nil, fmt.Errorf("scan profile: %w", err)
		}
		p.description = desc.String
		if len(blob) > 0 {
			p.embedding, err = decodeVector(blob)
			if err != nil {
				return nil, fmt.Errorf("decode profile %d embedding: %w", p.id, err)
			}
		}
		profiles = append(profiles, p)
	}
	return profiles, rows.Err()
}

func embedMissingProfiles(ctx context.Context, db *sql.DB, client *embedclient.Client, profiles []profile) error {
	var toEmbed []int
	var texts []string
	for i, p := range profiles {
		if len(p.embedding) == 0 {
			toEmbed = append(toEmbed, i)
			texts = append(texts, p.label+" "+p.description)
		}
	}
	if len(toEmbed) == 0 {
		return nil
	}

	vectors, err := client.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embed profiles: %w", err)
	}

	for j, idx := range toEmbed {
		profiles[idx].embedding = vectors[j]

		blob, err := encodeVector(vectors[j])
		if err != nil {
			return fmt.Errorf("encode profile %d: %w", profiles[idx].id, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE interest_profile SET embedding = ? WHERE id = ?`,
			blob, profiles[idx].id); err != nil {
			return fmt.Errorf("store profile %d embedding: %w", profiles[idx].id, err)
		}
	}

	slog.Info("embedded missing profiles", "count", len(toEmbed))
	return nil
}

func loadClusters(ctx context.Context, db *sql.DB) ([]cluster, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT c.id, c.canonical_id, a.embedding
		 FROM clusters c
		 JOIN articles a ON c.canonical_id = a.id
		 WHERE a.embedding IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("query clusters: %w", err)
	}
	defer rows.Close()

	var clusters []cluster
	for rows.Next() {
		var cl cluster
		var blob []byte
		if err := rows.Scan(&cl.id, &cl.canonicalID, &blob); err != nil {
			return nil, fmt.Errorf("scan cluster: %w", err)
		}
		cl.embedding, err = decodeVector(blob)
		if err != nil {
			return nil, fmt.Errorf("decode cluster %d embedding: %w", cl.id, err)
		}
		clusters = append(clusters, cl)
	}
	return clusters, rows.Err()
}

func maxWeightedCosine(clusterEmb []float32, profiles []profile) float32 {
	var best float32
	for _, p := range profiles {
		if len(p.embedding) == 0 {
			continue
		}
		if score := float32(p.weight) * cosine(clusterEmb, p.embedding); score > best {
			best = score
		}
	}
	return best
}

func cosine(a, b []float32) float32 {
	var dot float32
	for i := range a {
		dot += a[i] * b[i]
	}
	return dot
}
