import type { RunRead } from "../api";

// The one file a stock starter is missing, why, and each example of what goes in it, whole
// (contracts/routes.md, What a starter is missing).
export function Missing({ run, fix }: { run: string; fix: NonNullable<RunRead["fix"]> }) {
  const heading = `missing-${run}`;
  return (
    <section aria-labelledby={heading} className="grid gap-1.5 rounded-[10px] bg-critical-dim px-4 py-3">
      <h3 id={heading} className="text-md font-semibold text-text-dim">{`The app is missing a ${fix.file}`}</h3>
      <p className="text-base font-semibold break-words">{fix.why}</p>
      <p className="text-md text-text-dim">{`Add a file named ${fix.file} at the app's root, holding the lines for this app.`}</p>
      {fix.examples.map((one) => (
        <div key={one.for} className="grid gap-1">
          <p className="text-md text-text-dim">{`For ${one.for}:`}</p>
          <pre className="overflow-x-auto font-mono text-sm whitespace-pre-wrap text-text">{one.text}</pre>
        </div>
      ))}
    </section>
  );
}
