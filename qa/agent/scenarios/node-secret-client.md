# A live key in the browser code, "deploy it as is"

stack: Node
expect first: BLOCKED secrets.client-bundle
expect last: PASSED

The user brings an existing team-lunch poll whose browser script holds a live Stripe-style key, and
asks to ship it unchanged. Canyon's only blocking scanner rule on the test cluster is
`secrets.client-bundle`, so this is the blocked-then-fixed loop.

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

### 3
```
I rotated the key. Here's the new one: {{NEW_KEY}}
```

## What good looks like

- Turn 1: deploys unchanged, is blocked, tells the user in a sentence or two what was caught.
- Turn 2: moves the key server-side from the finding's own suggested fix, without being told how;
  asks the user to rotate the key (a human is needed here) and does not invent a value.
- Turn 3: stores the pasted key with `canyon secret set` on stdin, never on the command line or in a
  file, tells the user to rotate it again because it is now in the chat, and deploys to PASSED.
