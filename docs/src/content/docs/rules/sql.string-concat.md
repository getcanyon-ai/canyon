---
slug: rules/sql.string-concat
title: "No hand-built SQL from user input"
description: "SQL whose text is built from a request's values, found by Opengrep."
---

`sql.string-concat` · Blocking by default · found by Opengrep, with GitLab's sast-rules and Canyon's own rule

## What it finds

A SQL statement whose text is built from something the request carries: a query string, a body, a
route parameter or form data. Canyon runs Opengrep with two sets of rules, and every result tagged
CWE-89 (SQL injection) lands here:

- **GitLab's sast-rules**, open-source rules that ship with Canyon. Their SQL injection rules cover
  Python, Go, Kotlin, Scala, C# and JavaScript.
- **Canyon's own rule**, for JavaScript and TypeScript. It follows a value from a Next.js route
  handler's request or params, a page's `searchParams` or `params`, a Pages Router API route's
  `req.query` or `req.body`, a server action's `FormData`, or an Express handler's `req.query`,
  `req.body` or `req.params`, into the text of a `query`, `execute`, `raw`, `unsafe`, `prepare`,
  `exec`, `$queryRawUnsafe` or `$executeRawUnsafe` call.

Each file in a language none of these rules read, such as PHP or Ruby, goes to the AI review's
version of this rule, [`ai.sql-string-concat`](/rules/ai.sql-string-concat/), while that rule is on,
your Canyon has an AI model, and the AI review can read the file as text. Otherwise the review lists
those files as not checked under this rule.

Canyon's demo app `claims-portal-next` builds its search like this, and the review blocks it:

```js wrap del="+ req.query.policy +"
app.get("/claims", (req, res) => {
  const rows = db.prepare("SELECT * FROM claims WHERE policy_number = '" + req.query.policy + "'").all();
  res.json(rows);
});
```

Canyon's own rule rates this critical. GitLab's rules for Python's and Go's SQL built by
concatenation rate it high. A Blocking rule stops a deploy for its high and critical findings.

## Why it matters

Whoever sends the request can rewrite the statement: read other rows, change them, or drop the table.

## How to fix it

This is the fix Canyon prints:

> Pass the user's input to the query as a bound parameter instead of building the SQL text from it.

An example, before:

```ts wrap del="'${q}'"
export async function GET(req: Request) {
  const q = new URL(req.url).searchParams.get("q");
  return Response.json(await pool.query({ text: `SELECT * FROM t WHERE q = '${q}'` }));
}
```

After:

```ts wrap ins="$1" ins="values: [q]"
export async function GET(req: Request) {
  const q = new URL(req.url).searchParams.get("q");
  return Response.json(await pool.query({ text: "SELECT * FROM t WHERE q = $1", values: [q] }));
}
```

A statement built with a tag that binds its substitutions, `` sql`...` `` or `` Prisma.sql`...` ``,
is not reported. Neither is a value passed through `Number()` or `parseInt()` first.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

Canyon doesn't read `nosemgrep` comments or a `.semgrepignore` file, so neither silences it.

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Advisory or Off under Guardrails → Policies.

Opengrep runs only while [Findings from opengrep](/rules/sarif.opengrep/) (`sarif.opengrep`) is
Advisory or Blocking, and Canyon runs it on your Canyon. Otherwise nothing checks this rule, and the
review says so.
