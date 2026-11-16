-- Whether a blocking finding of a run a GitHub push started is approved only by someone who linked a
-- GitHub account (#2342): each org's call in Guardrails, off until an admin turns it on.
ALTER TABLE org_preferences ADD COLUMN push_approvers_linked boolean NOT NULL DEFAULT false;
