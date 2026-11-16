-- App teams (design/PERMISSIONS.md; contracts/routes.md, App teams). Expand only: new tables, and new
-- columns defaulted or nullable, so the previous binary's reads and writes still work.
-- An app's grants: who × level × roles. grantee is the lower-cased address for email, the group name
-- for group, '' for everyone; subject is the identity-provider subject when the grant came from a
-- session (create, claim, this backfill), '' for someone who has never signed in.
CREATE TABLE project_access (
    project_id         text NOT NULL REFERENCES projects(id),
    kind               text NOT NULL CHECK (kind IN ('email', 'group', 'everyone')),
    grantee            text NOT NULL,
    level              text NOT NULL CHECK (level IN ('owner', 'contributor', 'viewer')),
    roles              text[] NOT NULL DEFAULT '{}',
    subject            text NOT NULL DEFAULT '',
    display            text NOT NULL DEFAULT '',
    granted_by_kind    text NOT NULL CHECK (granted_by_kind IN ('person', 'agent', 'canyon')),
    granted_by_subject text NOT NULL,
    granted_by_display text NOT NULL,
    granted_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, kind, grantee),
    CHECK ((kind = 'everyone') = (grantee = '')),
    CHECK (kind <> 'everyone' OR level = 'viewer')
);
-- One pending request per person per app.
CREATE TABLE access_requests (
    project_id   text NOT NULL REFERENCES projects(id),
    email        text NOT NULL,
    subject      text NOT NULL,
    display      text NOT NULL DEFAULT '',
    requested_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, email)
);
-- The app cookie Canyon's own proxy issues: its own token, never the Canyon session, gone with it.
CREATE TABLE app_sessions (
    id              text PRIMARY KEY,
    session_id      text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    project_id      text NOT NULL,
    code_hash       bytea UNIQUE,
    flow_hash       bytea,
    code_expires_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL
);
ALTER TABLE sessions ADD COLUMN groups text[] NOT NULL DEFAULT '{}';
ALTER TABLE org_preferences ADD COLUMN groups_claim text NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN roles jsonb, ADD COLUMN description text; -- declared in canyon.toml, as resources are (0021)

-- The owners so far become owner grants: each person on project_owners, then each person who deployed,
-- under an address — the subject when it is one, else the address the subject's newest session stored.
-- A subject resolving to neither has no address to hold a grant and is left out; one person named twice
-- is one grant, the owner row's. project_owners is read by nothing from here on.
INSERT INTO project_access (project_id, kind, grantee, level, subject, display, granted_by_kind, granted_by_subject, granted_by_display, granted_at)
SELECT DISTINCT ON (o.project_id, a.address) o.project_id, 'email', a.address, 'owner', o.subject, o.display, 'canyon', 'backfill', 'Canyon', o.at
FROM (SELECT project_id, subject, display, set_at AS at, 0 AS source FROM project_owners WHERE kind = 'person'
      UNION ALL
      SELECT project_id, deployed_by_subject, COALESCE(deployed_by_display, ''), deployed_at, 1 FROM deploys WHERE deployed_by_kind = 'person') o
CROSS JOIN LATERAL (SELECT lower(CASE WHEN o.subject ~ '^[^@\s]+@[^@\s]+$' THEN o.subject
    ELSE (SELECT s.email FROM sessions s WHERE s.actor_kind = 'person' AND s.actor_subject = o.subject AND s.email <> '' ORDER BY s.created_at DESC LIMIT 1) END) AS address) a
WHERE a.address IS NOT NULL
ORDER BY o.project_id, a.address, o.source, o.at;
