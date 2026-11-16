-- The responder and the hold (design/OPERATIONS.md §3; design/PERMISSIONS.md, Security responders).
-- Expand only: every column defaulted or nullable, so the previous binary's rows still read and write.

-- Responders by address, or by the one claim and value that mean responder, mirroring the admin pair:
-- both or neither, matched at sign-in and kept on the session as claimed_responder.
ALTER TABLE org_preferences
    ADD COLUMN responder_emails text[] NOT NULL DEFAULT '{}',
    ADD COLUMN responder_claim  text,
    ADD COLUMN responder_value  text;

ALTER TABLE sessions ADD COLUMN claimed_responder boolean NOT NULL DEFAULT false;

-- Whether the sign-in's address is one the provider asserted verified in its email claim (design/AUTH.md
-- §3): only then does a grant written for that person carry their subject. A session before this column,
-- and an access request recorded before it, bind nothing.
ALTER TABLE sessions ADD COLUMN email_verified boolean NOT NULL DEFAULT false;
UPDATE access_requests SET subject = '';

-- Whether the sign-in read the person's groups whole ('ok'), or none were read: unset, absent, past the
-- provider's limit (Entra's overage). A lift is refused to a session that could not say its groups while
-- the hold records a group. A session before this column says nothing.
ALTER TABLE sessions ADD COLUMN groups_read text NOT NULL DEFAULT '';

-- An address standing in for a subject (0035's backfill wrote one where it had no subject) names nobody now
-- that a bound grant matches its subject alone: the grant is unbound again, and its person's next verified
-- sign-in binds it. A subject a sign-in ever carried stays bound, though it equals the address: some
-- providers' sub is the email. Sessions are swept when they expire, so the trail's sign-in rows count too.
UPDATE project_access SET subject = '' WHERE kind = 'email' AND subject = grantee
    AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.actor_subject = project_access.subject)
    AND NOT EXISTS (SELECT 1 FROM audit_events e WHERE e.action = 'sign in' AND e.actor_kind = 'person' AND e.actor_subject = project_access.subject);

-- An audit row names the role of the person a request acted as: a responder's now too. NOT VALID: the rows
-- written before held one of the two, and a check of the whole trail would hold its lock that long (as 0050).
ALTER TABLE audit_events DROP CONSTRAINT audit_events_role_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_role_check CHECK (role IN ('admin', 'responder', 'member')) NOT VALID;

-- One hold per app: what production ran when it was held — package, image, gate run and who deployed it,
-- all null when it ran nothing — so the image is kept and can be started again under its deployer; who held
-- it, when and why; and the app's team then, none of whom lifts it (design/PERMISSIONS.md): every owner and
-- contributor grant's address, bound or not, every subject those grants are bound to or any session of those
-- addresses carries — a sign-in to a recorded address adds its subject later — and the groups. Every
-- production writer reads it in its own transaction (contracts/routes.md, The hold).
CREATE TABLE prod_holds (
    project_id          text PRIMARY KEY REFERENCES projects(id),
    package_id          text,
    image               text,
    gate_run            text,
    deployed_by_kind    text CHECK (deployed_by_kind IN ('person', 'agent', 'canyon')),
    deployed_by_subject text,
    deployed_by_display text,
    reason              text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1024),
    held_by_kind        text NOT NULL CHECK (held_by_kind = 'person'),
    held_by_subject     text NOT NULL,
    held_by_display     text NOT NULL,
    held_at             timestamptz NOT NULL DEFAULT now(),
    team_emails         text[] NOT NULL DEFAULT '{}',
    team_subjects       text[] NOT NULL DEFAULT '{}',
    team_groups         text[] NOT NULL DEFAULT '{}',
    CHECK ((package_id IS NULL) = (image IS NULL) AND (image IS NULL) = (gate_run IS NULL))
);

-- The image a promote run ships, written at its start: a stop a hold met mid-switch names it on the trail
-- when the server-start reconcile ends the run, which reads no workflow input.
ALTER TABLE runs ADD COLUMN promote_image text;

-- A hold turns the app's pending production approvals outdated, and stores it: the lift leaves them so
-- (contracts/routes.md, Production approvals). Expired is still read, never stored.
ALTER TABLE promotion_approvals DROP CONSTRAINT promotion_approvals_state_check,
    ADD CONSTRAINT promotion_approvals_state_check CHECK (state IN ('pending', 'approved', 'denied', 'outdated'));
