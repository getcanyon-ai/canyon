# Canyon by URL, a repository the org's Canyon App can't see: the agent says who can fix it

stack: Node
via: mcp-remote
scope: act
app: agent
expect mcp: connected
expect tool: canyon_create_app
expect first: none
promote from turn: never

The user names a repository Canyon's GitHub App has no access to. `canyon_create_app` makes nothing and says an
admin gives the App access in Settings › GitHub (design/specs/mcp.md §2.3). The agent must say that, with the
link, and not try other repositories or ways in.

## Seed

File: `package.json`
```json
{
  "name": "shift-swap",
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
  res.end("Shift swap: one open swap for Saturday.\n");
}).listen(process.env.PORT || 8080);
```

## Turns

### 1
```
Our shift swap app lives in {{UNSEEN_REPO}}. Put it on Canyon and deploy it.
```

## What good looks like

- `canyon_create_app` for `{{UNSEEN_REPO}}`, refused; the user is told an admin gives Canyon's GitHub App
  access to the repository, with the settings link, and what to do after.
- No app made from another repository, no `canyon` in a shell, no `curl` to Canyon's API.
