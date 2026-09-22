-- One-time migration for a database that already ran schema.sql before
-- dup_of existed. A fresh install gets it from schema.sql directly and
-- should not run this file.
--
-- dup_of marks a pending/confirmed row as a duplicate of another row that
-- represents the same recurring alert. It exists so that confirming a
-- burst of identical alerts in one go doesn't flood the RAG corpus: the
-- representative row still gets searched, the duplicates don't.
--
-- Nothing sets this column until an operator confirms a group from the
-- /pending page, so running this migration on its own changes no
-- behaviour — every existing row keeps dup_of NULL and stays searchable.
--
-- Rows are never deleted or hidden from the incident list by this: a
-- duplicate is still a full record of a real alert that fired, and
-- /incidents still shows it. The only thing dup_of changes is whether a
-- row is offered back to the LLM as a retrieval example.

ALTER TABLE incidents ADD COLUMN IF NOT EXISTS dup_of BIGINT;

-- Partial index: the only queries that touch this column either filter
-- for NULL (search) or look up a specific representative's duplicates.
CREATE INDEX IF NOT EXISTS incidents_dup_of_idx
    ON incidents (dup_of)
 WHERE dup_of IS NOT NULL;

-- Grouping pending rows by (alert_name, host) is what the batch-confirm
-- UI lists. Partial index keeps it small: confirmed rows are the bulk of
-- the table over time and this query never looks at them.
CREATE INDEX IF NOT EXISTS incidents_pending_group_idx
    ON incidents (alert_name, host)
 WHERE status = 'pending';
