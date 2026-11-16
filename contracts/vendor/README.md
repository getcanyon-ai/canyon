# Vendored schemas

Third-party. Nothing here is Canyon's to edit — a change comes from re-running the refresh
command, never from a hand edit.

## `sarif-2.1.0.schema.json`

| | |
|---|---|
| Source | `https://json.schemastore.org/sarif-2.1.0.json` |
| Upstream | SchemaStore's redistribution of the OASIS SARIF TC schema — **not** byte-identical to the OASIS original, which is draft-04; SchemaStore's copy is draft-07, adds the root `$id`, drops `definitions/region.anyOf`, and loosens two `language` patterns. Those edits do not weaken Canyon: the profile schema re-requires all four `region` bounds itself. |
| License | Apache-2.0, via the SchemaStore distribution — the OASIS original publishes no SPDX license. Its text is `LICENSE.schemastore`, SchemaStore's `LICENSE` at commit `43ae0fcbcc262c2f84e49c950529570235d3ad28`, which THIRD_PARTY_NOTICES carries with every binary that embeds the schema |
| Vendored | 2026-09-08 |
| sha256 (as fetched) | `7c9688f0a1c4a4e1649ecc78521087e664729c1dff56ee8212ff195c7b16132a` |
| sha256 (as stored) | `981917f597e8c22da30a5c79b55c9dc9a7b9092b359fa12c083aca69fcfbdcc7` — pinned in `contracts/validate.py` |
| Embedded | `contracts/contracts.go` (`//go:embed`) — the gate validates against this copy; the sha256 pin lives only in `validate.py`, so it moves with that check if the check ever moves |

Stored minified, one line. Be honest about the trade: this buys a 1-line diff instead of a
2,882-line one in the PR that vendors it, and it costs the reviewability of a future upstream
bump, which will also render as one changed line. So the sha256 below is pinned in
`contracts/validate.py` and checked on every run, and a PR that refreshes this file should
paste a pretty-printed diff of what actually changed. Refresh with `make vendor-sarif`, which
rewrites the file the same way — a non-empty `git diff` afterwards means upstream moved, and
that is a decision, not a merge.

It is vendored rather than fetched because the seam-1 check must not depend on the network:
a contract that cannot be validated offline fails open, and nothing on a gate path may.
