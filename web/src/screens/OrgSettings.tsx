import { useEffect, useState, type ReactNode } from "react";
import { endSession, listSessions, putPreferences, said, type ListedSession, type Overridable, type Preferences, type PreferencesEdit } from "../api";
import { APPROVAL, S, SESSIONS } from "../strings";
import { Button } from "../ui/Button";
import { EnterpriseLine, unlocks, useLicence } from "../ui/Enterprise";

const H2 = "mt-5 text-2xl font-semibold";
const SUB = "mt-1 text-md text-text-dim";
const LABEL = "mb-1 block text-md font-semibold text-text-dim";
const FIELD = "rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const NOTE = "mt-2 text-md text-text-dim";
const PINNED = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";

// The variable each pinned key is set from (design/ONBOARDING.md §1). A read-only field names it,
// so nobody edits a value that will not take, or guesses why the box will not accept typing.
const VARIABLE: Record<Overridable, string> = {
  org_name: "CANYON_ORG",
  allowed_domains: "CANYON_OIDC_ALLOWED_DOMAINS",
  allowed_emails: "CANYON_OIDC_ALLOWED_EMAILS",
  admin_claim: "CANYON_ADMIN_CLAIM",
  admin_value: "CANYON_ADMIN_VALUE",
  responder_claim: "CANYON_RESPONDER_CLAIM",
  responder_value: "CANYON_RESPONDER_VALUE",
  groups_claim: "CANYON_GROUPS_CLAIM",
};

const pinnedIn = (record: Preferences, key: Overridable) => record.overridden.includes(key);
// A demo host answers no viewer and every caller on it is an admin, which is what the server
// decides too — so an absent viewer is not a member.
export const isAdmin = (record: Preferences) => (record.viewer?.role ?? "admin") === "admin";
// The bare domain of an address, or null for a string with no `@` — a half-typed row is not a domain.
export function domainOf(email: string): string | null {
  const at = email.indexOf("@");
  return at === -1 ? null : email.slice(at + 1).toLowerCase();
}

// The domains as a sentence names them: a, a or b, a, b or c.
const listed = (domains: string[]) => (domains.length < 2 ? domains.join("") : `${domains.slice(0, -1).join(", ")} or ${domains.at(-1)}`);

// Values is a list nobody on this screen can change: an operator-pinned key, the bootstrap admins,
// or every list when a member is reading. An empty one says so rather than rendering as a gap.
function Values({ values }: { values: string[] }) {
  if (values.length === 0) return <p className="text-base text-text-dim">Nobody.</p>;
  return <ul className="flex flex-col gap-1">{values.map((one) => <li key={one} className="font-mono text-base">{one}</li>)}</ul>;
}

// List is one editable list of addresses or domains. Nothing is checked here: the shapes, the
// bounds and the three refusals are the server's, and the admin reads them in the server's own
// sentence (contracts/routes.md row 219). `mine` marks the reader's own row `you`.
function List({ name, values, onChange, add, mine = null }: { name: string; values: string[]; onChange: (next: string[]) => void; add: string; mine?: string | null }) {
  const [added, setAdded] = useState(false); // a row the button added takes the cursor; one drawn from the read doesn't
  return (
    <>
      {/* Ordinal labels, so a row keeps its name while its address is being typed; wide enough for a whole address. */}
      {values.map((one, i) => (
        <span key={i} className="mt-1.5 flex items-center gap-1">
          <input aria-label={`${name} ${i + 1}`} className={`${FIELD} max-w-md min-w-0 flex-1`} value={one} autoFocus={added && i === values.length - 1}
            onChange={(event) => onChange(values.map((was, at) => (at === i ? event.target.value : was)))} />
          <button type="button" className="text-base text-link" aria-label={`Remove ${name.toLowerCase()} ${i + 1}`}
            onClick={() => onChange(values.filter((_, at) => at !== i))}>×</button>
          {one === mine && <span className="text-md text-text-dim">you</span>}
        </span>
      ))}
      <span className="mt-2 flex">
        <button type="button" className="text-md font-medium text-link hover:underline" onClick={() => { setAdded(true); onChange([...values, ""]); }}><span aria-hidden="true">+ </span>{add}</button>
      </span>
    </>
  );
}

