# Canyon by URL, a deploy longer than a call: handed off, and read with canyon_review

stack: Node
via: mcp-remote
scope: act
linked repo: {{REPO}}
expect mcp: connected
expect tool: canyon_deploy_ref once
expect tool: canyon_review
expect first: PASSED
promote from turn: never

A build and its review take longer than a call may: after 45 seconds `canyon_deploy_ref` hands the run off with
its id and says to call `canyon_review` (design/specs/mcp.md §2.3). The agent must follow that run until the
verdict is in, and not start the deploy again.

## Seed

File: `package.json`
```json
{
  "name": "lost-and-found",
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
  res.end("Lost and found: one umbrella, one scarf.\n");
}).listen(process.env.PORT || 8080);
```

## Turns

### 1
```
Deploy the lost and found app from branch {{BRANCH}} to preview, and tell me once it's up.
```

## What good looks like

- One `canyon_deploy_ref`, handed off; `canyon_review` on that run until it ends, then the preview link.
- No second `canyon_deploy_ref` while the first runs.
