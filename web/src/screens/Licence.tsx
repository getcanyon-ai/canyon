import { useEffect, useState } from "react";
import { getLicence, putLicence, said, type LicenceFeature, type LicenceState } from "../api";
import { S } from "../strings";
import { SaveSection } from "../ui/SaveSection";
import { EnterpriseBadge, FEATURE_NAME, licenceDate } from "../ui/Enterprise";

// Settings › Licence (#2879; contracts/routes.md, Licence): an admin's alone, as Telemetry is: a member's screen never asks
// the routes. The licence's state in D5's words, what it says, then the box an admin pastes a new one into, or, while
// CANYON_LICENSE sets it, the line that says so in its place.

const LABEL = "text-xs tracking-caps text-text-faint uppercase";

// stateLine is the licence's state as D5 says it.
export function stateLine(l: LicenceState) {
  const fill = (template: string) => template
    .replace("<customer>", () => l.customer ?? "")
    .replace("<date>", () => (l.expires === undefined ? "" : licenceDate(l.expires)))
    .replace("<grace>", () => (l.grace_until === undefined ? "" : licenceDate(l.grace_until)));
  switch (l.state) {
    case "valid": return fill(S.licenceValid);
    case "grace": return fill(S.licenceGrace);
    case "lapsed": return fill(S.licenceLapsed);
    case "invalid": return l.problem === "unsigned" ? S.licenceUnsigned : S.licenceUnreadable;
    default: return S.licenceNone;
  }
}

const featureName = (id: string) => FEATURE_NAME[id as LicenceFeature] ?? id;

export function LicenceSettings() {
  const [licence, setLicence] = useState<LicenceState | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [text, setText] = useState("");
  useEffect(() => { getLicence().then(setLicence, (err: unknown) => setFailure(said(err))); }, []);
  if (licence === null) return failure === null ? null : <p className="text-base text-critical">{failure}</p>;
  // Typed, so it has its Save, shown once something is pasted (D16).
  const save = () => putLicence(text.trim()).then((next) => { setLicence(next); setText(""); });
  return (
    <section aria-label="Licence" className="grid gap-3">
      <p className="text-base">{stateLine(licence)}</p>
      {licence.features !== undefined && (
        <dl className="grid grid-cols-[max-content_minmax(0,1fr)] items-baseline gap-x-6 gap-y-2">
          <dt className={LABEL}>{S.licenceSeats}</dt><dd className="text-base">{licence.seats}</dd>
          <dt className={LABEL}>{S.licenceValidUntil}</dt><dd className="text-base">{licence.expires === undefined ? "" : licenceDate(licence.expires)}</dd>
          <dt className={LABEL}>{S.licenceFeatures}</dt>
          <dd><ul className="grid gap-1.5">{licence.features.map((id) => <li key={id} className="flex items-center gap-2 text-base"><EnterpriseBadge />{featureName(id)}</li>)}</ul></dd>
        </dl>
      )}
      {licence.source === "env"
        ? <p className="text-md text-text-dim">{S.licenceFromEnv}</p>
        : (
          <div className="grid max-w-[640px] gap-2">
            <label htmlFor="licence-text" className={LABEL}>{S.licencePaste}</label>
            <textarea id="licence-text" value={text} onChange={(e) => setText(e.target.value)} rows={5} spellCheck={false}
              className="w-full rounded-lg border border-border bg-surface px-2.5 py-1.5 font-mono text-sm" />
            <SaveSection label={S.licenceSave} changed={text.trim() !== ""} save={save} />
          </div>
        )}
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
    </section>
  );
}
