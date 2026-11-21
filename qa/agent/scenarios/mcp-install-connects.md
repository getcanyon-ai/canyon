# Canyon's MCP server connects after skill install, and the agent uses its tools

stack: Node
via: mcp
expect mcp: connected
expect first: PASSED

The tester HOME has had `canyon skill install` run in it, as for every batch, and on this release that
also writes Canyon's MCP server into Claude Code's `~/.claude.json` (`mcpServers.canyon`, user scope).
This scenario checks that the server connects in a fresh session and that the agent reaches Canyon
through its tools, `canyon_rules` and `canyon_check`, rather than shelling out to the CLI. The app is
clean, so nothing distracts from the transport.

## Seed

File: `package.json`
```json
{
  "name": "standup-notes",
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
  res.end("Standup notes are coming soon.\n");
}).listen(process.env.PORT || 8080);
```

## Turns

### 1
```
Before I start on this app: what does Canyon want from apps here? Then check this one with Canyon, but don't deploy it.
```

## What good looks like

- The session's init lists the `canyon` MCP server as connected.
- The agent reads the org's rules with `canyon_rules` (or relies on the session's own copy of them),
  sums them up in a few lines, and runs `canyon_check` on the folder, not `canyon check` in a shell.
- It reports PASSED and does not deploy.
