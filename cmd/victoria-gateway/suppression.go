// suppression.go implements `victoria-gateway suppression-candidates`: it
// reads the confirmed-incident history, groups it into (alertname, host)
// buckets that keep getting confirmed as the same known thing, and prints
// each one as a candidate suppression rule for a human to review. See
// pkg/suppress for the analysis and the caveats on what Count does and
// doesn't measure.
//
// By default this command only ever reads and prints — it never writes to
// Alertmanager's config, never calls its API, and never touches the RAG
// store. Applying a candidate as a *permanent* route (editing
// Alertmanager's config.yml and reloading it) is left entirely to the
// human who decided the printed rule is right — an automated writeback
// there would need to be trusted with the ability to permanently silence
// a class of alert with no review step, which this command deliberately
// does not do. See suppressionWritebackNote for what that path would
// still need before it could be trusted with that.
//
// -apply-silences is narrower and safer than that: it creates a real,
// time-bounded Alertmanager *silence* via the v2 silence API (see
// pkg/alertmanager) — never a config file edit, never a reload, and it
// self-expires on its own, so a wrong call here doesn't need a human to
// remember to undo it. Even that is dry-run by default (prints what it
// would create) and only actually calls the API with -yes, so a human
// still reviews the candidate list before anything real happens.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/alertmanager"
	"github.com/gordonwei/victoria-gateway/pkg/audit"
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
	applySilences := fs.Bool("apply-silences", false, "for each candidate, show whether a time-bounded Alertmanager silence would be created via the API (dry run unless -yes is also passed); never touches Alertmanager's config file — see this file's package doc")
	yes := fs.Bool("yes", false, "with -apply-silences, actually create the silences shown, instead of a dry run")
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

	if *applySilences {
		applySuppressionSilences(cfg, candidates, *silenceDuration, *yes)
	}
}

// applySuppressionSilences implements -apply-silences: for each candidate,
// create (or, without -yes, describe) the same time-bounded silence Option
// B's printed amtool command would — via the Alertmanager API directly
// instead of a human running amtool by hand. See this file's package doc
// for why a silence is the one kind of "apply" this command does
// automatically, and pkg/alertmanager for the client itself.
func applySuppressionSilences(cfg *config.Config, candidates []suppress.Candidate, silenceDurationFlag string, yes bool) {
	fmt.Println("\n=== -apply-silences ===")
	if cfg.Alertmanager == nil || cfg.Alertmanager.Endpoint == "" {
		fmt.Fprintln(os.Stderr, "❌ -apply-silences requires an `alertmanager:` block (at least `endpoint`) in config.yaml")
		os.Exit(1)
	}
	// amtool accepts duration forms Go's time.ParseDuration doesn't (e.g.
	// "30d") — Option B's printed command passes silenceDurationFlag to
	// amtool verbatim and can use those, but the API call below needs a
	// real time.Duration, so this path is deliberately stricter about the
	// syntax it accepts.
	duration, err := time.ParseDuration(silenceDurationFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ -apply-silences needs -silence-duration in Go duration syntax (e.g. \"720h\"), not amtool's — got %q: %v\n", silenceDurationFlag, err)
		os.Exit(1)
	}

	amClient := alertmanager.NewClient(alertmanager.Config{
		Endpoint: cfg.Alertmanager.Endpoint,
		Username: cfg.Alertmanager.Username,
		Password: cfg.Alertmanager.Password,
	})

	var auditLogger audit.Logger = audit.NoopLogger{}
	if cfg.RAG.AuditLog { // config.Load/Validate already required rag.enabled by this point (see the rag.enabled check above)
		l, err := audit.OpenPostgres(cfg.RAG.PostgresDSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ audit: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = l.Close() }()
		auditLogger = l
	}
	actor := "cli"
	if u := os.Getenv("USER"); u != "" {
		actor = "cli:" + u
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for i, c := range candidates {
		matchers := []alertmanager.Matcher{
			alertmanager.NewExactMatcher("alertname", c.AlertName),
			alertmanager.NewExactMatcher("host", c.Host),
		}
		comment := fmt.Sprintf("confirmed %d times as known noise via victoria-gateway suppression-candidates", c.Count)

		exists, err := amClient.ActiveSilenceExists(ctx, matchers)
		if err != nil {
			fmt.Fprintf(os.Stderr, "candidate %d/%d (alertname=%q host=%q): ❌ check for an existing silence failed: %v\n", i+1, len(candidates), c.AlertName, c.Host, err)
			continue
		}
		if exists {
			fmt.Printf("candidate %d/%d (alertname=%q host=%q): already has an active silence, skipping\n", i+1, len(candidates), c.AlertName, c.Host)
			continue
		}

		if !yes {
			fmt.Printf("candidate %d/%d (alertname=%q host=%q): [dry run] would create a %s silence — re-run with -yes to actually create it\n",
				i+1, len(candidates), c.AlertName, c.Host, duration)
			continue
		}

		id, err := amClient.CreateSilence(ctx, matchers, duration, "victoria-gateway", comment)
		if err != nil {
			fmt.Fprintf(os.Stderr, "candidate %d/%d (alertname=%q host=%q): ❌ create silence failed: %v\n", i+1, len(candidates), c.AlertName, c.Host, err)
			continue
		}
		fmt.Printf("candidate %d/%d (alertname=%q host=%q): ✅ created silence %s, expires in %s\n", i+1, len(candidates), c.AlertName, c.Host, id, duration)

		if err := auditLogger.Record(ctx, audit.Entry{
			Actor:  actor,
			Action: "suppression.apply_silence",
			Target: fmt.Sprintf("alertname=%s host=%s", c.AlertName, c.Host),
			Detail: fmt.Sprintf("silence id=%s duration=%s confirmed_count=%d", id, duration, c.Count),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "audit: record suppression.apply_silence: %v\n", err)
		}
	}
}

// suppressionWritebackNote documents, for whoever next picks this up, the
// current state of writeback and what's still deliberately not built.
// Option B (a time-bounded silence) CAN now be auto-applied — see
// -apply-silences and applySuppressionSilences — because a silence
// self-expires, so a wrong one doesn't need a human to remember to revert
// it. Option A (a permanent route.routes entry) still cannot: before that
// could be trusted with write access to Alertmanager's config, it would
// need (1) a way to read Alertmanager's *current* config so an edit
// doesn't clobber unrelated routes/receivers changed by hand since this
// tool last looked, (2) a dry-run diff shown to the human before any
// write, (3) confirmation that the target `receiver` in --null-receiver
// actually exists and has no integrations (nothing here does any of that
// today) — and, unlike a silence, a bad permanent edit doesn't fix itself.
const suppressionWritebackNote = "(-apply-silences can create Option B for you now (dry run by default, see -yes). Option A stays print-only — see suppression.go's package doc for what a permanent-route writeback would still need before being trustworthy.)"
