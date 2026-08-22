package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/gitea"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

// runSync implements `victoria-gateway sync`: it checks every Pending
// record that has a linked Gitea issue (filed automatically when the
// alert was analyzed — see captureIncident in main.go), and for any
// issue that's since been closed, pulls its last comment in as the
// resolution and marks the record Confirmed. Meant to run on a schedule
// (cron), not as a long-running process — it does one pass and exits.
//
// This is the automated half of the capture/confirm split: capture
// happens for free when an alert fires, and if you're in the habit of
// leaving a closing comment on the Gitea issue before closing it, this
// picks that up without you ever having to run `note` by hand. Issues
// closed without a comment are left Pending — there's nothing to
// confirm with.
func runSync(args []string) {
	fs := flag.NewFlagSet("victoria-gateway sync", flag.ExitOnError)
	configPath := os.Getenv("VICTORIA_GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "/etc/victoria-gateway/config.yaml"
	}
	fs.StringVar(&configPath, "config", configPath, "path to config.yaml")
	fs.Parse(args)

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if cfg.RAG == nil || !cfg.RAG.Enabled {
		fmt.Fprintln(os.Stderr, "❌ rag.enabled is not set to true in config.yaml")
		os.Exit(1)
	}
	if cfg.RAG.Gitea == nil {
		fmt.Fprintln(os.Stderr, "❌ rag.gitea is not configured in config.yaml — sync has nothing to check against")
		os.Exit(1)
	}

	store, err := rag.OpenPostgres(cfg.RAG.PostgresDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	giteaClient := gitea.NewClient(gitea.ClientConfig{
		Endpoint: cfg.RAG.Gitea.Endpoint,
		Token:    cfg.RAG.Gitea.Token,
		Owner:    cfg.RAG.Gitea.Owner,
		Repo:     cfg.RAG.Gitea.Repo,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pending, err := store.PendingWithGiteaIssue(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if len(pending) == 0 {
		fmt.Println("nothing pending with a linked Gitea issue")
		return
	}

	var confirmed, stillOpen, skippedNoComment, failed int
	for _, rec := range pending {
		issue, err := giteaClient.GetIssue(ctx, rec.GiteaIssueNumber)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  id=%d issue=#%d: check failed: %v\n", rec.ID, rec.GiteaIssueNumber, err)
			failed++
			continue
		}
		if issue.State != "closed" {
			stillOpen++
			continue
		}

		resolution, err := giteaClient.LastComment(ctx, rec.GiteaIssueNumber)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  id=%d issue=#%d: fetch comments failed: %v\n", rec.ID, rec.GiteaIssueNumber, err)
			failed++
			continue
		}
		if resolution == "" {
			// Closed with no comment — nothing to confirm with. Left
			// Pending rather than guessed at; someone can still
			// `note --id` it by hand later.
			skippedNoComment++
			continue
		}

		if err := store.Confirm(ctx, rec.ID, resolution); err != nil {
			fmt.Fprintf(os.Stderr, "  id=%d: confirm failed: %v\n", rec.ID, err)
			failed++
			continue
		}
		fmt.Printf("  ✅ confirmed id=%d from issue #%d (%s)\n", rec.ID, rec.GiteaIssueNumber, rec.AlertName)
		confirmed++
	}

	fmt.Printf("done: %d confirmed, %d still open, %d closed with no comment, %d failed\n",
		confirmed, stillOpen, skippedNoComment, failed)
}
