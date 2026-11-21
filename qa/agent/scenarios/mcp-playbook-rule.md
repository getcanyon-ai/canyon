# A custom rule points at a playbook: the agent reads it with canyon_playbook

stack: Node
via: mcp
expect mcp: connected
playbook: service-conventions
expect first: BLOCKED custom.customer-email-encrypted
expect last: PASSED

The org has docs/org/playbooks.md's worked example in place, which an admin sets up once in the
dashboard before the batch (the runner can't: a playbook and a custom rule are an admin's, from a
browser):

- the playbook **How Meridian writes a service**, slug `service-conventions`, kind skill, its `when`
  "Read before you add a route or a query to a Meridian service";
- the custom rule **Customer emails are encrypted at rest** (`custom.customer-email-encrypted`),
  Blocking, its **Playbook** set to `service-conventions`.

The runner reads the playbook as the tester before turn 1 and stops the scenario when it isn't there.
The user's app stores a customer's email as it came in. The finding's last step names the playbook,
through the tools as `call canyon_playbook with slug service-conventions`. The scorecard records
whether the agent read it, and whether the session hook synced it as a native skill
(`~/.claude/skills/canyon-service-conventions/SKILL.md`).

## Seed

File: `package.json`
```json
{
  "name": "newsletter-signup",
  "version": "1.0.0",
  "private": true,
  "scripts": { "start": "node server.js" },
  "engines": { "node": "22.x" }
}
```

File: `server.js`
```js
const http = require("http");
const fs = require("fs");

// Sign-ups go to a file the marketing team imports each Monday.
http.createServer((req, res) => {
  if (req.method === "POST" && req.url === "/signup") {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      const { email, name } = JSON.parse(body || "{}");
      fs.appendFileSync("/tmp/signups.jsonl", JSON.stringify({ email, name, at: Date.now() }) + "\n");
      res.writeHead(201);
      res.end("thanks\n");
    });
    return;
  }
  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end("Sign up for the Meridian newsletter: POST /signup\n");
}).listen(process.env.PORT || 8080);
```

## Turns

### 1
```
Ship the newsletter signup to Canyon please.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, fix it the way the platform team wants it done, then deploy.
```

## What good looks like

- Turn 1: deploys, is blocked on `custom.customer-email-encrypted`, and tells the user.
- Turn 2: reads the playbook with `canyon_playbook` (or opens the synced skill) before changing code,
  encrypts the address as the finding and the playbook say, checks, and deploys to PASSED.
- The synced skill exists after the session; the agent never edits it.
