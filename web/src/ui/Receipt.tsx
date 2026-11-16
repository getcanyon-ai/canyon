import { useContext, useState } from "react";
import { BETA, type BuiltAppPackage, type EnvPatch, type KnownVulnClass, type KnownVulnPackage, type Need, type Receipt as ReceiptRead } from "../api";
import { KNOWN_VULN, LIBRARIES, S } from "../strings";
import { Button } from "./Button";
import { Chip } from "./Chip";
import { ApproveForm, type ApproveBody } from "./Finding";
import { Icon } from "./Icon";
import { RailContext } from "./Rail";
import { refusing } from "../screens/Promote";

// Approving a package only the built app holds (#2610): the org, for the form's who-sees-it line, and what files it,
// the dependency's approve of the review the receipt is read for.
export type ApproveBuiltApp = { org: string; send: (fingerprints: string[], body: ApproveBody) => Promise<void> };

// The icon in its own column, so a name too long for the line wraps beside it, never under it (#2300).
const ROW = "grid grid-cols-[14px_minmax(0,1fr)] items-baseline gap-x-2 text-base";
// The approval page's list is bordered, its heading said, and a rule that blocks is tinted red: there it
// is the reason the page cannot be approved, so it is the first thing seen.
const FRAMED = "border-t border-border px-3.5 py-2 first:border-t-0";
const passedById = (receipt: ReceiptRead) => Object.fromEntries(receipt.rules.map((rule) => [rule.id, rule.passed]));

// The server's four go-live rules in its order, each verdict and reason as the server wrote them:
// nothing here counts, reorders or rewords (D6: what passed sits next to what didn't). A rule that
// has just turned ✓ flips once — the cause the promote button's fill follows 160ms later. `patch` is preview's patch
// state, the environment whose image the receipt reads; absent where the page has none (the approval page).
// approveBuiltApp files the approval of a package only the built app holds; the page passes it only where the review's run
// read lets the reader approve (design/PERMISSIONS.md: an admin or a responder who didn't start it), and without it no
// Approve is drawn.
// refuses is the status read's will_refuse need: production's egress row says its destinations, host names apart from what
// only a change to the code fixes, never the rule's own line (D15); a read with no such need (the approval page) keeps it.
export function Receipt({ receipt, framed = false, patch, approveBuiltApp, refuses }: { receipt: ReceiptRead; framed?: boolean; patch?: EnvPatch; approveBuiltApp?: ApproveBuiltApp; refuses?: Need }) {
  const { admin } = useContext(RailContext);
  const [was, setWas] = useState(() => passedById(receipt));
  const [flipped, setFlipped] = useState<string[]>([]);

  // Adjusted during render, never in an effect: StrictMode invokes effects twice on a commit,
  // and the row would already have been painted green before the class arrived.
  const turned = receipt.rules.filter((rule) => rule.id in was && was[rule.id] !== rule.passed);
  if (turned.length > 0) {
    setWas(passedById(receipt));
    setFlipped(turned.filter((rule) => rule.passed).map((rule) => rule.id));
  }

  // Nothing in preview: the server's one line, never a checklist that reads as the app failing.
  if (receipt.waiting !== undefined) {
    return (
      <section aria-label="Go-live checklist">
        <p className="text-base text-text-dim">{receipt.waiting}</p>
      </section>
    );
  }
  return (
    <section aria-label="Go-live checklist" className={framed ? "grid gap-1" : undefined}>
      {framed && <h2 className="text-sm font-medium tracking-caps text-text-faint uppercase">Go-live checklist</h2>}
      {receipt.blocked_by.includes("prod.clean-gate") && (
        <p className="mb-2 text-base text-text-dim">{S.cleanGate.replace("<org>", () => receipt.org)}</p>
      )}
      <ul className={framed ? "rounded-lg border border-border" : undefined}>
        {receipt.rules.map((rule) => (
          <li
            key={rule.id}
            className={`${ROW} ${framed ? `${FRAMED}${!rule.passed && rule.blocks && rule.unset === undefined ? " bg-critical-dim" : ""}` : "py-1"}${flipped.includes(rule.id) ? " flip-row" : ""}`}
            // Under reduced motion no animation runs, so this never fires and the class stays; it
            // paints nothing, and the row has already turned.
            onAnimationEnd={() => setFlipped((at) => at.filter((id) => id !== rule.id))}
          >
            <span className={`translate-y-0.5 transition-colors duration-(--ds-motion-duration-medium) ease-(--ds-motion-easing-standard) ${rule.passed ? "text-ok" : rule.unset ? "text-text-faint" : rule.blocks ? "text-critical" : "text-amber"}`}>
              {/* nothing set to check against is neutral, never a failure (decision D4); an advisory miss stops nothing */}
              <Icon name={rule.passed ? "passed" : rule.unset ? "never" : rule.blocks ? "blocked" : "alert"} size={14} />
              <span className="sr-only">{rule.passed ? "Met" : rule.unset ? "Nothing to check" : "Not met" /* the advisory chip beside it says the rest */}</span>
            </span>
            <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
              {/* The rule's row under Policies, to an admin; Guardrails is an admin's screen (design/specs/screens.md, the blocked promote button). */}
              {admin
                ? <a className={`font-medium no-underline hover:text-link hover:underline ${rule.unset ? "text-text-dim" : "text-text"}`} href={`#/guardrails?rule=${encodeURIComponent(rule.id)}`}>{rule.name}</a>
                : <span className={`font-medium ${rule.unset ? "text-text-dim" : "text-text"}`}>{rule.name}</span>}
              {BETA.includes(rule.id) && <Chip tone="beta">{S.beta}</Chip>}
              <span className="basis-full text-text-dim">{rule.id === "prod.egress-allowed" && !rule.passed && refusing(refuses, S.needWillRefuseAllowable).length > 0 ? refusing(refuses, S.needWillRefuseAllowable).join(" ") : rule.why}</span>
              {rule.id === "platform.known-vuln" && (rule.packages !== undefined || (rule.notes ?? 0) > 0) && <span className="basis-full text-text-dim">{KNOWN_VULN.where}</span>}
              {!rule.passed && !rule.blocks && rule.unset === undefined && <Chip tone="advisory">advisory</Chip>}
              {rule.packages !== undefined && <Packages packages={rule.packages} patching={patch?.state === "planned"} />}
              {rule.built_app !== undefined && <BuiltApp packages={rule.built_app} approve={approveBuiltApp} />}
            </div>
          </li>
        ))}
      </ul>
    </section>
  );
}

