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
    embedding_model    TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at       TIMESTAMPTZ,
    -- Set when an operator batch-confirms a group of identical recurring
    -- alerts from /pending: every row but the representative points at
    -- the representative's id. It keeps the duplicates out of RAG
    -- retrieval (see PGStore.Search) without deleting or hiding them —
    -- they are still real alerts that fired and still show in /incidents.
    dup_of             BIGINT
);

-- A fresh install gets embedding_model for free from CREATE TABLE above.
-- An existing deployment from before this column existed should run
-- migrate_0002_embedding_model.sql instead (see that file) — a fresh
-- install does not need it.

-- Only the representative of a confirmed duplicate group is offered to
-- the LLM as a retrieval example, so Search filters on dup_of IS NULL.
CREATE INDEX IF NOT EXISTS incidents_dup_of_idx
    ON incidents (dup_of)
 WHERE dup_of IS NOT NULL;

-- Backs the (alert_name, host) grouping the batch-confirm UI lists.
CREATE INDEX IF NOT EXISTS incidents_pending_group_idx
    ON incidents (alert_name, host)
 WHERE status = 'pending';

-- An existing deployment from before dup_of existed should run
-- migrate_0003_dup_of.sql instead of re-running this file.

-- HNSW over cosine distance, matching the `<=>` operator PGStore.Search
-- uses in pkg/rag/store.go. Built after rows exist (or empty is fine too
-- — HNSW builds incrementally as rows are inserted).
CREATE INDEX IF NOT EXISTS incidents_embedding_hnsw_idx
    ON incidents USING hnsw (embedding vector_cosine_ops);

-- Sync (see cmd/victoria-gateway/sync.go) needs to find pending rows with
-- a linked Gitea issue without a full table scan.
CREATE INDEX IF NOT EXISTS incidents_pending_gitea_idx
    ON incidents (gitea_issue_number) WHERE status = 'pending' AND gitea_issue_number IS NOT NULL;

-- pkg/audit's table, for rag.audit_log: true. Lives in this file (not a
-- separate migration) because CREATE TABLE/INDEX IF NOT EXISTS makes
-- re-running this whole file against an already-provisioned database safe
-- — an existing deployment just gains this table the next time it runs
-- schema.sql, same as a fresh install.
CREATE TABLE IF NOT EXISTS audit_log (
    id     BIGSERIAL PRIMARY KEY,
    at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor  TEXT NOT NULL,
    action TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS audit_log_at_idx ON audit_log (at DESC);
