import type { Actor as ActorRef } from "../api";
import { S } from "../strings";

// A person shortens to first name and last initial; anyone else keeps its name and takes its kind
// as a mono mark — unless the name already says it ("Dana's agent" is not "Dana's agent agent").
// A blank display name falls back to the subject, but for a host with sign-in off: its one caller, the
// anonymous admin, has no display (contracts/routes.md, `person / anonymous`), and nobody signed in is
// what the screens say of it, never the subject as if it were a name (#2696).
// Every filter and every ownership check keys on subject, never on what is here.
export function shortName(display: string): string {
  const parts = display.trim().split(/\s+/);
  const last = parts[parts.length - 1];
  return parts.length < 2 || last === undefined ? display : `${parts[0]} ${last[0]}.`;
}

export const anonymous = (actor: ActorRef) => actor.kind === "person" && actor.subject === "anonymous" && actor.display === "";

// The name an actor is said by, wherever a screen names one in words.
export const actorName = (actor: ActorRef) => actor.display || (anonymous(actor) ? S.notSignedIn : actor.subject);

export function Actor({ actor }: { actor: ActorRef | null }) {
  if (actor === null) return <span className="text-text-faint">—</span>;
  const name = actorName(actor);
  if (anonymous(actor)) return <span>{name}</span>;
  if (actor.kind === "person") return <span>{shortName(name)}</span>;
  return (
    <span>{name}{!name.toLowerCase().includes(actor.kind) && <span className="ml-1.5 font-mono text-sm text-text-faint">{actor.kind}</span>}</span>
  );
}
