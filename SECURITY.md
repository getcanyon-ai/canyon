# Security policy

Canyon reviews other people's code for security problems, so a hole in Canyon itself matters twice.
Thank you for reporting one.

## Report a vulnerability

Don't open a public issue, discussion or pull request. Report it privately through GitHub's private
vulnerability reporting: on the repository's **Security** tab, choose **Report a vulnerability**
([direct link](https://github.com/getcanyon-ai/canyon/security/advisories/new)).

Tell us what you found, how to reproduce it, and what an attacker gets. A proof of concept against
your own install helps more than a scanner report.

We reply within two business days, agree a fix and a disclosure date with you, and credit you in the
advisory unless you'd rather not be named.

## What's in scope

Everything in this repository: the server and dashboard, the executor, the router, the identity
sidecar, the CLI and the skill it installs, the Helm chart, the Kubernetes manifests, and the compose
files, the team setup on one Docker server (`deploy/docker/team.yml`) among them.

These are deliberate and are not vulnerabilities:

- **The demo runs without sign-in.** `docker compose up` sets `CANYON_AUTH=demo` and says
  so on every screen. `CANYON_AUTH=off` refuses to bind beyond localhost, and both it and `demo`
  refuse to start on a `CANYON_PUBLIC_URL` other than localhost, and close a connection from another
  machine's own address before reading it; with `CANYON_PUBLISHED_ADDR` set, as the compose demo sets
  it, `demo` stops when Docker publishes its port beyond loopback. Not closed, and out of scope: a relay
  on the host that forwards other machines to the port with `Host: localhost` (nginx or Caddy rewriting
  `Host`, `ngrok --host-header=rewrite`, cloudflared's `httpHostHeader`, `ssh -g -L`, a mesh sidecar),
  and, on Docker Desktop, OrbStack and Colima, an app the demo runs calling the host's forward through
  `host.docker.internal`. The team setup on one Docker server
  (`deploy/docker/team.yml`) signs people in (`CANYON_AUTH=oidc`) and is in scope like any install.
- **The sample apps are insecure on purpose.** The apps under `demo/apps/` and the test apps under
  `gate/testdata/` and `executor/testdata/` hold planted secrets and vulnerabilities for the review to
  find.

## Supported versions

Fixes land on `main` and ship in the next release. Only the latest release gets security fixes.
