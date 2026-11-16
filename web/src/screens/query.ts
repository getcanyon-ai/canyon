// The fleet's query string: read off the hash, and rewritten one key at a time. The screen and the
// filter bar both build links and both build them here — a second copy would be a second place to
// forget the page. Nothing below reads a value's meaning: the string goes to the server as it
// stands, and the server is the one that says no (contracts/routes.md, the fleet read).
export function hashQuery(): string {
  const at = window.location.hash.indexOf("?");
  return at === -1 ? "" : window.location.hash.slice(at + 1);
}

export function fleetHref(query: string, set: Record<string, string | null>): string {
  const params = new URLSearchParams(query);
  for (const [key, value] of Object.entries(set)) {
    if (value === null) params.delete(key);
    else params.set(key, value);
  }
  const next = params.toString();
  return next === "" ? "#/fleet" : `#/fleet?${next}`;
}

// A fresh view of the fleet (a chip, Clear filters) keeps what is the reader's own choice and no filter:
// the order the rows are in and how many a page holds.
export function freshQuery(query: string): string {
  const params = new URLSearchParams(query);
  const kept = new URLSearchParams();
  for (const key of ["sort", "limit"]) {
    const value = params.get(key);
    if (value !== null) kept.set(key, value);
  }
  return kept.toString();
}
