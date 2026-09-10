package rag

import (
	"strings"
	"testing"
)

func TestCheckEmbeddingModelDrift_NoRows(t *testing.T) {
	warning, ok := CheckEmbeddingModelDrift("bge-m3", nil)
	if !ok || warning != "" {
		t.Errorf("got (%q, %v), want (\"\", true) when there are no existing rows", warning, ok)
	}
}

func TestCheckEmbeddingModelDrift_AllMatch(t *testing.T) {
	warning, ok := CheckEmbeddingModelDrift("bge-m3", []string{"bge-m3", "bge-m3"})
	if !ok || warning != "" {
		t.Errorf("got (%q, %v), want (\"\", true) when every row matches the configured model", warning, ok)
	}
}

func TestCheckEmbeddingModelDrift_OtherModel(t *testing.T) {
	warning, ok := CheckEmbeddingModelDrift("bge-m3", []string{"bge-m3", "nomic-embed-text-v1.5"})
	if ok {
		t.Fatal("expected ok=false when a row was embedded with a different model")
	}
	for _, want := range []string{"bge-m3", "nomic-embed-text-v1.5"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning = %q, missing %q", warning, want)
		}
	}
}

func TestCheckEmbeddingModelDrift_UnknownLegacyRows(t *testing.T) {
	warning, ok := CheckEmbeddingModelDrift("bge-m3", []string{"bge-m3", ""})
	if ok {
		t.Fatal("expected ok=false when some rows have no recorded embedding_model")
	}
	if !strings.Contains(warning, "before this column existed") {
		t.Errorf("warning = %q, expected it to call out legacy rows", warning)
	}
}

func TestCheckEmbeddingModelDrift_BothOtherModelAndUnknown(t *testing.T) {
	warning, ok := CheckEmbeddingModelDrift("bge-m3", []string{"bge-m3", "nomic-embed-text-v1.5", ""})
	if ok {
		t.Fatal("expected ok=false")
	}
	for _, want := range []string{"nomic-embed-text-v1.5", "before this column existed"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning = %q, missing %q", warning, want)
		}
	}
}

func TestCheckEmbeddingModelDrift_OnlyUnknownNoOtherModel(t *testing.T) {
	// Every row is legacy (pre-migration) and none names a wrong model —
	// still a drift, since there's no way to know those rows were
	// actually produced by the configured model.
	warning, ok := CheckEmbeddingModelDrift("bge-m3", []string{""})
	if ok {
		t.Fatal("expected ok=false when all rows are unlabeled")
	}
	if strings.Contains(warning, "row(s) embedded with") {
		t.Errorf("warning = %q, should not claim a specific mismatched model when there is none", warning)
	}
}
