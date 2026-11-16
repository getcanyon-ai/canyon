export async function GET(req: Request, { params }: { params: { id: string } }) {
  return Response.json(await db.query(raw`SELECT * FROM t WHERE id = ${req.url}`));
}
