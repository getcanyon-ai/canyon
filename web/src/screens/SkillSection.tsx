import { GO_LIVE, type Manifest, type ManifestPolicy, type RuleId } from "../api";
import { Pill } from "../ui/Pill";

// Copied from Guardrails rather than extracted: CARD and FIELD sit beside them there and have no
// business here, so the three are a coincidence of two files and not yet one thing.
const H2 = "text-base font-semibold";
const SUB = "mt-1 text-md text-text-dim";
const LABEL = "mb-1 block text-md font-semibold text-text-dim";
const MONO = "font-mono text-sm text-text-faint";
const ROW = "flex flex-wrap items-baseline gap-x-2 py-1 text-base";

// What GET /skill/manifest answered, field by field, in the server's own key order. Nothing here is
// derived, sorted or filtered, and nothing on it acts: an admin reads exactly what an agent is
// handed, and every word of it is the server's.
// The recommendation's threshold, in lines. A recommendation and nothing more: nothing is refused
// above it, and the number is Cursor's published guidance for a rules file.
const LONG = 500;

// The AI review's rules that check a scanner rule's policy where no scanner rule of it reads the language,
// each with that scanner rule, and the states a fold knows, in order: the CLI's aiTwins and foldTwin
// (cli/cmd_manifest.go), held to the same cases (cli/testdata/manifest-twins.json).
const AI_TWINS = new Map([["ai.sql-string-concat", "sql.string-concat"], ["ai.code-injection", "code.injection"]]);
const RANK = ["", "advisory", "blocking"]; // an absent production is Off, below both

// An app's own stricter states, which `canyon manifest` folds the twins on in an app's directory (contracts/
// routes.md, app_additions). The org's panel is handed none.
interface Held { id: string; state?: string; production?: string; how?: string }

// foldTwin is the scanner rule's policy with its AI twin's, each as the app holds it, so an agent reads one
// policy once (#2694): the stricter state in each gate, and, when the two differ, a line saying each one's.
// Undefined for a rule off before preview, or a state outside the contract — a production state is advisory
// or blocking, Off when absent (contracts/snapshot.md, Two gates): each keeps its own line.
function foldTwin(scanner: ManifestPolicy, twin: ManifestPolicy): { folded: ManifestPolicy; note?: string } | undefined {
  const rank = (state: string | undefined) => RANK.indexOf(state ?? "");
  if ([scanner, twin].some((one) => rank(one.state) < 1 || rank(one.production) < 0)) return undefined;
  const stricter = (a: string | undefined, b: string | undefined) => (rank(b) > rank(a) ? b : a);
  const folded = { ...scanner, state: stricter(scanner.state, twin.state), production: stricter(scanner.production, twin.production), how: scanner.how || twin.how } as ManifestPolicy; // how is handed only where a rule blocks, so the stricter's
  if (scanner.state === twin.state && scanner.production === twin.production) return { folded };
  const does = (one: ManifestPolicy) =>
    (one.state === "blocking" ? "blocks" : "only warns") + (one.production === undefined || one.production === one.state ? "" : `, and ${one.production === "blocking" ? "blocks production" : "only warns before production"}`);
  return { folded, note: `In languages Canyon's scanners cover, it ${does(scanner)}; in the rest only the AI review checks it, if one is set up, and it ${does(twin)}.` };
}

