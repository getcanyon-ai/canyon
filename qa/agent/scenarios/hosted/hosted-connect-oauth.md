# Canyon by URL: the agent connects over OAuth and works with no CLI

stack: Node
via: mcp-remote
scope: act
expect mcp: connected
expect first: none

The agent's only Canyon is `https://<canyon>/mcp`, allowed once by a person in the browser. There is no
`canyon` on the machine. It must read the org's rules and the app's status through the remote tools, and
say plainly that deploying a local folder needs the CLI or a linked repository (§10).

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

- The session init lists canyon connected over HTTP.
- The agent calls `canyon_rules`, sums the rules up, and explains the ways to deploy: link the app to a
  repository and use `canyon_deploy_ref`, or install the CLI. It doesn't try to run `canyon`, install it
  unasked, or call Canyon's API with curl.
