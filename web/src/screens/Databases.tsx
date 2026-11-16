import { useState } from "react";
import { putPreferences, said, type Preferences } from "../api";
import { DATABASES } from "../strings";
import { Button } from "../ui/Button";

const NOTE = "mt-1 text-md text-text-dim";

// Settings › Databases (design/DATABASE-SESSIONS.md §5; #2835): whether people may open database sessions at all. An
// admin turns them on, from this browser alone, as the server requires, or off, which ends every open one; a member
// reads which it is.
export function DatabasesSettings({ record, admin, adopt }: { record: Preferences; admin: boolean; adopt: (row: Preferences) => void }) {
  const [failure, setFailure] = useState<string | null>(null);
  const on = record.db_sessions === "on";
  const turn = (to: "on" | "off") => {
    setFailure(null);
    putPreferences({ db_sessions: to }).then(adopt, (err: unknown) => setFailure(said(err)));
  };
  return (
    <section aria-labelledby="databases" className="mt-2">
      <h2 id="databases" className="text-base font-semibold">{DATABASES.title}</h2>
      <p className={NOTE}>{on ? DATABASES.on : DATABASES.off}</p>
      {admin ? (
        <>
          <p className={NOTE}>{on ? DATABASES.turnOffMeans : DATABASES.turnOnMeans}</p>
          <div className="mt-2">
            <Button variant={on ? "secondary" : "primary"} size="sm" onClick={() => turn(on ? "off" : "on")}>{on ? DATABASES.turnOff : DATABASES.turnOn}</Button>
          </div>
        </>
      ) : <p className={NOTE}>{DATABASES.admin}</p>}
      {failure !== null && <p className="mt-1 text-base text-critical">{failure}</p>}
    </section>
  );
}
