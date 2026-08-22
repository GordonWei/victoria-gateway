package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gordonwei/orch-gateway/pkg/config"
	"github.com/gordonwei/orch-gateway/pkg/rag"
)

// runNote implements `orch-gateway note`: an operator confirms what a
// past alert actually turned out to be, and that gets embedded + stored
// in the RAG database for future alerts to retrieve as reference. This is
// the only way records get into the store — orch-gateway itself never
// writes one automatically, on purpose: an LLM's own guess about an
// alert isn't confirmed truth, and seeding the store with unconfirmed
// guesses risks reinforcing wrong ones. See pkg/rag and schema.sql.
func runNote(args []string) {
	fs := flag.NewFlagSet("orch-gateway note", flag.ExitOnError)
	configPath := os.Getenv("ORCH_GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "/etc/orch-gateway/config.yaml"
	}
	fs.StringVar(&configPath, "config", configPath, "path to config.yaml")
	alertName := fs.String("alert-name", "", "the alertname this note is about (required)")
	host := fs.String("host", "", "the host/instance the alert was about")
	logExcerpt := fs.String("log-excerpt", "", "a relevant log excerpt, for your own future reference (optional)")
	summary := fs.String("summary", "", "the AI-generated summary at the time, if you have it (optional)")
	resolution := fs.String("resolution", "", "what it actually turned out to be and/or how it was fixed (required)")
	fs.Parse(args)

	if *alertName == "" || *resolution == "" {
		fmt.Fprintln(os.Stderr, "❌ --alert-name and --resolution are required")
		fs.Usage()
		os.Exit(1)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if cfg.RAG == nil || !cfg.RAG.Enabled {
		fmt.Fprintln(os.Stderr, "❌ rag.enabled is not set to true in config.yaml — there's no store to write this note into")
		os.Exit(1)
	}
	if cfg.RAG.PostgresDSN == "" || cfg.RAG.EmbeddingEndpoint == "" || cfg.RAG.EmbeddingModel == "" {
		fmt.Fprintln(os.Stderr, "❌ rag.postgres_dsn/embedding_endpoint/embedding_model is missing in config.yaml")
		os.Exit(1)
	}

	store, err := rag.OpenPostgres(cfg.RAG.PostgresDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	embedder := rag.NewEmbedder(cfg.RAG.EmbeddingEndpoint, cfg.RAG.EmbeddingModel)
	queryText := rag.BuildQueryText(*alertName, *host, *summary, []string{*logExcerpt})
	embedding, err := embedder.Embed(queryText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ embed: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rec := rag.Record{
		AlertName:  *alertName,
		Host:       *host,
		LogExcerpt: *logExcerpt,
		Summary:    *summary,
		Resolution: *resolution,
	}
	if err := store.Insert(ctx, rec, embedding); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ saved: %s (%s)\n", *alertName, *host)
}
