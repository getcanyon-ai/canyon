export async function POST(req: Request) {
  const { id } = await req.json();
  return Response.json(await db.query(sql`SELECT * FROM t WHERE id = ${id}`));
}