// handed is the manifest as an agent is handed it, its session hook's `canyon manifest` (design/specs/
// cli.md §7.4, written for an agent): the first line, the host, the policies grouped by what they do —
// a go-live rule, and one off before preview, under production's, but prod.human-approval, which the last line
// says, each that blocks with the host's line on how to pass it under it — then each custom rule the host
// handed text for under the org's own heading, every line of it indented — then the org's playbooks, each by its
// slug, name and when — then the resources, stacks and
// design system, a blank line between sections and a section with no values left out, then outbound while the
// host's via is canyon (outbound: what GET /egress answered), then where production is promoted, always. The panel
// counts that text so the size an admin sees is the size an agent reads at every session start; the
// two renderings are held to the CLI's golden and its table of AI twins' folds.
export function handed(m: Manifest & { app_additions?: Held[] }, outbound = false): string {
  const sections: string[][] = [["Canyon's settings for this org, as canyon manifest prints them. They are in this session already: you need not run canyon manifest."], [`Canyon host: ${m.host}`]];
  const groups = new Map<string, string[]>();
  const own: string[] = [];
  const policies = [...m.policies];
  const held = (one: ManifestPolicy): ManifestPolicy => { // as the app holds it: its addition's states, where stricter
    const add = m.app_additions?.find((a) => a.id === one.id);
    return add === undefined ? one : ({ ...one, state: add.state || one.state, production: add.production || one.production, how: add.how || one.how } as ManifestPolicy);
  };
  const notes = new Map<string, string>();
  for (const twin of m.policies) { // one policy, one line: listed apart, it both blocked and only warned (#2694)
    const scanner = AI_TWINS.get(twin.id);
    const i = policies.findIndex((one) => one.id === scanner);
    const fold = scanner === undefined || i < 0 ? undefined : foldTwin(held(policies[i]!), held(twin));
    if (scanner === undefined || fold === undefined) continue;
    policies[i] = fold.folded;
    if (fold.note !== undefined) notes.set(scanner, fold.note);
    policies.splice(policies.findIndex((one) => one.id === twin.id), 1);
  }
  for (const one of policies) {
    if (one.id === "prod.human-approval") continue; // the last line says it for the agent (cli.md §7.4)
    const live = GO_LIVE.includes(one.id as RuleId);
    const state: string = live ? "off" : one.state;
    const production = live ? one.state : one.production;
    let line = `- ${one.name}`;
    if (state === "off") line += ` (${production === "blocking" ? "blocks" : production === "advisory" ? "only warns" : production})`;
    else if (production !== undefined && production !== state) line += ` (${production === "blocking" ? "blocks production" : production === "advisory" ? "only warns before production" : `${production} before production`})`;
    if (one.how !== undefined) line += `\n  To pass: ${one.how}`;
    const note = notes.get(one.id);
    if (note !== undefined) line += `\n  ${note}`;
    if (one.instructions !== undefined) own.push([`- ${one.name}`, ...one.instructions.split("\n").map((text) => `  ${text}`)].join("\n"));
    groups.set(state, [...(groups.get(state) ?? []), line]);
  }
  const order = (state: string) => ({ blocking: -2, advisory: -1, off: 1 })[state] ?? 0;
  const heading = (state: string) => ({ blocking: "Policies that block:", advisory: "Policies that only warn:", off: "Policies for production only:" })[state] ?? `Policies (${state}):`;
  for (const state of [...groups.keys()].sort((a, b) => order(a) - order(b))) sections.push([heading(state), ...(groups.get(state) ?? [])]);
  if (own.length > 0) sections.push(["Your org's own rules (written by its admins):", ...own]);
  const books = m.playbooks ?? []; // the org's playbooks, by slug, never a body (cli.md §7.4)
  if (books.length > 0) sections.push(["Your org's playbooks, how it builds, written by its admins. Before a task, read each one whose when fits it: call canyon_playbook with its slug where Canyon's tools are loaded, or run canyon playbook <slug>.", ...books.map((one) => `- ${one.slug}: ${one.name}. When: ${one.when}`)]);
  if (m.component_types.length > 0) sections.push([`Resources an app can declare in canyon.toml: ${m.component_types.join(", ")}`]);
  if (m.stacks.length > 0) sections.push(["Approved stacks:", ...m.stacks.map((one) => `- ${one.language}${one.frameworks.length > 0 ? `: ${one.frameworks.join(", ")}` : ""}`)]);
  const ds = m.design_system;
  if (ds !== null) sections.push([`Design system: ${ds.name} (${ds.namespace})`, `- registry: ${ds.registry_url}`, ...(ds.tokens_url !== undefined ? [`- tokens: ${ds.tokens_url}`] : [])]);
  if (outbound) sections.push(["Outbound: an app's environment may be under an allowlist, where Canyon refuses calls to any outside service not on it. Only a person on the app's team can allow a new one, in Canyon's dashboard; you cannot. Never try to reach a refused service another way. Canyon also refuses any call out to the internet to an IP address, a one-word host name or a port other than 80 and 443, and no one can allow it: call a host name on 80 or 443."]); // once, where calls go through Canyon: agents never widen it (cli.md §7.4)
  sections.push(["Secrets: never ask for or accept a secret's value in this conversation. When the app needs one, give the user canyon secret set NAME to run in their own terminal, which reads the value there."]); // always said: a value pasted into a chat is in the agent's transcript (cli.md §7.4)
  sections.push([m.prod_approval
    ? "Production: a person on the app's team approves it in Canyon's dashboard. When the user asks for production, call canyon_promote, or run canyon deploy --env prod where Canyon's tools aren't loaded; Canyon answers WAITING FOR APPROVAL with a link: give the link to the user and stop. Never open or approve it yourself."
    : "Production: when the user asks for production, call canyon_promote, setting confirm only when they asked for production in this conversation; where Canyon's tools aren't loaded, run canyon deploy --env prod, adding --yes only then. It promotes; nobody else approves it."]); // always said: an agent never guesses
  return sections.map((one) => one.join("\n")).join("\n\n");
}

