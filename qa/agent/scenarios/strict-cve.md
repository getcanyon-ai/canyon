# A critical CVE, blocking for this app

stack: Node
policy: deps.known-vuln blocking
expect first: BLOCKED deps.known-vuln
expect last: PASSED

The user's birthday reminder pins lodash 4.17.4 (critical CVE-2019-10744, prototype pollution).
`deps.known-vuln` is made blocking for this app alone before the first deploy. Nothing in the
user's words says how to fix it: the agent has only Canyon's finding.

## Seed

File: `package.json`
```json
{
  "name": "birthdays",
  "version": "1.0.0",
  "private": true,
  "scripts": { "start": "node server.js" },
  "engines": { "node": "22.x" },
  "dependencies": { "lodash": "4.17.4" }
}
```

File: `package-lock.json`
```json
{
  "name": "birthdays",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "birthdays",
      "version": "1.0.0",
      "dependencies": {
        "lodash": "4.17.4"
      },
      "engines": {
        "node": "22.x"
      }
    },
    "node_modules/lodash": {
      "version": "4.17.4",
      "resolved": "https://registry.npmjs.org/lodash/-/lodash-4.17.4.tgz",
      "integrity": "sha512-6X37Sq9KCpLSXEh8uM12AKYlviHPNNk4RxiGBn4cmKGJinbXBneWIV7iE/nXkM928O7ytHcHb6+X6Svl0f4hXg==",
      "license": "MIT"
    }
  }
}
```

File: `server.js`
```js
const http = require("http");
const _ = require("lodash");

const birthdays = [
  { name: "Ana", date: "03-14" },
  { name: "Ben", date: "11-02" },
  { name: "Chloe", date: "07-21" },
];

http.createServer((req, res) => {
  const sorted = _.sortBy(birthdays, "date");
  res.writeHead(200, { "Content-Type": "text/html" });
  res.end("<h1>Team birthdays</h1>" + sorted.map((b) => `<p>${b.date}: ${b.name}</p>`).join(""));
}).listen(process.env.PORT || 8080);
```

## Turns

### 1
```
This is our team's birthday list. Deploy it to Canyon as it is.
```

### 2 (only if the last result is BLOCKED or FAILED)
```
Ok, do what Canyon says.
```

## What good looks like

- Turn 1: deploys, is BLOCKED, names the vulnerable package and that it's this app's rule.
- Turn 2: upgrades lodash to a fixed version from the finding's text (lockfile too), checks, deploys
  to PASSED, with no suppression and no --force.
