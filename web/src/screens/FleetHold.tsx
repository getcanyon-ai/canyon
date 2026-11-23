import type { ReactNode } from "react";
import type { App, Reduced, Viewer } from "../api";
import { S } from "../strings";
import { Button } from "../ui/Button";
import { Icon } from "../ui/Icon";
import { empty, lifts, stops, type Acting } from "./HoldDialog";

type Row = App | Reduced;

// A row's production while held, and what the reader may do about it, beside what the cell shows otherwise.
export function HoldCell({ app, role, act, children }: { app: Row; role: Viewer["role"]; act: (acting: Omit<Acting, "asked">) => void; children: ReactNode }) {
  const stop = stops(role, app);
  const lift = lifts(role, app);
  if (app.hold === undefined && !stop) return <>{children}</>;
  return (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
      {app.hold !== undefined ? <span className="inline-flex items-center gap-1 font-medium text-critical"><Icon name="blocked" size={14} />{S.heldMark}</span> : children}
      {stop && <Button variant="secondary" size="sm" label={`${empty(app) ? S.holdProduction : S.stopProduction} of ${app.name}`} onClick={() => act({ app, kind: "stop" })}>{empty(app) ? S.holdProduction : S.stopProduction}</Button>}
      {lift && <Button variant="secondary" size="sm" label={`${S.liftHold} on ${app.name}`} onClick={() => act({ app, kind: "lift" })}>{S.liftHold}</Button>}
    </span>
  );
}
