package rag

import (
	"fmt"
	"strings"
)

// CheckEmbeddingModelDrift compares the currently configured embedding
// model against the distinct embedding_model values already recorded on
// rows in the incidents table (see Store.DistinctEmbeddingModels) and
// reports whether they agree.
//
// Why this exists: the incidents.embedding column is a fixed-dimension
// pgvector column, and pgvector only enforces dimension — not which model
// produced a vector. Swap rag.embedding_model to a different model with
// the same output dimension (or the same model name after it's silently
// re-released with different weights) and every write/read still
// succeeds. The old rows' vectors now live in a different coordinate
// system than new queries, so Search keeps returning results — they're
// just meaningless, and nothing about that fails loudly. This check is
// the only thing that turns that into something an operator can see.
//
// distinct is expected to come straight from Store.DistinctEmbeddingModels
// and may contain "" for rows written before this column existed.
func CheckEmbeddingModelDrift(configured string, distinct []string) (warning string, ok bool) {
	if len(distinct) == 0 {
		return "", true // no rows yet, nothing to compare against
	}

	hasUnknown := false
	var mismatched []string
	for _, m := range distinct {
		switch m {
		case configured:
			// matches what's configured now — fine
		case "":
			hasUnknown = true
		default:
			mismatched = append(mismatched, m)
		}
	}
	if !hasUnknown && len(mismatched) == 0 {
		return "", true
	}

	var found []string
	if len(mismatched) > 0 {
		found = append(found, fmt.Sprintf("row(s) embedded with %s", strings.Join(mismatched, ", ")))
	}
	if hasUnknown {
		found = append(found, "row(s) with no recorded embedding_model (written before this column existed)")
	}
	warning = fmt.Sprintf(
		"rag: embedding model drift — configured embedding_model is %q, but the incidents table has %s. "+
			"Vectors from a different model occupy a different coordinate space; Search will keep returning "+
			"results without erroring, they'll just be meaningless. Re-embed those rows (or confirm they were "+
			"actually produced by %[1]q) before trusting retrieval quality.",
		configured, strings.Join(found, " and "),
	)
	return warning, false
}
