import { useState } from "react";
import { said } from "../api";
import { SAVE } from "../strings";

// What a change says once the server has it (design/EXPERIENCE.md, D16): "Saved." everywhere, then what more is true of it,
// if anything. pending is the line for a save some part of Canyon hasn't taken yet, said in place of it.
export function Saved({ then, pending = null }: { then?: string; pending?: string | null }) {
  if (pending !== null) return <p role="status" className="mt-2 text-md text-amber">{pending}</p>;
  return <p role="status" className="mt-2 text-md text-ok">{then === undefined ? SAVE.saved : `${SAVE.saved} ${then}`}</p>;
}

// A choice that saves the moment it is made (D16): what it says once the server has it, or the server's refusal, and
// whether it is still on its way. `then` and `pending` are as Saved's.
export function useSaveChoice() {
  const [state, setState] = useState<{ sending: boolean; saved: boolean; refusal: string | null }>({ sending: false, saved: false, refusal: null });
  const save = async (send: () => Promise<unknown>) => {
    setState({ sending: true, saved: false, refusal: null });
    try {
      await send();
      setState({ sending: false, saved: true, refusal: null });
    } catch (err) {
      setState({ sending: false, saved: false, refusal: said(err) });
    }
  };
  const line = (then?: string, pending: string | null = null) => state.refusal !== null ? <p role="alert" className="mt-2 text-md text-critical">{state.refusal}</p>
    : state.saved ? <Saved then={then} pending={pending} /> : null;
  return { save, line, sending: state.sending, clear: () => setState({ sending: false, saved: false, refusal: null }) };
}
