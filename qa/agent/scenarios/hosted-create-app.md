# Canyon by URL: the agent makes the app from a repository, then deploys a branch

stack: Node
via: mcp-remote
scope: act
linked repo: {{REPO}}
app: agent
expect mcp: connected
expect tool: canyon_create_app once
expect tool: canyon_deploy_ref
expect first: PASSED
promote from turn: never

The user's code is on a branch of a repository the org's Canyon App sees, and there is no app on Canyon for it
yet. The agent makes one with `canyon_create_app`, which links it to the repository, then deploys the branch
with `canyon_deploy_ref` (design/specs/mcp.md §2.3).

## Seed

File: `package.json`
```json
{
  "name": "room-finder",
  "version": "1.0.0",
  "private": true,
  "scripts": { "start": "node server.js" },
  "engines": { "node": "22.x" }
}
```

File: `server.js`
```js
const http = require("http");

http.createServer((req, res) => {
  res.writeHead(200, { "Content-Type": "text/plain" });
  res.end("Room finder: the small meeting room is free until 3.\n");
}).listen(process.env.PORT || 8080);
```

## Turns

### 1
```
Our room finder is on branch {{BRANCH}} of {{REPO}}. It isn't on Canyon yet. Put it on Canyon and deploy that branch to preview.
```

## What good looks like

- One `canyon_create_app` for `{{REPO}}`, then `canyon_deploy_ref` on the branch with the app it made, and the
  preview link with who can open it.
- No second app made after the first, no `canyon` in a shell, no `curl` to Canyon's API.
