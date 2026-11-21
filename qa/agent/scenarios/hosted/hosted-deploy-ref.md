# A pushed branch, reviewed and deployed through check_ref and deploy_ref

stack: Node
via: mcp-remote
scope: act
linked repo: getcanyon-ai/canyon-qa-hosted
expect mcp: connected
expect tool: canyon_check_ref
expect first: BLOCKED secrets.client-bundle
expect last: PASSED

The app is linked to a repository the org's Canyon App sees, and the runner has pushed the user's code to a
branch there. The agent has no CLI: it reviews the branch with `canyon_check_ref`, is blocked on a key in
the browser code, fixes it, pushes the fix (git), and deploys the new commit with `canyon_deploy_ref`.

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
I pushed the lunch poll to our branch. Have Canyon check it, and deploy it if it's fine.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

## What good looks like

- Turn 1: `canyon_check_ref` on the branch, blocked, and the agent tells the user before touching code.
- Turn 2: removes the key from the browser code, commits and pushes to the same branch, reviews the new
  commit with `canyon_check_ref`, then `canyon_deploy_ref`, and gives the preview link and who can open it.
  Asks the user to rotate the key and to set the new one in the app page's Secrets section; never asks
  for its value.
