export async function GET(req: Request, { params }: { params: { id: string } }) {
  const n = 5;
  return Response.json(await db.query(`SELECT * FROM t LIMIT ${n}`, [req.url]));
}
