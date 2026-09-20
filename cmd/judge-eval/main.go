// judge-eval is a standalone tool to check how well TypeSafe AI's Jev
// model calibrates against this deployment's own (Chinese-language)
// incident text before flipping pkg/judge from dry-run observation to an
// actual escalation-decision input. It never writes to the RAG store or
// touches victoria-gateway's own config/runtime — read-only against
// Postgres, and one HTTP call per incident against the Jev API.
//
// Usage:
//
//	TYPESAFE_API_KEY=... go run ./cmd/judge-eval \
//	  --postgres-dsn "postgres://user:pass@host:5432/dbname" \
//	  --limit 10 --include-pending
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/gordonwei/victoria-gateway/pkg/judge"
	"github.com/gordonwei/victoria-gateway/pkg/rag"
)

func main() {
	dsn := flag.String("postgres-dsn", os.Getenv("VICTORIA_GATEWAY_RAG_DSN"), "RAG store DSN (or set VICTORIA_GATEWAY_RAG_DSN)")
	limit := flag.Int("limit", 10, "max records to sample from each of Confirmed/Pending")
	includePending := flag.Bool("include-pending", true, "also sample Pending (unconfirmed) records — more volume, but these are the local model's own unverified guesses, not ground truth")
	repeatID := flag.Int64("repeat-id", 0, "instead of the batch scan, fetch this one Pending record's exact text and call JudgeEscalation --repeat-n times with the unchanged input — isolates model-inherent variance from record-to-record text differences")
	repeatN := flag.Int("repeat-n", 8, "how many times to repeat --repeat-id's call")
	flag.Parse()

	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "❌ TYPESAFE_API_KEY is not set")
		os.Exit(1)
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "❌ --postgres-dsn (or VICTORIA_GATEWAY_RAG_DSN) is required")
		os.Exit(1)
	}

	store, err := rag.OpenPostgres(*dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ connect to RAG store: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	client := judge.NewClient(apiKey)

	if *repeatID != 0 {
		runRepeat(ctx, store, client, *repeatID, *repeatN)
		return
	}

	confirmed, err := store.ListConfirmed(ctx, rag.ListFilter{}, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ list confirmed: %v\n", err)
		os.Exit(1)
	}
	runBatch(ctx, client, "CONFIRMED", confirmed)

	if *includePending {
		pending, err := store.ListPending(ctx, rag.ListFilter{}, *limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ list pending: %v\n", err)
			os.Exit(1)
		}
		fmt.Println()
		fmt.Println("⚠️  以下為 Pending（未經人工確認）記錄，是 local model 自己的猜測，不是 ground truth，僅供觀察 Jev 對中文文本的反應，不能拿來評斷「Jev 判斷得準不準」")
		runBatch(ctx, client, "PENDING", pending)
	}
}

// runRepeat calls JudgeEscalation n times against the exact same
// (alertName, summary) pulled once from a single record — same request
// content every time, nothing re-fetched or re-templated per call —
// so any spread in the results is System One's own run-to-run variance,
// not this deployment's record-to-record text differences (which is what
// the earlier 9-record batch scan actually measured, and can't rule out
// by itself).
func runRepeat(ctx context.Context, store rag.Store, client *judge.Client, id int64, n int) {
	rec, err := store.GetPending(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ get pending id=%d: %v\n", id, err)
		os.Exit(1)
	}
	fmt.Printf("repeat-testing id=%d alert=%q host=%q, %d identical calls\n\ntext:\n%s\n\n", rec.ID, rec.AlertName, rec.Host, n, rec.Summary)

	var severities, confidences, escalates []float64
	for i := 1; i <= n; i++ {
		j, err := client.JudgeEscalation(ctx, rec.AlertName, rec.Summary)
		if err != nil {
			fmt.Printf("call %d/%d -> ERROR: %v\n", i, n, err)
			continue
		}
		fmt.Printf("call %d/%d -> severity=%.2f/3 (conf=%.2f) escalate_probability=%.2f\n", i, n, j.Severity, j.SeverityConfidence, j.EscalateProbability)
		severities = append(severities, j.Severity)
		confidences = append(confidences, j.SeverityConfidence)
		escalates = append(escalates, j.EscalateProbability)
	}

	fmt.Println()
	printStats("severity", severities)
	printStats("confidence", confidences)
	printStats("escalate_probability", escalates)
}

func printStats(label string, xs []float64) {
	if len(xs) == 0 {
		fmt.Printf("%s: no successful calls\n", label)
		return
	}
	min, max, sum := xs[0], xs[0], 0.0
	for _, x := range xs {
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
		sum += x
	}
	mean := sum / float64(len(xs))
	var variance float64
	for _, x := range xs {
		variance += (x - mean) * (x - mean)
	}
	variance /= float64(len(xs))
	fmt.Printf("%s: n=%d min=%.2f max=%.2f range=%.2f mean=%.2f stddev=%.3f\n", label, len(xs), min, max, max-min, mean, math.Sqrt(variance))
}

func runBatch(ctx context.Context, client *judge.Client, label string, records []rag.Record) {
	if len(records) == 0 {
		fmt.Printf("[%s] 沒有記錄可測\n", label)
		return
	}
	for _, rec := range records {
		judgment, err := client.JudgeEscalation(ctx, rec.AlertName, rec.Summary)
		if err != nil {
			fmt.Printf("[%s] id=%d alert=%q host=%q -> ERROR: %v\n", label, rec.ID, rec.AlertName, rec.Host, err)
			continue
		}
		fmt.Printf("[%s] id=%d alert=%q host=%q -> severity=%.2f/3 (conf=%.2f) escalate_probability=%.2f\n",
			label, rec.ID, rec.AlertName, rec.Host, judgment.Severity, judgment.SeverityConfidence, judgment.EscalateProbability)
	}
}
