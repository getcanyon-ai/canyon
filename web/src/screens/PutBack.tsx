import { useEffect, useRef, useState } from "react";
import { ApiError, cancelRun, getRun, getStatus, promote, putBack, said, version, type Earlier, type Env, type Hold, type Receipt as ReceiptRead, type RunRead } from "../api";
import { S } from "../strings";
import { Actor } from "../ui/Actor";
import { Button } from "../ui/Button";
import { shortDate, TONE, verdict } from "../ui/Verdict";
import { blockedBy, PromotionProgress } from "./Promote";

const POLL_MS = 2000;
const going = (run: RunRead | null) => run === null || run.state === "pending" || run.state === "running";
// The promotion refusals that mean the review must be asked again: its confirm is spent.
const SPENT = new Set(["outdated", "nothing_to_ship"]);

// Production's earlier versions, on its card (design/OPERATIONS.md §1.7): each can be put back. Put back
// reviews that package under today's rules, preview untouched; once the review passes, the card offers to
// promote it with the confirm's lines, and the promotion ships the exact image production ran. The status
// read that answers earlier[] is a contributor's and above, so a reader below never reaches this card.
export function EarlierVersions({ id, earlier, prod, hold, ended }: { id: string; earlier: Earlier[]; prod: Env | null; hold?: Hold; ended: () => void }) {
  const [asked, setAsked] = useState<{ row: Earlier; run: string } | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [unread, setUnread] = useState<string | null>(null); // a read of the review that failed; the next one is asked anyway
  const [run, setRun] = useState<RunRead | null>(null);
  const [receipt, setReceipt] = useState<ReceiptRead | null>(null);
  const [spent, setSpent] = useState(false);
  const [following, setFollowing] = useState<{ run: string; at: number } | null>(null); // the promotion, and the Put back it promoted
  const [busy, setBusy] = useState<string | null>(null); // what is being asked: a row's package, "promote" or "cancel"
  const asking = useRef(false); // one request at a time: a double click is never a second run
  const generation = useRef(0); // which Put back is on screen: an answer for an earlier one is dropped, even before the screen redraws
  const progress = useRef<HTMLDivElement>(null);

  const show = (next: { row: Earlier; run: string } | null) => {
    generation.current += 1;
    setAsked(next);
  };

  const once = async (what: string, ask: () => Promise<void>) => {
    if (asking.current) return;
    asking.current = true;
    setBusy(what);
    setFailure(null);
    try {
      await ask();
    } finally {
      asking.current = false;
      setBusy(null);
    }
  };

  const start = (row: Earlier) => once(row.package, async () => {
    try {
      const { run: started } = await putBack(id, row.package, row.image_digest);
      setRun(null);
      setReceipt(null);
      setUnread(null);
      setSpent(false);
      setFollowing(null); // a promotion still going would have refused the review: it has ended
      show({ row, run: started.id });
    } catch (err) {
      setFailure(said(err)); // not_earlier, run_in_progress, rate_limited: the server's sentence
    }
  });

  // The review is read until it ends, each read after the last has answered, then its receipt: whether
  // a promotion of that run can ship. A failed read is said, and the next one asked all the same. A review
  // that blocks is read again while its receipt does: its findings are approved on its run's page (#2198).
  const review = asked?.run ?? null;
  useEffect(() => {
    if (review === null) return;
    const mine = generation.current;
    let timer: ReturnType<typeof setTimeout>;
    const read = async () => {
      try {
        const { run: at } = await getRun(review);
        if (generation.current !== mine) return;
        setUnread(null);
        setRun(at);
        if (going(at)) { timer = setTimeout(() => void read(), POLL_MS); return; }
        const { receipt: answer } = await getStatus(id, review);
        if (generation.current !== mine) return;
        setReceipt(answer);
        if (at.state === "blocked" && blockedBy(answer) !== null) timer = setTimeout(() => void read(), POLL_MS);
      } catch (err) {
        if (generation.current !== mine) return;
        setUnread(said(err));
        timer = setTimeout(() => void read(), POLL_MS);
      }
    };
    void read();
    return () => { generation.current += 1; clearTimeout(timer); };
  }, [id, review]);

  const send = () => once("promote", async () => {
    if (review === null) return;
    try {
      const { run: promoting } = await promote(id, review, false);
      setFollowing(promoting === undefined ? null : { run: promoting.id, at: generation.current });
    } catch (err) {
      const body = err instanceof ApiError ? (err.body as { error?: string; receipt?: ReceiptRead } | null) : null;
      if (err instanceof ApiError && err.status === 409 && body?.receipt !== undefined) setReceipt(body.receipt);
      else {
        setFailure(said(err)); // nothing_to_ship, outdated, run_in_progress: the server's sentence
        if (body?.error !== undefined && SPENT.has(body.error)) setSpent(true);
      }
    }
  });

  // The keyboard follows the promotion, as Promote's question does: the button it pressed is gone.
  useEffect(() => { if (following !== null) progress.current?.focus(); }, [following]);

  // A review that ended before the cancel reached it (`ended`) has nothing to cancel: the card closes quietly, as Promote's does.
  const cancel = () => once("cancel", async () => {
    if (review !== null && going(run)) {
      try {
        await cancelRun(review);
      } catch (err) {
        if (((err as ApiError).body as { error?: string } | null)?.error !== "ended") {
          setFailure(said(err));
          return;
        }
      }
    }
    show(null);
  });

  const shown = asked?.row.version ?? "";
  const reviewing = asked !== null && going(run);
  const gone = !reviewing && run?.state === "failed" && run.failure_reason === "image_gone";
  const why = receipt === null || gone ? null : blockedBy(receipt, hold); // the hold as the page reads it (#2696)
  const ships = receipt?.ships;
  // Fail closed, as the CLI does: a passing review that ships no image, or another than the row production ran, promotes nothing.
  const open = asked !== null && receipt !== null && why === null && !gone && !spent;
  const promotable = open && ships !== undefined && ships.image_digest === asked.row.image_digest;
  const replaces = prod === null ? null : version(prod.package, prod.label);
  // What production refuses of what a person dismissed, an advisory row: said before anything is sent (contracts/routes.md).
  const refuses = receipt?.rules.find((rule) => rule.id === "prod.egress-allowed" && !rule.passed && !rule.blocks);
  return (
    <div className="col-span-full mt-2 grid gap-2 border-t border-border pt-2">
      <details>
        <summary className="cursor-pointer text-sm font-semibold text-text-dim">{`Earlier versions · ${earlier.length}`}</summary>
        <section aria-label="Earlier versions">
          <ul className="mt-1.5 grid gap-1.5">
            {earlier.map((row) => (
              <li key={`${row.package} ${row.image_digest}`} className="flex flex-wrap items-center justify-between gap-x-2 gap-y-1 text-sm">
                <span className="min-w-0">
                  <bdi className="tnum">{row.patched ? S.patched.replace("<version>", () => row.version).replace("<date>", () => shortDate(row.deployed_at)) : `${row.version} · ${shortDate(row.deployed_at)}`}</bdi>
                  {row.promoted_by !== null && <span className="text-text-faint"> · promoted by <Actor actor={row.promoted_by} /></span>}
                </span>
                {/* An image Canyon no longer keeps can't be put back as it ran: no review is spent finding that out. */}
                {row.kept
                  ? <Button variant="ghost" size="sm" label={`Put back ${row.version}`} onClick={() => void start(row)}>{busy === row.package ? "Putting back…" : "Put back"}</Button>
                  : <Button disabled size="sm" label={`Put back ${row.version}`} reason={S.imageGone.replaceAll("<version>", () => row.version)}>Put back</Button>}
              </li>
            ))}
          </ul>
        </section>
      </details>
      <div aria-live="polite" className="grid gap-1.5 text-md text-text-dim">
        {asked !== null && (
          <>
            {reviewing && <p>{S.putBackReviewing.replace("<version>", () => shown)}</p>}
            {run !== null && !reviewing && !gone && run.state !== "clean" && <p className={TONE[run.state]}>{verdict(run, shown)}</p>}
            <a className="text-sm text-link no-underline hover:underline" href={`#/apps/${id}?run=${encodeURIComponent(asked.run)}`} target="_blank" rel="noopener">the run →</a>
            {unread !== null && <p className="text-critical">{unread}</p>}
            {gone && <p className="text-critical">{S.imageGone.replaceAll("<version>", () => shown)}</p>}
            {why !== null && <Button disabled reason={why}>{`Promote ${shown}`}</Button>}
            {open && !promotable && <p className="text-critical">{S.putBackNotThat.replace("<version>", () => shown)}</p>}
            {promotable && following === null && (
              <>
                <p>{`${S.putBack.replace("<version>", () => ships.version)} ${replaces === null ? S.putBackNothing : S.putBackReplaces.replace("<version>", () => replaces)}`}</p>
                <p>{S.putBackImage.replace("<date>", () => shortDate(asked.row.deployed_at))}</p>
                {/* the first <version> is what goes back, the second what it replaces */}
                <p>{replaces === null ? S.putBackCodeBare : S.putBackCode.replace("<version>", () => ships.version).replace("<version>", () => replaces)}</p>
                {refuses !== undefined && <p className="text-amber">{refuses.why}</p>}
                <span><Button variant="primary" onClick={() => void send()}>{busy === "promote" ? `Promoting ${ships.version}…` : `Promote ${ships.version}`}</Button></span>
              </>
            )}
            {busy === null && following === null && (
              <span><Button variant="ghost" size="sm" onClick={() => void cancel()}>{reviewing ? "Cancel review" : "Close"}</Button></span>
            )}
          </>
        )}
        {/* The promotion shows to its end whatever happens to the review above; once it ends the review is done with. */}
        {following !== null && (
          <div ref={progress} tabIndex={-1} className="outline-none">
            <PromotionProgress key={following.run} run={following.run} ended={() => { if (generation.current === following.at) show(null); ended(); }} />
          </div>
        )}
        {failure !== null && <p className="text-critical">{failure}</p>}
      </div>
    </div>
  );
}
