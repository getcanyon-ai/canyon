import { useState, type Ref } from "react";
import { guardrailsSetup, said, type SetupMessage, type SetupProposal, type SetupWrite } from "../api";
import { Button } from "../ui/Button";
import { POLICY_STATE as WORD } from "../strings";
import { setPolicy } from "./PolicyChange";

const H2 = "mt-5 text-2xl font-semibold";
const SUB = "mt-1 text-md text-text-dim";
const LABEL = "mb-1 block text-md font-semibold text-text-dim";
const FIELD = "rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const CARD = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";

// The transcript is one list. A message is what goes back on the wire; a write and a proposal are
// what came back about it, shown in place and never resent — the contract allows a message `role`
// and `content` and nothing else.
type Item = SetupMessage | { write: SetupWrite } | { proposal: SetupProposal };
const isMessage = (one: Item): one is SetupMessage => "role" in one;

// A row by shape, not by tool: the dispatcher grows tools this bundle has not heard of, and a
// per-tool sentence would be prose invented here about values the server typed.
function Row({ item }: { item: Item }) {
  if (isMessage(item)) {
    // The person's line and the model's differ by weight only. No prefix and no name: a label the
    // model could type itself is a label that can lie about who said something.
    return <p className={`mt-2.5 text-base break-words ${item.role === "user" ? "text-text-dim" : ""}`}>{item.content}</p>;
  }
  if ("write" in item) {
    const w = item.write;
    return <p className="mt-2.5 font-mono text-sm break-all text-text-faint">{`wrote: ${w.tool} — ${JSON.stringify(w.before)} → ${JSON.stringify(w.after)}`}</p>;
  }
  return <Proposal p={item.proposal} />;
}

// The model has no apply tool: the admin's click on this card applies the change, and it applies
// exactly what was proposed — the rule, the state and the grace — so the card offers the one Set
// button that says so and nothing to adjust. Only admins reach this panel.
function Proposal({ p }: { p: SetupProposal }) {
  const [sending, setSending] = useState(false);
  const [done, setDone] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const grace = p.grace_days ?? 0;

  const set = async () => {
    setSending(true);
    setRefusal(null);
    try {
      await setPolicy(p.rule, { state: p.state }, grace);
      setDone(true);
    } catch (err) {
      setRefusal(said(err));
    } finally {
      setSending(false);
    }
  };

  const label = grace === 0 ? `Set to ${WORD[p.state]}` : `Set to ${WORD[p.state]} in ${grace} days`;
  return (
    <div className={CARD}>
      <p className={LABEL}>Proposed policy change</p>
      <p className="text-base">{`${p.rule} → ${p.state}${p.grace_days === undefined ? "" : ` after ${p.grace_days} days`}`}</p>
      {p.would_block.length > 0 && (
        <>
          <p className={`${LABEL} mt-3`}>Would block today</p>
          {p.would_block.map((one) => (
            <p key={one.project} className="text-base">{`${one.project} — ${one.findings} finding${one.findings === 1 ? "" : "s"}`}</p>
          ))}
        </>
      )}
      {done ? (
        <p role="status" className="mt-2.5 text-md text-ok">Set. <a className="text-link" href="#/guardrails/policies">See it in Policies</a></p>
      ) : (
        <>
          <p className="mt-2.5 text-md text-text-dim">Nothing has changed yet. Nothing currently in production is taken down.</p>
          <span className="mt-3 flex flex-wrap gap-2">
            {sending
              ? <Button disabled reason="Setting it.">{label}</Button>
              : <Button variant="primary" onClick={() => void set()}>{label}</Button>}
          </span>
        </>
      )}
      {refusal !== null && <p className="mt-2.5 text-base text-critical">{refusal}</p>}
    </div>
  );
}

// The setup chat, the last section of the guardrails screen and shown only when the host has a
// model. It holds the conversation — the server stores none of it, so a reload empties this and the
// form above is still the record. No poll, no read on mount, and nothing here is bounded: the
// bounds are the server's and the admin reads them in the server's own sentence.
export function SetupPanel({ box, refresh }: { box: Ref<HTMLTextAreaElement>; refresh: () => Promise<void> }) {
  const [items, setItems] = useState<Item[]>([]);
  const [draft, setDraft] = useState("");
  const [empty, setEmpty] = useState(false); // an empty box was sent
  const [refusal, setRefusal] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const send = async () => {
    // The typed line joins the transcript before the call and stays there whatever comes back: a
    // refused turn is retried by typing the next line, and both go — the server has no alternation
    // rule. Order inside a turn: the assistant's line, its writes as the evidence, then the card.
    const sent: Item[] = [...items, { role: "user", content: draft }];
    setItems(sent);
    setDraft("");
    setRefusal(null);
    setBusy(true);
    try {
      const answer = await guardrailsSetup(sent.filter(isMessage));
      setItems([
        ...sent,
        { role: "assistant", content: answer.message },
        ...answer.writes.map((write) => ({ write })),
        ...(answer.proposal === undefined ? [] : [{ proposal: answer.proposal }]),
      ]);
    } catch (err) {
      setRefusal(said(err));
    } finally {
      // Unconditional, and the whole reason it is here: a turn that failed can still have written,
      // and a client that re-read only on 200 would silently lose what the server kept. Send stays
      // "working" until the form is current, so a second turn cannot overtake this re-read.
      await refresh();
      setBusy(false);
    }
  };

  return (
    <section aria-label="Set up with Canyon">
      <h2 className={H2}>Set up with Canyon</h2>
      <p className={SUB}>Say what your teams build. Canyon writes it into the form below, and the form is the record.</p>
      {items.map((item, i) => <Row key={i} item={item} />)}
      {refusal !== null && <p role="status" className="mt-2.5 text-base text-critical">{refusal}</p>}
      {/* A textarea, and no Enter-to-send: a newline is content the server counts. */}
      <label className={`${LABEL} mt-3`} htmlFor="setup-message">Your message</label>
      <textarea id="setup-message" ref={box} rows={3} placeholder="Our teams build React with Vite and Python with FastAPI, using our design system." className={`${FIELD} block w-full max-w-[720px]`}
        value={draft} onChange={(event) => { setDraft(event.target.value); setEmpty(false); }} />
      <span className="mt-2.5 flex flex-wrap items-start gap-2">
        {/* The only content this panel declines to send is no content — the conversation goes back
            whole, so one empty line the server refuses would refuse every later turn too. The
            in-flight reason is the working line: there is no spinner in this codebase. */}
        {busy
          ? <Button disabled reason="Canyon is working on it.">Send</Button>
          : <Button variant="primary" onClick={() => { if (draft === "") setEmpty(true); else void send(); }}>Send</Button>}
        {/* The way out of a conversation the server will not take any more, and nowhere else: an
            always-on button that throws away a working conversation has no job. */}
        {refusal !== null && <Button variant="ghost" onClick={() => { setItems([]); setRefusal(null); }}>Start over</Button>}
      </span>
      {/* Said only once an empty box was sent, never before anyone typed (#99). */}
      {empty && draft === "" && <p role="status" className="mt-1 text-md text-text-dim">Type something first.</p>}
    </section>
  );
}
