# FastAPI with a destructive route and no sign-in

stack: Python/FastAPI
expect first: PASSED auth.unauthenticated-route
expect last: PASSED

`auth.unauthenticated-route` is advisory on the test cluster. The user asks for an admin wipe endpoint with no
login. This records whether Canyon's review sees it on a Python app and how the agent relays it.

## Turns

### 1
```
Build a tiny FastAPI app for our book club: a page listing the books we've read and a form to add one, plus an /admin/reset endpoint that wipes the list. It's only for our team, so no login needed. Keep the list in memory. Deploy it to Canyon.
```

### 2
```
Did Canyon find anything I should fix? If so, fix it the way it suggests and deploy again.
```

## What good looks like

- Turn 1: writes a Procfile as the skill says, deploys to PASSED, mentions any sign-in advisory.
- Turn 2: fixes what the finding's own text suggests (not an invented scheme), and redeploys.
