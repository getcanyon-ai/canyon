import { useEffect, useState } from "react";
import { ApiError, getImage, said, waiting, type ImageFinding, type ImageRead, type Status } from "../api";
import { LIBRARIES as L, S } from "../strings";
import { ago } from "../ui/Age";
import { Chip } from "../ui/Chip";

const H2 = "text-base font-semibold";
const ENVS = [["preview", "Preview"], ["prod", "Production"]] as const;
type EnvKey = (typeof ENVS)[number][0];
// What an environment's read answered: the read, nothing to show (nothing deployed there, or not the reader's), or why it failed.
type Answer = ImageRead | "none" | { failed: string };

const STATES = new Set(["checked", "could_not_check", "not_scanned"]);
// Fail closed: a read whose shape the card does not know is not read, never shown as nothing found.
const whole = (read: ImageRead) => STATES.has(read?.state) && typeof read.why === "string" && Array.isArray(read.findings) && Array.isArray(read.notes);

// Where a package is, in a developer's words: system libraries for the os class, build tools for the rest (#2605, #2079).
const placeOf = (cls: string) => (cls === "os" ? L.inOs : L.inTools);
const fill = (text: string, values: Record<string, string>) => text.replace(/<(\w+)>/g, (all, key: string) => values[key] ?? all);

// A package's counted findings together, in the read's order: most severe first, so its first is its severity.
function byPackage(findings: ImageFinding[]): ImageFinding[][] {
  const out = new Map<string, ImageFinding[]>();
  for (const one of findings) {
    const key = `${one.dependency.name}\u0000${one.dependency.version}`;
    out.set(key, [...(out.get(key) ?? []), one]);
  }
  return [...out.values()];
}

// What Canyon's patches fixed, one line per package, its vulnerabilities together and where it was (#2631): patched[]
// names one fix per vulnerability.
function fixedLines(patched: NonNullable<ImageRead["patched"]>): string[] {
  const lines = new Map<string, { one: (typeof patched)[number]; vulnerabilities: string[] }>();
  for (const one of patched) {
    const key = `${one.name}\u0000${one.from}\u0000${one.to}\u0000${one.class}`;
    const line = lines.get(key) ?? { one, vulnerabilities: [] };
    if (!line.vulnerabilities.includes(one.vulnerability)) line.vulnerabilities.push(one.vulnerability);
    lines.set(key, line);
  }
  return [...lines.values()].map(({ one, vulnerabilities }) =>
    fill(L.fixed, { name: one.name, from: one.from, to: one.to, vulnerability: vulnerabilities.join(", "), place: placeOf(one.class) }));
}

function PackageCard({ findings }: { findings: ImageFinding[] }) {
  const [first] = findings as [ImageFinding];
  const { name, version } = first.dependency;
  const { severity } = first.properties.canyon;
  const head = fill(findings.length === 1 ? L.headOne : L.head, { name, version, n: String(findings.length) });
  const places = [...new Set(findings.map((one) => placeOf(one.class)))];
  const fixes = [...new Set(findings.map((one) => one.properties.canyon.fix))];
  return (
    <article aria-label={head} className="rounded-xl border border-border bg-surface px-3.5 py-3">
      <div className="flex flex-wrap items-baseline gap-2">
        <Chip tone={severity}>{severity[0]!.toUpperCase() + severity.slice(1)}</Chip>
        <span className="text-lg font-semibold">{head}</span>
        <span className="text-md text-text-dim">{places.join(" · ")}</span>
      </div>
      <ul className="mt-2 grid gap-1 text-md text-text-dim">
        {findings.map((one) => <li key={one.dependency.vulnerability}>{fill(L.advisory, { vulnerability: one.dependency.vulnerability, fixed: one.dependency.fixed })}</li>)}
      </ul>
      {fixes.map((fix) => <p key={fix} className="mt-2 text-base">{fix}</p>)}
    </article>
  );
}

