export async function GET(req: Request, { params }: { params: { id: string } }) {
  const u = new URL(req.url);
  const { searchParams: sp } = u, n = 1;
  const q = sp.get("q");
  return Response.json(await db.query("WITH x AS (SELECT 1) SELECT * FROM t WHERE q = '" + q + "'"));
}
