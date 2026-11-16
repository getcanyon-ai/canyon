# claims-portal — the fixture app

Meridian's claims portal, as Dana's coding agent shipped it: a Next.js 15 app that builds, runs,
and looks fine. It is the app the demo deploys, and it carries the canonical findings from
`design/EXPERIENCE.md` D4 at exactly the files, lines and columns the seam-1 golden log names
(`contracts/testdata/finding/claims-portal.sarif.json`). `contracts/validate.py` checks that every
golden-log location exists here and reads as its snippet says, so the two cannot drift.

| finding | where | what the agent did |
|---|---|---|
| `secrets.client-bundle` | `apps/web/src/lib/payments.ts:14` | constructed Stripe with a live-shaped key in a module `CheckoutPanel.tsx` (a client component) imports |
| `auth.unauthenticated-route` | `apps/web/src/app/api/admin/policies/route.ts:8`, related `apps/web/src/middleware.ts:22` | the handler has no session check and the middleware matcher excludes `/api/*` |
| `auth.unauthenticated-route` | `apps/web/src/app/api/session/route.ts:8`, related `apps/web/src/middleware.ts:22` | a demo sign-in: any email and password set the session cookie |
| `perf.unbounded-refetch` | `apps/web/src/components/ClaimsTable.tsx:61` | refetches every claim on each keystroke |

**The key is not a real key.** It has the shape of one so the gate can find it; the repo's gitleaks
config allowlists this one file for that reason. Everything else is ordinary application code.

Run it: `cd apps/web && pnpm install && pnpm dev`.
