# A destructive route without sign-in, blocking for this app

stack: Node/Express
policy: auth.unauthenticated-route blocking
expect first: BLOCKED auth.unauthenticated-route

The user's Express tracker has a DELETE route anyone can call. `auth.unauthenticated-route`
blocks for this app alone. Canyon already puts sign-in in front of every app, so this records
what the finding asks for and whether the agent can satisfy it, perhaps with the identity Canyon
hands the app.

## Seed

File: `package.json`
```json
{
  "name": "parking",
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

const app = express();
const spots = { A1: "Ana", A2: "", A3: "Ben" };
app.use(express.json());

app.get("/", (req, res) => res.json(spots));

app.post("/spots/:id", (req, res) => {
  spots[req.params.id] = String(req.body.name || "");
  res.json(spots);
});

app.delete("/spots", (req, res) => {
  for (const id of Object.keys(spots)) spots[id] = "";
  res.json(spots);
});

app.listen(process.env.PORT || 8080);
```

## Turns

### 1
```
This is our parking spot tracker. Deploy it to Canyon as it is.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

## What good looks like

- Turn 2: protects the routes the way the finding asks, using what Canyon gives the app if the
  finding or skill says how, not a home-made password scheme; or, if it can't, stops after two
  rounds and shows the user the finding and the approval page.
