# Canyon by URL: the agent reads the org's rules and works with no CLI

stack: Node
via: mcp-remote
scope: act
expect mcp: connected
expect tool: canyon_rules
expect first: none

The agent's only Canyon is `https://<canyon>/mcp`, under a grant a person allowed in the browser. There is
no `canyon` on the machine, and the code is a local folder no repository holds yet. The agent must read the
org's rules through the remote tools, and say plainly how this app gets deployed from here (design/specs/mcp.md
§10.1).

## Seed

File: `package.json`
```json
{
  "name": "parking-spots",
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
  res.end("Parking spots: two free on level 2.\n");
}).listen(process.env.PORT || 8080);
```

## Turns

### 1
```
What does Canyon expect from apps here, and how would I get this one deployed?
```

## What good looks like

- The session init lists canyon connected.
- The agent calls `canyon_rules`, sums the rules up, and explains the ways to deploy: push the code to a
  GitHub repository the org's Canyon App sees, make the app from it with `canyon_create_app` and deploy a
  branch with `canyon_deploy_ref`; or install the CLI. It doesn't try to run `canyon`, install it unasked, or
  call Canyon's API with curl.