// Section is one page's form with its one Save and its own refusal: each change is one audit row
// with its before and after, which the preferences route already writes per key. A failed save
// keeps what was typed — you need it to fix what the server refused — and a refusal and "Saved."
// never share the line.
function Section({ name, admin, adopt, body, children }: { name: string; admin: boolean; adopt: (answer: Preferences) => void; body: () => PreferencesEdit; children: ReactNode }) {
  const [refusal, setRefusal] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [sending, setSending] = useState(false);
  const save = async () => {
    if (sending) return;
    setSending(true);
    try {
      adopt(await putPreferences(body()));
      setRefusal(null);
      setSaved(true);
    } catch (err) {
      setSaved(false);
      setRefusal(said(err));
    } finally {
      setSending(false);
    }
  };
  return (
    <section aria-label={name}>
      {children}
      {/* No Save for a member: the route answers them 403, and a button whose only outcome is a
          refusal is not how the screen says an admin changes these. */}
      {admin && (
        <span className="mt-6 flex flex-wrap items-center gap-3 border-t border-border pt-4">
          <Button variant="primary" label={`Save ${name.toLowerCase()}`} onClick={() => void save()}>Save</Button>
          {saved && <span role="status" className="text-base text-ok">Saved.</span>}
          {refusal !== null && <span className="text-base text-critical">{refusal}</span>}
        </span>
      )}
    </section>
  );
}

// Pinned is a key the environment sets: read-only, naming the variable it comes from, so it is an admin's alone; a member
// reads what the key holds, plain (#118). An empty
// list shows the variable and no values, never "Nobody." — a member's read redacts the admin claim
// while `overridden` still names the pair, and a reader who cannot tell a redacted field from an
// unset one reads an operator-pinned claim as no claim at all (contracts/routes.md row 219).
function Pinned({ keys, values }: { keys: Overridable[]; values: string[] }) {
  return (
    <div className={PINNED}>
      {values.length > 0 && <Values values={values} />}
      <p className={NOTE}>Set on the server, by {keys.map((key) => VARIABLE[key]).join(" and ")}. Change it there, not here.</p>
    </div>
  );
}

// The draft the three sections edit, held by whoever draws them rather than by any one of them:
// this page saves each section on its own, and onboarding's four steps write the row once at the
// end. Seeded from the record, so the form holds what the server stored, never what was typed at it.
export function useOrgDraft(record: Preferences) {
  const [orgName, setOrgName] = useState(record.org_name);
  const [emails, setEmails] = useState(record.allowed_emails);
  const [domains, setDomains] = useState(record.allowed_domains);
  const [admins, setAdmins] = useState(record.admin_emails);
  const [claim, setClaim] = useState(record.admin_claim ?? "");
  const [value, setValue] = useState(record.admin_value ?? "");
  return { orgName, setOrgName, emails, setEmails, domains, setDomains, admins, setAdmins, claim, setClaim, value, setValue };
}

// The props every section takes. A section draws its own fields and nothing around them: the
// heading above it and the Save under it belong to whoever is drawing it. `mine` is the reader's
// own address on the screen setting this host up for the first time, and null on the settings
// page: it is what marks a row `you` and what the domain box is about.
interface Fields { record: Preferences; draft: ReturnType<typeof useOrgDraft>; admin: boolean; mine?: string | null }

export function OrganisationFields({ record, draft, admin }: Fields) {
  return (
    <>
      <label className={`${LABEL} mt-3`} htmlFor="org-name">Organization name</label>
      {admin && pinnedIn(record, "org_name")
        ? <Pinned keys={["org_name"]} values={[record.org_name]} />
        : admin
          ? <input id="org-name" className={FIELD} value={draft.orgName} onChange={(event) => draft.setOrgName(event.target.value)} />
          : <p className="text-base">{record.org_name}</p>}
      <p className={NOTE}>Shown on every app's go-live checklist.</p>
    </>
  );
}