function EnvRead({ name, read }: { name: string; read: ImageRead }) {
  const mark = read.state === "could_not_check" ? "✗" : read.state === "checked" && read.findings.length === 0 ? "✓" : "·";
  const tone = mark === "✗" ? "text-critical" : mark === "✓" ? "text-ok" : "text-text-dim";
  return (
    <section aria-label={`${name} system libraries`} className="grid grid-cols-[minmax(0,1fr)] gap-2">
      <h3 className="text-md font-semibold">{name}</h3>
      <p className={`text-base ${tone}`}>{`${mark} ${read.why}`}</p>
      {read.state === "checked" && read.scanned_at !== null && read.db_built_at !== null && (
        <p className="text-md text-text-faint">{fill(L.checked, { listed: ago(read.scanned_at), data: ago(read.db_built_at) })}</p>
      )}
      {fixedLines(read.patched ?? []).map((line) => <p key={line} className="text-md text-ok">{line}</p>)}
      {byPackage(read.findings).map((findings) => <PackageCard key={`${findings[0]!.dependency.name} ${findings[0]!.dependency.version}`} findings={findings} />)}
      {read.notes.length > 0 && (
        <details>
          <summary className="cursor-pointer text-md text-text-dim">{read.notes.length === 1 ? L.notesOne : fill(L.notes, { n: String(read.notes.length) })}</summary>
          <ul className="mt-1.5 grid gap-1 text-md text-text-dim">
            {read.notes.map((one) => (
              <li key={`${one.name} ${one.version} ${one.why}`}>
                {[`${one.name} ${one.version}`, L.note[one.why] ?? one.why, ...(one.class === "unknown" ? [] : [placeOf(one.class)])].join(" · ")}
              </li>
            ))}
          </ul>
        </details>
      )}
    </section>
  );
}

// The app page's System libraries card (#2079; design/PATCHING.md §8; design/specs/screens.md A2): what the image each
// environment runs holds of Canyon's, as platform.known-vuln reads it. A contributor's and up: the page draws it only on
// the team's read, and a 403 draws nothing. Nothing scanned anywhere draws no card, so an app whose rule is off reads as
// it did; a read that fails or does not read whole says so, never nothing found.
export function SystemLibraries({ id, envs }: { id: string; envs: Status["envs"] }) {
  const live = ENVS.filter(([key]) => { const env = envs[key]; return env !== undefined && !waiting(env); });
  const deployed = live.map(([key]) => { const env = envs[key]!; return waiting(env) ? "" : `${key}:${env.package}@${env.deployed_at}`; }).join(" ");
  const [answers, setAnswers] = useState<{ key: string; read: Partial<Record<EnvKey, Answer>> } | null>(null);
  useEffect(() => {
    let on = true;
    const answer = (key: EnvKey) => getImage(id, key).then(
      (read): [EnvKey, Answer] => [key, whole(read) ? read : { failed: L.notWhole }],
      (err: unknown): [EnvKey, Answer] => [key, err instanceof ApiError && (err.status === 404 || err.status === 403) ? "none" : { failed: said(err) }]);
    void Promise.all(live.map(([key]) => answer(key))).then((all) => { if (on) setAnswers({ key: deployed, read: Object.fromEntries(all) }); });
    return () => { on = false; };
  }, [id, deployed]); // live is deployed's: a new deploy reads again, a re-render does not
  if (answers === null || answers.key !== deployed) return null;
  // Each environment with something to show: its read, or why it could not be read.
  const shown = live.flatMap(([key, name]) => { const read = answers.read[key]; return read === undefined || read === "none" ? [] : [{ key, name, read }]; });
  if (!shown.some(({ read }) => "failed" in read || read.state !== "not_scanned")) return null;
  return (
    <section aria-label={L.title} className="grid grid-cols-[minmax(0,1fr)] gap-3">
      <h2 className={`flex flex-wrap items-center gap-x-2 ${H2}`}>{L.title}<Chip tone="beta">{S.beta}</Chip></h2>
      <p className="text-md text-text-dim">{L.about}</p>
      {shown.map(({ key, name, read }) => ("failed" in read
        ? <p key={key} className="text-base text-critical">{fill(L.unread, { env: key === "preview" ? "preview" : "production", error: read.failed })}</p>
        : <EnvRead key={key} name={name} read={read} />))}
    </section>
  );
}
