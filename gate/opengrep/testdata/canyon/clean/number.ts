export async function GET(req: Request) {
  const n = Number(new URL(req.url).searchParams.get("n"));
  return Response.json(await db.query(`SELECT * FROM t LIMIT ${n}`));
}
