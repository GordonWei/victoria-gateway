// suppression.go implements `victoria-gateway suppression-candidates`: it
// reads the confirmed-incident history, groups it into (alertname, host)
// buckets that keep getting confirmed as the same known thing, and prints
// each one as a candidate suppression rule for a human to review. See
// pkg/suppress for the analysis and the caveats on what Count does and
// doesn't measure.
//
// This command only ever reads and prints — it never writes to
// Alertmanager's config, never calls its API, and never touches the RAG
// store. Applying a candidate (editing Alertmanager's config.yml and
// reloading it, or running the printed amtool command) is left entirely
// to the human who decided the printed rule is right — an automated
// writeback here would need to be trusted with the ability to
// permanently silence a class of alert with no review step, which we're
// deliberately not building. See the design note below for what a
// writeback path would need before it could be trusted with that.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/config"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
	"github.com/gordonwei/victoria-gateway/pkg/suppress"
)

func runSuppressionCandidates(args []string) {
	fs := flag.NewFlagSet("victoria-gateway suppression-candidates", flag.ExitOnError)
	configPath := os.Getenv("VICTORIA_GATEWAY_CONFIG")
	if configPath == "" {
		configPath = "/etc/victoria-gateway/config.yaml"
	}
	fs.StringVar(&configPath, "config", configPath, "path to config.yaml")
	minCount := fs.Int("min-count", 3, "only show (alertname, host) pairs confirmed at least this many times")
	receiver := fs.String("null-receiver", "null", "receiver name to use in the printed route YAML — must already exist in your Alertmanager config with no integrations configured")
	silenceDuration := fs.String("silence-duration", "720h", "duration to pass to the printed amtool silence command (amtool duration syntax, e.g. \"720h\" for 30 days)")
	_ = fs.Parse(args) // flag.ExitOnError already exits the process on a parse error

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	if cfg.RAG == nil || !cfg.RAG.Enabled {
		fmt.Fprintln(os.Stderr, "❌ rag.enabled is not set to true in config.yaml — there's no confirmed-incident history to analyze")
		os.Exit(1)
	}

	store, err := rag.OpenPostgres(cfg.RAG.PostgresDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	confirmed, err := store.AllConfirmed(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}

	candidates := suppress.FindCandidates(confirmed, *minCount)
	if len(candidates) == 0 {
		fmt.Printf("No candidates: nothing has been confirmed %d+ times for the same (alertname, host). Nothing applied, nothing to do.\n", *minCount)
		return
	}

	fmt.Printf("%d candidate(s) — confirmed history only, NOTHING BELOW HAS BEEN APPLIED.\n", len(candidates))
	fmt.Println("Review each one, then apply the route (permanent) or the amtool command (time-bounded) yourself.")
	fmt.Println(suppressionWritebackNote)
	for i, c := range candidates {
		fmt.Printf("\n--- Candidate %d/%d ---\n", i+1, len(candidates))
		fmt.Printf("alertname=%q host=%q\n", c.AlertName, c.Host)
		fmt.Printf("confirmed %d times, %s .. %s\n", c.Count, c.FirstConfirmed.Format("2006-01-02"), c.LastConfirmed.Format("2006-01-02"))
		fmt.Printf("most recent resolution: %s\n", c.SampleResolution)
		fmt.Println("\nOption A — permanent, paste under Alertmanager's route.routes:")
		fmt.Println(c.RouteYAML(*receiver))
		fmt.Println("\nOption B — time-bounded, run against your Alertmanager:")
		fmt.Println(c.SilenceCLI(*silenceDuration, fmt.Sprintf("confirmed %d times as known noise via victoria-gateway", c.Count)))
	}
}

// suppressionWritebackNote documents, for whoever next picks this up, what
// an "apply this candidate for me" path would need before it could be
// trusted with write access to Alertmanager: (1) a way to read
// Alertmanager's *current* config so an edit doesn't clobber unrelated
// routes/receivers changed by hand since this tool last looked, (2) a
// dry-run diff shown to the human before any write, (3) confirmation that
// the target `receiver` in --null-receiver actually exists and has no
// integrations (nothing here today).
const suppressionWritebackNote = "(No auto-apply exists yet — see suppression.go's package doc for what one would need before being trustworthy.)"
