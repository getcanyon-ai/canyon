---
slug: rules/code.injection
title: "No user input reaching commands, files, pages or URLs"
description: "User input that reaches a shell command, a file path, a page, a URL or code, found by Opengrep and ESLint."
---

`code.injection` · Advisory by default · found by Opengrep and ESLint

## What it finds

A value the user controls that reaches somewhere it can change what the app does. Two tools report
into this rule:

- **Opengrep**, with GitLab's sast-rules, open-source rules that ship with Canyon: every result
  tagged as command or code injection, path traversal, cross-site scripting, open redirect,
  server-side request forgery, XML external entities or unsafe deserialization (CWE-78, 77, 94, 95,
  22, 23, 79, 601, 918, 611 and 502).
- **ESLint**, with four rules set in Canyon's own ESLint config: `security/detect-child-process`,
  `security/detect-non-literal-fs-filename`, `no-eval` and `no-new-func`. ESLint reads JavaScript
  and TypeScript files outside `node_modules`.

For example, a page that renders a query parameter as raw HTML, which Opengrep reports
as cross-site scripting:

```tsx wrap
export default function Page({ searchParams }: { searchParams: { note: string } }) {
  return <div dangerouslySetInnerHTML={{ __html: searchParams.note }} />;
}
```

And a function ESLint reports on four lines, one per rule:

```ts
export async function run(name: string, code: string) {
  exec("convert " + name);
  await readFile("/data/" + name);
  eval(code);
  return new Function(code);
}
```

Each file in a language none of these rules read, such as PHP or Ruby, goes to the AI review's
version of this rule, [`ai.code-injection`](/rules/ai.code-injection/), while that rule is on, your
Canyon has an AI model, and the AI review can read the file as text. Otherwise the review lists those
files as not checked under this rule.

## Why it matters

Whoever controls the input can run a command, read a file, put script in another user's page, or
send the app's requests somewhere else.

## How to fix it

This is the fix Canyon prints:

> Keep the user's input data: pass it as an argument or a parameter, never as part of a command,
> path, page, URL or code, and check it against the values allowed.

## Check the fix

`canyon check` runs the same review `canyon deploy` runs, without deploying.

## If you think it's wrong

Canyon doesn't read `nosemgrep` comments, a `.semgrepignore` file, or `eslint-disable` comments, so
none of them silences it.

If the code is right as it is, [have the finding approved](/build/fix-or-approve/#approve-it-instead).
An admin can set the rule to Off or Blocking under Guardrails → Policies. Set to Blocking, it stops a deploy for its high
and critical findings.

Each tool runs only while its own rule, [Findings from opengrep](/rules/sarif.opengrep/)
(`sarif.opengrep`) or [Findings from eslint](/rules/sarif.eslint/) (`sarif.eslint`), is Advisory or
Blocking, and Canyon runs it on your Canyon.
