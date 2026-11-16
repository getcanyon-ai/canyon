-- A run's once-per-run rows (its states, its producer logs, its verdict) carry a dedupe key:
-- the run driver replays a step that was interrupted before its checkpoint
-- (design/ARCHITECTURE.md, DBOS), and the append-only table cannot take the same row twice.
ALTER TABLE timeline_rows ADD COLUMN dedupe text;
CREATE UNIQUE INDEX timeline_rows_dedupe ON timeline_rows (run_id, dedupe) WHERE dedupe IS NOT NULL;
