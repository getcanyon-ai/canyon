# A custom rule points at a playbook: the agent reads it with canyon_playbook

stack: Node
via: mcp
expect mcp: connected
playbook: claims-portal-conventions
policy: custom.claims-conventions blocking
expect first: BLOCKED custom.claims-conventions
expect last: PASSED

The org needs two things an admin writes in the dashboard, which the runner can't: a playbook and a
custom rule that points at it. On the test cluster they are seeded once before the batch:

- the playbook **Claims portal conventions**, slug `claims-portal-conventions`, kind skill, its text
  `demo/apps/claims-portal-conventions.json`'s: each route lives in `server.js`, and every query
  passes the request's values as bound parameters;
- the custom rule **Claims apps keep their routes in server.js** (`custom.claims-conventions`),
  checked by code (`'server.js' in files`), Off on the org, with its **Playbook** set to
  `claims-portal-conventions`. The `policy:` line makes it Blocking for this app alone, as its owner
  may, so no other app on the cluster sees it.

The runner reads the playbook as the tester before turn 1 and stops the scenario when it isn't there.
The user's app keeps its routes in `index.js`, so the rule blocks, and the finding's last step names the
playbook: through the tools, `call canyon_playbook with slug claims-portal-conventions`. The scorecard
records whether the agent read it, and whether the session hook synced it as a native skill
(`~/.claude/skills/canyon-claims-portal-conventions/SKILL.md`).

## Seed

File: `package.json`
```json
{
  "name": "claims-notes",
  "version": "1.0.0",
  "private": true,
  "scripts": { "start": "node index.js" },
  "engines": { "node": "22.x" }
}
```

File: `index.js`
```js
const http = require("http");

// Adjusters' notes on a claim, kept in memory until the claims database is wired in.
const notes = { "MRD-1001": ["Photos received", "Garage quote due Friday"] };

http.createServer((req, res) => {
  const url = new URL(req.url, "http://localhost");
  if (url.pathname === "/notes") {
    res.writeHead(200, { "Content-Type": "application/json" });
    return res.end(JSON.stringify(notes[url.searchParams.get("policy")] || []));
  }
  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end("Claim notes: GET /notes?policy=MRD-1001\n");
}).listen(process.env.PORT || 8080);
```

## Turns

### 1
```
Ship the claim notes app to Canyon please.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

## What good looks like

- Turn 1: deploys, is blocked on `custom.claims-conventions`, and tells the user.
- Turn 2: from Canyon's own steps alone, reads the playbook with `canyon_playbook` (or opens the synced
  skill) before changing code, moves the routes into `server.js` and the start script with them, checks,
  and deploys to PASSED.
- The synced skill exists after the session; the agent never edits it.
