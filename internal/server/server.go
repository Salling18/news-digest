// Package server provides the HTTP server and cron scheduler for the
// news-digest pipeline (Layer 4).
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/robfig/cron/v3"

	"github.com/salling18/news-digest/internal/embedclient"
	"github.com/salling18/news-digest/internal/pipeline"
)

// Config holds the parameters needed to start the server.
type Config struct {
	DB         *sql.DB
	Client     *embedclient.Client
	Port       int
	MinCluster int
	MaxAgeDays int
	Threshold  float64
}

// ListenAndServe starts the HTTP server and the hourly pipeline cron job.
// It blocks until ctx is cancelled, then shuts down gracefully.
func ListenAndServe(ctx context.Context, cfg Config) error {
	r := chi.NewRouter()

	r.Get("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		handleFeed(w, r, cfg.DB)
	})
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		handleHealth(w, r, cfg.DB)
	})

	// Start the cron scheduler for the pipeline.
	c := cron.New()
	_, err := c.AddFunc("@every 1h", func() {
		runPipeline(ctx, cfg)
	})
	if err != nil {
		return fmt.Errorf("server: add cron job: %w", err)
	}
	c.Start()

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: r,
	}

	// Shut down gracefully when the context is cancelled.
	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")
		c.Stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "err", err)
		}
	}()

	slog.Info("starting server", "port", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server: listen: %w", err)
	}
	return nil
}

// runPipeline executes the full ingest -> embed -> rank pipeline.
func runPipeline(ctx context.Context, cfg Config) {
	slog.Info("pipeline: starting")

	if err := pipeline.Ingest(ctx, cfg.DB); err != nil {
		slog.Error("pipeline: ingester failed", "err", err)
		return
	}

	if err := pipeline.Embed(ctx, cfg.DB, cfg.Client, cfg.MinCluster, cfg.MaxAgeDays); err != nil {
		slog.Error("pipeline: embedder failed", "err", err)
		return
	}

	if err := pipeline.Rank(ctx, cfg.DB, cfg.Client, cfg.Threshold); err != nil {
		slog.Error("pipeline: ranker failed", "err", err)
		return
	}

	slog.Info("pipeline: complete")
}

// handleHealth returns JSON statistics about the database.
func handleHealth(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	type healthResponse struct {
		ArticleCount int    `json:"article_count"`
		SourceCount  int    `json:"source_count"`
		ClusterCount int    `json:"cluster_count"`
		LastFetched  string `json:"last_fetched"`
	}

	var resp healthResponse
	var lastFetched sql.NullString
	_ = db.QueryRowContext(r.Context(), `
		SELECT
			(SELECT COUNT(*) FROM articles),
			(SELECT COUNT(*) FROM sources),
			(SELECT COUNT(*) FROM clusters),
			(SELECT MAX(last_fetched_at) FROM sources)
	`).Scan(&resp.ArticleCount, &resp.SourceCount, &resp.ClusterCount, &lastFetched)
	if lastFetched.Valid {
		resp.LastFetched = lastFetched.String
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleFeed generates RSS XML from unpublished feed items.
func handleFeed(w http.ResponseWriter, r *http.Request, db *sql.DB) {
	data, err := generateRSS(r.Context(), db)
	if err != nil {
		slog.Error("handleFeed: generateRSS failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write(data)
}
