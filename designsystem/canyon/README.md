# Canyon's own design registry

Canyon dogfoods its design-system capability (ARCHITECTURE §3b): the platform ships
pre-registered with **stock shadcn** and **this** — Canyon's own tokens and components,
published in shadcn-registry format.

## The registry row this fulfils

```jsonc
// The row as it is seeded (db/migrations/0013_design_systems.sql, contracts/routes.md). The
// registry_url is a placeholder until the serving route exists; it moves in that route's migration.
{ "name": "Canyon", "namespace": "@canyon/ui", "source_type": "shipped",
  "registry_url": "https://canyon.example/registry/canyon" }
```

## Layout

| File | What it is |
|---|---|
| `tokens.json` | Canyon's own W3C DTCG tokens (hex-string draft, as implemented by Style Dictionary) |
| `themes/<name>/tokens.json` | A ported theme's tokens — each carries **its own vocabulary as its source defines it** (semantic for the ported five, upstream palette for Catppuccin); the item's cssVars are the uniform contract |
| `registry.json` | The authoring manifest (`https://ui.shadcn.com/schema/registry.json`) — the CLI never fetches it |
| `r/<item>.json` | The served registry items — what `npx shadcn add @canyon/<item>` consumes |
| `dashboard.tokens.json` | The Canyon dashboard's own tokens — gated here, published nowhere |
| `build-css.py` | Turns that file into `web/src/tokens.css` |
| `validate.py` | The registry's own gate — run it before committing changes here |

Canyon themes are **palettes**: no radius, spacing, or font variables — those stay the
builder's. Single-look themes carry the same values in light and dark (their one real look
survives the OS toggle); only Catppuccin's toggle changes flavor (Latte ↔ Mocha).

Consumers resolve the registry alias `@canyon` via their `components.json` (the alias names the registry; the row's namespace `@canyon/ui` above names the package an app declares):
`"registries": { "@canyon": "<registry_url>/{name}.json" }`.

The item JSONs are authored directly (content inline) — there is no build step and no node
toolchain here. If item count grows enough to hurt, adopt `shadcn build` then, not before.

The finding badge is published **for builders' apps**. The Canyon dashboard implements its
own severity chip, from the dashboard's original mock — it does not consume this item.

## The dashboard's own tokens

`dashboard.tokens.json` is the Canyon dashboard's set, a sibling of `tokens.json` and
deliberately **not** a registry item and **not** a theme: it carries radius, type, shadow,
focus and motion, which the palette rule above keeps out of anything published, and it must
not land in the `tokens_url` builders fetch. `validate.py` gates it like every other token
file — every leaf has a `$value`, every alias resolves, every colour is a plain hex value.
`build-css.py` resolves it through that same resolver and writes `web/src/tokens.css`: light
values on `:root`, dark values under `@media (prefers-color-scheme: dark)`, the seven
self-hosted faces (IBM Plex Sans, Space Grotesk, JetBrains Mono), and the Tailwind v4 `@theme inline` mapping. That file is
generated and committed, never hand-edited — `make tokens` rewrites it and `make tokens-check`
reds on any drift, in CI and in `make preflight`. A wrong value is wrong in the token file.

Space Grotesk and JetBrains Mono are static instances of google/fonts' variable TTFs
(`ofl/spacegrotesk` at 00a38a5, `ofl/jetbrainsmono` at 6e4b84c; fontTools' instancer and
subsetter), cut to IBM Plex's character set, JetBrains Mono without its `calt` code ligatures so
code reads as typed. The docs site sets its body in Space Grotesk, so `web/src/fonts` also holds its
Regular, Medium and Bold, cut the same way and declared in `docs/src/styles/brand.css`; the dashboard
bundle carries only the faces `tokens.css` declares.

## Invariants — enforced by `validate.py`

- Every dark/light value in a theme item is a plain hex color that traces to a value in
  that theme's own tokens file (`themes/<name>/tokens.json`; Canyon's own uses the root
  `tokens.json`).
- Every leaf under `color.` in every token file resolves to a plain hex value (`#rrggbb`
  or `#rrggbbaa`), and **every** resolved value in every token file — colours, font
  stacks, shadows, durations, alike — is free of `;`, `{`, `}` and `url(`, so a token
  cannot break out of the declaration it lands in.
- The dashboard's light and dark colour sets name the same leaves; one without the other
  is a finding.
- In each of the dashboard's sets every colour words are drawn in (text, text-dim, text-faint,
  accent, link and the statuses) clears WCAG AA's 4.5:1 on bg, surface and surface-2, each status
  and the accent on its own `-dim` fill, and accent-ink on accent.
- `registry.json` and `r/` agree in both directions: existence, title, description.
- Severity names (`critical | high | medium | low`) mirror `contracts/finding.md` — byte-exact.
- `blocks` is rendered, never derived, in components (derivation is the server's, per
  `contracts/snapshot.md`) — and it is a required prop, so display cannot fail open.

## Validation

```sh
python3 designsystem/canyon/validate.py
make tokens-check
```

CI's lint job runs both.