export function SignInFields({ record, draft, admin, mine = null }: Fields) {
  const myDomain = mine === null ? null : domainOf(mine);
  // The box owns the reader's own domain and the list below owns every other one, so one domain is
  // never two controls that can disagree about it.
  const others = myDomain === null ? draft.domains : draft.domains.filter((one) => one !== myDomain);
  const ticked = myDomain !== null && draft.domains.includes(myDomain);
  const box = myDomain !== null && admin && !pinnedIn(record, "allowed_domains");
  return (
    <>
      <p className={`${LABEL} mt-3`}>People</p>
      {admin && pinnedIn(record, "allowed_emails")
        ? <Pinned keys={["allowed_emails"]} values={record.allowed_emails} />
        : admin
          ? <List name="Address" values={draft.emails} onChange={draft.setEmails} add="Add person" mine={mine} />
          : record.allowed_emails.length === 0 && record.allowed_domains.length > 0
            // no one by address is not nobody while a domain lets people in (#118)
            ? <p className="text-base text-text-dim">{`No one by address; anyone at ${listed(record.allowed_domains)} can sign in.`}</p>
            : <Values values={record.allowed_emails} />}
      <p className={`${LABEL} mt-3`}>Domains</p>
      {admin && pinnedIn(record, "allowed_domains")
        ? <Pinned keys={["allowed_domains"]} values={record.allowed_domains} />
        : admin
          ? (
            <>
              {/* Never prefilled and never ticked: the reader's address proves their account works,
                  it does not prove the domain is a company's. An admin on a consumer mail provider
                  must not admit that whole provider on one Continue click, and there is no list of
                  consumer domains here to decide that for them (design/ONBOARDING.md §3). */}
              {box && (
                <div className={PINNED}>
                  <label className="flex items-center gap-2 text-base">
                    <input type="checkbox" checked={ticked}
                      onChange={(event) => draft.setDomains(event.target.checked ? [myDomain, ...others] : others)} />
                    {`Allow everyone at ${myDomain}`}
                  </label>
                  <p className={NOTE}>Anyone at this domain who can sign in to your identity provider can sign in to Canyon.</p>
                </div>
              )}
              <List name="Domain" values={others} onChange={(next) => draft.setDomains(ticked ? [myDomain, ...next] : next)} add="Add domain" />
            </>
          )
          : <Values values={record.allowed_domains} />}
      {/* The box already carries the consequence; without a box the lists need it said once. */}
      {!box && <p className={NOTE}>Anyone at an allowed domain who can sign in to your identity provider can sign in to Canyon.</p>}
      {/* Only where the reader can add one: a list the server pins offers no Add. */}
      {admin && !pinnedIn(record, "allowed_emails") && <p className={NOTE}>Canyon does not send invitations. Add an address, then tell that person to open Canyon and sign in.</p>}
    </>
  );
}

export function AdminFields({ record, draft, admin, mine = null }: Fields) {
  // The claim and its value are present together or absent together, and both are absent on a
  // member's read: the pair is whatever the read carries, never a blank standing in for one.
  const pair = [record.admin_claim, record.admin_value].filter((one) => one !== undefined);
  const everyone = [...new Set([...record.admin_emails, ...record.bootstrap_admins])];
  return (
    <>
      {/* Two groups, never one merged list. The environment's admins are answered apart from the
          row's and are not a key a PUT can write, so a single list would let somebody remove one,
          see the save succeed, and have that person still be an admin. */}
      {/* Who the environment makes an admin is everyone's to read, the variable's name an admin's alone (#118): a member
          reads every admin in one list. */}
      {admin && record.bootstrap_admins.length > 0 && (
        <>
          <p className={`${LABEL} mt-3`}>From the environment</p>
          <div className={PINNED}>
            <Values values={record.bootstrap_admins} />
            <p className={NOTE}>Granted by CANYON_ADMINS. They cannot be removed here.</p>
          </div>
        </>
      )}
      <p className={`${LABEL} mt-3`}>By email</p>
      {admin
        ? <List name="Admin" values={draft.admins} onChange={draft.setAdmins} add="Add an admin" mine={mine} />
        : everyone.length === 0 ? <p className="text-base text-text-dim">No one by address; a sign-in group decides.</p> : <Values values={everyone} />}
      {/* The sign-in group is an admin's alone (#118): a member is answered neither the claim nor its value, and the
          variables that pin them are an admin's to read. */}
      {admin && (<>
        <p className={`${LABEL} mt-3`}>By sign-in group (optional)</p>
        {pinnedIn(record, "admin_claim")
          ? <Pinned keys={["admin_claim", "admin_value"]} values={pair} />
          : (
            <span className="flex flex-wrap items-center gap-2">
              <input aria-label="Claim" placeholder="Claim" className={FIELD} value={draft.claim} onChange={(event) => draft.setClaim(event.target.value)} />
              <input aria-label="Value" placeholder="Value" className={FIELD} value={draft.value} onChange={(event) => draft.setValue(event.target.value)} />
            </span>
          )}
        <p className={NOTE}>Anyone whose sign-in carries this claim is an admin. Canyon reads it at sign-in, so this takes effect for you at your next one.</p>
      </>)}
      <p className={NOTE}>Admins change these settings and the policies. Everyone else deploys and promotes; only an admin or a security responder approves a blocking finding.</p>
    </>
  );
}

