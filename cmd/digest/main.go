// Package main provides the CLI entry point for the news-digest pipeline.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"news-digest/internal/db"
	"news-digest/internal/embedclient"
	"news-digest/internal/pipeline"
	"news-digest/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: digest <ingest|embed|rank|serve> [flags]")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "ingest":
		err = runIngest(ctx, os.Args[2:])
	case "embed":
		err = runEmbed(ctx, os.Args[2:])
	case "rank":
		err = runRank(ctx, os.Args[2:])
	case "serve":
		err = runServe(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(1)
	}

	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func runIngest(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	dbPath := fs.String("db", "digest.db", "path to SQLite database")
	addSource := fs.String("add-source", "", "URL of a new feed source to add")
	name := fs.String("name", "", "display name for the new source")
	if err := fs.Parse(args); err != nil {
		return err
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	if *addSource != "" {
		return pipeline.AddSource(ctx, database, *addSource, *name)
	}
	return pipeline.Ingest(ctx, database)
}

func runEmbed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	dbPath := fs.String("db", "digest.db", "path to SQLite database")
	embedURL := fs.String("embed-url", "http://127.0.0.1:8001", "URL of the embed service")
	minCluster := fs.Int("min-cluster-size", 3, "minimum articles per cluster")
	maxAgeDays := fs.Int("max-age-days", 0, "max article age in days (0 = no limit)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	client := embedclient.New(*embedURL)
	return pipeline.Embed(ctx, database, client, *minCluster, *maxAgeDays)
}

func runRank(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rank", flag.ExitOnError)
	dbPath := fs.String("db", "digest.db", "path to SQLite database")
	embedURL := fs.String("embed-url", "http://127.0.0.1:8001", "URL of the embed service")
	threshold := fs.Float64("threshold", 0.3, "minimum relevance score for feed inclusion")
	if err := fs.Parse(args); err != nil {
		return err
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	client := embedclient.New(*embedURL)
	return pipeline.Rank(ctx, database, client, *threshold)
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dbPath := fs.String("db", "digest.db", "path to SQLite database")
	embedURL := fs.String("embed-url", "http://127.0.0.1:8001", "URL of the embed service")
	port := fs.Int("port", 8000, "HTTP server port")
	minCluster := fs.Int("min-cluster-size", 3, "minimum articles per cluster")
	maxAgeDays := fs.Int("max-age-days", 0, "max article age in days (0 = no limit)")
	threshold := fs.Float64("threshold", 0.3, "minimum relevance score for feed inclusion")
	if err := fs.Parse(args); err != nil {
		return err
	}

	database, err := db.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer database.Close()

	client := embedclient.New(*embedURL)
	return server.ListenAndServe(ctx, server.Config{
		DB:         database,
		Client:     client,
		Port:       *port,
		MinCluster: *minCluster,
		MaxAgeDays: *maxAgeDays,
		Threshold:  *threshold,
	})
}
