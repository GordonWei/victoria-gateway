-- One-time migration for a database that already ran schema.sql before
-- the embedding_model column existed. A fresh install should just run
-- schema.sql directly instead of this file.
--
-- Existing rows end up with '' here — there is no way to recover which
-- model actually produced their vector after the fact. That's expected:
-- CheckEmbeddingModelDrift (pkg/rag/drift.go) treats '' as "unknown, can't
-- verify" rather than "matches whatever is configured now", and the
-- startup warning it drives will call those rows out until they're
-- re-embedded (`victoria-gateway note`) or the operator otherwise confirms
-- they were in fact produced by the currently configured model.

ALTER TABLE incidents ADD COLUMN IF NOT EXISTS embedding_model TEXT NOT NULL DEFAULT '';