// The security responders (design/PERMISSIONS.md, Security responders), an admin's alone: the read answers
// the three keys to an admin only, so whoever else reads the page is shown none of it. Like the admins,
// by address or by the sign-in claim whose pair is set both or neither.
function ResponderFields({ record, emails, setEmails, claim, setClaim, value, setValue }: {
  record: Preferences; emails: string[]; setEmails: (next: string[]) => void; claim: string; setClaim: (next: string) => void; value: string; setValue: (next: string) => void;
}) {
  const pair = [record.responder_claim, record.responder_value].filter((one) => one !== undefined);
  return (
    <>
      <p className={SUB}>{S.responders}</p>
      <p className={`${LABEL} mt-3`}>By email</p>
      <List name="Responder" values={emails} onChange={setEmails} add="Add a responder" />
      <p className={`${LABEL} mt-3`}>By sign-in group (optional)</p>
      {pinnedIn(record, "responder_claim")
        ? <Pinned keys={["responder_claim", "responder_value"]} values={pair} />
        : (
          <span className="flex flex-wrap items-center gap-2">
            <input aria-label="Responder claim" placeholder="Claim" className={FIELD} value={claim} onChange={(event) => setClaim(event.target.value)} />
            <input aria-label="Responder value" placeholder="Value" className={FIELD} value={value} onChange={(event) => setValue(event.target.value)} />
          </span>
        )}
      <p className={NOTE}>Anyone whose sign-in carries this claim is a security responder. Canyon reads it at sign-in, so this takes effect at their next one.</p>
    </>
  );
}

// Settings › Organisation: the org's name, saved on its own.
export function OrganisationSettings({ record, adopt }: { record: Preferences; adopt: (answer: Preferences) => void }) {
  const draft = useOrgDraft(record);
  const admin = isAdmin(record);
  return (
    <>
      {!admin && <p className={SUB}>An admin changes these settings.</p>}
      {/* A pinned name has no Save: its only answer is set_by_operator. */}
      <Section name="Organization" admin={admin && !pinnedIn(record, "org_name")} adopt={adopt} body={() => ({ org_name: draft.orgName })}>
        <OrganisationFields record={record} draft={draft} admin={admin} />
      </Section>
    </>
  );
}