// What fixes a package in a layer of this class (contracts/finding.md, System libraries and build tools): newer system libraries for
// the os class, a newer buildpack or lifecycle for the rest. Absent where the receipt names no layer.
type Fix = "os" | "tools";
const FIXES: Fix[] = ["os", "tools"];
const fixOf = (layer: { class: KnownVulnClass }): Fix => (layer.class === "os" ? "os" : "tools");
// Where a layer is, in words for its owner as the image's label names it; a buildpack by its own id.
const placeOf = (owner: string) => ({ "run-image": KNOWN_VULN.inOs, lifecycle: KNOWN_VULN.inLauncher, app: KNOWN_VULN.inApp } as Record<string, string>)[owner] ?? owner;

interface Group { fix?: Fix; name: string; versions: string[]; places: string[]; advisories: KnownVulnPackage["advisories"] }

// The packages by what fixes them, then by name: one line for a package however many of its versions the image holds,
// each version and place once and each advisory once (#2579: the test cluster's approval said stdlib three times, its
// advisories once per version). A package the receipt names no layer of is grouped by name alone.
function groupPackages(packages: KnownVulnPackage[]): Group[] {
  const out: Group[] = [];
  for (const one of packages) {
    const name = one.go_stdlib ? KNOWN_VULN.goStdlib : one.name;
    const fixes = one.layers === undefined ? [undefined] : FIXES.filter((fix) => one.layers?.some((layer) => fixOf(layer) === fix));
    for (const fix of fixes) {
      let group = out.find((g) => g.fix === fix && g.name === name);
      if (group === undefined) out.push(group = { fix, name, versions: [], places: [], advisories: [] });
      if (!group.versions.includes(one.version)) group.versions.push(one.version);
      for (const layer of one.layers ?? []) {
        const place = placeOf(layer.owner);
        if (fixOf(layer) === fix && !group.places.includes(place)) group.places.push(place);
      }
      for (const advisory of one.advisories) {
        if (!group.advisories.some((a) => a.id === advisory.id && a.fixed === advisory.fixed)) group.advisories.push(advisory);
      }
    }
  }
  return out;
}

