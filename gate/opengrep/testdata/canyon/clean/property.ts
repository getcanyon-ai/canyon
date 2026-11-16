export async function GET(req: Request, { params }: { params: { id: string } }) {
  const q = 1;
  return Response.json(await db.query(`SELECT * FROM t LIMIT ${x.req}`));
}
