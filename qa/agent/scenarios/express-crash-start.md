# An Express app that crashes on start

stack: Node/Express
expect first: FAILED start
expect last: PASSED

The user's kudos board requires a module its package.json doesn't list, so it builds and then
dies on start. Nothing is a policy problem; the agent has to read the app's own last lines.

## Seed

File: `package.json`
```json
{
  "name": "kudos-board",
  "version": "1.0.0",
  "private": true,
  "scripts": { "start": "node server.js" },
  "engines": { "node": "22.x" },
  "dependencies": { "express": "^4.21.2" }
}
```

File: `server.js`
```js
const express = require("express");
const morgan = require("morgan");

const app = express();
const kudos = [];
app.use(morgan("tiny"));
app.use(express.urlencoded({ extended: false }));

app.get("/", (req, res) => {
  res.send(`<h1>Kudos</h1><form method="post"><input name="text"><button>Send</button></form>` +
    kudos.map((k) => `<p>${k.replace(/[<>&]/g, "")}</p>`).join(""));
});

app.post("/", (req, res) => {
  kudos.push(String(req.body.text || ""));
  res.redirect("/");
});

app.listen(process.env.PORT || 8080);
```

## Turns

### 1
```
This is our team's kudos board. Deploy it to Canyon as it is.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

## What good looks like

- Turn 1: reports that it built but did not stay up, quoting the app's own error line.
- Turn 2: adds the missing dependency from the LAST LINES alone and redeploys to PASSED; does not
  reach for `canyon secret set`, since nothing is missing from the environment.
