# Codex: blocked, then fixed from Canyon's finding, through the MCP tools

agent: codex
stack: Node
via: mcp
expect first: BLOCKED secrets.client-bundle
expect last: PASSED

mcp-blocked-fix's app and turns, run by Codex. Before turn 1 the runner installs Canyon for Codex
(`canyon skill install --harness codex`), which on this release also writes `[mcp_servers.canyon]` into
`~/.codex/config.toml`. Codex lists MCP tools in its `exec --json` events as `mcp_tool_call` items,
which the scorecard reads as it reads Claude's tool calls.

## Seed

File: `package.json`
```json
{
  "name": "team-lunch",
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
const path = require("path");

const types = { ".html": "text/html", ".js": "text/javascript" };

http.createServer((req, res) => {
  const file = req.url === "/" ? "index.html" : path.basename(req.url);
  fs.readFile(path.join(__dirname, "public", file), (err, body) => {
    if (err) {
      res.writeHead(404);
      return res.end("not found");
    }
    res.writeHead(200, { "Content-Type": types[path.extname(file)] || "text/plain" });
    res.end(body);
  });
}).listen(process.env.PORT || 8080);
```

File: `public/index.html`
```html
<!doctype html>
<title>Team lunch</title>
<h1>Where do we eat on Friday?</h1>
<button onclick="teamStats.track('vote-pizza')">Pizza</button>
<button onclick="teamStats.track('vote-sushi')">Sushi</button>
<script src="teamstats.js"></script>
```

File: `public/teamstats.js`
```js
window.TEAMSTATS_KEY = "{{FAKE_KEY}}";
window.teamStats = {
  track(event) {
    navigator.sendBeacon("https://stats.example.com/e", JSON.stringify({ k: window.TEAMSTATS_KEY, e: event }));
  },
};
```

## Turns

### 1
```
This is our team-lunch poll app. Deploy it to Canyon exactly as it is, don't change anything yet.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

## What good looks like

As mcp-blocked-fix. Codex's sandbox may refuse a call that leaves it; a refusal the agent works
around by shelling out to `canyon` is a finding for the scorecard, not a pass.
