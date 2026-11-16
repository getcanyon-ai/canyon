// An app's name, and its id beside it when the two differ: a name may repeat across apps, the id is
// the app's address and never does (contracts/routes.md, POST /projects; #1911).
export function AppName({ app }: { app: { id: string; name: string } }) {
  return (
    <>
      {app.name}
      {app.name !== app.id && <>{" "}<span className="ml-1 font-mono text-md font-normal tracking-normal text-text-faint">{app.id}</span></>}
    </>
  );
}

// The name each app goes by in a list: its display name, with its id beside it where two apps in the
// list share the name, since names repeat and ids never do (#1911).
export function labels(apps: { id: string; name: string }[]): Map<string, string> {
  const count = new Map<string, number>();
  for (const app of apps) count.set(app.name, (count.get(app.name) ?? 0) + 1);
  return new Map(apps.map((app) => [app.id, count.get(app.name)! > 1 ? `${app.name} (${app.id})` : app.name]));
}
