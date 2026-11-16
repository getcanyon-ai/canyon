import type { ProjectResource } from "../api";
import { PROVIDED as P } from "../strings";

const H2 = "text-base font-semibold";
const WORD = { preview: "preview", prod: "production" } as const;

// What the org's platform provided for the app (design/HAND-OFF.md §7, #2432): a team member's read
// alone carries resources[], so a reduced row draws nothing. The endpoint is where it is, never how to
// sign in; it is drawn as text and never as a link, since the platform wrote it.
export function Provided({ resources }: { resources: ProjectResource[] | undefined }) {
  const rows = (resources ?? []).filter((one) => one.state === "provided" && one.env !== undefined && one.endpoint !== undefined);
  if (rows.length === 0) return null;
  return (
    <section aria-label={P.title} className="grid grid-cols-[minmax(0,1fr)] gap-2.5">
      <h2 className={H2}>{P.title}</h2>
      <ul className="grid gap-1.5">
        {rows.map((one) => (
          <li key={`${one.env}/${one.name}`} className="flex flex-wrap items-baseline gap-x-2 text-base">
            <span>{P.row.replace("<type>", () => one.type).replace("<name>", () => one.name).replace("<env>", () => WORD[one.env as "preview" | "prod"])}</span>
            <code className="break-all font-mono text-md text-text-dim">{one.endpoint}</code>
          </li>
        ))}
      </ul>
    </section>
  );
}
