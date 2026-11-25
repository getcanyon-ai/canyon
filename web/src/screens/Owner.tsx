import { reduced, version, type App, type Reduced } from "../api";
import { S } from "../strings";
import { Age } from "../ui/Age";
import { EnvCell } from "../ui/EnvLink";
import { Stage } from "../ui/Stage";

const CARD = "rounded-xl border border-border bg-surface px-3.5 py-3";
const ENV_NAME = "mb-1 text-xs tracking-caps text-text-faint uppercase";

// One person's apps: the fleet read with ?owner=<subject>, so the cards are the server's answer to
// that query and nothing else. A link to this view is built from the subject — two people can share
// a display name, and the server matches the subject exactly (contracts/routes.md, the fleet read).
export const ownerHref = (subject: string) => `#/fleet?owner=${encodeURIComponent(subject)}`;

// The person's name as any row the server answered spells it; a subject that owns nothing here
// keeps its own name on the heading, because no row carries a display to use.
export function ownerName(apps: (App | Reduced)[], subject: string): string {
  for (const app of apps) {
    const owner = app.owners.find((one) => one.subject === subject);
    if (owner !== undefined && owner.display !== "") return owner.display;
  }
  return subject;
}

export function OwnerApps({ apps }: { apps: (App | Reduced)[] }) {
  if (apps.length === 0) {
    return <p className="mt-3 text-base text-text-faint">No apps — nobody owns anything here under that name.</p>;
  }
  return (
    <>
      {/* Once above the cards, never per card (EXPERIENCE.md D5). */}
      {apps.some((app) => !reduced(app) && app.gate_state === "never_reviewed") && <p className="mt-2.5 text-md text-amber">{S.neverReviewed}</p>}
      <ul className="mt-3 grid grid-cols-2 gap-3 max-[860px]:grid-cols-1">
        {apps.map((app) => (
          <li key={app.id} className={CARD}>
            <h2 className="flex flex-wrap items-baseline gap-2 text-2xl font-semibold">
              <a className="text-link no-underline hover:underline" href={`#/apps/${app.id}`}>{app.name}</a>
              {!reduced(app) && <span className="text-md font-regular text-text-dim">{app.stack}</span>}
              {!reduced(app) && app.gate_state === "never_reviewed" && <span className="text-md font-regular text-amber">{S.neverReviewedMark}</span>}
            </h2>
            {/* An app off the viewer's teams is its name, owners and summary, and nothing of the team's. */}
            {reduced(app) ? <p className="mt-1 text-base text-text-dim">{app.summary || S.noDescription}</p> : (
              <>
                <p className="mt-1">
                  <Stage stage={app.latest.stage} version={app.latest.package === null ? undefined : version(app.latest.package, app.latest.label)} />
                  {" · "}<Age at={app.latest.since} />
                </p>
                <div className="mt-2.5 grid grid-cols-2 gap-3 max-[520px]:grid-cols-1">
                  <div>
                    <h3 className={ENV_NAME}>Production</h3>
                    <EnvCell env={app.envs.prod} />
                  </div>
                  <div>
                    <h3 className={ENV_NAME}>Preview</h3>
                    <EnvCell env={app.envs.preview} />
                  </div>
                </div>
              </>
            )}
          </li>
        ))}
      </ul>
    </>
  );
}
