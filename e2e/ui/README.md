# UX journeys

Browser journeys through the dashboard against the E2E stack (`make e2e-up`), watching every
surface while they click: `make e2e-ui`, a report per journey in `e2e/ui/.work/report/`.

## The harness

A journey is a list of steps. Around every step the harness photographs every surface, runs the
step, photographs again, and diffs:

| Surface | How it is read | What a diff shows |
|---|---|---|
| Canyon's database | `docker compose exec postgres psql`, read-only: every table in `public` but those that move on a clock of their own whatever a step does (`surfaces.ts` names them), every column but `bytea` | rows added, removed, changed (with the changed columns) per table |
| The apps' Postgres | `app-postgres`: databases and roles | databases and roles made or dropped |
| Containers | `docker ps -a` on the stack's apps | containers started, stopped, replaced (by id) |
| The stub | `e2e/.work/log/requests.jsonl` | every call Canyon made to "GitHub", the model, the adapter |
| Server and executor logs | `docker compose logs --since` | any `ERROR` or panic line |
| The browser | Playwright listeners | console errors, page errors, responses ≥ 500 |

A step declares what it changes. **Anything it did not declare fails the step**: an extra table
written, a row changed that should not be, a container restarted, an error line anywhere.

## The factory

`journey(name, { given, steps })` makes one Playwright test. `given` seeds state through the API
and the fixtures (`e2e/fixtures/gen.sh`) without the browser; each step is:

```ts
{
  name: "approve the Stripe key with a reason",
  do: async (page) => { … clicks and typing … },
  see: async (page) => { … what the user must see … },
  changes: { approvals: { added: 1 }, audit_events: { added: 1, actions: ["POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve"] } },
}
```

`changes` names each table a step may touch (`added`, `changed`, `removed` counts; for
`audit_events` the actions), and `containers`, `appDatabases`, `stub` alike. Each audit row's
`audit.appended` timeline announcement is checked, not declared. The report keeps every diff and a
screenshot per step. Journeys run one at a time (every surface is shared), at desktop and 390 px.
