"use client";

import { useState } from "react";
import { createCheckout } from "@/lib/payments";

// Settle an approved claim by card. Client component: it drives the button state.
export function CheckoutPanel({ claimId, amountCents }: { claimId: string; amountCents: number }) {
  const [state, setState] = useState<"idle" | "working" | "done">("idle");
  async function settle() {
    setState("working");
    await createCheckout([{ claimId, amountCents }]);
    setState("done");
  }
  return (
    <button onClick={settle} disabled={state !== "idle"}>
      {state === "done" ? "Settled" : state === "working" ? "Settling…" : `Settle ${claimId}`}
    </button>
  );
}
