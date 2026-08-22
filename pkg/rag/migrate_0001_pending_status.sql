-- One-time migration for a database that already ran the original
-- schema.sql (resolution NOT NULL, no status/gitea_issue_number/
-- confirmed_at columns) before the pending/confirmed capture flow
-- existed. A fresh install should just run schema.sql directly instead
-- of this file — this is only for upgrading an existing incidents table
-- in place without losing any rows already in it.

ALTER TABLE incidents ALTER COLUMN resolution SET DEFAULT '';
ALTER TABLE incidents ALTER COLUMN resolution DROP NOT NULL;
UPDATE incidents SET resolution = '' WHERE resolution IS NULL;
ALTER TABLE incidents ALTER COLUMN resolution SET NOT NULL;

ALTER TABLE incidents ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'confirmed'
    CHECK (status IN ('pending', 'confirmed'));
-- Existing rows (if any) all have a real resolution already (the old
-- schema required one), so they're confirmed by definition — only rows
-- inserted after this migration can ever be 'pending'.

ALTER TABLE incidents ADD COLUMN IF NOT EXISTS gitea_issue_number BIGINT;
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS incidents_pending_gitea_idx
    ON incidents (gitea_issue_number) WHERE status = 'pending' AND gitea_issue_number IS NOT NULL;
