import { useState } from "react";
import { putPreferences, type Preferences } from "../api";
import { DATABASES } from "../strings";
import { AskFirst } from "../ui/AskFirst";
import { Button } from "../ui/Button";
import { useSaveChoice } from "../ui/Saved";

const NOTE = "mt-1 text-md text-text-dim";

// Settings › Databases (design/DATABASE-SESSIONS.md §5; #2835): whether people may open database sessions at all. An
// admin turns them on, from this browser alone, as the server requires, or off, which ends every open one; a member
// reads which it is. Turning them on saves as it is pressed; turning them off asks first, since it ends every open
// session (D16).
export function DatabasesSettings({ record, admin, adopt }: { record: Preferences; admin: boolean; adopt: (row: Preferences) => void }) {
  const [asking, setAsking] = useState(false);
  const choice = useSaveChoice();
  const on = record.db_sessions === "on";
  const turn = (to: "on" | "off") => void choice.save(() => putPreferences({ db_sessions: to }).then(adopt)).then(() => setAsking(false));
  return (
    <section aria-labelledby="databases" className="mt-2">
      <h2 id="databases" className="text-base font-semibold">{DATABASES.title}</h2>
      <p className={NOTE}>{on ? DATABASES.on : DATABASES.off}</p>
      {admin ? (
        <>
          {!on && <p className={NOTE}>{DATABASES.turnOnMeans}</p>}
          {asking
            ? <AskFirst question={DATABASES.turnOffAsk} yes={DATABASES.turnOff} danger busy={choice.sending} onYes={() => turn("off")} onNo={() => setAsking(false)}>
              <p className="text-md">{DATABASES.turnOffMeans}</p>
            </AskFirst>
            : (
              <div className="mt-2">
                <Button variant={on ? "secondary" : "primary"} size="sm" onClick={() => { choice.clear(); if (on) setAsking(true); else turn("on"); }}>{on ? DATABASES.turnOff : DATABASES.turnOn}</Button>
              </div>
            )}
        </>
      ) : <p className={NOTE}>{DATABASES.admin}</p>}
      {choice.line()}
    </section>
  );
}
