import { useEffect, useState } from "react";
import { getPreferences } from "../api";
import { AUTH } from "../strings";
import { Avatar } from "./Avatar";
import { Pill } from "./Pill";

// Whose access a click hands over, on the device and production approval pages: a strip of its own
// directly above the buttons, the name in bold, and an admin said as one — an admin approved two CLI
// codes without noticing it was the admin approving. The role is the preferences read's viewer; a
// read that fails names no role, never a guess.
export function ApprovingAs({ approver, onSwitch }: { approver: { display: string; email: string }; onSwitch: () => void }) {
  const [admin, setAdmin] = useState(false);
  useEffect(() => { getPreferences().then((read) => setAdmin(read.viewer?.role === "admin"), () => {}); }, []);
  return (
    <section aria-label="Approving as" className="flex items-center gap-3 rounded-lg border border-border-strong bg-surface-2 px-3.5 py-3 text-md">
      <Avatar name={approver.display} />
      <div className="min-w-0 flex-1 leading-snug">
        <p className="flex flex-wrap items-center gap-x-1.5">
          <span>{AUTH.approvingAs}<strong className="font-semibold">{approver.display}</strong></span>
          {admin && <Pill tone="amber">{AUTH.role.admin}</Pill>}
        </p>
        <p className="text-text-dim [overflow-wrap:anywhere]">{approver.email}</p>
      </div>
      <button type="button" className="flex-none font-medium text-link hover:underline" onClick={onSwitch}>{AUTH.deviceSwitch}</button>
    </section>
  );
}