// Settings › People & sign-in: who may sign in, who administers Canyon, and the groups claim — the
// same fields as onboarding's steps, in the same order and the same words, because an org changing
// its membership a year later should meet the form it already filled in (design/ONBOARDING.md §3).
// One Save for the page, sending only the keys that changed: a key the environment pins is never
// sent, since its only answer is set_by_operator, and an untouched list is not re-judged by the
// server's membership checks. The record is the page's: a Save's answer is the record.
export function PeopleSettings({ record, adopt }: { record: Preferences; adopt: (answer: Preferences) => void }) {
  const draft = useOrgDraft(record);
  const admin = isAdmin(record);
  const [groups, setGroups] = useState(record.groups_claim);
  const groupsPinned = pinnedIn(record, "groups_claim");
  // Setting or changing the groups claim is Canyon Enterprise's (#2879); clearing it never is.
  const licence = useLicence();
  const groupsUnlocked = unlocks(licence, "group_sync");
  const [prodApproval, setProdApproval] = useState(record.prod_approval);
  const [responders, setResponders] = useState(record.responder_emails ?? []);
  const [responderClaim, setResponderClaim] = useState(record.responder_claim ?? "");
  const [responderValue, setResponderValue] = useState(record.responder_value ?? "");
  const changed = (was: unknown, now: unknown) => JSON.stringify(was) !== JSON.stringify(now);
  const body = (): PreferencesEdit => {
    const edit: PreferencesEdit = {};
    if (changed(record.admin_emails, draft.admins)) edit.admin_emails = draft.admins;
    if (!pinnedIn(record, "allowed_emails") && changed(record.allowed_emails, draft.emails)) edit.allowed_emails = draft.emails;
    if (!pinnedIn(record, "allowed_domains") && changed(record.allowed_domains, draft.domains)) edit.allowed_domains = draft.domains;
    if (!pinnedIn(record, "admin_claim") && changed([record.admin_claim ?? "", record.admin_value ?? ""], [draft.claim, draft.value])) {
      edit.admin_claim = draft.claim === "" ? null : draft.claim;
      edit.admin_value = draft.claim === "" ? null : draft.value;
    }
    if (!groupsPinned && changed(record.groups_claim, groups)) edit.groups_claim = groups;
    if (admin && changed(record.responder_emails ?? [], responders)) edit.responder_emails = responders;
    if (admin && !pinnedIn(record, "responder_claim") && changed([record.responder_claim ?? "", record.responder_value ?? ""], [responderClaim, responderValue])) {
      edit.responder_claim = responderClaim === "" ? null : responderClaim;
      edit.responder_value = responderClaim === "" ? null : responderValue;
    }
    if (record.prod_approval !== prodApproval) edit.prod_approval = prodApproval;
    return edit;
  };

  return (
    <>
      {!admin && <p className={SUB}>An admin changes these settings.</p>}
      <Section name="People and sign-in" admin={admin} adopt={adopt} body={body}>
        <section aria-label="Who may sign in">
          <h2 className={H2}>Who may sign in</h2>
          <SignInFields record={record} draft={draft} admin={admin} />
          <p className={NOTE}>A removal takes effect on that person's next request.</p>
        </section>
        <section aria-label="Who administers Canyon">
          <h2 className={H2}>Who administers Canyon</h2>
          <AdminFields record={record} draft={draft} admin={admin} />
        </section>
        {admin && (
          <section aria-label="Security responders">
            <h2 className={H2}>Security responders</h2>
            <ResponderFields record={record} emails={responders} setEmails={setResponders} claim={responderClaim} setClaim={setResponderClaim} value={responderValue} setValue={setResponderValue} />
          </section>
        )}
        <section aria-label="Groups">
          <h2 className={H2}>Groups</h2>
          <label className={`${LABEL} mt-3`} htmlFor="groups-claim">Groups claim</label>
          {admin && groupsPinned
            ? <Pinned keys={["groups_claim"]} values={[record.groups_claim]} />
            : admin && groupsUnlocked
              ? <input id="groups-claim" className={FIELD} value={groups} onChange={(event) => setGroups(event.target.value)} />
              : admin && licence !== null
                ? (<>
                  <Values values={groups === "" ? [] : [groups]} />
                  {groups !== "" && <span className="mt-1 flex"><Button size="sm" variant="ghost" label="Clear the groups claim" onClick={() => setGroups("")}>Clear</Button></span>}
                  <div className="mt-1"><EnterpriseLine feature="group_sync" /></div>
                </>)
                : <Values values={record.groups_claim === "" ? [] : [record.groups_claim]} />}
          <p className={NOTE}>The sign-in claim that lists a person's groups. Once it is set, owners can share an app with a group. Canyon reads it at sign-in.</p>
        </section>
        <section aria-label="Production">
          <h2 className={H2}>Production</h2>
          <label className="mt-3 flex items-center gap-2 text-base">
            <input type="checkbox" checked={prodApproval} disabled={!admin} onChange={(event) => setProdApproval(event.target.checked)} />
            {APPROVAL.switchLabel}
          </label>
          <p className={NOTE}>{APPROVAL.switchNote}</p>
        </section>
      </Section>
    </>
  );
}

