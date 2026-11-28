import { useEffect, useRef } from "react";
import { SAVE } from "../strings";
import { Button } from "./Button";
import { Modal } from "./Modal";

// Typed changes not yet saved, by the section holding them (design/EXPERIENCE.md, D16): while any is held, leaving the page
// asks first, and so does a reload or a closed tab. A section holds one while what it shows differs from what is saved.
const held = new Set<symbol>();

export function useUnsaved(changed: boolean) {
  const id = useRef(Symbol("unsaved"));
  useEffect(() => {
    if (!changed) return;
    const mine = id.current;
    held.add(mine);
    return () => { held.delete(mine); };
  }, [changed]);
}

export const unsaved = () => held.size > 0;

// Leaving is let through once the person chose to discard: the sections still hold their changes until they unmount.
let discarding = false;
export const leave = (to: string) => {
  discarding = true;
  window.location.hash = to;
};
export const discarded = () => {
  const was = discarding;
  discarding = false;
  return was;
};

// The browser's own question before a reload or a closed tab, while anything is held.
export function guardUnload(event: BeforeUnloadEvent) {
  if (unsaved()) event.preventDefault();
}

// The question before the page changes under unsaved typing: Discard goes where the person was going, Keep editing stays.
export function DiscardQuestion({ to, stay }: { to: string; stay: () => void }) {
  return (
    <Modal label={SAVE.discardTitle} close={stay}>
      <h2 className="text-base font-semibold">{SAVE.discardTitle}</h2>
      <p className="text-base">{SAVE.discardWhy}</p>
      <span className="flex flex-wrap gap-2">
        <Button variant="ghost" onClick={stay}>{SAVE.keepEditing}</Button>
        <Button variant="danger" onClick={() => leave(to)}>{SAVE.discard}</Button>
      </span>
    </Modal>
  );
}
