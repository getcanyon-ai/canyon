import Database from "better-sqlite3";
const db = new Database("app.db");

export async function POST(req: Request) {
  const { name } = await req.json();
  db.prepare(`INSERT INTO t (name) VALUES ('${name}')`).run();
  return Response.json({});
}
