# Canyon's docs site

Built with [Starlight](https://starlight.astro.build) into static files, and public at
https://www.getcanyon.ai/docs/. CI deploys nothing: each pull request that changes the docs builds the
site and attaches it to its run as the `docs-site` artifact.

```sh
cd docs
npm ci
npm run dev      # http://localhost:4321, reloads on save
npm run build    # dist/: the site, llms.txt, llms-full.txt, llms-small.txt, and every page again as .md
```

Set `CANYON_DOCS_URL` to the address the site will be served at before a build you deploy:
`llms.txt` and the sitemap use it for their links, and without it they point at
`http://localhost:4321`. A build on Vercel, the one that ships, stops without it.

Every page's header names the Canyon release the site documents, once for the whole site:
`Docs for Canyon v0.6.0` when `CANYON_DOCS_VERSION` is set to that release's tag, and
`Docs for Canyon main (unreleased)` when it isn't, as in a preview or a local build. Nobody sets it
by hand: `scripts/publish-docs.sh` takes it from the release tag (below). A value that isn't a
version tag stops the build, and so does a build on Vercel without one.

The published site is served under a path, `/docs`, and `CANYON_DOCS_BASE=/docs` builds it there.
Pages here link from the root (`/build/secrets/`); Starlight leaves a page's links as written, and the
`.md` copies and `llms-full.txt` carry them verbatim, so `node scripts/base-links.mjs <copy> /docs`
puts the base in front of every root-relative link in a copy of the pages, never inside code.
`scripts/publish-docs.sh` does both. Under the base, the link checker fails on a link without it.

## Publishing

At each release, from a clone of Canyon at the release:

```sh
scripts/publish-docs.sh <workspace>   # a checkout of the pnpm workspace that serves the site, on a new branch; needs pnpm
```

It writes the site into the workspace's `apps/canyon-docs`, whose Vercel project builds that directory,
served at https://www.getcanyon.ai/docs/: the workspace's website rewrites `/docs` to it. Run `npm ci`
here first. It refuses to run unless the clone's HEAD carries the release's
version tag, such as `v0.6.0` or `v0.7.0-rc.1` (`git describe --tags --exact-match`), and that tag is the
version the published site names. It replaces `apps/canyon-docs` with a copy of this directory (the fonts
and the published contracts copied in, this README, `scripts/screens.mjs` and `package-lock.json` left
out) whose links `scripts/base-links.mjs` gives the `/docs` base, writes the app's `vercel.json`, whose
build command sets `CANYON_DOCS_VERSION` to the tag and `CANYON_DOCS_BASE` to `/docs` and which
redirects the old address, docs.getcanyon.ai, to the new one, and `agent.md`, and refreshes the workspace's `pnpm-lock.yaml`. Open a pull request on the workspace's
repository with the change. Nobody edits `apps/canyon-docs` by hand: the next publish overwrites it.

## Writing a page

- Pages are Markdown in `src/content/docs/`. The sidebar is in `astro.config.mjs`.
- Every command and every output shown is one somebody ran, pasted from the run.
  `start/take-the-tour.md` is held to it: `make tour-check` runs the tour on CI and fails on a pasted
  line the run didn't print, so take its blocks from that job's log.
- A diagram is a fenced `mermaid` block with a `title="…"`, its caption and its text for screen
  readers. The build draws it as an SVG in Canyon's colours, and GitHub draws the same block when
  you read the `.md` in the repository. Four classes give a box meaning: `canyon`, `blocked`,
  `person` and `passed`. Use them for nothing else.
- The build fails on a link to a page or heading that doesn't exist (starlight-links-validator), and
  `make docs-links` checks the repository's other Markdown.

## Generated pages

`src/content/docs/reference/cli.md` is the CLI's own `--help`, written by `make docs-cli`.
`go test ./cli` fails while it differs from what the CLI prints, so a change to a command's help
updates the page in the same pull request.

Two contracts are pages too, `finding.md` and `provisioner.md`: `contracts/` here links to them and
to their two JSON schemas, `finding.schema.json` and `release.schema.json`, and `npm run dev` and
`npm run build` first run `scripts/contracts.mjs`, which
writes each linked contract to `src/content/docs/reference/contracts/` with its heading as the title
and copies the schemas to `public/contracts/schema/`. Neither output is committed. Publishing another
contract is a link in `contracts/` and a link in `reference/contracts.md`'s table.

## Public means anonymous

The site is public, so nothing in it points back at the people, customers or internals behind
Canyon, and a reader never meets a reference to something they can't open: no real person's or
customer's name, no issue or pull request number, no internal host, no dated decision notes, and no
source, test or fixture path, `make` target, design document or unpublished contract. Where such a
reference carried meaning, the meaning stays in prose. A published contract or schema follows the
same rule: what was only for us, such as its fixtures and the tests that pin it, moves to
`contracts/README.md`. `npm run build` ends with `scripts/public-check.mjs`, which fails on
the patterns it lists anywhere in `dist/`, the `.md` copies and `llms*.txt` included. The names it
refuses come from `CANYON_PUBLIC_CHECK_NAMES`, a secret set in CI and in the site's build, never
in this repository: unset there, the check fails; unset on your machine or in a pull request from a fork,
which GitHub hands no secrets, it skips the names and says so, and the published site's build checks them.

## Screenshots

The admin guides show the dashboard: `public/screens/*-light.webp` and `*-dark.webp`, one of each
picked by the site's theme. They come from the laptop demo
(`docker compose --profile demo-apps up --build`), taken by `make docs-screens`
(`scripts/screens.mjs`). Retake them in the same pull request as a change to Fleet, an app's page,
Guardrails › Policies or the audit trail.

## The theme

`src/styles/tokens.css` is generated from the dashboard's own tokens
(`designsystem/canyon/dashboard.tokens.json`) by `make tokens`, and `make tokens-check` fails on a
hand edit. `src/styles/canyon.css` points Starlight's variables at them. The fonts are the
dashboard's: `src/fonts` links to `web/src/fonts`. `src/styles/brand.css` then gives the site the website's look, and the
header is the website's too: `src/components/SiteTitle.astro` (the Canyon mark and name, which lead back to
getcanyon.ai) and `src/components/SocialIcons.astro` (the button that copies the setup prompt).

## The repository's address

The repository is `github.com/getcanyon-ai/canyon`. Downloads are
`github.com/getcanyon-ai/canyon-releases` (`releases` in `astro.config.mjs`, the install one-liners,
`cli/help.go`, `cli/install.sh`). The site is public, and doesn't name the repository:
a reader installs from the downloads and reads the published contracts as pages. Outside the site, these files name it:

- `README.md`, `SECURITY.md`, `CONTRIBUTING.md`
- `.github/ISSUE_TEMPLATE/config.yml`, `.github/old-cli.sh`
- `designsystem/canyon/registry.json`
- `scripts/publish-release.sh`
- `go.mod`, as the Go module path, and every import with it

## On launch day

The README, CONTRIBUTING.md, SECURITY.md and the issue chooser link two GitHub
features that a private repository doesn't have yet. Turn both on when the repository goes public:
**Discussions**, and **private vulnerability reporting** (Settings › Code security).

Also on launch day: turn on content reports to the repository's admins (Settings › Moderation
options → Reported content), where CODE_OF_CONDUCT.md sends conduct reports, and give
CODE_OF_CONDUCT.md a conduct address on the project's own domain (such as `conduct@<domain>`)
beside them, once the domain is chosen.