const when = (at: string) => new Date(at).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });

// The reader's live sessions, and everyone's to an admin (contracts/routes.md, Sessions): how each signed in, an MCP
// client by its name, and Disconnect on every one but the reader's own browser session, which keeps Sign out.
export function Sessions({ admin }: { admin: boolean }) {
  const [sessions, setSessions] = useState<ListedSession[] | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  // An answer without the list draws none: the list is a part of the page, and never takes the rest of it down.
  const read = () => listSessions().then((answer) => setSessions(Array.isArray(answer?.sessions) ? answer.sessions : null), (err: unknown) => setFailure(said(err)));
  useEffect(() => { void read(); }, []);
  const end = (id: string) => () => void endSession(id).then(read, (err: unknown) => setFailure(said(err)));
  return (
    <section aria-label={SESSIONS.title} className="mt-8">
      <h2 className={H2}>{SESSIONS.title}</h2>
      <p className={SUB}>{SESSIONS.line}</p>
      {failure !== null && <p className="mt-2 text-md text-critical">{failure}</p>}
      {sessions !== null && (
        // Below 600px a row is two lines, how over when, with Disconnect beside them (#100's review): the columns are a
        // wide screen's, and the second line says what each date is.
        <table className="mt-3 w-full text-left text-md max-[600px]:block">
          <thead className="text-text-dim max-[600px]:hidden">
            <tr>
              {admin && <th className="py-1.5 font-medium">{SESSIONS.person}</th>}
              <th className="py-1.5 font-medium">{SESSIONS.how}</th>
              <th className="py-1.5 font-medium">{SESSIONS.since}</th>
              <th className="py-1.5 font-medium">{SESSIONS.lastUsed}</th>
              <th className="py-1.5 font-medium">{SESSIONS.expires}</th>
              <th className="py-1.5"><span className="sr-only">{SESSIONS.disconnect}</span></th>
            </tr>
          </thead>
          <tbody className="max-[600px]:block">
            {sessions.map((s) => {
              const how = s.flow === "oauth" ? SESSIONS.flow.oauth.replace("<client>", s.client ?? "") : SESSIONS.flow[s.flow];
              const own = s.current && s.flow === "browser";
              const dates = [`${SESSIONS.since} ${when(s.created_at)}`, ...(s.last_seen_at === null ? [] : [`${SESSIONS.lastUsed.toLowerCase()} ${when(s.last_seen_at)}`]), `${SESSIONS.expires.toLowerCase()} ${when(s.expires_at)}`];
              return (
                <tr key={s.id} className="border-t border-border max-[600px]:grid max-[600px]:grid-cols-[minmax(0,1fr)_auto] max-[600px]:items-center max-[600px]:gap-x-3 max-[600px]:py-2">
                  {admin && <td className="py-2 max-[600px]:col-start-1 max-[600px]:py-0 max-[600px]:font-medium">{s.person?.email ?? ""}</td>}
                  <td className="py-2 max-[600px]:col-start-1 max-[600px]:py-0">
                    <span className="max-[600px]:block">{how}{own && <span className="hidden max-[600px]:inline">{` · ${SESSIONS.current.toLowerCase()}`}</span>}</span>
                    <span className="hidden text-sm text-text-dim max-[600px]:block">{dates.join(" · ")}</span>
                  </td>
                  <td className="py-2 max-[600px]:hidden">{when(s.created_at)}</td>
                  <td className="py-2 max-[600px]:hidden">{s.last_seen_at === null ? "" : when(s.last_seen_at)}</td>
                  <td className="py-2 max-[600px]:hidden">{when(s.expires_at)}</td>
                  <td className={`py-2 text-right max-[600px]:col-start-2 max-[600px]:row-start-1 max-[600px]:row-span-3 max-[600px]:py-0 ${own ? "max-[600px]:hidden" : ""}`}>
                    {own
                      ? <span className="text-text-dim">{SESSIONS.current}</span>
                      : <Button size="sm" variant="ghost" label={`${SESSIONS.disconnect} ${s.id}`} onClick={end(s.id)}>{SESSIONS.disconnect}</Button>}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </section>
  );
}
