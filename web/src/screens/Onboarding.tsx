import { useState } from "react";
import { putPreferences, said, type Overridable, type Preferences, type PreferencesEdit } from "../api";
import { Button } from "../ui/Button";
import { AdminFields, domainOf, OrganisationFields, SignInFields, useOrgDraft } from "./OrgSettings";

const H2 = "mt-5 text-2xl font-semibold";
const NOTE = "mt-2 text-md text-text-dim";
const ROW = "text-xs tracking-caps text-text-faint uppercase";

// The four questions, in the order design/ONBOARDING.md §3 asks them. The step heading is the
// question; the fields under it are the settings page's own sections, composed rather than copied,
// so the form somebody meets a year later is the form they filled in on day one.
const STEPS = ["What is this organisation called?", "Who may sign in?", "Who administers Canyon?", "Ready"] as const;
const NOBODY = "At least one person or one domain has to be allowed to sign in.";

// A list read as prose: "dana@meridian.example, and anyone at meridian.example". Blank rows are the
// half-typed ones a list grows when Add is clicked, and they are nobody.
function sentence(parts: string[]): string {
  const kept = parts.filter((one) => one.trim() !== "");
  return kept.length < 2 ? kept.join("") : `${kept.slice(0, -1).join(", ")}, and ${kept.at(-1)}`;
}

function Row({ label, value, edit }: { label: string; value: string; edit: () => void }) {
  return (
    <div className="flex flex-wrap items-baseline gap-3">
      <dt className={`${ROW} w-[120px]`}>{label}</dt>
      <dd className="min-w-0 flex-1 text-base">{value}</dd>
      <button type="button" className="text-base text-link" onClick={edit}>{`Edit ${label.toLowerCase()}`}</button>
    </div>
  );
}

// The first screen anybody sees on a host nobody has configured (design/ONBOARDING.md §3). Four
// steps, one row written at the end: nothing is saved until Finish, so somebody who walks away
// half-way leaves the host exactly as they found it, still in the setup state.
export function Onboarding({ record, done }: { record: Preferences; done: () => void }) {
  const [step, setStep] = useState(1);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  // The reader's own address: the one value that is always right, because they are signed in and it
  // demonstrably works. Without it an admin can finish setup having locked themselves out.
  const mine = record.viewer?.email ?? null;
  const pinned = (key: Overridable) => record.overridden.includes(key);
  const seed = (list: string[], key: Overridable | null) =>
    mine === null || list.includes(mine) || (key !== null && pinned(key)) ? list : [...list, mine];
  const draft = useOrgDraft({ ...record, allowed_emails: seed(record.allowed_emails, "allowed_emails"), admin_emails: seed(record.admin_emails, null) });

  // An admin address on neither membership list goes onto the allowed addresses, and step 3 says so:
  // two lists that disagree about one person is a bug report waiting to happen.
  const unlisted = pinned("allowed_emails") ? [] : draft.admins.filter((one) =>
    one.trim() !== "" && !draft.emails.includes(one) && !draft.domains.includes(domainOf(one) ?? ""));
  const nobody = [...draft.emails, ...draft.domains].every((one) => one.trim() === "");

  const finish = async () => {
    if (sending) return;
    setSending(true);
    try {
      // Every key the environment pins is left out: naming one is `409 set_by_operator`, and the
      // value on screen is already the environment's.
      const body: PreferencesEdit = { admin_emails: draft.admins, onboarded_at: new Date().toISOString() };
      if (!pinned("org_name")) body.org_name = draft.orgName;
      if (!pinned("allowed_emails")) body.allowed_emails = [...draft.emails, ...unlisted];
      if (!pinned("allowed_domains")) body.allowed_domains = draft.domains;
      if (!pinned("admin_claim")) {
        body.admin_claim = draft.claim === "" ? null : draft.claim;
        body.admin_value = draft.claim === "" ? null : draft.value;
      }
      await putPreferences(body);
      done();
    } catch (err) {
      setRefusal(said(err));
    } finally {
      setSending(false);
    }
  };

  const fields = { record, draft, admin: true, mine };
  return (
    <main className="mx-auto max-w-[680px] px-8 pt-8 pb-24 max-[860px]:px-4">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <h1 className="text-4xl font-semibold tracking-tight">Set up Canyon</h1>
        <span className="text-md text-text-dim">{`${step} of ${STEPS.length}`}</span>
      </div>
      <section aria-label={STEPS[step - 1]}>
        <h2 className={H2}>{STEPS[step - 1]}</h2>
        {step === 1 && <OrganisationFields {...fields} />}
        {step === 2 && <SignInFields {...fields} />}
        {step === 3 && (
          <>
            <AdminFields {...fields} />
            {unlisted.length > 0 && <p className={NOTE}>{`${sentence(unlisted)} will also be added to the addresses allowed to sign in.`}</p>}
          </>
        )}
        {step === 4 && (
          <dl className="mt-3 flex flex-col gap-2">
            <Row label="Organisation" value={draft.orgName} edit={() => setStep(1)} />
            <Row label="Sign-in" value={sentence([...draft.emails, ...unlisted, ...draft.domains.map((one) => `anyone at ${one}`)])} edit={() => setStep(2)} />
            <Row label="Admins" value={sentence([...draft.admins, ...(draft.claim === "" ? [] : [`anyone in ${draft.claim} ${draft.value}`])])} edit={() => setStep(3)} />
          </dl>
        )}
      </section>
      {refusal !== null && <p className="mt-3 text-base text-critical">{refusal}</p>}
      <span className="mt-5 flex flex-wrap items-center gap-3">
        {step > 1 && <Button variant="ghost" onClick={() => setStep(step - 1)}>Back</Button>}
        {step === STEPS.length
          ? <Button variant="primary" onClick={() => void finish()}>Finish setup</Button>
          : step === 2 && nobody
            ? <Button disabled reason={NOBODY}>Continue</Button>
            : <Button variant="primary" onClick={() => setStep(step + 1)}>Continue</Button>}
      </span>
    </main>
  );
}
