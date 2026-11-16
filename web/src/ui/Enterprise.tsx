import { useEffect, useState, type ReactNode } from "react";
import { getLicence, type LicenceFeature, type LicenceState } from "../api";
import { S } from "../strings";
import { Pill } from "./Pill";

// The Enterprise licence as a screen reads it (contracts/routes.md, Licence; #2879): every member reads {state, allows},
// so a screen shows an Enterprise feature's add or change only while the licence in force unlocks it. The server refuses
// the change itself (409 licence_required); this only says so before anyone tries.

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

// licenceDate is a licence's instant as D5 writes it, `14 Nov 2027`, in UTC.
export const licenceDate = (at: string) => {
  const d = new Date(at);
  return `${d.getUTCDate()} ${MONTHS[d.getUTCMonth()]} ${d.getUTCFullYear()}`;
};

export const FEATURE_NAME: Record<LicenceFeature, string> = { outbound_keys: S.featureOutbound, platform_patching: S.featurePatching, group_sync: S.featureGroups };
const REQUIRED: Record<LicenceFeature, string> = { outbound_keys: S.licenceRequiredOutbound, platform_patching: S.licenceRequiredPatching, group_sync: S.licenceRequiredGroups };

// useLicence reads the licence once; null until it answers, and null when the read fails: then nothing gated is offered.
export function useLicence(): LicenceState | null {
  const [licence, setLicence] = useState<LicenceState | null>(null);
  useEffect(() => {
    let live = true;
    getLicence().then((read) => { if (live) setLicence(read); }, () => undefined);
    return () => { live = false; };
  }, []);
  return licence;
}

export const unlocks = (licence: LicenceState | null, feature: LicenceFeature) => licence !== null && Array.isArray(licence.allows) && licence.allows.includes(feature); // a read without allows unlocks nothing

// EnterpriseBadge is the mark beside a feature that needs Canyon Enterprise.
export const EnterpriseBadge = () => <Pill tone="beta">{S.enterprise}</Pill>;

// EnterpriseLine stands in place of an action the licence doesn't unlock: the badge and the server's own words for it.
export function EnterpriseLine({ feature }: { feature: LicenceFeature }) {
  return (
    <p className="flex flex-wrap items-center gap-2 text-md text-text-dim">
      <EnterpriseBadge />
      <span>{REQUIRED[feature]}</span>
    </p>
  );
}

// Enterprise shows its children while the licence unlocks feature, EnterpriseLine once it is read and doesn't, and nothing
// before it is read.
export function Enterprise({ licence, feature, children }: { licence: LicenceState | null; feature: LicenceFeature; children: ReactNode }) {
  if (licence === null) return null;
  return unlocks(licence, feature) ? <>{children}</> : <EnterpriseLine feature={feature} />;
}
