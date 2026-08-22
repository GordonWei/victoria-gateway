-- Victoria Gateway RAG store schema. Run this once against the Postgres
-- database named in config.yaml's rag.postgres_dsn before setting
-- rag.enabled: true.
--
-- Requires the pgvector extension to be installed at the OS/package level
-- first (e.g. `apt install postgresql-16-pgvector` on the DB host) —
-- CREATE EXTENSION below only registers it inside this database, it can't
-- install the extension binary itself.
--
-- Vector dimension is 1024, matching bge-m3's dense embedding output. If
-- you configure a different rag.embedding_model, check its output
-- dimension and adjust the `vector(1024)` column below to match before
-- running this — pgvector enforces the declared dimension per column.
--
-- status is 'pending' or 'confirmed'. A row starts pending the moment an
-- alert is analyzed (see rag.Gitea) — nothing about a pending row is
-- verified truth yet, so Search only ever returns confirmed rows (see
-- store.go). A row becomes confirmed once a human resolution is attached
-- (either via `victoria-gateway note` directly, or via `victoria-gateway
-- sync` reading the closing comment off the linked Gitea issue).

CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS incidents (
    id                 BIGSERIAL PRIMARY KEY,
    alert_name         TEXT NOT NULL,
    host               TEXT NOT NULL DEFAULT '',
    log_excerpt        TEXT NOT NULL DEFAULT '',
    summary            TEXT NOT NULL DEFAULT '',
    resolution         TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'confirmed')),
    gitea_issue_number BIGINT,
    embedding          vector(1024) NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at       TIMESTAMPTZ
);

-- HNSW over cosine distance, matching the `<=>` operator PGStore.Search
-- uses in pkg/rag/store.go. Built after rows exist (or empty is fine too
-- — HNSW builds incrementally as rows are inserted).
CREATE INDEX IF NOT EXISTS incidents_embedding_hnsw_idx
    ON incidents USING hnsw (embedding vector_cosine_ops);

-- Sync (see cmd/victoria-gateway/sync.go) needs to find pending rows with
-- a linked Gitea issue without a full table scan.
CREATE INDEX IF NOT EXISTS incidents_pending_gitea_idx
    ON incidents (gitea_issue_number) WHERE status = 'pending' AND gitea_issue_number IS NOT NULL;