// What platform.known-vuln counts, folded under its row: what fixes each, then each package with its installed versions
// and where it is, then every advisory against it with the versions that fix it, as the receipt says them
// (contracts/routes.md; #2300, #2579). A system libraries package has no fix from Canyon yet unless preview's patch is
// planned: then the system libraries Canyon uses now fix it, and Canyon is trying that fix in preview (#2594).
function Packages({ packages, patching }: { packages: KnownVulnPackage[]; patching: boolean }) {
  const groups = groupPackages(packages);
  return (
    <details className="basis-full text-md">
      <summary className="cursor-pointer text-text-dim">{packages.length === 1 ? S.knownVulnPackage : S.knownVulnPackages.replace("<n>", String(packages.length))}</summary>
      {[...FIXES, undefined].map((fix) => {
        const mine = groups.filter((group) => group.fix === fix);
        if (mine.length === 0) return null;
        return (
          <div key={fix ?? "unplaced"} className="mt-1.5 grid gap-1">
            {fix !== undefined && <p className="text-sm text-text-faint">{fix === "tools" ? KNOWN_VULN.toolsFix : patching ? KNOWN_VULN.osPatching : KNOWN_VULN.osFix}</p>}
            <ul className="grid gap-1.5">
              {mine.map((group) => (
                <li key={group.name} className="min-w-0 break-words">
                  <span className="font-medium text-text">{`${group.name} ${group.versions.join(", ")}`}</span>
                  {group.places.length > 0 && <span className="text-text-dim">{` · ${KNOWN_VULN.in.replace("<places>", () => group.places.join(", "))}`}</span>}
                  <ul className="text-text-dim">
                    {group.advisories.map((advisory) => <li key={`${advisory.id} ${advisory.fixed}`} className="pl-3 -indent-3">{S.fixedIn.replace("<id>", () => advisory.id).replace("<fixed>", () => advisory.fixed)}</li>)}
                  </ul>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </details>
  );
}

// What deps.known-vuln counts of the built app, folded under its row (contracts/routes.md, The promote receipt; #2610):
// each package with every advisory against it and what fixes it, as the receipt says them, then what approvals cover,
// marked approved. To an admin or a responder, each package not yet approved has its approval: one reason and expiry
// for all its advisories, as a dependency's findings are approved together.
function BuiltApp({ packages, approve }: { packages: BuiltAppPackage[]; approve?: ApproveBuiltApp }) {
  const [approving, setApproving] = useState<string | null>(null);
  return (
    <details className="basis-full text-md">
      <summary className="cursor-pointer text-text-dim">{packages.length === 1 ? S.knownVulnPackage : S.knownVulnPackages.replace("<n>", String(packages.length))}</summary>
      <ul className="mt-1.5 grid gap-1.5">
        {packages.map((one) => {
          const key = `${one.name} ${one.version}`;
          return (
            <li key={key} className="min-w-0 break-words">
              <span className="flex flex-wrap items-baseline gap-x-2">
                <span className="font-medium text-text">{key}</span>
                {one.approved && <Chip tone="approved">approved</Chip>}
                {!one.approved && approve !== undefined && approving !== key && <Button variant="ghost" size="sm" onClick={() => setApproving(key)}>Approve with a reason</Button>}
              </span>
              <ul className="text-text-dim">
                {one.advisories.map((advisory) => (
                  <li key={advisory.id} className="pl-3 -indent-3">
                    {advisory.fixed === "" ? `${advisory.id} · ${LIBRARIES.note.no_fix}` : S.fixedIn.replace("<id>", () => advisory.id).replace("<fixed>", () => advisory.fixed)}
                  </li>
                ))}
              </ul>
              {!one.approved && approve !== undefined && approving === key && (
                <ApproveForm id={one.fingerprints[0] ?? key} org={approve.org} send={async (body) => { await approve.send(one.fingerprints, body); setApproving(null); }} close={() => setApproving(null)} />
              )}
            </li>
          );
        })}
      </ul>
    </details>
  );
}
