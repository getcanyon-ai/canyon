import { useRef, useState, type FormEvent } from "react";
import { claim, getAccess, said, share, type FleetRow, type Reduced } from "../api";
import { Button } from "../ui/Button";

const FIELD = "h-7 w-[200px] rounded-[7px] border border-border-strong bg-surface px-2.5 text-md";

// One send at a time, and what the server said when it refused: the fleet's poll brings the owner.
function useSend() {
  const [sent, setSent] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const sending = useRef(false);
  const send = async (call: () => Promise<unknown>) => {
    if (sending.current) return;
    sending.current = true;
    try {
      await call();
      setSent(true);
    } catch (err) {
      setFailure(said(err));
    }
    sending.current = false;
  };
  return { sent, failure, send };
}

// Claim this app (design/specs/screens.md §A3.5): the owner set gains whoever clicks, under the
// name the fleet read gives the viewer. A demo host signs nobody in and that name is empty, which
// the server refuses, so there the button asks for one instead. Offered only where the read says the
// server would accept the claim: an app with a secret, a deploy, a repository or a group owner is not.
export function Claim({ app, display }: { app: FleetRow | Reduced; display: string }) {
  const [asking, setAsking] = useState(false);
  const [name, setName] = useState("");
  const { sent, failure, send } = useSend();
  if (sent || !app.claimable) return null;
  return (
    <span className="inline-flex flex-wrap items-center gap-1.5">
      {asking ? (
        <>
          <input aria-label="Your name" placeholder="Your name" className={FIELD} maxLength={128} value={name} onChange={(event) => setName(event.target.value)} />
          {name.trim() === "" ? <Button size="sm" disabled reason={`Enter your name to claim ${app.name}.`}>Claim</Button>
            : <Button size="sm" variant="primary" onClick={() => void send(() => claim(app.id, name.trim()))}>Claim</Button>}
        </>
      ) : (
        <Button size="sm" label={"Claim " + app.name} onClick={() => (display === "" ? setAsking(true) : void send(() => claim(app.id, display)))}>Claim</Button>
      )}
      {failure !== null && <span className="text-md text-critical">{failure}</span>}
    </span>
  );
}

// Assign an owner (the redesign: "Admins see Assign and pick a person"): an admin makes someone the app's
// owner through the share route, which admins may call on any app (contracts/routes.md, Access).
// The route writes a grant whole, so the app roles that person already holds are read first and
// sent again: becoming the owner takes nothing away. The person is named by their sign-in address,
// so only a host with sign-in offers it.
export function Assign({ app }: { app: FleetRow | Reduced }) {
  const [asking, setAsking] = useState(false);
  const [email, setEmail] = useState("");
  const { sent, failure, send } = useSend();
  if (sent) return null;
  const submit = (event: FormEvent) => {
    event.preventDefault();
    const address = email.trim().toLowerCase();
    if (address === "") return;
    void send(async () => {
      const held = (await getAccess(app.id)).entries.find((one) => one.kind === "email" && one.grantee === address);
      return share(app.id, "email", address, { level: "owner", roles: held?.roles ?? [] });
    });
  };
  if (!asking) return <Button size="sm" label={"Assign an owner to " + app.name} onClick={() => setAsking(true)}>Assign</Button>;
  return (
    <form onSubmit={submit} className="inline-flex flex-wrap items-center gap-1.5">
      <input type="email" aria-label={"Owner of " + app.name} placeholder="name@example.com" className={FIELD} maxLength={254} value={email} onChange={(event) => setEmail(event.target.value)} />
      <button type="submit" className="h-7 rounded-[7px] border border-accent bg-accent px-2.5 text-md font-medium text-accent-ink">Assign</button>
      {failure !== null && <span className="text-md text-critical">{failure}</span>}
    </form>
  );
}