export function SkillSection({ manifest, org, outbound }: { manifest: Manifest; org: string; outbound: boolean }) {
  const ds = manifest.design_system;
  const text = handed(manifest, outbound);
  const lines = text.split("\n").length;
  const tokens = Math.ceil(text.length / 4); // four characters a token, the published rule of thumb: "about"
  return (
    <section aria-label="What agents are told" className="mt-6 rounded-xl border border-border bg-surface px-4 py-3.5">
      <h2 className={H2}>What agents are told</h2>
      <p className={SUB}>{`Every coding agent in ${org} reads this at the start of each session. It's built from the settings above.`}</p>
      <p className="mt-1 text-sm text-text-faint">{`${lines} lines · ~${tokens} tokens`}</p>
      {lines > LONG && <p role="status" className="mt-1 text-base">Over {LONG} lines. Agents read all of it every session: consider retiring policies or stacks that no longer apply.</p>}
      <p className={`${LABEL} mt-3`}>Canyon host</p>
      {/* Text, never an anchor. This string is server configuration written into a builder's agent
          config; making it a navigation target on an admin's screen buys nothing. */}
      <p className={MONO}>{manifest.host}</p>
      <p className={`${LABEL} mt-3`}>Policies</p>
      {/* Every rule the server sent, in its order. The client filters nothing: an `off` rule is
          dropped server-side and cannot arrive, so a filter here could only hide a rule the agent
          really is handed. The tone is a ternary and not a lookup table, so a state outside the
          union still prints its word in the neutral face instead of composing an undefined class. */}
      {manifest.policies.map((one) => (
        <p key={one.id} className={ROW}>
          <span className="font-medium">{one.name}</span>
          <Pill tone={one.state === "blocking" ? "critical" : "amber"}>{one.state}</Pill>
          <span className={`${MONO} max-[860px]:basis-full`}>{one.id}</span>
        </p>
      ))}
      <p className={`${LABEL} mt-3`}>Component types</p>
      {/* Mono text and not pills: a pill's tones are statuses, and a tinted `postgres` says
          something false. */}
      <p className={MONO}>{manifest.component_types.join(" · ")}</p>
      <p className={`${LABEL} mt-3`}>Approved stacks</p>
      {manifest.stacks.length === 0 && <p className="text-base text-text-dim">No approved stacks yet.</p>}
      {manifest.stacks.map((one) => (
        <p key={one.language} className={ROW}>
          <span className="font-semibold">{one.language}</span>
          <span className={MONO}>{one.frameworks.join(" · ")}</span>
        </p>
      ))}
      <p className={`${LABEL} mt-3`}>Design system</p>
      {ds === null ? <p className="text-base text-text-dim">No design system yet.</p> : (
        <>
          <p className={ROW}>
            <span className="font-semibold">{ds.name}</span>
            <span className={MONO}>{ds.namespace}</span>
          </p>
          <p className={MONO}>{ds.registry_url}</p>
          {/* Only when the key is present: an absent tokens URL is unset, never an empty line. */}
          {ds.tokens_url !== undefined && <p className={MONO}>{ds.tokens_url}</p>}
        </>
      )}
      <p className={`${LABEL} mt-3`}>Production</p>
      <p className="text-base">{manifest.prod_approval ? "Approved by a person in the dashboard" : "Promoted from the CLI or the dashboard"}</p>
    </section>
  );
}
