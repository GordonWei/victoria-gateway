package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// runNote implements `victoria-gateway note`: an operator confirms what a
// past alert actually turned out to be. Two modes:
//
//   - `--id <n> --resolution "..."` confirms an existing Pending record
//     (one auto-captured by the server when the alert fired — see
//     captureIncident in main.go) — this is the common case now that
//     capture is automatic.
//   - `--alert-name ... --resolution ...` (no `--id`) creates a brand new
//     Confirmed record from scratch, for an incident that predates RAG
//     being enabled or was never auto-captured.
//
// Either way, victoria-gateway itself never marks anything Confirmed on
// its own — an LLM's own guess isn't confirmed truth, and seeding the
// store with unconfirmed guesses risks reinforcing wrong ones. See
// pkg/rag and schema.sql.
func runNote(args []string) {
	fs := flag.NewFlagSet("victoria-gateway note", flag.ExitOnError)
	configPath := os.Getenv("VICTORIA_GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "/etc/victoria-gateway/config.yaml"
	}
	fs.StringVar(&configPath, "config", configPath, "path to config.yaml")
	id := fs.Int64("id", 0, "confirm this existing pending record's id instead of creating a new one")
	alertName := fs.String("alert-name", "", "the alertname this note is about (required unless --id is set)")
	host := fs.String("host", "", "the host/instance the alert was about")
	logExcerpt := fs.String("log-excerpt", "", "a relevant log excerpt, for your own future reference (optional)")
	summary := fs.String("summary", "", "the AI-generated summary at the time, if you have it (optional)")
	resolution := fs.String("resolution", "", "what it actually turned out to be and/or how it was fixed (required)")
	fs.Parse(args)

	if *resolution == "" || (*id == 0 && *alertName == "") {
		fmt.Fprintln(os.Stderr, "❌ --resolution is required, and either --id or --alert-name")
		fs.Usage()
		os.Exit(1)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if cfg.RAG == nil || !cfg.RAG.Enabled {
		fmt.Fprintln(os.Stderr, "❌ rag.enabled is not set to true in config.yaml — there's no store to write this note into")
		os.Exit(1)
	}

	store, err := rag.OpenPostgres(cfg.RAG.PostgresDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if *id != 0 {
		if err := store.Confirm(ctx, *id, *resolution); err != nil {
			fmt.Fprintf(os.Stderr, "❌ %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ confirmed: id=%d\n", *id)
		return
	}

	embedder := rag.NewEmbedder(cfg.RAG.EmbeddingEndpoint, cfg.RAG.EmbeddingModel, cfg.RAG.EmbeddingAPIKey)
	queryText := rag.BuildQueryText(*alertName, *host, *summary, []string{*logExcerpt})
	embedding, err := embedder.Embed(queryText)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ embed: %v\n", err)
		os.Exit(1)
	}

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
