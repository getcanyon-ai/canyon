-- The Canyon Enterprise licence an admin pasted in Settings → Licence (#2879; contracts/routes.md, Licence): one
-- row, the licence file's text exactly as issued, verified against the server's compiled keys at each use.
-- CANYON_LICENSE wins over it. Not sealed: a licence is signed, not secret.
CREATE TABLE licence (
    one boolean PRIMARY KEY DEFAULT true CHECK (one),
    body text NOT NULL
);
