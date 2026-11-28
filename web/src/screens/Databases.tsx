import { useState } from "react";
import { putPreferences, type Preferences } from "../api";
import { DATABASES } from "../strings";
import { AskFirst } from "../ui/AskFirst";
import { Button } from "../ui/Button";
import { useSaveChoice } from "../ui/Saved";

const NOTE = "mt-1 text-md text-text-dim";

// Settings › Databases (design/DATABASE-SESSIONS.md §5; #2835): whether people may open database sessions at all. An
// admin turns them on, from this browser alone, as the server requires, or off, which ends every open one; a member
// reads which it is. Either way asks first (D16): turning them on opens a way into apps' databases, and turning them
// off ends every open session.
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
            ? <AskFirst question={on ? DATABASES.turnOffAsk : DATABASES.turnOnAsk} yes={on ? DATABASES.turnOff : DATABASES.turnOn} danger={on} busy={choice.sending}
              onYes={() => turn(on ? "off" : "on")} onNo={() => setAsking(false)}>
              <p className="text-md">{on ? DATABASES.turnOffMeans : DATABASES.turnOnMeans}</p>
            </AskFirst>
            : (
              <div className="mt-2">
                <Button variant={on ? "secondary" : "primary"} size="sm" onClick={() => { choice.clear(); setAsking(true); }}>{on ? DATABASES.turnOff : DATABASES.turnOn}</Button>
              </div>
            )}
        </>
      ) : <p className={NOTE}>{DATABASES.admin}</p>}
      {choice.line()}
    </section>
  );
}
