// One formatter for every elapsed time on every surface. Under an hour reads minutes, under a day
// hours, under sixty days days, beyond that months — always at least 1, so nothing reads 0m. The
// past is clamped: a timestamp in the future is not a negative age. Policy expiry needs the other
// read and gets its own when it lands.
type Unit = "m" | "h" | "d" | "mo";

function elapsed(at: string | null): [number, Unit] | null {
  if (at === null) return null;
  const ms = Date.now() - new Date(at).getTime();
  if (Number.isNaN(ms)) return null;
  const minutes = Math.max(1, Math.floor(ms / 60_000));
  if (minutes < 60) return [minutes, "m"];
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return [hours, "h"];
  const days = Math.floor(hours / 24);
  return days < 60 ? [days, "d"] : [Math.floor(days / 30), "mo"];
}

export function age(at: string | null): string {
  const span = elapsed(at);
  return span === null ? "—" : `${span[0]}${span[1]}`;
}

// The same span said out loud, for a column with room for the words: "29 days ago".
const WORDS: Record<Unit, [string, string]> = { m: ["minute", "minutes"], h: ["hour", "hours"], d: ["day", "days"], mo: ["month", "months"] };
export function ago(at: string | null): string {
  const span = elapsed(at);
  if (span === null) return "—";
  const [n, unit] = span;
  return `${n} ${WORDS[unit][n === 1 ? 0 : 1]} ago`;
}

export function Age({ at, long = false }: { at: string | null; long?: boolean }) {
  return <span className="tnum text-sm text-text-faint">{long ? ago(at) : age(at)}</span>;
}
