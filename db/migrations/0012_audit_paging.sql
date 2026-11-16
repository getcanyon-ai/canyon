-- The audit trail is paged newest first, whole and by project (contracts/routes.md): the two keyset orders. The first is a strict superset of audit_events_ts (0003), whose only reader was the trail's list.
CREATE INDEX audit_events_page         ON audit_events (ts DESC, id DESC);
CREATE INDEX audit_events_project_page ON audit_events (project_id, ts DESC, id DESC);
DROP INDEX audit_events_ts;
