-- The issuer each grant's subject is from (design/PERMISSIONS.md, Grants; #2306): a subject names a person only
-- under the issuer that issued it, so a grant bound under any issuer but CANYON_OIDC_ISSUER is read as not yet
-- bound. An access request records its subject's issuer too, which the share that grants it binds under.
-- Expand only: defaulted, so the previous binary's reads and writes still work. A subject with no issuer
-- recorded is the current issuer's, as that binary reads it; the server also records the issuer on it at its
-- start, before it serves (stampIssuer), as a migration cannot know CANYON_OIDC_ISSUER.
ALTER TABLE project_access ADD COLUMN subject_issuer text NOT NULL DEFAULT '';
ALTER TABLE access_requests ADD COLUMN subject_issuer text NOT NULL DEFAULT '';
